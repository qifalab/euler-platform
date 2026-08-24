// Package alertcenter implements the alert convergence engine — the core of
// 云监控高级告警产品化 (alert-center, 03§4.4.6; deferred from phase 2, 09§4.0
// "延后三期的项").
//
// alert-center is the convergence / customer-notification gateway between the
// platform's alert producers (platform Alertmanager webhook L0/L1, tenant
// alert-engine) and svc-notify. Its product logic is the four-stage
// convergence applied to the stream of cloud.sys.alert.event (03§4.4.6):
//
//  1. 去重 (dedup)     — the same alert re-firing within a window collapses
//     to one, so a flapping metric does not page the customer every cycle.
//  2. 分组 (group)     — alerts are aggregated per tenant×product into one
//     notification, so a single incident does not fan out N messages.
//  3. 抑制 (inhibit)   — a higher-severity alert suppresses lower-severity
//     alerts in the same group; the customer sees the root cause, not the
//     cascade.
//  4. 静默 (silence)   — a muted tenant/product is dropped entirely (the
//     maintenance-window mechanism, 05§8.3).
//
// The single-tenant notification rate limit (10 条/分钟, 03§4.4.6) is a
// separate concern applied at notify time, modelled here as TenantLimiter so
// the "防通知风暴" guarantee is a tested contract, not a hope.
package alertcenter

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// MaxTenantRate is the single-tenant notification rate limit (03§4.4.6:
// 10 条/分钟 防通知风暴).
const MaxTenantRate = 10

// RateWindow is the rate-limit window (one minute).
const RateWindow = time.Minute

// Severity is an alert severity.
type Severity string

const (
	SeverityCritical Severity = "CRITICAL"
	SeverityWarning  Severity = "WARNING"
	SeverityInfo     Severity = "INFO"
)

// severityRank orders severities for inhibition: a higher rank suppresses a
// lower one in the same group.
var severityRank = map[Severity]int{
	SeverityCritical: 3,
	SeverityWarning:  2,
	SeverityInfo:     1,
}

// Rank returns the severity's rank; unknown severities rank lowest so an
// unclassified alert never suppresses a classified one.
func (s Severity) Rank() int {
	if r, ok := severityRank[s]; ok {
		return r
	}
	return 0
}

// Alert is the unified record flowing through cloud.sys.alert.event.
type Alert struct {
	AlertID    string
	TenantID   int64
	Product    string
	Metric     string
	Severity   Severity
	Labels     map[string]string
	OccurredAt time.Time
}

// DedupKey is the 去重 key: the same tenant×product×metric with the same labels
// is the same alert re-firing, and must collapse to one within the window.
func (a Alert) DedupKey() string {
	parts := make([]string, 0, len(a.Labels))
	for k, v := range a.Labels {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return fmt.Sprintf("%d|%s|%s|%s", a.TenantID, a.Product, a.Metric, strings.Join(parts, ","))
}

// GroupKey is the 分组 key: one tenant×product is one notification group, so an
// incident that raises several metrics still lands as one customer message.
func (a Alert) GroupKey() string {
	return fmt.Sprintf("%d|%s", a.TenantID, a.Product)
}

// InhibitedBy reports whether a is suppressed by b: b is in the same group and
// carries a strictly higher severity. Inhibition is one-directional — a higher
// alert suppresses a lower one, never the reverse.
func (a Alert) InhibitedBy(b Alert) bool {
	return a.GroupKey() == b.GroupKey() && b.Severity.Rank() > a.Severity.Rank()
}

// Silence is the mute table: key = tenant|product (the group key), value =
// muted. A silenced group drops every alert until un-muted.
type Silence struct {
	keys map[string]bool
}

// NewSilence builds an empty silence table.
func NewSilence() *Silence { return &Silence{keys: make(map[string]bool)} }

// Mute mutes a tenant×product group.
func (s *Silence) Mute(tenantID int64, product string) { s.keys[groupKey(tenantID, product)] = true }

// Unmute clears a group's mute.
func (s *Silence) Unmute(tenantID int64, product string) { delete(s.keys, groupKey(tenantID, product)) }

// Muted reports whether the alert's group is muted.
func (s *Silence) Muted(a Alert) bool { return s.keys[a.GroupKey()] }

func groupKey(tenantID int64, product string) string {
	return fmt.Sprintf("%d|%s", tenantID, product)
}

// Converge applies the four-stage convergence to a batch of alerts. Order of
// application matters: dedup collapses re-fires, grouping then keeps only the
// highest-severity alert per group (inhibition falls out of that), and silence
// drops muted groups last — a muted group's alerts vanish entirely regardless
// of severity.
func Converge(alerts []Alert, silence *Silence) []Alert {
	seen := make(map[string]bool)
	best := make(map[string]Alert)

	for _, a := range alerts {
		if seen[a.DedupKey()] {
			continue // 1. 去重
		}
		seen[a.DedupKey()] = true

		if silence != nil && silence.Muted(a) {
			continue // 4. 静默 (before grouping so a muted group never re-enters)
		}

		cur, ok := best[a.GroupKey()]
		if !ok || a.Severity.Rank() > cur.Severity.Rank() {
			best[a.GroupKey()] = a // 2+3. 分组 + 抑制: keep the highest severity per group
		}
	}

	out := make([]Alert, 0, len(best))
	for _, a := range best {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OccurredAt.Before(out[j].OccurredAt) })
	return out
}

// TenantLimiter is a fixed-window per-tenant rate limiter (03§4.4.6: 10 条/分钟).
// It is deliberately a fixed window, not a token bucket: alert notification is
// bursty by nature, and the guarantee "no more than MaxTenantRate notifications
// per minute per tenant" is what a support plan SLA can promise.
type TenantLimiter struct {
	windowStart time.Time
	counts      map[int64]int
}

// NewTenantLimiter builds a limiter starting at now.
func NewTenantLimiter(now time.Time) *TenantLimiter {
	return &TenantLimiter{windowStart: now, counts: make(map[int64]int)}
}

// Allow reports whether the tenant may send one more notification in the
// current window, advancing the window when it has elapsed. The window reset is
// lazy: it happens on the next call after RateWindow passes.
func (l *TenantLimiter) Allow(tenantID int64, now time.Time) bool {
	if now.Sub(l.windowStart) >= RateWindow {
		l.windowStart = now
		l.counts = make(map[int64]int)
	}
	if l.counts[tenantID] >= MaxTenantRate {
		return false
	}
	l.counts[tenantID]++
	return true
}
