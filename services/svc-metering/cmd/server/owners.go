package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/qifalab/euler-platform/httpmw"
)

// ownerVerifier confirms that a resource belongs to an account (and is live)
// before usage readings for it are accepted; checkOwner re-asks it every
// ownerCheckTTL.
//
// Ingest used to adopt the caller as the owner of any resource id it had not
// seen yet. Resource ids are predictable ({product}-{region}-{seq}-{orderId}),
// so a tenant could pre-claim another tenant's resource: the real owner's
// readings were then rejected as "resource belongs to another account" — usage
// that was never billed — or a tenant could fabricate usage for ids it does
// not own. Ownership now comes from the resource ledger (svc-orchestrator).
type ownerVerifier interface {
	Owns(ctx context.Context, accountID int64, resourceID string) (bool, error)
}

// newOwnerVerifier is swapped by tests.
var newOwnerVerifier = func() ownerVerifier { return newOrchestratorOwners() }

// orchestratorOwners asks svc-orchestrator GET /api/v1/orchestrator/resources/{id}
// as the account: 200 = owned, 404 = not this account's resource. A 200 for a
// resource in a dead state (RELEASED, CREATE_FAILED) is not a meterable
// resource: it is reported as errResourceInactive.
type orchestratorOwners struct {
	base string
	http *http.Client
}

func newOrchestratorOwners() orchestratorOwners {
	base := strings.TrimSpace(os.Getenv("EULER_SVC_ORCHESTRATOR_URL"))
	if base == "" {
		base = "http://localhost:9203"
	}
	return orchestratorOwners{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 3 * time.Second}}
}

var errOwnerLookup = errors.New("svc-metering: resource owner lookup failed")

// errResourceNotOwned: the ledger says the resource is not the caller's.
var errResourceNotOwned = errors.New("svc-metering: resource belongs to another account")

// ownerCheckTTL is how long the ledger's verdict on a resource is trusted
// before the next reading asks again — the bound on how long a released
// resource can keep accruing usage.
const ownerCheckTTL = 60 * time.Second

// ownerCheck is a cached ledger verdict: the account it was given for, whether
// the resource was inactive (released / never created), and when it was asked.
type ownerCheck struct {
	account  int64
	inactive bool
	at       time.Time
}

// checkOwner verifies that acct owns resourceID and that the resource is live,
// asking the ledger at most once per ownerCheckTTL per resource (the store's
// injectable clock decides expiry). Only definite verdicts are cached — owned
// and live, or inactive (both terminal states) — never a refusal or a failed
// lookup. It returns nil, errResourceNotOwned, errResourceInactive or a
// lookup error.
func (s *meteringStore) checkOwner(ctx context.Context, acct int64, resourceID string) error {
	now := s.now()
	s.mu.RLock()
	c, cached := s.ownerChecks[resourceID]
	s.mu.RUnlock()
	if cached && c.account == acct && now.Sub(c.at) < ownerCheckTTL && !now.Before(c.at) {
		if c.inactive {
			return errResourceInactive
		}
		return nil
	}
	owned, err := s.owners.Owns(ctx, acct, resourceID)
	switch {
	case errors.Is(err, errResourceInactive):
		s.mu.Lock()
		s.ownerChecks[resourceID] = ownerCheck{account: acct, inactive: true, at: now}
		s.mu.Unlock()
		return errResourceInactive
	case err != nil:
		return err
	case !owned:
		return errResourceNotOwned
	}
	s.mu.Lock()
	s.ownerChecks[resourceID] = ownerCheck{account: acct, at: now}
	s.mu.Unlock()
	return nil
}

// errResourceInactive: the caller owns the resource, but it is in a state that
// accrues no usage — released, or never created. Usage for it is rejected
// rather than billed.
var errResourceInactive = errors.New("svc-metering: resource is not active")

// inactiveStates are the orchestrator lifecycle states (pkg-go/resource) in
// which a resource cannot legitimately produce usage.
var inactiveStates = map[string]bool{
	"RELEASED":      true,
	"CREATE_FAILED": true,
}

// resourceDetail is the part of the orchestrator's resource-detail envelope
// ({RequestId, Code, Data}) this check reads.
type resourceDetail struct {
	Data struct {
		State string `json:"state"`
	} `json:"Data"`
}

func (o orchestratorOwners) Owns(ctx context.Context, accountID int64, resourceID string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		o.base+"/api/v1/orchestrator/resources/"+url.PathEscape(resourceID), nil)
	if err != nil {
		return false, err
	}
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
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&detail); err != nil {
			// Ownership is only half the verdict; an unreadable state fails
			// closed like any other lookup failure.
			return false, errOwnerLookup
		}
		if inactiveStates[strings.ToUpper(detail.Data.State)] {
			return false, errResourceInactive
		}
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, errOwnerLookup
	}
}
