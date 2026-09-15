package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
			strings.NewReader(`{"productCode":"scecs","count":1}`))
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
