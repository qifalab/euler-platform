// Package trust implements Euler-owned verification workflows. Functional
// reference: miaojilab/trust-center at 28ae1b3dacacb468047d407e13e35694c61ab8f7.
package trust

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"net/http"
)

type Module struct{ rt *appkit.Runtime }

func New(rt *appkit.Runtime) *Module { return &Module{rt: rt} }
func (m *Module) ID() string         { return "trust" }

type Rules struct {
	MinLength *int     `json:"minLength,omitempty"`
	MaxLength *int     `json:"maxLength,omitempty"`
	Min       *float64 `json:"min,omitempty"`
	Max       *float64 `json:"max,omitempty"`
	Pattern   string   `json:"pattern,omitempty"`
	MaxBytes  int64    `json:"maxBytes,omitempty"`
	Accept    []string `json:"accept,omitempty"`
}
type Field struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Type        string   `json:"type"`
	Required    bool     `json:"required"`
	Description string   `json:"description"`
	Options     []string `json:"options,omitempty"`
	Validations Rules    `json:"validations"`
}
type Scheme struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Status      string  `json:"status"`
	Fields      []Field `json:"fields"`
	Version     int     `json:"version"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
}
type Submission struct {
	ID         string                     `json:"id"`
	SchemeID   string                     `json:"schemeId"`
	SchemeName string                     `json:"schemeName"`
	ActorID    string                     `json:"actorId"`
	ActorName  string                     `json:"actorName,omitempty"`
	Email      string                     `json:"email,omitempty"`
	Status     string                     `json:"status"`
	Data       map[string]json.RawMessage `json:"data,omitempty"`
	Fields     []Field                    `json:"fields,omitempty"`
	Reason     string                     `json:"reason,omitempty"`
	Version    int                        `json:"version"`
	CreatedAt  string                     `json:"createdAt"`
	UpdatedAt  string                     `json:"updatedAt"`
	ReviewedAt string                     `json:"reviewedAt,omitempty"`
	History    []History                  `json:"history,omitempty"`
	Materials  []Material                 `json:"materials,omitempty"`
}
type History struct {
	ID        string `json:"id"`
	Action    string `json:"action"`
	Status    string `json:"status"`
	ActorID   string `json:"actorId"`
	Reason    string `json:"reason,omitempty"`
	CreatedAt string `json:"createdAt"`
	Version   int    `json:"version"`
}
type Material struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Size      int64  `json:"size"`
	FieldName string `json:"fieldName"`
	CreatedAt string `json:"createdAt"`
}
type actor struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	CreatedAt string `json:"createdAt"`
	LastSeen  string `json:"lastSeen"`
}
type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func scopeArgs(s appkit.Scope) []any { return []any{s.TenantID, s.ProjectID} }
func (m *Module) seal(value any, aad string) ([]byte, error) {
	b, e := json.Marshal(value)
	if e != nil {
		return nil, e
	}
	return m.rt.Encrypt(string(b), "trust:"+aad)
}
func (m *Module) open(b []byte, aad string, value any) error {
	p, e := m.rt.Decrypt(b, "trust:"+aad)
	if e != nil {
		return e
	}
	return json.Unmarshal([]byte(p), value)
}
func notFound(e error) error {
	if e == sql.ErrNoRows {
		return appkit.NotFound()
	}
	return e
}
func (m *Module) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, r := range []struct {
		p, perm string
		f       appkit.Endpoint
	}{
		{"GET /bootstrap", "read", m.bootstrap}, {"GET /schemes", "read", m.schemes}, {"GET /schemes/{id}", "read", m.scheme},
		{"POST /schemes", "admin", m.createScheme}, {"PUT /schemes/{id}", "admin", m.updateScheme}, {"DELETE /schemes/{id}", "admin", m.deleteScheme},
		{"GET /submissions", "read", m.ownSubmissions}, {"POST /submissions", "write", m.submit}, {"GET /submissions/{id}", "read", m.ownDetail},
		{"GET /materials", "read", m.ownMaterials}, {"POST /materials", "write", m.uploadMaterial}, {"GET /materials/{id}", "read", m.ownMaterial}, {"DELETE /materials/{id}", "write", m.deleteMaterial},
		{"GET /review/submissions", "review", m.reviewQueue}, {"GET /review/submissions/{id}", "review", m.reviewDetail}, {"POST /review/submissions/{id}", "review", m.review},
		{"GET /review/materials/{id}", "review", m.reviewMaterial}, {"GET /review/stats", "review", m.stats}, {"GET /review/users", "review", m.users},
		{"GET /keys", "admin", m.keys}, {"POST /keys", "admin", m.createKey}, {"DELETE /keys/{id}", "admin", m.revokeKey},
		{"GET /notifications", "admin", m.notifications}, {"POST /notifications/{id}/retry", "admin", m.retryNotification},
	} {
		appkit.Handle(mux, r.p, r.perm, r.f)
	}
	return mux
}
func (m *Module) Migrate(ctx context.Context) error {
	if m.rt == nil || m.rt.DB == nil || m.rt.Encrypt == nil || m.rt.Decrypt == nil {
		return appkit.Unavailable("Trust 持久化与材料加密尚未配置")
	}
	_, e := m.rt.DB.ExecContext(ctx, `
 CREATE TABLE IF NOT EXISTS trust_schemes(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,name TEXT NOT NULL,description TEXT NOT NULL,status TEXT NOT NULL,fields TEXT NOT NULL,version INTEGER NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS trust_schemes_scope ON trust_schemes(tenant_id,project_id,status);
 CREATE TABLE IF NOT EXISTS trust_actors(tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,actor_id TEXT NOT NULL,profile BLOB NOT NULL,created_at TEXT NOT NULL,last_seen TEXT NOT NULL,PRIMARY KEY(tenant_id,project_id,actor_id));
 CREATE TABLE IF NOT EXISTS trust_submissions(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,scheme_id TEXT NOT NULL,actor_id TEXT NOT NULL,status TEXT NOT NULL,payload BLOB NOT NULL,fields TEXT NOT NULL,reason BLOB NOT NULL,version INTEGER NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,reviewed_at TEXT NOT NULL,UNIQUE(tenant_id,project_id,scheme_id,actor_id));
 CREATE INDEX IF NOT EXISTS trust_submissions_scope ON trust_submissions(tenant_id,project_id,status,updated_at);
 CREATE TABLE IF NOT EXISTS trust_history(id TEXT PRIMARY KEY,submission_id TEXT NOT NULL,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,actor_id TEXT NOT NULL,action TEXT NOT NULL,status TEXT NOT NULL,reason BLOB NOT NULL,version INTEGER NOT NULL,created_at TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS trust_history_submission ON trust_history(submission_id,version);
 CREATE TABLE IF NOT EXISTS trust_materials(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,actor_id TEXT NOT NULL,scheme_id TEXT NOT NULL,field_name TEXT NOT NULL,submission_id TEXT NOT NULL DEFAULT '',metadata BLOB NOT NULL,body BLOB NOT NULL,created_at TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS trust_materials_scope ON trust_materials(tenant_id,project_id,submission_id);
 CREATE TABLE IF NOT EXISTS trust_keys(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,name TEXT NOT NULL,token_hash TEXT NOT NULL UNIQUE,schemes TEXT NOT NULL,actors TEXT NOT NULL,details INTEGER NOT NULL,expires_at TEXT NOT NULL,revoked_at TEXT NOT NULL,created_at TEXT NOT NULL,created_by TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS trust_notifications(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,submission_id TEXT NOT NULL,channel TEXT NOT NULL,payload BLOB NOT NULL,status TEXT NOT NULL,attempts INTEGER NOT NULL,last_error TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
 `)
	return e
}
