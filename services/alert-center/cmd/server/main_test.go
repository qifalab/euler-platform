package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/starcloud/sc-platform/alertcenter"
)

func doReq(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("X-Sc-Account-Id", "100123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func ingest(t *testing.T, h http.Handler, product, metric string, sev alertcenter.Severity) {
	t.Helper()
	rec := doReq(t, h, http.MethodPost, "/api/v1/alertcenter/ingest", map[string]any{
		"product": product, "metric": metric, "severity": sev,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest %s/%s: status %d body %s", product, metric, rec.Code, rec.Body.String())
	}
}

func flushNotifications(t *testing.T, h http.Handler) []map[string]any {
	t.Helper()
	rec := doReq(t, h, http.MethodPost, "/api/v1/alertcenter/flush", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("flush: status %d body %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data struct {
			Notifications []map[string]any `json:"notifications"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("flush unmarshal: %v", err)
	}
	return env.Data.Notifications
}

func TestConvergenceOverHTTP(t *testing.T) {
	h := newMux(newStore())
	// Three same-group alerts (tenant 100123 × scecs): dedup + inhibit → 1.
	ingest(t, h, "scecs", "cpu", alertcenter.SeverityCritical)
	ingest(t, h, "scecs", "memory", alertcenter.SeverityWarning)
	ingest(t, h, "scecs", "disk", alertcenter.SeverityInfo)
	// Different group survives.
	ingest(t, h, "scoss", "requests", alertcenter.SeverityInfo)

	notes := flushNotifications(t, h)
	if len(notes) != 2 {
		t.Fatalf("want 2 notifications (scecs + scoss groups), got %d", len(notes))
	}
}

func TestDedupOverHTTP(t *testing.T) {
	h := newMux(newStore())
	ingest(t, h, "scecs", "cpu", alertcenter.SeverityCritical)
	ingest(t, h, "scecs", "cpu", alertcenter.SeverityCritical) // identical → deduped
	notes := flushNotifications(t, h)
	if len(notes) != 1 {
		t.Fatalf("identical alerts must dedup to one notification, got %d", len(notes))
	}
}

func TestRateLimitOverHTTP(t *testing.T) {
	h := newMux(newStore())
	// 15 distinct groups (distinct products) for ONE tenant: converge keeps all
	// 15, then the per-tenant rate limit (10/min) admits only the first 10.
	for i := 0; i < alertcenter.MaxTenantRate+5; i++ {
		ingest(t, h, "p-"+string(rune('a'+i%26))+string(rune('0'+i/26)), "m", alertcenter.SeverityWarning)
	}
	notes := flushNotifications(t, h)
	if len(notes) != alertcenter.MaxTenantRate {
		t.Fatalf("want %d notifications (10/min rate limit), got %d", alertcenter.MaxTenantRate, len(notes))
	}
}

func TestMissingAccountRejected(t *testing.T) {
	h := newMux(newStore())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/alertcenter/alerts", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing account: status %d, want 403", rec.Code)
	}
}
