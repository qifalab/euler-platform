package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/storage/sqltest"
)

// support_db is shared: the quota service's migrations create id_sequence (the
// rule-id allocator needs it), the monitor's create alert_rule itself.
func newSQLTestRepo(t *testing.T) *sqlRuleRepo {
	t.Helper()
	db := sqltest.Open(t, sqltest.Services("svc-quota"), sqltest.Services("svc-monitor"))
	repo, err := newSQLRuleRepo(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

var testNow = time.Date(2026, 9, 15, 14, 0, 0, 0, time.UTC)

func newRule(acct int64, metric string) *AlertRule {
	return &AlertRule{
		AccountID: acct, ProductCode: "euecs", ResourceType: "instance",
		Metric: metric, Threshold: "80.0000", ComparisonOperator: cmpGreaterThanOrEqual,
		Period: 60, EvalPeriods: 1, NotificationChannels: []string{"IN_APP", "EMAIL"},
		Status: 1, Version: 1, CreatedAt: testNow, UpdatedAt: testNow,
	}
}

func TestSQLRuleRepoRoundTrip(t *testing.T) {
	repo := newSQLTestRepo(t)
	const acct = int64(100123)

	rule := newRule(acct, "cpu_utilization")
	if err := repo.Create(rule); err != nil {
		t.Fatal(err)
	}
	if rule.RuleID == 0 {
		t.Fatal("Create did not assign a rule id")
	}

	got, err := repo.Get(acct, rule.RuleID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("created rule not found")
	}
	if got.Metric != "cpu_utilization" || got.Threshold != "80.0000" ||
		got.ComparisonOperator != cmpGreaterThanOrEqual || got.Version != 1 ||
		len(got.NotificationChannels) != 2 {
		t.Fatalf("rule round trip = %+v (channels %v)", got, got.NotificationChannels)
	}

	// Get is scoped to the shard: another account cannot read the id.
	if other, err := repo.Get(999, rule.RuleID); err != nil || other != nil {
		t.Fatalf("cross-account Get = %+v (err %v)", other, err)
	}

	list, err := repo.List(acct)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list has %d rules, want 1", len(list))
	}
}

func TestSQLRuleRepoOptimisticLock(t *testing.T) {
	repo := newSQLTestRepo(t)
	const acct = int64(100123)

	rule := newRule(acct, "memory_utilization")
	if err := repo.Create(rule); err != nil {
		t.Fatal(err)
	}

	// Two readers, one writer: the loser must see ErrRuleConflict, not a silent
	// overwrite of the winner's edit.
	first, err := repo.Get(acct, rule.RuleID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.Get(acct, rule.RuleID)
	if err != nil {
		t.Fatal(err)
	}

	first.Threshold = "95.0000"
	first.Version++
	first.UpdatedAt = time.Now()
	if err := repo.Save(first, 1); err != nil {
		t.Fatalf("winner: %v", err)
	}

	second.Threshold = "99.0000"
	second.Version++
	if err := repo.Save(second, 1); !errors.Is(err, ErrRuleConflict) {
		t.Fatalf("loser: %v, want ErrRuleConflict", err)
	}

	// And the Delete is shard-scoped the same way.
	if err := repo.Delete(999, rule.RuleID); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.Get(acct, rule.RuleID); err != nil || got == nil {
		t.Fatalf("rule vanished after a cross-account delete: %+v (err %v)", got, err)
	}
	if err := repo.Delete(acct, rule.RuleID); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.Get(acct, rule.RuleID); err != nil || got != nil {
		t.Fatalf("deleted rule still visible: %+v (err %v)", got, err)
	}
}

// TestSQLRuleRepoSurvivesRestart is the alert-engine contract: the engine and
// the monitor are separate processes, and neither may forget what a tenant
// asked to be alerted about because the other one restarted.
func TestSQLRuleRepoSurvivesRestart(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-quota"), sqltest.Services("svc-monitor"))
	ctx := context.Background()

	first, err := newSQLRuleRepo(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	rule := newRule(100123, "request_count")
	if err := first.Create(rule); err != nil {
		t.Fatal(err)
	}

	// A fresh process (new repo, fresh allocator segment) sees the rule.
	restarted, err := newSQLRuleRepo(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.Get(100123, rule.RuleID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Metric != "request_count" {
		t.Fatalf("restarted Get = %+v", got)
	}

	// Delete then create again is a NEW rule (new id), not a resurrection of
	// the old row with the old id: a stale console tab holding ruleId must not
	// silently re-target a different rule.
	if err := restarted.Delete(100123, rule.RuleID); err != nil {
		t.Fatal(err)
	}
	again := newRule(100123, "request_count")
	if err := restarted.Create(again); err != nil {
		t.Fatal(err)
	}
	if again.RuleID == rule.RuleID {
		t.Fatalf("re-created rule reused id %d", rule.RuleID)
	}
}

// TestSQLRuleRepoMatchesMemoryStore runs one create→edit→read script through
// both implementations and requires the same observable outcome.
func TestSQLRuleRepoMatchesMemoryStore(t *testing.T) {
	run := func(repo ruleRepo) *AlertRule {
		t.Helper()
		rule := newRule(100123, "disk_usage")
		if err := repo.Create(rule); err != nil {
			t.Fatalf("create: %v", err)
		}
		rule.Threshold = "85.5000"
		rule.Version++
		rule.UpdatedAt = time.Now()
		if err := repo.Save(rule, 1); err != nil {
			t.Fatalf("save: %v", err)
		}
		got, err := repo.Get(100123, rule.RuleID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return got
	}

	sqlGot := run(newSQLTestRepo(t))
	memGot := run(newMemRuleRepo())

	if sqlGot.Threshold != memGot.Threshold || sqlGot.Version != memGot.Version ||
		sqlGot.Status != memGot.Status || len(sqlGot.NotificationChannels) != len(memGot.NotificationChannels) {
		t.Fatalf("sql %+v, memory %+v", sqlGot, memGot)
	}
	if sqlGot.Threshold != "85.5000" {
		t.Fatalf("threshold = %s, want the DECIMAL round trip to be exact", sqlGot.Threshold)
	}
}
