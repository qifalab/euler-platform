// Package weauth adapts the Apache-2.0 WeAuth PoW and risk algorithms for Euler.
// Original: ctipscn/weauth c670c207. Modified: SQLite transactions, project scope,
// encrypted secrets, one-use tokens, bounded input and Euler authorization.
package weauth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

type Module struct {
	rt        *appkit.Runtime
	trusted   []netip.Prefix
	configErr error
}

func New(rt *appkit.Runtime) *Module {
	m := &Module{rt: rt}
	for _, value := range strings.Split(os.Getenv("EULER_WEAUTH_TRUSTED_PROXIES"), ",") {
		if value = strings.TrimSpace(value); value != "" {
			p, e := netip.ParsePrefix(value)
			if e != nil {
				m.configErr = e
			} else {
				m.trusted = append(m.trusted, p)
			}
		}
	}
	return m
}
func (*Module) ID() string { return "weauth" }
func (m *Module) Migrate(ctx context.Context) error {
	if m.configErr != nil {
		return m.configErr
	}
	_, err := m.rt.DB.ExecContext(ctx, `
 CREATE TABLE IF NOT EXISTS weauth_sites(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,installation_id TEXT NOT NULL,sitekey TEXT UNIQUE NOT NULL,secret BLOB NOT NULL,secret_hash TEXT UNIQUE NOT NULL,body TEXT NOT NULL,created_at TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS weauth_sites_scope ON weauth_sites(tenant_id,project_id,installation_id);
 CREATE TABLE IF NOT EXISTS weauth_ip(site_id TEXT NOT NULL REFERENCES weauth_sites(id) ON DELETE CASCADE,ip TEXT NOT NULL,window_start INTEGER NOT NULL,requests INTEGER NOT NULL DEFAULT 0,failures INTEGER NOT NULL DEFAULT 0,last_at INTEGER NOT NULL,PRIMARY KEY(site_id,ip));
 CREATE TABLE IF NOT EXISTS weauth_challenges(id TEXT PRIMARY KEY,site_id TEXT NOT NULL REFERENCES weauth_sites(id) ON DELETE CASCADE,body TEXT NOT NULL,expires_at INTEGER NOT NULL,solved_at INTEGER NOT NULL DEFAULT 0,token_hash TEXT UNIQUE,token_used INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS weauth_challenges_site ON weauth_challenges(site_id,created_at);
 CREATE TABLE IF NOT EXISTS weauth_events(site_id TEXT NOT NULL REFERENCES weauth_sites(id) ON DELETE CASCADE,day TEXT NOT NULL,challenges INTEGER NOT NULL DEFAULT 0,solved INTEGER NOT NULL DEFAULT 0,verifications INTEGER NOT NULL DEFAULT 0,success INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(site_id,day));
 `)
	return err
}

type Policy struct {
	Enabled             bool    `json:"enabled"`
	TimeWindowSeconds   int     `json:"timeWindowSeconds"`
	RequestThreshold    int     `json:"requestThreshold"`
	DifficultyIncrement int     `json:"difficultyIncrement"`
	BatchIncrement      int     `json:"batchIncrement"`
	FailureWeight       float64 `json:"failureWeight"`
}
type Settings struct {
	Name             string `json:"name"`
	Enabled          bool   `json:"enabled"`
	BaseDifficulty   int    `json:"baseDifficulty"`
	MaxDifficulty    int    `json:"maxDifficulty"`
	ChallengeTimeout int    `json:"challengeTimeout"`
	TokenTimeout     int    `json:"tokenTimeout"`
	BatchModeEnabled bool   `json:"batchModeEnabled"`
	BatchDifficulty  int    `json:"batchDifficulty"`
	MinBatchCount    int    `json:"minBatchCount"`
	MaxBatchCount    int    `json:"maxBatchCount"`
}
type Site struct {
	snapshot string
	Settings
	ID             string   `json:"id"`
	Sitekey        string   `json:"sitekey"`
	Domains        []string `json:"domains"`
	Policy         Policy   `json:"policy"`
	CreatedAt      string   `json:"createdAt"`
	TenantID       string   `json:"-"`
	ProjectID      string   `json:"-"`
	InstallationID string   `json:"-"`
}

func defaults() Site {
	return Site{Settings: Settings{Enabled: true, BaseDifficulty: 4, MaxDifficulty: 6, ChallengeTimeout: 300, TokenTimeout: 300, BatchModeEnabled: true, BatchDifficulty: 4, MinBatchCount: 1, MaxBatchCount: 10}, Domains: []string{}, Policy: Policy{true, 300, 10, 1, 1, 2}}
}
func validateSettings(s *Settings) error {
	var err error
	s.Name, err = appkit.Name(s.Name, 100)
	if err != nil {
		return err
	}
	if s.BaseDifficulty < 1 || s.BaseDifficulty > 10 || s.MaxDifficulty < s.BaseDifficulty || s.MaxDifficulty > 12 || s.BatchDifficulty < 1 || s.BatchDifficulty > 5 || s.MinBatchCount < 1 || s.MinBatchCount > 10 || s.MaxBatchCount < s.MinBatchCount || s.MaxBatchCount > 20 || s.ChallengeTimeout < 10 || s.ChallengeTimeout > 3600 || s.TokenTimeout < 10 || s.TokenTimeout > 3600 {
		return appkit.Invalid("难度、批次数或有效期超出允许范围")
	}
	return nil
}
func validatePolicy(p Policy) error {
	if p.TimeWindowSeconds < 10 || p.TimeWindowSeconds > 86400 || p.RequestThreshold < 1 || p.RequestThreshold > 100000 || p.DifficultyIncrement < 0 || p.DifficultyIncrement > 10 || p.BatchIncrement < 0 || p.BatchIncrement > 20 || p.FailureWeight < 0 || p.FailureWeight > 100 {
		return appkit.Invalid("IP 策略参数超出允许范围")
	}
	return nil
}
func domain(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	wild := strings.HasPrefix(value, "*.")
	host := strings.TrimPrefix(value, "*.")
	if len(host) > 253 || host == "" || strings.ContainsAny(host, "/:?#@\\ \t\r\n") {
		return "", appkit.Invalid("请输入域名，不含协议、端口或路径")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", appkit.Invalid("域名格式无效")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", appkit.Invalid("请使用 ASCII / Punycode 域名")
			}
		}
	}
	if wild && !strings.Contains(host, ".") {
		return "", appkit.Invalid("通配域名需要完整主域名")
	}
	return value, nil
}
func allowedOrigin(s Site, raw string) bool {
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	for _, d := range s.Domains {
		if h == d || strings.HasPrefix(d, "*.") && strings.HasSuffix(h, d[1:]) && h != d[2:] {
			return true
		}
	}
	return false
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadSite(ctx context.Context, q queryer, where string, args ...any) (Site, error) {
	var s Site
	var body string
	err := q.QueryRowContext(ctx, "SELECT id,sitekey,body,created_at,tenant_id,project_id,installation_id FROM weauth_sites WHERE "+where, args...).Scan(&s.ID, &s.Sitekey, &body, &s.CreatedAt, &s.TenantID, &s.ProjectID, &s.InstallationID)
	if err == nil {
		err = json.Unmarshal([]byte(body), &s)
		s.snapshot = body
	}
	return s, err
}
func (m *Module) scoped(r *http.Request, s appkit.Scope) (Site, error) {
	return loadSite(r.Context(), m.rt.DB, "id=? AND tenant_id=? AND project_id=? AND installation_id=?", r.PathValue("id"), s.TenantID, s.ProjectID, s.InstallationID)
}
func digest(value string) string { h := sha256.Sum256([]byte(value)); return hex.EncodeToString(h[:]) }
func (m *Module) active(ctx context.Context, s Site) error {
	if !s.Enabled {
		return appkit.Forbidden("站点已停用")
	}
	if m.rt.ApplicationEnabled == nil {
		return appkit.Unavailable("应用状态校验尚未配置")
	}
	ok, e := m.rt.ApplicationEnabled(ctx, s.TenantID, s.ProjectID, m.ID())
	if e != nil {
		return e
	}
	if !ok {
		return appkit.Forbidden("应用已停用")
	}
	return nil
}
func (m *Module) save(ctx context.Context, scope appkit.Scope, s Site, action string) error {
	body, e := json.Marshal(s)
	if e != nil {
		return e
	}
	return m.rt.Transaction(ctx, func(tx *sql.Tx) error {
		res, e := tx.ExecContext(ctx, "UPDATE weauth_sites SET body=? WHERE id=? AND tenant_id=? AND project_id=? AND installation_id=? AND body=?", string(body), s.ID, scope.TenantID, scope.ProjectID, scope.InstallationID, s.snapshot)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return appkit.Conflict("站点配置已变化，请刷新后再保存")
		}
		return m.rt.Audit(ctx, tx, scope, action, s.ID, "更新人机验证站点配置")
	})
}
