package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Flush is tenant-scoped. It used to converge the GLOBAL buffer and return
// every tenant's notifications, so one tenant's flush leaked another's alerts
// — and consumed them, meaning the owner never saw them.
func TestFlushIsTenantScoped(t *testing.T) {
	s := newStore()

	ingest := func(tenant int64, alertID string) {
		t.Helper()
		body := `{"alertId":"` + alertID + `","product":"euecs","metric":"cpu_util","severity":"CRITICAL"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/alertcenter/ingest", strings.NewReader(body))
		req.Header.Set(accountIDHeader, fmt.Sprintf("%d", tenant))
		rr := httptest.NewRecorder()
		s.handleIngest(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("ingest %s: %d %s", alertID, rr.Code, rr.Body.String())
		}
	}
	flush := func(tenant int64) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/alertcenter/flush", nil)
		req.Header.Set(accountIDHeader, fmt.Sprintf("%d", tenant))
		rr := httptest.NewRecorder()
		s.handleFlush(rr, req)
		return rr
	}

	ingest(1, "a-1")
	ingest(2, "a-2")

	first := flush(1)
	if first.Code != http.StatusOK {
		t.Fatalf("flush: %d %s", first.Code, first.Body.String())
	}
	if strings.Contains(first.Body.String(), "a-2") {
		t.Fatalf("tenant 1's flush leaked tenant 2's notification: %s", first.Body.String())
	}
	if !strings.Contains(first.Body.String(), "a-1") {
		t.Fatalf("tenant 1's own notification is missing: %s", first.Body.String())
	}

	// Tenant 2's alert was not consumed by tenant 1's flush.
	second := flush(2)
	if !strings.Contains(second.Body.String(), "a-2") {
		t.Fatalf("tenant 2's alert was consumed by another tenant: %s", second.Body.String())
	}
}
