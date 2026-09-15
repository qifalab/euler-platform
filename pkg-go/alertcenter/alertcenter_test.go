package alertcenter

import (
	"testing"
	"time"
)

func alert(tenant int64, product, metric string, sev Severity, labels map[string]string) Alert {
	return Alert{
		AlertID:    "ev-" + product + "-" + metric,
		TenantID:   tenant,
		Product:    product,
		Metric:     metric,
		Severity:   sev,
		Labels:     labels,
		OccurredAt: time.Now(),
	}
}

func TestDedupCollapsesReFires(t *testing.T) {
	a := alert(1, "euecs", "cpu", SeverityCritical, map[string]string{"instance": "i-1"})
	b := a // same tenant/product/metric/labels → same dedup key
	if a.DedupKey() != b.DedupKey() {
		t.Fatal("identical alerts must share a dedup key")
	}
	out := Converge([]Alert{a, b}, nil)
	if len(out) != 1 {
		t.Fatalf("two identical alerts should collapse to one, got %d", len(out))
	}
}

func TestInhibitionKeepsHighestSeverityPerGroup(t *testing.T) {
	critical := alert(1, "euecs", "cpu", SeverityCritical, map[string]string{"instance": "i-1"})
	warning := alert(1, "euecs", "memory", SeverityWarning, map[string]string{"instance": "i-1"})
	info := alert(1, "euecs", "disk", SeverityInfo, map[string]string{"instance": "i-1"})
	out := Converge([]Alert{info, warning, critical}, nil)
	if len(out) != 1 {
		t.Fatalf("same-group alerts should inhibit to one, got %d", len(out))
	}
	if out[0].Severity != SeverityCritical {
		t.Fatalf("kept severity = %v, want CRITICAL", out[0].Severity)
	}
}

func TestDifferentGroupsDoNotInhibit(t *testing.T) {
	a := alert(1, "euecs", "cpu", SeverityCritical, nil)
	b := alert(1, "euoss", "requests", SeverityInfo, nil)
	out := Converge([]Alert{a, b}, nil)
	if len(out) != 2 {
		t.Fatalf("different products are different groups, got %d alerts", len(out))
	}
}

func TestSilenceDropsMutedGroup(t *testing.T) {
	s := NewSilence()
	s.Mute(1, "euecs")
	critical := alert(1, "euecs", "cpu", SeverityCritical, nil)
	other := alert(1, "euoss", "requests", SeverityInfo, nil)
	out := Converge([]Alert{critical, other}, s)
	if len(out) != 1 || out[0].Product != "euoss" {
		t.Fatalf("muted group must be dropped, got %d alerts", len(out))
	}
	// unmute restores delivery
	s.Unmute(1, "euecs")
	if len(Converge([]Alert{critical}, s)) != 1 {
		t.Fatal("unmuted group must deliver again")
	}
}

func TestInhibitedByIsOneDirectional(t *testing.T) {
	critical := alert(1, "euecs", "cpu", SeverityCritical, nil)
	info := alert(1, "euecs", "disk", SeverityInfo, nil)
	if !info.InhibitedBy(critical) {
		t.Error("lower severity must be inhibited by higher in the same group")
	}
	if critical.InhibitedBy(info) {
		t.Error("higher severity must NOT be inhibited by lower")
	}
}

func TestTenantLimiterEnforcesTenPerMinute(t *testing.T) {
	now := time.Now()
	l := NewTenantLimiter(now)
	for i := 0; i < MaxTenantRate; i++ {
		if !l.Allow(1, now) {
			t.Fatalf("notification %d within limit rejected", i+1)
		}
	}
	if l.Allow(1, now) {
		t.Fatal("11th notification in the same minute must be rate-limited")
	}
}

func TestTenantLimiterResetsAfterWindow(t *testing.T) {
	now := time.Now()
	l := NewTenantLimiter(now)
	for i := 0; i < MaxTenantRate; i++ {
		l.Allow(1, now)
	}
	if !l.Allow(1, now.Add(RateWindow)) {
		t.Fatal("after the window elapses the tenant must be allowed again")
	}
}

func TestTenantLimiterIsPerTenant(t *testing.T) {
	now := time.Now()
	l := NewTenantLimiter(now)
	for i := 0; i < MaxTenantRate; i++ {
		l.Allow(1, now)
	}
	if !l.Allow(2, now) {
		t.Fatal("one tenant's limit must not throttle another tenant")
	}
}
