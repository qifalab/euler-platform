package main

// Resource ownership for GET /api/v1/monitor/metrics.
//
// The metrics proxy selects a series by resource id alone, and resource ids
// are predictable ({product}-{region}-{shard}-{hex}). The console reaches the
// proxy directly through the gateway route, with no product facade checking
// first, so the proxy must make sure the id is the calling account's before it
// answers — otherwise any tenant can read any other tenant's metrics.
//
// The resource ledger (svc-orchestrator) is the authority:
// GET /api/v1/orchestrator/resources/{id} as the account answers 200 for the
// account's own resource and 404 for anything else (another tenant's resource
// and an unknown id alike). A resource in any lifecycle state counts as owned:
// the history of a released resource is still its owner's to read.
//
// EULER_SVC_ORCHESTRATOR_URL names the ledger. It is required in production
// (pkg-go/runmode); outside production an unset URL skips the check, with a
// warning at startup, so a single-service dev setup keeps working.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/httpmw"
	"github.com/qifalab/euler-platform/runmode"
)

// orchestratorURLEnv names svc-orchestrator's base URL, e.g.
// http://svc-orchestrator.euler-svc-orchestrator.svc:9203.
const orchestratorURLEnv = "EULER_SVC_ORCHESTRATOR_URL"

const (
	// ownerCheckTTL is how long a confirmed ownership is trusted before the
	// ledger is asked again: a dashboard refreshing every few seconds costs
	// one ledger call per resource per TTL, and a resource that changes hands
	// (or a cache entry that should not exist) is corrected within it.
	ownerCheckTTL = 30 * time.Second
	// ownerLookupTimeout bounds one ledger round trip.
	ownerLookupTimeout = 3 * time.Second
	// maxOwnerCacheEntries bounds the verdict cache; past it, expired entries
	// are swept and, if that is not enough, the cache starts over.
	maxOwnerCacheEntries = 10000
	// maxOwnerBody bounds the ledger's resource-detail read.
	maxOwnerBody = 1 << 20
)

// errResourceNotOwned: the ledger says the resource is not the caller's (or
// does not exist — the two are deliberately indistinguishable).
var errResourceNotOwned = errors.New("svc-monitor: resource not found for this account")

// errOwnershipUnconfigured: production without a ledger URL. main() refuses
// to start in that state; this guards stores built without it.
var errOwnershipUnconfigured = errors.New("svc-monitor: resource ownership check not configured (" + orchestratorURLEnv + ")")

// ownerVerifier confirms that a resource belongs to an account.
type ownerVerifier interface {
	Owns(ctx context.Context, accountID int64, resourceID string) (bool, error)
}

// orchestratorOwners asks svc-orchestrator for the resource as the account,
// with the internal shared secret, exactly as the gateway would.
type orchestratorOwners struct {
	base string // scheme://host[:port][/prefix], no trailing slash
	http *http.Client
}

func newOrchestratorOwners(raw string) (*orchestratorOwners, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%s must be an http(s) base URL without query, got %q", orchestratorURLEnv, raw)
	}
	return &orchestratorOwners{
		base: strings.TrimRight(u.String(), "/"),
		http: &http.Client{
			Timeout: ownerLookupTimeout,
			// Internal services never redirect; following one would replay
			// the account header and internal token to another location.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// resourceDetail is the part of the ledger's resource-detail envelope this
// check reads.
type resourceDetail struct {
	Code string `json:"Code"`
	Data struct {
		ResourceID string `json:"resourceId"`
	} `json:"Data"`
}

// Owns answers true for 200 naming the resource, false for 404; anything else
// (transport failure, 5xx, a refused internal token, a 200 that is not the
// resource asked for) is an error — never a verdict either way.
func (o *orchestratorOwners) Owns(ctx context.Context, accountID int64, resourceID string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		o.base+"/api/v1/orchestrator/resources/"+url.PathEscape(resourceID), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(accountIDHeader, strconv.FormatInt(accountID, 10))
	httpmw.SetInternalToken(req)
	resp, err := o.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var detail resourceDetail
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxOwnerBody)).Decode(&detail); err != nil {
			return false, fmt.Errorf("svc-orchestrator resource detail: %w", err)
		}
		if detail.Code != "OK" || detail.Data.ResourceID != resourceID {
			return false, fmt.Errorf("svc-orchestrator answered 200 %q for resource %q", detail.Code, detail.Data.ResourceID)
		}
		return true, nil
	case http.StatusNotFound:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxOwnerBody))
		return false, nil
	default:
		return false, fmt.Errorf("svc-orchestrator answered HTTP %d", resp.StatusCode)
	}
}

// ownerKey is one (account, resource) verdict.
type ownerKey struct {
	account    int64
	resourceID string
}

// resourceOwnership decides whether an account may read a resource's
// metrics, caching confirmed ownership for ownerCheckTTL. Only a confirmed
// "owned" is cached — never a refusal (a resource created a moment later must
// not stay hidden) and never a failed lookup.
type resourceOwnership struct {
	verifier   ownerVerifier // nil: no ledger configured
	production bool
	now        func() time.Time

	mu    sync.Mutex
	owned map[ownerKey]time.Time // confirmed ownership → when it was confirmed
}

// defaultResourceOwnership has no ledger; its production flag comes from
// runmode, so a store built without newResourceOwnershipFromEnv still refuses
// (rather than skips) the check in production.
func defaultResourceOwnership() *resourceOwnership {
	return &resourceOwnership{production: runmode.Production(), now: time.Now, owned: map[ownerKey]time.Time{}}
}

// newResourceOwnershipFromEnv reads EULER_SVC_ORCHESTRATOR_URL: required in
// production, optional (check skipped, one warning) elsewhere. A set but
// unusable URL is a startup error rather than a silently skipped check.
func newResourceOwnershipFromEnv() (*resourceOwnership, error) {
	o := defaultResourceOwnership()
	raw := strings.TrimSpace(os.Getenv(orchestratorURLEnv))
	if raw == "" {
		if o.production {
			return nil, fmt.Errorf("runmode: %s=%s requires %s (metrics resource ownership check)", runmode.EnvVar, runmode.Env(), orchestratorURLEnv)
		}
		slog.Warn("resource ownership check disabled: "+orchestratorURLEnv+" is unset; /api/v1/monitor/metrics serves any resource id (non-production only)", "env", runmode.Env())
		return o, nil
	}
	v, err := newOrchestratorOwners(raw)
	if err != nil {
		return nil, err
	}
	o.verifier = v
	return o, nil
}

// check returns nil when acct may read resourceID's metrics (owned, or no
// ledger outside production), errResourceNotOwned, errOwnershipUnconfigured
// or the lookup error.
func (o *resourceOwnership) check(ctx context.Context, acct int64, resourceID string) error {
	if o.verifier == nil {
		if o.production {
			return errOwnershipUnconfigured
		}
		return nil
	}
	now := o.now()
	key := ownerKey{account: acct, resourceID: resourceID}
	o.mu.Lock()
	at, cached := o.owned[key]
	o.mu.Unlock()
	if cached && !now.Before(at) && now.Sub(at) < ownerCheckTTL {
		return nil
	}
	owned, err := o.verifier.Owns(ctx, acct, resourceID)
	if err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !owned {
		delete(o.owned, key)
		return errResourceNotOwned
	}
	if o.owned == nil {
		o.owned = map[ownerKey]time.Time{}
	}
	if _, exists := o.owned[key]; !exists && len(o.owned) >= maxOwnerCacheEntries {
		for k, t := range o.owned {
			if now.Before(t) || now.Sub(t) >= ownerCheckTTL {
				delete(o.owned, k)
			}
		}
		if len(o.owned) >= maxOwnerCacheEntries {
			o.owned = map[ownerKey]time.Time{}
		}
	}
	o.owned[key] = now
	return nil
}
