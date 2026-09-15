package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The anomaly scan must apply the same owner check as every other read path.
// It used to accept any resourceId from any authenticated caller and return
// the usage series (and its trailing baseline) of a resource belonging to
// someone else.
func TestAnomalyScanEnforcesOwnership(t *testing.T) {
	s := newMeteringStore()
	if rr := ingest(t, s, 1, "res-a", "cpu_core_hour", "1"); rr.Code != http.StatusOK {
		t.Fatalf("ingest: %d %s", rr.Code, rr.Body.String())
	}

	scan := func(acct int64, resourceID string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/metering/anomaly-scan?resourceId="+resourceID+"&metric=cpu_core_hour", nil)
		req.Header.Set(accountIDHeader, fmt.Sprintf("%d", acct))
		rr := httptest.NewRecorder()
		s.handleAnomalyScan(rr, req)
		return rr
	}

	if rr := scan(2, "res-a"); rr.Code != http.StatusForbidden {
		t.Fatalf("foreign scan: %d %s, want 403", rr.Code, rr.Body.String())
	}
	if rr := scan(1, "res-a"); rr.Code != http.StatusOK {
		t.Fatalf("owner scan: %d %s, want 200", rr.Code, rr.Body.String())
	}
	// An unknown resource is a 404, not an empty series.
	if rr := scan(1, "res-nope"); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown resource: %d, want 404", rr.Code)
	}
}
