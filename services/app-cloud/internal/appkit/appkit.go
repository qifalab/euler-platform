// Package appkit is the shared boundary for Euler-owned application modules.
// Browser headers never create a Scope: the platform injects it only after
// validating its server session, CSRF, project membership and enabled app.
package appkit

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Scope struct {
	ActorID        string   `json:"actorId"`
	ActorName      string   `json:"actorName"`
	Email          string   `json:"email"`
	EmailVerified  bool     `json:"emailVerified"`
	Provider       string   `json:"provider"`
	Subject        string   `json:"subject"`
	TenantID       string   `json:"tenantId"`
	ProjectID      string   `json:"projectId"`
	InstallationID string   `json:"installationId"`
	ApplicationID  string   `json:"applicationId"`
	Permissions    []string `json:"permissions"`
}

type scopeKey struct{}

func WithScope(ctx context.Context, s Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, s)
}
func FromContext(ctx context.Context) (Scope, bool) {
	s, ok := ctx.Value(scopeKey{}).(Scope)
	return s, ok && s.ActorID != "" && s.TenantID != "" && s.ProjectID != "" && s.InstallationID != "" && s.ApplicationID != ""
}
func (s Scope) Can(permission string) bool {
	for _, value := range s.Permissions {
		if value == permission {
			return true
		}
	}
	return false
}
func (s Scope) Require(permission string) error {
	if !s.Can(permission) {
		return Forbidden("你没有执行此操作的权限")
	}
	return nil
}

// Runtime shares the platform's durable database and encryption boundary.
// SQLite uses one connection: close Rows before starting another query, and
// use the transaction for every query performed inside that transaction.
type Runtime struct {
	DB                 *sql.DB
	DataDir            string
	PublicURL          string
	Encrypt            func(plaintext, associatedData string) ([]byte, error)
	Decrypt            func(ciphertext []byte, associatedData string) (string, error)
	DeriveKey          func(purpose string) []byte
	ApplicationEnabled func(ctx context.Context, tenantID, projectID, applicationID string) (bool, error)
}

type Module interface {
	ID() string
	Migrate(context.Context) error
	Handler() http.Handler
	PublicHandler() http.Handler
}

type Execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// Audit may participate in the business transaction so state and history agree.
// Summaries must be fixed descriptions, never request bodies, secrets or files.
func (rt *Runtime) Audit(ctx context.Context, tx Execer, s Scope, action, target, summary string) error {
	if tx == nil {
		tx = rt.DB
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO audit(id,tenant_id,project_id,actor_id,action,target_id,summary,created_at) VALUES(?,?,?,?,?,?,?,?)", NewID("evt_"), s.TenantID, s.ProjectID, s.ActorID, s.ApplicationID+"."+action, target, summary, time.Now().UTC().UnixMilli())
	return err
}
func (rt *Runtime) Transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := rt.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string         { return e.Message }
func Invalid(message string) error     { return &Error{400, "invalid_request", message} }
func Forbidden(message string) error   { return &Error{403, "forbidden", message} }
func NotFound() error                  { return &Error{404, "not_found", "记录不存在或你无权访问"} }
func Conflict(message string) error    { return &Error{409, "conflict", message} }
func Unavailable(message string) error { return &Error{503, "unavailable", message} }

func JSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if status != http.StatusNoContent {
		_ = json.NewEncoder(w).Encode(value)
	}
}
func RespondError(w http.ResponseWriter, err error) {
	var appError *Error
	if errors.As(err, &appError) {
		JSON(w, appError.Status, map[string]any{"error": map[string]string{"code": appError.Code, "message": appError.Message}})
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		RespondError(w, NotFound())
		return
	}
	JSON(w, 500, map[string]any{"error": map[string]string{"code": "internal_error", "message": "操作暂时无法完成，请稍后重试"}})
}
func Decode(w http.ResponseWriter, r *http.Request, value any) error {
	return DecodeLimit(w, r, value, 2<<20)
}
func DecodeLimit(w http.ResponseWriter, r *http.Request, value any, limit int64) error {
	reader := http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return Invalid("请求内容或大小不符合要求")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return Invalid("请求只允许一个 JSON 对象")
	}
	return nil
}

type Endpoint func(http.ResponseWriter, *http.Request, Scope) error

func Handle(mux *http.ServeMux, pattern, permission string, endpoint Endpoint) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		s, ok := FromContext(r.Context())
		if !ok {
			JSON(w, 401, map[string]any{"error": map[string]string{"code": "unauthenticated", "message": "请先登录欧拉"}})
			return
		}
		if err := s.Require(permission); err != nil {
			RespondError(w, err)
			return
		}
		if err := endpoint(w, r, s); err != nil {
			RespondError(w, err)
		}
	})
}
func NewID(prefix string) string {
	data := make([]byte, 18)
	if _, err := rand.Read(data); err != nil {
		panic(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(data)
}
func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func Name(value string, max int) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len([]rune(value)) > max {
		return "", Invalid(fmt.Sprintf("名称长度应为 1–%d 个字符", max))
	}
	return value, nil
}
