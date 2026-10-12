package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/httpmw"
)

// fakeLedger is svc-orchestrator's GET /api/v1/orchestrator/resources/{id}:
// 200 with the resource detail for its owner, 404 Resource.NotFound for
// everyone else, and the internal-token check every platform service applies.
type fakeLedger struct {
	mu     sync.Mutex
	owners map[string]int64 // resource id → owning account
	fail   int              // >0: answer HTTP fail instead
	asked  []string         // "account:resourceId" per request
	token  []string         // X-Euler-Internal-Token per request
	srv    *httptest.Server
}

func newFakeLedger(t *testing.T, owners map[string]int64) *fakeLedger {
	t.Helper()
	f := &fakeLedger{owners: owners}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/orchestrator/resources/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := r.PathValue("id")
		f.asked = append(f.asked, r.Header.Get(accountIDHeader)+":"+id)
		f.token = append(f.token, r.Header.Get(httpmw.InternalTokenHeader))
		if f.fail > 0 {
			writeErr(w, "Resource.LookupFailed", f.fail, "资源查询失败")
			return
		}
		acct, err := strconv.ParseInt(r.Header.Get(accountIDHeader), 10, 64)
		if owner, ok := f.owners[id]; !ok || err != nil || owner != acct {
			writeErr(w, "Resource.NotFound", 404, "资源不存在")
			return
		}
		writeJSON(w, "OK", map[string]any{"resourceId": id, "state": "RUNNING", "attributes": map[string]string{}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLedger) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked)
}

// ownershipFor wires a resourceOwnership to the fake ledger with a settable clock.
func ownershipFor(t *testing.T, ledgerURL string, now *time.Time) *resourceOwnership {
	t.Helper()
	v, err := newOrchestratorOwners(ledgerURL)
	if err != nil {
		t.Fatal(err)
	}
	o := defaultResourceOwnership()
	o.verifier = v
	o.now = func() time.Time { return *now }
	return o
}

// queryOwned runs GET /api/v1/monitor/metrics as acct with the given
// ownership and a synthetic metrics backend.
func queryOwned(t *testing.T, o *resourceOwnership, acct int64, query string) metricsReply {
	t.Helper()
	s := newRuleStoreWith(newMemRuleRepo())
	s.metrics = syntheticBackend()
	s.owners = o
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/monitor/metrics", s.handleQueryMetrics)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/metrics?"+query, nil)
	req.Header.Set(accountIDHeader, strconv.FormatInt(acct, 10))
	rr := httptest.NewRecorder()
	recoverMiddleware(requestIDMiddleware(mux)).ServeHTTP(rr, req)
	var out metricsReply
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	out.status = rr.Code
	return out
}

func TestMetricsRequireResourceOwnership(t *testing.T) {
	t.Setenv(httpmw.InternalTokenEnv, "tok-1")
	ledger := newFakeLedger(t, map[string]int64{"euecs-cn-north-1-01-a1": 100123, "euecs-cn-north-1-01-b2": 200456})
	now := metricsNow
	o := ownershipFor(t, ledger.srv.URL+"/", &now)

	// The caller's own resource: answered, the ledger asked as the caller
	// with the internal token.
	got := queryOwned(t, o, 100123, "resourceId=euecs-cn-north-1-01-a1")
	if got.status != 200 || got.Code != "OK" || got.Data.ResourceID != "euecs-cn-north-1-01-a1" || len(got.Data.Points) == 0 {
		t.Fatalf("own resource: %d %s %+v", got.status, got.Code, got.Data)
	}
	if ledger.asked[0] != "100123:euecs-cn-north-1-01-a1" || ledger.token[0] != "tok-1" {
		t.Fatalf("ledger asked %v with tokens %v", ledger.asked, ledger.token)
	}

	// Another tenant's resource and an unknown id answer alike.
	for _, id := range []string{"euecs-cn-north-1-01-b2", "euecs-cn-north-1-01-zz"} {
		got := queryOwned(t, o, 100123, "resourceId="+id)
		if got.status != 404 || got.Code != "Monitor.ResourceNotFound" {
			t.Fatalf("%s: %d %s, want 404 Monitor.ResourceNotFound", id, got.status, got.Code)
		}
	}

	// A malformed query is refused before the ledger is asked.
	before := ledger.calls()
	if got := queryOwned(t, o, 100123, "resourceId=euecs-cn-north-1-01-a1&period=5"); got.status != 400 || got.Code != "Common.InvalidParameter" {
		t.Fatalf("bad query: %d %s", got.status, got.Code)
	}
	if ledger.calls() != before {
		t.Fatal("a malformed query reached the ledger")
	}
}

func TestOwnershipCacheTTLAndScope(t *testing.T) {
	ledger := newFakeLedger(t, map[string]int64{"r-1": 100123})
	now := metricsNow
	o := ownershipFor(t, ledger.srv.URL, &now)
	ctx := context.Background()

	if err := o.check(ctx, 100123, "r-1"); err != nil {
		t.Fatal(err)
	}
	// Within the TTL a confirmed ownership is not asked again...
	now = now.Add(ownerCheckTTL - time.Second)
	if err := o.check(ctx, 100123, "r-1"); err != nil || ledger.calls() != 1 {
		t.Fatalf("cached check: %v, ledger calls %d", err, ledger.calls())
	}
	// ...but the cache is per account: another account is always asked.
	if err := o.check(ctx, 200456, "r-1"); err != errResourceNotOwned || ledger.calls() != 2 {
		t.Fatalf("other account: %v, ledger calls %d", err, ledger.calls())
	}
	// Past the TTL the ledger is asked again and its new verdict wins.
	now = now.Add(2 * time.Second)
	ledger.mu.Lock()
	ledger.owners["r-1"] = 200456 // the resource changed hands
	ledger.mu.Unlock()
	if err := o.check(ctx, 100123, "r-1"); err != errResourceNotOwned || ledger.calls() != 3 {
		t.Fatalf("expired check: %v, ledger calls %d", err, ledger.calls())
	}
	// A refusal is never cached: the resource appearing is seen at once.
	ledger.mu.Lock()
	ledger.owners["r-1"] = 100123
	ledger.mu.Unlock()
	if err := o.check(ctx, 100123, "r-1"); err != nil || ledger.calls() != 4 {
		t.Fatalf("after refusal: %v, ledger calls %d", err, ledger.calls())
	}
	// A clock that went backwards does not stretch a cached verdict.
	now = now.Add(-time.Hour)
	if err := o.check(ctx, 100123, "r-1"); err != nil || ledger.calls() != 5 {
		t.Fatalf("clock skew: %v, ledger calls %d", err, ledger.calls())
	}
}

func TestOwnershipLookupFailureIs503AndNotCached(t *testing.T) {
	ledger := newFakeLedger(t, map[string]int64{"r-1": 100123})
	now := metricsNow
	o := ownershipFor(t, ledger.srv.URL, &now)
	for _, status := range []int{500, 503, 403, 400} {
		ledger.mu.Lock()
		ledger.fail = status
		ledger.mu.Unlock()
		if got := queryOwned(t, o, 100123, "resourceId=r-1"); got.status != 503 || got.Code != "Monitor.OwnerLookupFailed" {
			t.Fatalf("ledger %d: %d %s, want 503 Monitor.OwnerLookupFailed", status, got.status, got.Code)
		}
	}
	ledger.mu.Lock()
	ledger.fail = 0
	ledger.mu.Unlock()
	calls := ledger.calls()
	if got := queryOwned(t, o, 100123, "resourceId=r-1"); got.status != 200 || ledger.calls() != calls+1 {
		t.Fatalf("after recovery: %d, ledger calls %d (a failure must not be cached)", got.status, ledger.calls()-calls)
	}

	// Unreachable ledger.
	down := ownershipFor(t, "http://127.0.0.1:1", &now)
	if got := queryOwned(t, down, 100123, "resourceId=r-1"); got.status != 503 || got.Code != "Monitor.OwnerLookupFailed" {
		t.Fatalf("unreachable: %d %s", got.status, got.Code)
	}

	// A 200 that is not the resource asked for (a misrouted URL) is not a
	// verdict either.
	odd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/r-1") {
			writeJSON(w, "OK", map[string]any{"resourceId": "r-2"})
			return
		}
		_, _ = w.Write([]byte("<html>ok</html>"))
	}))
	t.Cleanup(odd.Close)
	misrouted := ownershipFor(t, odd.URL, &now)
	for _, id := range []string{"r-1", "r-3"} {
		if got := queryOwned(t, misrouted, 100123, "resourceId="+id); got.status != 503 || got.Code != "Monitor.OwnerLookupFailed" {
			t.Fatalf("misrouted %s: %d %s", id, got.status, got.Code)
		}
	}
}

func TestOwnershipWithoutLedger(t *testing.T) {
	// Outside production: the check is skipped.
	dev := &resourceOwnership{now: time.Now}
	if got := queryOwned(t, dev, 100123, "resourceId=r-1"); got.status != 200 {
		t.Fatalf("dev without ledger: %d %s", got.status, got.Code)
	}
	// Production: refused, never skipped.
	prod := &resourceOwnership{production: true, now: time.Now}
	if got := queryOwned(t, prod, 100123, "resourceId=r-1"); got.status != 503 || got.Code != "Monitor.OwnerLookupFailed" {
		t.Fatalf("prod without ledger: %d %s", got.status, got.Code)
	}
}

func TestOwnershipFromEnv(t *testing.T) {
	t.Setenv("EULER_ENV", "dev")
	t.Setenv(orchestratorURLEnv, "")
	o, err := newResourceOwnershipFromEnv()
	if err != nil || o.verifier != nil || o.production {
		t.Fatalf("dev unset = %+v, %v", o, err)
	}
	for _, bad := range []string{"ftp://orch:9203", "orch:9203", "http://", "http://orch:9203/?x=1"} {
		t.Setenv(orchestratorURLEnv, bad)
		if _, err := newResourceOwnershipFromEnv(); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	t.Setenv(orchestratorURLEnv, " http://svc-orchestrator.euler-svc-orchestrator.svc:9203/ ")
	o, err = newResourceOwnershipFromEnv()
	if err != nil || o.verifier == nil || o.verifier.(*orchestratorOwners).base != "http://svc-orchestrator.euler-svc-orchestrator.svc:9203" {
		t.Fatalf("configured = %+v, %v", o, err)
	}

	// Required in production.
	t.Setenv("EULER_ENV", "prod")
	t.Setenv(orchestratorURLEnv, "")
	if _, err := newResourceOwnershipFromEnv(); err == nil || !strings.Contains(err.Error(), orchestratorURLEnv) {
		t.Fatalf("prod unset: %v, want an error naming %s", err, orchestratorURLEnv)
	}
	if !defaultResourceOwnership().production {
		t.Fatal("default ownership skips the check in production")
	}
}

func TestOwnershipCacheIsBounded(t *testing.T) {
	ledger := newFakeLedger(t, map[string]int64{"r-new": 100123})
	now := metricsNow
	o := ownershipFor(t, ledger.srv.URL, &now)
	for i := 0; i < maxOwnerCacheEntries; i++ {
		o.owned[ownerKey{account: 100123, resourceID: "r-" + strconv.Itoa(i)}] = now.Add(-ownerCheckTTL)
	}
	o.owned[ownerKey{account: 100123, resourceID: "r-fresh"}] = now
	if err := o.check(context.Background(), 100123, "r-new"); err != nil {
		t.Fatal(err)
	}
	if len(o.owned) != 2 {
		t.Fatalf("cache holds %d entries after the sweep, want the 2 live ones", len(o.owned))
	}
}
