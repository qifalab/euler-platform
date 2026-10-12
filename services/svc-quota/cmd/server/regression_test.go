package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/quota"
)

// Reservation ids must be unique across requests. This service builds a
// quota.Manager per request, and a Manager's default generator restarts at
// "qt-1" — with a store-wide token table that made the second request's token
// overwrite the first's, leaking the first account's occupancy (and letting a
// cross-account lookup return someone else's reservation).
func TestOccupyTokenIDsAreProcessUnique(t *testing.T) {
	s := newInMemoryQuotaStore()
	occupy := func(acct int64) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/quota/occupy",
			strings.NewReader(`{"productCode":"euecs","count":1}`))
		req.Header.Set(accountIDHeader, fmt.Sprintf("%d", acct))
		rr := httptest.NewRecorder()
		s.handleOccupy(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("occupy: %d %s", rr.Code, rr.Body.String())
		}
		var env struct {
			Data map[string]any `json:"Data"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatalf("envelope: %v (%s)", err, rr.Body.String())
		}
		return env.Data
	}

	first := occupy(1)
	second := occupy(2)
	if first["reservationId"] == second["reservationId"] {
		t.Fatalf("two accounts were issued the same reservation id %v", first["reservationId"])
	}

	// Both reservations must still be findable. The bug this guarded against was
	// the second request overwriting the first one's token — and "still
	// retrievable" is the property that holds regardless of which backend is
	// wired in, where counting rows in an in-memory map is not.
	for _, id := range []any{first["reservationId"], second["reservationId"]} {
		if _, err := s.GetToken(fmt.Sprintf("%v", id)); err != nil {
			t.Fatalf("reservation %v is gone: %v", id, err)
		}
	}

	// The first account's reservation is still findable under its own id.
	if _, err := s.GetToken(fmt.Sprintf("%v", first["reservationId"])); err != nil {
		t.Fatalf("first reservation is gone: %v", err)
	}
}

// postAs sends a JSON body to a handler as the given account.
func postAs(t *testing.T, h http.HandlerFunc, path string, acct int64, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set(accountIDHeader, fmt.Sprintf("%d", acct))
	rr := httptest.NewRecorder()
	h(rr, req)
	var env map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	return rr, env
}

func occupyID(t *testing.T, s *quotaStore, req occupyRequest) string {
	t.Helper()
	rec := doOccupy(t, s, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("occupy: %d %s", rec.Code, rec.Body.String())
	}
	var d occupyData
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	return d.Data.ReservationId
}

// The in-memory store's DeleteToken must report a missing token like SQLStore
// does — the claim-first protocol depends on exactly one deleter winning.
func TestMemStoreDeleteTokenReportsNotFound(t *testing.T) {
	s := newMemQuotaStore()
	if err := s.DeleteToken("qt-missing"); !errors.Is(err, quota.ErrTokenNotFound) {
		t.Fatalf("delete missing token: err = %v, want ErrTokenNotFound", err)
	}
	if err := s.PutToken(quota.Token{TokenID: "qt-1", AccountID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteToken("qt-1"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := s.DeleteToken("qt-1"); !errors.Is(err, quota.ErrTokenNotFound) {
		t.Fatalf("second delete: err = %v, want ErrTokenNotFound", err)
	}
}

// Concurrent first attempts with one idempotency key must produce ONE
// reservation (the key is recorded atomically with the reservation).
func TestOccupyIdempotencyKeyConcurrent(t *testing.T) {
	s := newInMemoryQuotaStore()
	req := occupyRequest{ProductCode: "euecs", Region: "cn-north-1", Count: 2, IdempotencyKey: "idem-race"}
	const n = 16
	ids := make([]string, n)
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b, _ := json.Marshal(req)
			r := httptest.NewRequest(http.MethodPost, "/api/v1/quota/occupy", bytes.NewReader(b))
			r.Header.Set(accountIDHeader, "100123")
			rr := httptest.NewRecorder()
			s.handleOccupy(rr, r)
			codes[i] = rr.Code
			var d occupyData
			_ = json.Unmarshal(rr.Body.Bytes(), &d)
			ids[i] = d.Data.ReservationId
		}(i)
	}
	wg.Wait()
	for i := range ids {
		if codes[i] != http.StatusOK {
			t.Fatalf("request %d: code %d", i, codes[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("request %d got reservation %q, request 0 got %q", i, ids[i], ids[0])
		}
	}
	u, _ := s.GetUsage(100123, "quota_euecs_instance", "cn-north-1")
	if u.Occupying != 2 {
		t.Fatalf("Occupying = %d, want 2 (one reservation for one key)", u.Occupying)
	}
}

// conflictingStore fails the next `failures` usage updates with a version
// conflict, as a concurrent writer would.
type conflictingStore struct {
	quota.Store
	mu       sync.Mutex
	failures int
}

func (c *conflictingStore) UpdateUsage(u quota.Usage, v int) error {
	c.mu.Lock()
	fail := c.failures > 0
	if fail {
		c.failures--
	}
	c.mu.Unlock()
	if fail {
		return quota.ErrVersionConflict
	}
	return c.Store.UpdateUsage(u, v)
}

// A lost optimistic-lock race is retried against fresh state (the Manager's
// contract) instead of surfacing as a 409; only a persistent conflict does.
func TestOccupyRetriesVersionConflict(t *testing.T) {
	s := newInMemoryQuotaStore()
	s.backend = &conflictingStore{Store: s.backend, failures: versionAttempts - 1}
	if rec := doOccupy(t, s, occupyRequest{ProductCode: "euecs", Count: 3}); rec.Code != http.StatusOK {
		t.Fatalf("occupy after transient conflicts: %d %s", rec.Code, rec.Body.String())
	}
	u, _ := s.GetUsage(100123, "quota_euecs_instance", "cn-north-1")
	if u.Occupying != 3 {
		t.Fatalf("Occupying = %d, want 3", u.Occupying)
	}

	s2 := newInMemoryQuotaStore()
	s2.backend = &conflictingStore{Store: s2.backend, failures: 1000}
	rec := doOccupy(t, s2, occupyRequest{ProductCode: "euecs", Count: 3})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Quota.VersionConflict") {
		t.Fatalf("persistent conflict: %d %s, want 409 Quota.VersionConflict", rec.Code, rec.Body.String())
	}
}

// POST /api/v1/quota/commit converts a reservation into used capacity, with
// the same ownership rule as release.
func TestCommitEndpoint(t *testing.T) {
	s := newInMemoryQuotaStore()
	id := occupyID(t, s, occupyRequest{ProductCode: "euecs", Region: "cn-north-1", Count: 3})

	// Another account cannot commit it.
	if rr, env := postAs(t, s.handleCommit, "/api/v1/quota/commit", 999, map[string]any{"reservationId": id}); rr.Code != http.StatusNotFound || env["Code"] != "Quota.NotFound" {
		t.Fatalf("cross-account commit: %d %v, want 404 Quota.NotFound", rr.Code, env)
	}
	if rr, _ := postAs(t, s.handleCommit, "/api/v1/quota/commit", 100123, map[string]any{}); rr.Code != http.StatusBadRequest {
		t.Fatalf("commit without reservationId: %d, want 400", rr.Code)
	}

	rr, env := postAs(t, s.handleCommit, "/api/v1/quota/commit", 100123, map[string]any{"reservationId": id})
	if rr.Code != http.StatusOK {
		t.Fatalf("commit: %d %v", rr.Code, env)
	}
	u, _ := s.GetUsage(100123, "quota_euecs_instance", "cn-north-1")
	if u.Used != 3 || u.Occupying != 0 {
		t.Fatalf("usage after commit = %+v, want Used 3 / Occupying 0", u)
	}

	// A second commit finds nothing: 404, and usage does not move.
	if rr, env := postAs(t, s.handleCommit, "/api/v1/quota/commit", 100123, map[string]any{"reservationId": id}); rr.Code != http.StatusNotFound || env["Code"] != "Quota.NotFound" {
		t.Fatalf("repeat commit: %d %v, want 404 Quota.NotFound", rr.Code, env)
	}
	if u2, _ := s.GetUsage(100123, "quota_euecs_instance", "cn-north-1"); u2.Used != 3 {
		t.Fatalf("Used = %d after repeat commit, want 3", u2.Used)
	}

	// An expired reservation is refused with 409 (its capacity went back).
	id2 := occupyID(t, s, occupyRequest{ProductCode: "euecs", Region: "cn-north-1", Count: 2})
	tok, err := s.GetToken(id2)
	if err != nil {
		t.Fatal(err)
	}
	tok.ExpiresAt = time.Now().Add(-time.Second)
	if err := s.PutToken(tok); err != nil {
		t.Fatal(err)
	}
	if rr, env := postAs(t, s.handleCommit, "/api/v1/quota/commit", 100123, map[string]any{"reservationId": id2}); rr.Code != http.StatusConflict || env["Code"] != "Quota.ReservationExpired" {
		t.Fatalf("expired commit: %d %v, want 409 Quota.ReservationExpired", rr.Code, env)
	}
}

// The TTL sweep runs: an abandoned reservation is returned to the pool.
func TestSweeperReclaimsExpiredReservations(t *testing.T) {
	future := func() time.Time { return time.Now().Add(quota.DefaultTokenTTL + time.Minute) }

	s := newInMemoryQuotaStore()
	occupyID(t, s, occupyRequest{ProductCode: "euecs", Region: "cn-north-1", Count: 4})
	n, err := s.sweepOnce(future)
	if err != nil || n != 1 {
		t.Fatalf("sweepOnce = %d, %v; want 1, nil", n, err)
	}
	if u, _ := s.GetUsage(100123, "quota_euecs_instance", "cn-north-1"); u.Occupying != 0 {
		t.Fatalf("Occupying = %d after sweep, want 0", u.Occupying)
	}

	// The background loop sweeps on its ticker and stops on cancel.
	occupyID(t, s, occupyRequest{ProductCode: "euecs", Region: "cn-north-1", Count: 2})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runSweeper(ctx, 5*time.Millisecond, future)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		u, _ := s.GetUsage(100123, "quota_euecs_instance", "cn-north-1")
		if u.Occupying == 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("background sweeper did not reclaim: Occupying = %d", u.Occupying)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweeper did not stop on cancel")
	}
}

// A replayed release (saga retry) returns the reservation exactly once.
func TestReleaseReplayReturnsOnce(t *testing.T) {
	s := newInMemoryQuotaStore()
	id := occupyID(t, s, occupyRequest{ProductCode: "euecs", Region: "cn-north-1", Count: 5})
	occupyID(t, s, occupyRequest{ProductCode: "euecs", Region: "cn-north-1", Count: 3})
	for i := 0; i < 3; i++ {
		if rr, env := postAs(t, s.handleRelease, "/api/v1/quota/release", 100123, map[string]any{"reservationId": id}); rr.Code != http.StatusOK {
			t.Fatalf("release %d: %d %v", i, rr.Code, env)
		}
	}
	if u, _ := s.GetUsage(100123, "quota_euecs_instance", "cn-north-1"); u.Occupying != 3 {
		t.Fatalf("Occupying = %d, want 3", u.Occupying)
	}
}
