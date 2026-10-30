package automation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

// crossProject lets the fixture resolve a second destination project with the
// same membership and permission rows, so a package rule can target it.
func (f *fixture) crossProject() {
	f.engine = New(f.rt, func(ctx context.Context, tx *sql.Tx, actor, tenant, project, app string) (appkit.Scope, error) {
		if project != "project2" {
			return f.resolve(ctx, tx, actor, tenant, project, app)
		}
		s, err := f.resolve(ctx, tx, actor, tenant, "project", app)
		s.ProjectID = project
		return s, err
	})
}

func (f *fixture) execution(t *testing.T, id string) (state string, attempts int) {
	t.Helper()
	if err := f.rt.DB.QueryRow("SELECT state,attempts FROM automation_executions WHERE id=?", id).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	return state, attempts
}

func (f *fixture) onlyExecution(t *testing.T) Execution {
	t.Helper()
	xs, err := f.engine.Executions(context.Background(), "tenant", "project")
	if err != nil || len(xs) != 1 {
		t.Fatalf("want exactly one execution: %+v %v", xs, err)
	}
	return xs[0]
}

func (f *fixture) databaseQuotaMB(t *testing.T) float64 {
	t.Helper()
	v := request(t, f.database, "owner", "GET", "/packages", "", 200)
	return v["quota"].(map[string]any)["mysqlMB"].(float64)
}

// A package rule may target another project of the same team. Rule-level audit
// (create, pause/resume, retry) belongs to the rule's own project, like Create;
// only the resource-package audit belongs to the destination application.
func TestCrossProjectRuleAuditStaysInRuleProject(t *testing.T) {
	f := newFixture(t)
	f.crossProject()
	scheme, _ := f.prepare(t)
	if _, err := f.rt.DB.Exec(`INSERT INTO database_templates(id,tenant_id,project_id,name,description,quota,days,active,created_at) VALUES('tpl-project2','tenant','project2','跨项目资源','','{"mysqlMB":200,"mysqlCount":1}',1,1,?)`, appkit.Now()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rule, err := f.engine.Create(ctx, "owner", "tenant", "project", Rule{Name: "跨项目奖励", Source: "trust.status", FilterID: scheme, Action: "package", TargetProjectID: "project2", TargetApp: "database", TemplateID: "tpl-project2"})
	if err != nil {
		t.Fatal(err)
	}
	f.approve(t, scheme)
	// The rule creator temporarily loses the destination grant, so the task fails.
	if _, err = f.rt.DB.Exec("DELETE FROM permissions WHERE actor='owner' AND app='database' AND permission='admin'"); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	x := f.onlyExecution(t)
	if x.State != "failed" {
		t.Fatalf("revoked destination grant did not fail the task: %+v", x)
	}
	if _, err = f.rt.DB.Exec("INSERT INTO permissions VALUES('owner','database','admin')"); err != nil {
		t.Fatal(err)
	}
	if err = f.engine.Retry(ctx, "owner", "tenant", "project", x.ID); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if n := f.count(t, "SELECT COUNT(*) FROM database_grants WHERE tenant_id='tenant' AND project_id='project2'"); n != 1 {
		t.Fatalf("destination project grant missing: %d", n)
	}
	if err = f.engine.SetEnabled(ctx, "owner", "tenant", "project", rule.ID, false); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"automation.rule.created", "automation.execution.retry", "automation.rule.status"} {
		var project string
		if err = f.rt.DB.QueryRow("SELECT project_id FROM audit WHERE action=?", action).Scan(&project); err != nil {
			t.Fatalf("%s audit missing: %v", action, err)
		}
		if project != "project" {
			t.Fatalf("%s audit written to %q instead of the rule's project", action, project)
		}
	}
	var project string
	if err = f.rt.DB.QueryRow("SELECT project_id FROM audit WHERE action='database.package.automation_granted'").Scan(&project); err != nil || project != "project2" {
		t.Fatalf("package audit belongs to the destination project: %q %v", project, err)
	}
}

// AUTOMATION.md: pausing a rule cancels tasks that have not run yet. A task that
// was queued (or waiting in retry backoff) must not run after a quick re-enable.
func TestPausingRuleCancelsQueuedTasks(t *testing.T) {
	f := newFixture(t)
	scheme, template := f.prepare(t)
	rule := f.rule(t, "trust.status", scheme, "package", template)
	f.approve(t, scheme)
	ctx := context.Background()
	if err := f.engine.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	x := f.onlyExecution(t)
	if x.State != "pending" {
		t.Fatalf("queued task missing: %+v", x)
	}
	if err := f.engine.SetEnabled(ctx, "owner", "tenant", "project", rule.ID, false); err != nil {
		t.Fatal(err)
	}
	if state, _ := f.execution(t, x.ID); state != "cancelled" {
		t.Fatalf("paused rule left its queued task %s", state)
	}
	// Re-enabling subscribes to later events only; the cancelled task stays cancelled.
	if err := f.engine.SetEnabled(ctx, "owner", "tenant", "project", rule.ID, true); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if state, _ := f.execution(t, x.ID); state != "cancelled" {
		t.Fatalf("cancelled task resumed as %s", state)
	}
	if n := f.count(t, "SELECT COUNT(*) FROM database_grants"); n != 0 {
		t.Fatalf("task queued before the pause still granted a package: %d", n)
	}
}

// A claimed task whose worker has not committed yet is cancelled too; the
// stale worker must not complete it after the pause.
func TestPausingRuleCancelsClaimedTask(t *testing.T) {
	f := newFixture(t)
	scheme, template := f.prepare(t)
	rule := f.rule(t, "trust.status", scheme, "package", template)
	f.approve(t, scheme)
	ctx := context.Background()
	if err := f.engine.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	j, err := f.engine.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.engine.SetEnabled(ctx, "owner", "tenant", "project", rule.ID, false); err != nil {
		t.Fatal(err)
	}
	if err = f.engine.SetEnabled(ctx, "owner", "tenant", "project", rule.ID, true); err != nil {
		t.Fatal(err)
	}
	if err = f.engine.execute(ctx, j); err != nil {
		t.Fatal(err)
	}
	if state, _ := f.execution(t, j.id); state != "cancelled" {
		t.Fatalf("claimed task completed after the pause: %s", state)
	}
	if n := f.count(t, "SELECT COUNT(*) FROM database_grants"); n != 0 {
		t.Fatalf("claimed task granted after the pause: %d", n)
	}
}

// AUTOMATION.md: revocation makes the package expire immediately. The existing
// revocation test only checks the receipt; this checks the quota really ends.
func TestRevocationEndsActivePackageQuota(t *testing.T) {
	f := newFixture(t)
	scheme, template := f.prepare(t)
	f.rule(t, "trust.status", scheme, "package", template)
	id := f.approve(t, scheme)
	f.tick(t)
	if got := f.databaseQuotaMB(t); got != 300 {
		t.Fatalf("granted package not counted: %v", got)
	}
	request(t, f.trust, "owner", "POST", "/review/submissions/"+id, `{"status":"rejected","reason":"撤销资格","version":2}`, 200)
	f.tick(t)
	if got := f.databaseQuotaMB(t); got != 100 {
		t.Fatalf("revoked package still counted: %v", got)
	}
}

// Revoking a package that already expired must keep its recorded expiry, not
// move it forward to the revocation time.
func TestRevocationNeverExtendsAnExpiredPackage(t *testing.T) {
	f := newFixture(t)
	scheme, template := f.prepare(t)
	f.rule(t, "trust.status", scheme, "package", template)
	id := f.approve(t, scheme)
	f.tick(t)
	expired := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := f.rt.DB.Exec("UPDATE database_grants SET expires_at=?", expired); err != nil {
		t.Fatal(err)
	}
	request(t, f.trust, "owner", "POST", "/review/submissions/"+id, `{"status":"rejected","reason":"撤销资格","version":2}`, 200)
	f.tick(t)
	if n := f.count(t, "SELECT COUNT(*) FROM automation_entitlements WHERE revoked_at>0"); n != 1 {
		t.Fatalf("revocation receipt missing: %d", n)
	}
	var got string
	if err := f.rt.DB.QueryRow("SELECT expires_at FROM database_grants").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != expired {
		t.Fatalf("revocation moved an expired package from %s to %s", expired, got)
	}
}

// AUTOMATION.md: invalid configuration is a deterministic failure that goes to
// the failed queue at once; after the admin fixes it, an explicit retry runs.
func TestDisabledTemplateFailsAtOnceAndRetryAfterFix(t *testing.T) {
	f := newFixture(t)
	scheme, template := f.prepare(t)
	f.rule(t, "trust.status", scheme, "package", template)
	request(t, f.database, "owner", "DELETE", "/admin/templates/"+template, "", 200)
	f.approve(t, scheme)
	f.tick(t)
	x := f.onlyExecution(t)
	if x.State != "failed" || x.Attempts != 1 || x.Error != "资源包模板已停用或无效" {
		// %+q keeps the log ASCII-only.
		t.Fatalf("disabled template treated as transient: state=%s attempts=%d error=%+q", x.State, x.Attempts, x.Error)
	}
	request(t, f.database, "owner", "POST", "/admin/templates/"+template+"/enable", "", 200)
	if err := f.engine.Retry(context.Background(), "owner", "tenant", "project", x.ID); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if state, _ := f.execution(t, x.ID); state != "succeeded" {
		t.Fatalf("retry after fixing the template did not finish: %s", state)
	}
	if n := f.count(t, "SELECT COUNT(*) FROM database_grants"); n != 1 {
		t.Fatalf("grant missing after retry: %d", n)
	}
}

// Transient failures back off by attempts² seconds and stop after 5 attempts.
func TestTransientFailureBacksOffThenFails(t *testing.T) {
	f := newFixture(t)
	scheme, template := f.prepare(t)
	f.rule(t, "trust.status", scheme, "package", template)
	f.approve(t, scheme)
	ctx := context.Background()
	if err := f.engine.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 5; attempt++ {
		j, err := f.engine.claim(ctx)
		if err != nil {
			t.Fatalf("attempt %d not claimable: %v", attempt, err)
		}
		before := time.Now()
		if err = f.engine.fail(ctx, j, sql.ErrConnDone); err != nil {
			t.Fatal(err)
		}
		var state string
		var next int64
		if err = f.rt.DB.QueryRow("SELECT state,next_at FROM automation_executions WHERE id=?", j.id).Scan(&state, &next); err != nil {
			t.Fatal(err)
		}
		if attempt < 5 {
			delay := time.Duration(attempt*attempt) * time.Second
			if state != "pending" || next < before.Add(delay).UnixMilli() || next > time.Now().Add(delay).UnixMilli() {
				t.Fatalf("attempt %d: state %s next in %v, want pending after %v", attempt, state, time.UnixMilli(next).Sub(before), delay)
			}
			if _, err = f.rt.DB.Exec("UPDATE automation_executions SET next_at=0 WHERE id=?", j.id); err != nil {
				t.Fatal(err)
			}
		} else if state != "failed" {
			t.Fatalf("five transient failures left the task %s", state)
		}
	}
	if _, err := f.engine.claim(ctx); err != sql.ErrNoRows {
		t.Fatalf("failed task still claimable: %v", err)
	}
}

func appStatus(err error) int {
	var ae *appkit.Error
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// ascii keeps failure output ASCII-only (error texts are Chinese).
func ascii(err error) string { return fmt.Sprintf("%+q", fmt.Sprint(err)) }

// The platform's respondError maps an untyped sql.ErrNoRows to HTTP 500. The
// engine must return typed errors: 404 for an unknown or foreign rule/task and
// 409 when retrying a failed task whose rule is paused.
func TestUnknownIDsAndPausedRetryReturnTypedErrors(t *testing.T) {
	f := newFixture(t)
	scheme, template := f.prepare(t)
	rule := f.rule(t, "trust.status", scheme, "package", template)
	ctx := context.Background()
	if err := f.engine.SetEnabled(ctx, "owner", "tenant", "project", "rule_missing", false); appStatus(err) != 404 {
		t.Fatalf("unknown rule: %s", ascii(err))
	}
	if err := f.engine.SetEnabled(ctx, "owner", "tenant", "other", rule.ID, false); appStatus(err) != 404 {
		t.Fatalf("rule of another project: %s", ascii(err))
	}
	if err := f.engine.Retry(ctx, "owner", "tenant", "project", "missing"); appStatus(err) != 404 {
		t.Fatalf("unknown execution: %s", ascii(err))
	}
	if _, err := f.engine.Create(ctx, "owner", "tenant", "project", Rule{Name: "x", Source: "trust.status", FilterID: scheme, Action: "package", TargetProjectID: "project", TargetApp: "database", TemplateID: "missing"}); appStatus(err) != 404 {
		t.Fatalf("rule with unknown template: %s", ascii(err))
	}
	f.approve(t, scheme)
	if _, err := f.rt.DB.Exec("DELETE FROM permissions WHERE actor='owner' AND app='database' AND permission='admin'"); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	x := f.onlyExecution(t)
	if x.State != "failed" {
		t.Fatalf("want a failed task, got %s", x.State)
	}
	if _, err := f.rt.DB.Exec("INSERT INTO permissions VALUES('owner','database','admin')"); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Retry(ctx, "owner", "tenant", "other", x.ID); appStatus(err) != 404 {
		t.Fatalf("task of another project: %s", ascii(err))
	}
	if err := f.engine.SetEnabled(ctx, "owner", "tenant", "project", rule.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Retry(ctx, "owner", "tenant", "project", x.ID); appStatus(err) != 409 {
		t.Fatalf("retry of a paused rule's task: %s", ascii(err))
	}
	if state, _ := f.execution(t, x.ID); state != "failed" {
		t.Fatalf("refused retry changed the task to %s", state)
	}
	if err := f.engine.SetEnabled(ctx, "owner", "tenant", "project", rule.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Retry(ctx, "owner", "tenant", "project", x.ID); err != nil {
		t.Fatalf("retry after re-enabling: %s", ascii(err))
	}
	f.tick(t)
	if n := f.count(t, "SELECT COUNT(*) FROM database_grants"); n != 1 {
		t.Fatalf("explicit retry after re-enabling did not grant: %d", n)
	}
}
