package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/alertcenter"
	"github.com/qifalab/euler-platform/notify"
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

// recordingStore is a notify.Store that keeps saved notifications by id, the
// way the notification table's primary key does.
type recordingStore struct {
	notify.Store
	saved map[string]notify.Notification
}

func (r *recordingStore) Save(n notify.Notification) error {
	r.saved[n.NotificationID] = n
	return nil
}

func ingestBody(t *testing.T, s *store, tenant int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alertcenter/ingest", strings.NewReader(body))
	req.Header.Set(accountIDHeader, fmt.Sprintf("%d", tenant))
	rr := httptest.NewRecorder()
	s.handleIngest(rr, req)
	return rr
}

// E14: notification_id was "alert-"+alertId — a global primary key built from
// a client-chosen, optional id. Two tenants using the same alertId overwrote
// each other's record, and every id-less alert collapsed into "alert-".
func TestPersistedNotificationIDsAreTenantScoped(t *testing.T) {
	s := newStore()
	rec := &recordingStore{saved: map[string]notify.Notification{}}
	s.notifyStore = rec

	for _, tc := range []struct {
		tenant int64
		body   string
	}{
		{1, `{"alertId":"cpu-high","product":"euecs","metric":"cpu"}`},
		{2, `{"alertId":"cpu-high","product":"euecs","metric":"cpu"}`},
		{1, `{"product":"euoss","metric":"req"}`},
		{1, `{"product":"eurds","metric":"conn"}`},
	} {
		if rr := ingestBody(t, s, tc.tenant, tc.body); rr.Code != http.StatusOK {
			t.Fatalf("ingest: %d %s", rr.Code, rr.Body.String())
		}
	}
	s.flushForTenant(1)
	s.flushForTenant(2)

	if len(rec.saved) != 4 {
		t.Fatalf("persisted %d notifications, want 4 distinct records: %v", len(rec.saved), rec.saved)
	}
	for _, id := range []string{"alert-1-cpu-high", "alert-2-cpu-high"} {
		if _, ok := rec.saved[id]; !ok {
			t.Fatalf("missing %s in %v", id, rec.saved)
		}
	}
	for id, n := range rec.saved {
		if len(id) > 64 || n.BizKey == "" {
			t.Fatalf("bad persisted record %q: %+v", id, n)
		}
	}
	// Ids that would overflow the column stay deterministic and tenant-scoped.
	long := strings.Repeat("x", 64)
	if a, b := alertNotificationID(1, long), alertNotificationID(2, long); len(a) > 64 || a == b || a != alertNotificationID(1, long) {
		t.Fatalf("long-id derivation: %q %q", a, b)
	}
	if rr := ingestBody(t, s, 1, `{"alertId":"`+long+`x","product":"p","metric":"m"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("alertId over 64 chars: %d, want 400", rr.Code)
	}
}

// E15: severity was stored verbatim; the console sends lowercase, which ranks
// 0 and loses every inhibition decision.
func TestSeverityIsNormalized(t *testing.T) {
	s := newStore()
	for _, body := range []string{
		`{"alertId":"a1","product":"euecs","metric":"cpu","severity":"critical"}`,
		`{"alertId":"a2","product":"euecs","metric":"mem","severity":"warning"}`,
	} {
		if rr := ingestBody(t, s, 1, body); rr.Code != http.StatusOK {
			t.Fatalf("ingest: %d %s", rr.Code, rr.Body.String())
		}
	}
	out, _ := s.flushForTenant(1)
	if len(out) != 1 || out[0].Severity != alertcenter.SeverityCritical || out[0].AlertID != "a1" {
		t.Fatalf("lowercase critical must inhibit the warning: %+v", out)
	}
	if rr := ingestBody(t, s, 1, `{"product":"euecs","metric":"cpu","severity":"urgent"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown severity: %d, want 400", rr.Code)
	}
}

// E16: the rate limit was applied in time order, so a newer CRITICAL group
// could be cut while older INFO groups went out — and the cut alerts were lost.
func TestRateLimitKeepsCriticalAndDefersOverflow(t *testing.T) {
	s := newStore()
	clock := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	s.limiter = alertcenter.NewTenantLimiter(clock)
	at := clock.Add(-time.Minute)
	for i := 0; i < alertcenter.MaxTenantRate; i++ {
		s.ingest(alertcenter.Alert{AlertID: fmt.Sprintf("info-%d", i), TenantID: 1,
			Product: fmt.Sprintf("p-%d", i), Metric: "m", Severity: alertcenter.SeverityInfo, OccurredAt: at})
	}
	s.ingest(alertcenter.Alert{AlertID: "crit", TenantID: 1, Product: "db", Metric: "m",
		Severity: alertcenter.SeverityCritical, OccurredAt: at.Add(time.Second)})

	out, deferred := s.flushForTenant(1)
	if len(out) != alertcenter.MaxTenantRate || deferred != 1 {
		t.Fatalf("emitted %d deferred %d, want %d and 1", len(out), deferred, alertcenter.MaxTenantRate)
	}
	if out[0].AlertID != "crit" {
		t.Fatalf("the CRITICAL group must go out first, got %+v", out[0])
	}
	// The overflow INFO group waits for the next window instead of vanishing.
	clock = clock.Add(alertcenter.RateWindow)
	next, deferred := s.flushForTenant(1)
	if len(next) != 1 || deferred != 0 || next[0].Severity != alertcenter.SeverityInfo {
		t.Fatalf("next window: emitted %+v deferred %d, want the deferred INFO group", next, deferred)
	}
}
