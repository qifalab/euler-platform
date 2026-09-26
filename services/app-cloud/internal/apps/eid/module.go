// Package eid implements Euler-owned member credentials and club recruitment.
// Business reference: miaojilab/emoera-eid, Apache-2.0, revision f987e818e83752ceeab93611d76204c79877a855.
// This is an independent Go implementation; no original service or login is used.
package eid

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

type Module struct {
	rt            *appkit.Runtime
	mux           *http.ServeMux
	sendMu        sync.Mutex
	mailer        func(context.Context, Settings, string, string, string) error
	client        *http.Client
	smtpTLSConfig func(string) *tls.Config
}

func New(rt *appkit.Runtime) *Module {
	m := &Module{rt: rt, mux: http.NewServeMux(), client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect rejected") }}}
	m.mailer = m.sendSMTP
	m.routes()
	return m
}
func (m *Module) ID() string            { return "eid" }
func (m *Module) Handler() http.Handler { return m.mux }

func (m *Module) Migrate(ctx context.Context) error {
	_, err := m.rt.DB.ExecContext(ctx, `
 CREATE TABLE IF NOT EXISTS eid_members(installation_id TEXT NOT NULL,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,actor_id TEXT NOT NULL,display_name TEXT NOT NULL,data BLOB NOT NULL,identity_level INTEGER NOT NULL DEFAULT 0,identity_title TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(installation_id,actor_id));
 CREATE TABLE IF NOT EXISTS eid_verifications(id TEXT PRIMARY KEY,installation_id TEXT NOT NULL,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,actor_id TEXT NOT NULL,data BLOB NOT NULL,status TEXT NOT NULL CHECK(status IN ('pending','approved','rejected')),identity_type TEXT NOT NULL,version INTEGER NOT NULL DEFAULT 1,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
 CREATE UNIQUE INDEX IF NOT EXISTS eid_one_pending ON eid_verifications(installation_id,actor_id) WHERE status='pending';
 CREATE INDEX IF NOT EXISTS eid_verifications_scope ON eid_verifications(tenant_id,project_id,installation_id,actor_id,created_at);
 CREATE TABLE IF NOT EXISTS eid_club(id TEXT PRIMARY KEY,installation_id TEXT NOT NULL,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,actor_id TEXT NOT NULL,data BLOB NOT NULL,status TEXT NOT NULL CHECK(status IN ('pending','interview_sent','offer_sent','offer_confirmed','rejected')),trust_scheme_id TEXT NOT NULL,trust_verified INTEGER NOT NULL,version INTEGER NOT NULL DEFAULT 1,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,interview_sent_at TEXT NOT NULL DEFAULT '',offer_sent_at TEXT NOT NULL DEFAULT '',confirmed_at TEXT NOT NULL DEFAULT '');
 CREATE UNIQUE INDEX IF NOT EXISTS eid_one_active_club ON eid_club(installation_id,actor_id) WHERE status!='rejected';
 CREATE INDEX IF NOT EXISTS eid_club_scope ON eid_club(tenant_id,project_id,installation_id,actor_id,created_at);
 CREATE TABLE IF NOT EXISTS eid_settings(installation_id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,data BLOB NOT NULL,updated_at TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS eid_events(id TEXT PRIMARY KEY,installation_id TEXT NOT NULL,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,target_id TEXT NOT NULL,actor_id TEXT NOT NULL,action TEXT NOT NULL,from_status TEXT NOT NULL,to_status TEXT NOT NULL,created_at TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS eid_events_target ON eid_events(installation_id,target_id,created_at);
 CREATE TABLE IF NOT EXISTS eid_query_keys(id TEXT PRIMARY KEY,installation_id TEXT NOT NULL,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,name TEXT NOT NULL,token_hash TEXT NOT NULL UNIQUE,actors TEXT NOT NULL,expires_at INTEGER NOT NULL,created_at TEXT NOT NULL,revoked_at TEXT NOT NULL DEFAULT '');
 CREATE TABLE IF NOT EXISTS eid_notifications(id TEXT PRIMARY KEY,installation_id TEXT NOT NULL,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,target_id TEXT NOT NULL,actor_id TEXT NOT NULL,kind TEXT NOT NULL,data BLOB NOT NULL,state TEXT NOT NULL,error TEXT NOT NULL DEFAULT '',idempotency_key TEXT NOT NULL,created_at TEXT NOT NULL,completed_at TEXT NOT NULL DEFAULT '',UNIQUE(installation_id,idempotency_key));
 `)
	return err
}
func (m *Module) routes() {
	h := func(pattern, permission string, f appkit.Endpoint) {
		appkit.Handle(m.mux, pattern, permission, func(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
			if s.ApplicationID != "eid" {
				return appkit.NotFound()
			}
			err := f(w, r, s)
			if err == nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
				m.flushWebhooks(r.Context(), s)
			}
			return err
		})
	}
	h("GET /keys", "admin", m.keys)
	h("POST /keys", "admin", m.createKey)
	h("DELETE /keys/{id}", "admin", m.revokeKey)
	h("GET /qualification", "read", m.qualification)
	h("GET /profile", "read", m.profile)
	h("PATCH /profile", "write", m.updateProfile)
	h("GET /verifications", "read", m.myVerifications)
	h("POST /verifications", "write", m.applyVerification)
	h("GET /cards", "read", m.cards)
	h("GET /club", "read", m.clubOverview)
	h("POST /club/applications", "write", m.applyClub)
	h("POST /club/applications/{id}/confirm", "write", m.confirmOffer)
	h("GET /review/verifications", "review", m.reviewVerifications)
	h("PATCH /review/verifications/{id}", "review", m.editVerification)
	h("POST /review/verifications/{id}/decision", "review", m.decideVerification)
	h("GET /review/club", "review", m.reviewClub)
	h("POST /review/club/{id}/decision", "review", m.decideClub)
	h("POST /review/club/{id}/notifications", "review", m.sendClubNotification)
	h("GET /notifications", "review", m.notifications)
	h("POST /notifications/{id}/retry", "review", m.retryWebhook)
	h("GET /settings", "manage", m.getSettings)
	h("PUT /settings", "manage", m.putSettings)
	h("GET /admin/members", "admin", m.members)
	h("PATCH /admin/members/{actorID}", "admin", m.editMember)
	h("DELETE /admin/members/{actorID}", "admin", m.deleteMember)
	h("DELETE /admin/verifications/{id}", "admin", m.deleteVerification)
	h("DELETE /admin/club/{id}", "admin", m.deleteClub)
}
func (m *Module) seal(value any, aad string) ([]byte, error) {
	b, e := json.Marshal(value)
	if e != nil {
		return nil, e
	}
	return m.rt.Encrypt(string(b), "eid:"+aad)
}
func (m *Module) open(b []byte, aad string, v any) error {
	text, e := m.rt.Decrypt(b, "eid:"+aad)
	if e != nil {
		return e
	}
	return json.Unmarshal([]byte(text), v)
}
func scopeArgs(s appkit.Scope) []any { return []any{s.TenantID, s.ProjectID, s.InstallationID} }

const scoped = "tenant_id=? AND project_id=? AND installation_id=?"

func (m *Module) event(ctx context.Context, tx *sql.Tx, s appkit.Scope, id, action, from, to string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO eid_events(id,installation_id,tenant_id,project_id,target_id,actor_id,action,from_status,to_status,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)", appkit.NewID("eev_"), s.InstallationID, s.TenantID, s.ProjectID, id, s.ActorID, action, from, to, appkit.Now())
	if err != nil {
		return err
	}
	if err = m.rt.Audit(ctx, tx, s, action, id, "成员资格或社团记录已变更"); err != nil {
		return err
	}
	return m.queueWebhooks(ctx, tx, s, action, id)
}

type Event struct {
	ID        string `json:"id"`
	ActorID   string `json:"actorId"`
	Action    string `json:"action"`
	From      string `json:"fromStatus"`
	To        string `json:"toStatus"`
	CreatedAt string `json:"createdAt"`
}

func (m *Module) events(ctx context.Context, s appkit.Scope, id string) ([]Event, error) {
	rows, err := m.rt.DB.QueryContext(ctx, "SELECT id,actor_id,action,from_status,to_status,created_at FROM eid_events WHERE "+scoped+" AND target_id=? ORDER BY created_at", append(scopeArgs(s), id)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err = rows.Scan(&e.ID, &e.ActorID, &e.Action, &e.From, &e.To, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func clean(value string, max int) (string, error) {
	value = strings.TrimSpace(value)
	if len([]rune(value)) > max || strings.ContainsRune(value, 0) {
		return "", appkit.Invalid("输入内容过长或包含无效字符")
	}
	return value, nil
}
