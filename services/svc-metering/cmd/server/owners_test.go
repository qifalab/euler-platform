package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeOwners: ownership table resourceID → account; anything else is owned by
// whoever asks (the handler tests below exercise the first-claim cases
// explicitly through a strict table).
type fakeOwners struct {
	table  map[string]int64
	strict bool
	err    error
}

func (f fakeOwners) Owns(_ context.Context, acct int64, res string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	if owner, ok := f.table[res]; ok {
		return owner == acct, nil
	}
	return !f.strict, nil
}

// Existing handler tests ingest for arbitrary ids; they run against a ledger
// that owns everything for the caller. Ownership itself is tested below.
func init() { newOwnerVerifier = func() ownerVerifier { return fakeOwners{} } }

// Regression: the first account to name a resource id became its owner.
func TestIngestRequiresLedgerOwnership(t *testing.T) {
	s := newMeteringStore()
	s.owners = fakeOwners{table: map[string]int64{"euecs-victim": 7}, strict: true}
	if rr := ingest(t, s, 9, "euecs-victim", "cpu_core_hour", "1"); rr.Code != http.StatusForbidden {
		t.Fatalf("squatter's first ingest: %d, want 403", rr.Code)
	}
	if rr := ingest(t, s, 7, "euecs-victim", "cpu_core_hour", "1"); rr.Code != http.StatusOK {
		t.Fatalf("owner's ingest: %d %s", rr.Code, rr.Body.String())
	}
	if rr := ingest(t, s, 9, "euecs-unknown", "cpu_core_hour", "1"); rr.Code != http.StatusForbidden {
		t.Fatalf("ingest for a resource the ledger does not know: %d, want 403", rr.Code)
	}
}

func TestIngestFailsClosedWhenOwnershipUnknown(t *testing.T) {
	s := newMeteringStore()
	s.owners = fakeOwners{err: errors.New("connection refused")}
	if rr := ingest(t, s, 7, "euecs-x", "cpu_core_hour", "1"); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rr.Code)
	}
}

func TestOrchestratorOwnersClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(accountIDHeader) == "7" {
			// The orchestrator's resource-detail envelope.
			switch r.URL.Path {
			case "/api/v1/orchestrator/resources/euecs-a":
				_, _ = w.Write([]byte(`{"RequestId":"t","Code":"OK","Data":{"resourceId":"euecs-a","state":"RUNNING"}}`))
				return
			case "/api/v1/orchestrator/resources/euecs-gone":
				_, _ = w.Write([]byte(`{"RequestId":"t","Code":"OK","Data":{"resourceId":"euecs-gone","state":"RELEASED"}}`))
				return
			case "/api/v1/orchestrator/resources/euecs-failed":
				_, _ = w.Write([]byte(`{"RequestId":"t","Code":"OK","Data":{"resourceId":"euecs-failed","state":"CREATE_FAILED"}}`))
				return
			case "/api/v1/orchestrator/resources/euecs-garbled":
				_, _ = w.Write([]byte(`not json`))
				return
			}
		}
		if r.URL.Path == "/api/v1/orchestrator/resources/boom" {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	t.Setenv("EULER_SVC_ORCHESTRATOR_URL", srv.URL)
	o := newOrchestratorOwners()
	if ok, err := o.Owns(context.Background(), 7, "euecs-a"); !ok || err != nil {
		t.Fatalf("owner: %v %v", ok, err)
	}
	if ok, err := o.Owns(context.Background(), 9, "euecs-a"); ok || err != nil {
		t.Fatalf("non-owner: %v %v", ok, err)
	}
	if _, err := o.Owns(context.Background(), 7, "boom"); err == nil {
		t.Fatal("5xx must be an error, not a verdict")
	}
	// A 200 is ownership, not liveness: a released or never-created resource
	// produces no usage, so a reading for it is rejected.
	for _, id := range []string{"euecs-gone", "euecs-failed"} {
		if ok, err := o.Owns(context.Background(), 7, id); ok || !errors.Is(err, errResourceInactive) {
			t.Fatalf("%s: got %v %v, want errResourceInactive", id, ok, err)
		}
	}
	if ok, err := o.Owns(context.Background(), 7, "euecs-garbled"); ok || err == nil {
		t.Fatalf("unreadable detail must fail closed: %v %v", ok, err)
	}
}

func TestIngestRejectsInactiveResource(t *testing.T) {
	s := newMeteringStore()
	s.owners = fakeOwners{err: errResourceInactive}
	if rr := ingest(t, s, 7, "euecs-released", "cpu_core_hour", "1"); rr.Code != http.StatusConflict {
		t.Fatalf("usage for a released resource: %d %s, want 409", rr.Code, rr.Body.String())
	}
	if _, known := s.accounts["euecs-released"]; known {
		t.Fatal("rejected reading must not register the resource")
	}
}

// ledgerOwners is a mutable resource ledger (resource → owner, state) that
// counts how often it is asked.
type ledgerOwners struct {
	mu    sync.Mutex
	owner map[string]int64
	state map[string]string
	calls int
}

func (l *ledgerOwners) Owns(_ context.Context, acct int64, res string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if owner, ok := l.owner[res]; !ok || owner != acct {
		return false, nil
	}
	if inactiveStates[l.state[res]] {
		return false, errResourceInactive
	}
	return true, nil
}

func (l *ledgerOwners) set(res, state string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.state[res] = state
}

func (l *ledgerOwners) lookups() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// Regression (B3): the state check ran on a resource's first reading only;
// afterwards the cached owner was trusted forever, so a released resource kept
// being billed while its producer kept sending. The verdict now expires.
func TestIngestRechecksResourceStateAfterTTL(t *testing.T) {
	s := newMeteringStore()
	ledger := &ledgerOwners{owner: map[string]int64{"euecs-r1": 7}, state: map[string]string{"euecs-r1": "RUNNING"}}
	s.owners = ledger
	t0 := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	now := t0
	s.now = func() time.Time { return now }

	if rr := ingest(t, s, 7, "euecs-r1", "cpu_core_hour", "1"); rr.Code != http.StatusOK {
		t.Fatalf("first reading: %d %s", rr.Code, rr.Body.String())
	}
	now = t0.Add(30 * time.Second)
	if rr := ingest(t, s, 7, "euecs-r1", "cpu_core_hour", "1"); rr.Code != http.StatusOK {
		t.Fatalf("cached reading: %d %s", rr.Code, rr.Body.String())
	}
	if n := ledger.lookups(); n != 1 {
		t.Fatalf("ledger asked %d times within the TTL, want 1", n)
	}

	ledger.set("euecs-r1", "RELEASED")
	now = t0.Add(ownerCheckTTL + time.Second)
	rr := ingest(t, s, 7, "euecs-r1", "cpu_core_hour", "1")
	if rr.Code != http.StatusConflict {
		t.Fatalf("reading for a released resource after the TTL: %d %s, want 409", rr.Code, rr.Body.String())
	}
	now = now.Add(time.Second)
	if rr := ingest(t, s, 7, "euecs-r1", "cpu_core_hour", "1"); rr.Code != http.StatusConflict {
		t.Fatalf("next reading: %d, want 409", rr.Code)
	}
	if n := ledger.lookups(); n != 2 {
		t.Fatalf("ledger asked %d times, want 2 (the inactive verdict is cached too)", n)
	}
}

// Resources the store knew before any reading (seeded, or rehydrated from the
// aggregate ledger after a restart) are verified like any other.
func TestIngestVerifiesAlreadyKnownResource(t *testing.T) {
	s := newMeteringStore()
	s.owners = &ledgerOwners{
		owner: map[string]int64{seededResourceID: seededAccountID},
		state: map[string]string{seededResourceID: "RELEASED"},
	}
	if rr := ingest(t, s, seededAccountID, seededResourceID, "cpu_core_hour", "1"); rr.Code != http.StatusConflict {
		t.Fatalf("reading for a released, already-known resource: %d %s, want 409", rr.Code, rr.Body.String())
	}
}

// A cached verdict is per account: another account still asks the ledger and
// is refused, and the owner's cached verdict survives the refusal.
func TestOwnerCheckCacheIsPerAccount(t *testing.T) {
	s := newMeteringStore()
	ledger := &ledgerOwners{owner: map[string]int64{"euecs-r2": 7}, state: map[string]string{"euecs-r2": "RUNNING"}}
	s.owners = ledger
	if rr := ingest(t, s, 7, "euecs-r2", "cpu_core_hour", "1"); rr.Code != http.StatusOK {
		t.Fatalf("owner: %d", rr.Code)
	}
	if rr := ingest(t, s, 9, "euecs-r2", "cpu_core_hour", "1"); rr.Code != http.StatusForbidden {
		t.Fatalf("other account: %d, want 403", rr.Code)
	}
	if rr := ingest(t, s, 7, "euecs-r2", "cpu_core_hour", "1"); rr.Code != http.StatusOK {
		t.Fatalf("owner again: %d", rr.Code)
	}
	if n := ledger.lookups(); n != 2 {
		t.Fatalf("ledger asked %d times, want 2 (owner once, other account once)", n)
	}
}
