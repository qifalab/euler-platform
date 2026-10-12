// Package automation owns durable, project-scoped application workflows.
package automation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

// Resolve must use the supplied transaction for every permission/install query.
// No browser-created scope or historical permission snapshot authorizes a job.
type Resolve func(context.Context, *sql.Tx, string, string, string, string) (appkit.Scope, error)
type Engine struct {
	rt      *appkit.Runtime
	resolve Resolve
}

func New(rt *appkit.Runtime, resolve Resolve) *Engine { return &Engine{rt: rt, resolve: resolve} }

type Rule struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	TenantID        string `json:"tenantId"`
	ProjectID       string `json:"projectId"`
	CreatedBy       string `json:"createdBy"`
	Source          string `json:"source"`
	FilterID        string `json:"filterId"`
	Action          string `json:"action"`
	TargetProjectID string `json:"targetProjectId"`
	TargetApp       string `json:"targetApp"`
	TemplateID      string `json:"templateId"`
	Enabled         bool   `json:"enabled"`
	CreatedAt       int64  `json:"createdAt"`
}
type Execution struct {
	ID        string `json:"id"`
	RuleID    string `json:"ruleId"`
	EventID   string `json:"eventId"`
	State     string `json:"state"`
	Attempts  int    `json:"attempts"`
	Error     string `json:"error"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}
type Notification struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Read      bool   `json:"read"`
	CreatedAt int64  `json:"createdAt"`
}

func (e *Engine) Migrate(ctx context.Context) error {
	if err := appkit.EnsureEvents(ctx, e.rt.DB); err != nil {
		return err
	}
	_, err := e.rt.DB.ExecContext(ctx, `
 CREATE TABLE IF NOT EXISTS automation_rules(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,created_by TEXT NOT NULL,source TEXT NOT NULL,filter_id TEXT NOT NULL,body TEXT NOT NULL,enabled INTEGER NOT NULL,created_at INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS automation_rule_scope ON automation_rules(tenant_id,project_id,source,filter_id);
 CREATE TABLE IF NOT EXISTS automation_executions(id TEXT PRIMARY KEY,rule_id TEXT NOT NULL,event_id TEXT NOT NULL,state TEXT NOT NULL,attempts INTEGER NOT NULL DEFAULT 0,next_at INTEGER NOT NULL,lease_until INTEGER NOT NULL DEFAULT 0,lease_token TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,UNIQUE(rule_id,event_id));
 CREATE INDEX IF NOT EXISTS automation_execution_due ON automation_executions(state,next_at,lease_until);
 CREATE TABLE IF NOT EXISTS automation_notifications(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,actor_id TEXT NOT NULL,title TEXT NOT NULL,body TEXT NOT NULL,read_at INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS automation_notification_actor ON automation_notifications(tenant_id,project_id,actor_id,created_at);
 CREATE TABLE IF NOT EXISTS automation_entitlements(id TEXT PRIMARY KEY,rule_id TEXT NOT NULL,subject_id TEXT NOT NULL,grant_id TEXT NOT NULL,target_app TEXT NOT NULL,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,source_resource TEXT NOT NULL,revoked_at INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL,UNIQUE(rule_id,subject_id));
 `)
	return err
}

func sourcePermission(source string) (string, string) {
	switch source {
	case "trust.status":
		return "trust", "review"
	case "eid.status":
		return "eid", "review"
	case "lottery.won":
		return "lottery", "manage"
	}
	return "", ""
}
func (e *Engine) authorize(ctx context.Context, tx *sql.Tx, r Rule) (appkit.Scope, error) {
	app, permission := sourcePermission(r.Source)
	if app == "" {
		return appkit.Scope{}, appkit.Invalid("不支持的事件来源")
	}
	s, err := e.resolve(ctx, tx, r.CreatedBy, r.TenantID, r.ProjectID, app)
	if err != nil {
		return s, err
	}
	if err = s.Require(permission); err != nil {
		return s, err
	}
	if r.Action == "package" {
		target, err := e.resolve(ctx, tx, r.CreatedBy, r.TenantID, r.TargetProjectID, r.TargetApp)
		if err != nil {
			return target, err
		}
		if err = target.Require("admin"); err != nil {
			return target, err
		}
		return target, nil
	}
	return s, nil
}
func (e *Engine) Create(ctx context.Context, actor, tenant, project string, r Rule) (Rule, error) {
	var err error
	r.Name, err = appkit.Name(r.Name, 100)
	if err != nil {
		return r, err
	}
	if r.FilterID == "" || len(r.FilterID) > 200 {
		return r, appkit.Invalid("请选择明确的认证方案、成员申请或活动")
	}
	if r.Action != "package" && r.Action != "notify" {
		return r, appkit.Invalid("仅支持资源包和站内通知")
	}
	if r.Action == "package" && (r.TargetApp != "database" && r.TargetApp != "storage" || r.TemplateID == "" || r.TargetProjectID == "") {
		return r, appkit.Invalid("请指定目标项目、应用和资源包模板")
	}
	r.ID = appkit.NewID("rule_")
	r.TenantID = tenant
	r.ProjectID = project
	r.CreatedBy = actor
	r.Enabled = true
	r.CreatedAt = time.Now().UnixMilli()
	err = e.rt.Transaction(ctx, func(tx *sql.Tx) error {
		s, err := e.authorize(ctx, tx, r)
		if err != nil {
			return err
		}
		var count int
		switch r.Source {
		case "trust.status":
			err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM trust_schemes WHERE tenant_id=? AND project_id=? AND id=? AND status='active'", tenant, project, r.FilterID).Scan(&count)
		case "eid.status": // Explicit member enrollment campaign: the project is the scope.
			if r.FilterID != "club" {
				return appkit.Invalid("成员资格来源请选择 club")
			}
			count = 1
		case "lottery.won":
			err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM lottery_rooms WHERE tenant_id=? AND project_id=? AND id=?", tenant, project, r.FilterID).Scan(&count)
		}
		if err != nil {
			return err
		}
		if count != 1 {
			return appkit.NotFound()
		}
		if r.Action == "package" {
			if err = validateTemplate(ctx, tx, r); err != nil {
				return err
			}
		}
		raw, _ := json.Marshal(r)
		_, err = tx.ExecContext(ctx, "INSERT INTO automation_rules VALUES(?,?,?,?,?,?,?,?,?)", r.ID, tenant, project, actor, r.Source, r.FilterID, string(raw), true, r.CreatedAt)
		if err != nil {
			return err
		}
		s.ProjectID = project
		s.ApplicationID = "automation"
		return e.rt.Audit(ctx, tx, s, "rule.created", r.ID, "创建显式应用联动规则")
	})
	return r, err
}
func (e *Engine) Rules(ctx context.Context, tenant, project string) ([]Rule, error) {
	rows, err := e.rt.DB.QueryContext(ctx, "SELECT body,enabled FROM automation_rules WHERE tenant_id=? AND project_id=? ORDER BY created_at DESC", tenant, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Rule{}
	for rows.Next() {
		var r Rule
		var raw string
		var enabled bool
		if err = rows.Scan(&raw, &enabled); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, err
		}
		r.Enabled = enabled
		out = append(out, r)
	}
	return out, rows.Err()
}
func (e *Engine) SetEnabled(ctx context.Context, actor, tenant, project, id string, enabled bool) error {
	return e.rt.Transaction(ctx, func(tx *sql.Tx) error {
		var raw string
		if err := tx.QueryRowContext(ctx, "SELECT body FROM automation_rules WHERE id=? AND tenant_id=? AND project_id=?", id, tenant, project).Scan(&raw); err != nil {
			return err
		}
		var r Rule
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return err
		}
		r.CreatedBy = actor
		s, err := e.authorize(ctx, tx, r)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE automation_rules SET enabled=? WHERE id=?", enabled, id)
		if err != nil {
			return err
		}
		s.ApplicationID = "automation"
		return e.rt.Audit(ctx, tx, s, "rule.status", id, "调整应用联动规则状态；既有权益按原有效期保留")
	})
}

// Dispatch and action commit are distinct durable transactions. The unique
// rule/event key makes dispatch retry safe; action and completion share a commit.
func (e *Engine) Dispatch(ctx context.Context) error {
	return e.rt.Transaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT id,tenant_id,project_id,source,filter_id,created_at FROM automation_events WHERE dispatched_at=0 ORDER BY created_at,id LIMIT 100")
		if err != nil {
			return err
		}
		type pending struct {
			id, tenant, project, source, filter string
			at                                  int64
		}
		events := []pending{}
		for rows.Next() {
			var v pending
			if err = rows.Scan(&v.id, &v.tenant, &v.project, &v.source, &v.filter, &v.at); err != nil {
				rows.Close()
				return err
			}
			events = append(events, v)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		now := time.Now().UnixMilli()
		for _, v := range events {
			_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO automation_executions(id,rule_id,event_id,state,next_at,created_at,updated_at) SELECT id||':'||?,id,?,'pending',?,?,? FROM automation_rules WHERE tenant_id=? AND project_id=? AND source=? AND filter_id=? AND enabled=1 AND created_at<=?`, v.id, v.id, now, now, now, v.tenant, v.project, v.source, v.filter, v.at)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE automation_events SET dispatched_at=? WHERE id=?", now, v.id); err != nil {
				return err
			}
		}
		return nil
	})
}

type job struct {
	id, rule, event, token string
	attempts               int
}

func (e *Engine) claim(ctx context.Context) (job, error) {
	var j job
	err := e.rt.Transaction(ctx, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		err := tx.QueryRowContext(ctx, "SELECT id,rule_id,event_id,attempts FROM automation_executions WHERE (state='pending' AND next_at<=?) OR (state='running' AND lease_until<?) ORDER BY next_at,id LIMIT 1", now, now).Scan(&j.id, &j.rule, &j.event, &j.attempts)
		if err != nil {
			return err
		}
		j.token = appkit.NewID("")
		j.attempts++
		_, err = tx.ExecContext(ctx, "UPDATE automation_executions SET state='running',attempts=?,lease_token=?,lease_until=?,updated_at=? WHERE id=?", j.attempts, j.token, now+60000, now, j.id)
		return err
	})
	return j, err
}

func (e *Engine) execute(ctx context.Context, j job) error {
	return e.rt.Transaction(ctx, func(tx *sql.Tx) error {
		var token, state, raw, eventRaw string
		var enabled bool
		if err := tx.QueryRowContext(ctx, "SELECT lease_token,state FROM automation_executions WHERE id=?", j.id).Scan(&token, &state); err != nil {
			return err
		}
		if token != j.token || state != "running" {
			return nil
		}
		if err := tx.QueryRowContext(ctx, "SELECT body,enabled FROM automation_rules WHERE id=?", j.rule).Scan(&raw, &enabled); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, "SELECT body FROM automation_events WHERE id=?", j.event).Scan(&eventRaw); err != nil {
			return err
		}
		var rule Rule
		var event appkit.Event
		if err := json.Unmarshal([]byte(raw), &rule); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(eventRaw), &event); err != nil {
			return err
		}
		next := "succeeded"
		if !enabled {
			next = "cancelled"
		} else {
			s, err := e.authorize(ctx, tx, rule)
			if err != nil {
				return err
			}
			eligible, err := currentEligibility(ctx, tx, rule, event)
			if err != nil {
				return err
			}
			if eligible {
				app, _ := sourcePermission(rule.Source)
				if _, err = e.resolve(ctx, tx, event.SubjectID, rule.TenantID, rule.ProjectID, app); err != nil {
					return err
				}
			}
			if rule.Action == "package" {
				if eligible {
					// The beneficiary must still belong to the explicitly chosen destination.
					if _, err = e.resolve(ctx, tx, event.SubjectID, rule.TenantID, rule.TargetProjectID, rule.TargetApp); err != nil {
						return err
					}
					err = e.grant(ctx, tx, s, rule, event)
				} else {
					err = e.revoke(ctx, tx, s, rule, event)
				}
				if err != nil {
					return err
				}
			} else if eligible && event.SubjectID != "" {
				s.ApplicationID = "automation"
				_, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO automation_notifications(id,tenant_id,project_id,actor_id,title,body,created_at) VALUES(?,?,?,?,?,?,?)", j.id, rule.TenantID, rule.ProjectID, event.SubjectID, rule.Name, "您满足了活动或认证规则，请在对应应用查看最新结果。", time.Now().UnixMilli())
				if err != nil {
					return err
				}
				if err = e.rt.Audit(ctx, tx, s, "notification.created", j.id, "投递应用联动站内通知"); err != nil {
					return err
				}
			} else {
				next = "skipped"
			}
		}
		_, err := tx.ExecContext(ctx, "UPDATE automation_executions SET state=?,error='',lease_token='',lease_until=0,updated_at=? WHERE id=? AND lease_token=?", next, time.Now().UnixMilli(), j.id, j.token)
		return err
	})
}

func (e *Engine) fail(ctx context.Context, j job, cause error) error {
	state := "pending"
	if j.attempts >= 5 {
		state = "failed"
	}
	message := "应用暂不可用，等待重试"
	var ae *appkit.Error
	if errors.As(cause, &ae) {
		message = ae.Message
		if ae.Status == 400 || ae.Status == 403 || ae.Status == 404 {
			state = "failed"
		}
	}
	// Never persist raw SQL/network errors; they may contain connection secrets.
	_, err := e.rt.DB.ExecContext(ctx, "UPDATE automation_executions SET state=?,error=?,next_at=?,lease_until=0,lease_token='',updated_at=? WHERE id=? AND lease_token=?", state, message, time.Now().Add(time.Duration(j.attempts*j.attempts)*time.Second).UnixMilli(), time.Now().UnixMilli(), j.id, j.token)
	return err
}
func (e *Engine) Tick(ctx context.Context) error {
	if err := e.Dispatch(ctx); err != nil {
		return err
	}
	for i := 0; i < 100; i++ {
		j, err := e.claim(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = e.execute(ctx, j); err != nil {
			if err = e.fail(ctx, j, err); err != nil {
				return err
			}
		}
	}
	return nil
}
func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := e.Tick(ctx); err != nil && ctx.Err() == nil {
			// Avoid logging raw database/connection errors, which can include secrets.
			log.Printf("automation worker tick failed (%T); pending work remains durable", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func currentEligibility(ctx context.Context, tx *sql.Tx, r Rule, event appkit.Event) (bool, error) {
	if event.SubjectID == "" {
		return false, nil
	}
	var n int
	var err error
	switch r.Source {
	case "trust.status":
		err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM trust_submissions s JOIN trust_schemes c ON c.id=s.scheme_id AND c.tenant_id=s.tenant_id AND c.project_id=s.project_id WHERE s.tenant_id=? AND s.project_id=? AND s.scheme_id=? AND s.actor_id=? AND s.status='approved' AND c.status='active'", r.TenantID, r.ProjectID, r.FilterID, event.SubjectID).Scan(&n)
	case "eid.status":
		err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM eid_club WHERE tenant_id=? AND project_id=? AND actor_id=? AND status='offer_confirmed'", r.TenantID, r.ProjectID, event.SubjectID).Scan(&n)
	case "lottery.won":
		err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lottery_winners w JOIN lottery_draws d ON d.id=w.draw_id JOIN lottery_rooms r ON r.id=d.room_id JOIN lottery_identities i ON i.participant_id=w.participant_id WHERE r.tenant_id=? AND r.project_id=? AND r.id=? AND w.participant_id=? AND i.actor_id=? AND d.id=?`, r.TenantID, r.ProjectID, r.FilterID, event.ResourceID, event.SubjectID, event.Version).Scan(&n)
	default:
		return false, appkit.Invalid("不支持的联动来源")
	}
	return n > 0, err
}

func (e *Engine) Executions(ctx context.Context, tenant, project string) ([]Execution, error) {
	rows, err := e.rt.DB.QueryContext(ctx, `SELECT x.id,x.rule_id,x.event_id,x.state,x.attempts,x.error,x.created_at,x.updated_at FROM automation_executions x JOIN automation_rules r ON r.id=x.rule_id WHERE r.tenant_id=? AND r.project_id=? ORDER BY x.created_at DESC,x.id LIMIT 200`, tenant, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Execution{}
	for rows.Next() {
		var v Execution
		if err = rows.Scan(&v.ID, &v.RuleID, &v.EventID, &v.State, &v.Attempts, &v.Error, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (e *Engine) Retry(ctx context.Context, actor, tenant, project, id string) error {
	return e.rt.Transaction(ctx, func(tx *sql.Tx) error {
		var raw string
		if err := tx.QueryRowContext(ctx, "SELECT r.body FROM automation_rules r JOIN automation_executions x ON x.rule_id=r.id WHERE x.id=? AND r.tenant_id=? AND r.project_id=? AND r.enabled=1", id, tenant, project).Scan(&raw); err != nil {
			return err
		}
		var r Rule
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return err
		}
		r.CreatedBy = actor
		s, err := e.authorize(ctx, tx, r)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "UPDATE automation_executions SET state='pending',attempts=0,error='',next_at=?,updated_at=? WHERE id=? AND state='failed'", time.Now().UnixMilli(), time.Now().UnixMilli(), id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return appkit.Conflict("仅失败任务可以重新排队")
		}
		s.ApplicationID = "automation"
		return e.rt.Audit(ctx, tx, s, "execution.retry", id, "显式重试失败联动；执行时仍重新验证规则创建人")
	})
}
func (e *Engine) Notifications(ctx context.Context, actor, tenant, project string) ([]Notification, error) {
	rows, err := e.rt.DB.QueryContext(ctx, "SELECT id,title,body,read_at>0,created_at FROM automation_notifications WHERE tenant_id=? AND project_id=? AND actor_id=? ORDER BY created_at DESC LIMIT 100", tenant, project, actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var v Notification
		if err = rows.Scan(&v.ID, &v.Title, &v.Body, &v.Read, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (e *Engine) ReadNotification(ctx context.Context, actor, tenant, project, id string) error {
	_, err := e.rt.DB.ExecContext(ctx, "UPDATE automation_notifications SET read_at=? WHERE id=? AND actor_id=? AND tenant_id=? AND project_id=?", time.Now().UnixMilli(), id, actor, tenant, project)
	return err
}
