// Adapted from WeAuth services/pow_service.go and models/ip_policy.go (Apache-2.0).
// Euler changes: one row per batch, atomic solve/consume, scope status checks,
// bounded requests, encrypted credentials and trusted proxy handling.
package weauth

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

//go:embed assets/*
var assets embed.FS

type challenge struct {
	Seeds      []string `json:"seeds"`
	Difficulty int      `json:"difficulty"`
	Batch      bool     `json:"batch"`
	IP         string   `json:"ip"`
	Origin     string   `json:"origin"`
	Action     string   `json:"action"`
}

func (m *Module) PublicHandler() http.Handler {
	mux := http.NewServeMux()
	for _, prefix := range []string{"/pow", "/api/pow"} {
		mux.HandleFunc("POST "+prefix+"/challenge", m.public(m.challenge))
		mux.HandleFunc("POST "+prefix+"/verify", m.public(m.verify))
		mux.HandleFunc("POST "+prefix+"/siteverify", m.public(m.siteverify))
	}
	files, _ := fs.Sub(assets, "assets")
	fileHandler := http.FileServer(http.FS(files))
	for _, name := range []string{"weauth.js", "worker.js", "widget.html", "widget.js"} {
		mux.Handle("GET /"+name, fileHandler)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// Public PoW carries no browser cookies or personnel authority. The site
		// allowlist is validated separately; siteverify is server-to-server only.
		if !strings.HasSuffix(r.URL.Path, "/siteverify") {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.URL.Path == "/widget.html" {
			w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; connect-src 'self'; worker-src 'self'; frame-ancestors *")
			w.Header().Del("X-Frame-Options")
		}
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
func (m *Module) public(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if e := fn(w, r); e != nil {
			appkit.RespondError(w, e)
		}
	}
}
func (m *Module) trustedIP(ip netip.Addr) bool {
	for _, prefix := range m.trusted {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}
func (m *Module) clientIP(r *http.Request) string {
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		host = r.RemoteAddr
	}
	ip, e := netip.ParseAddr(host)
	if e != nil {
		return "unknown"
	}
	ip = ip.Unmap()
	if !m.trustedIP(ip) {
		return ip.String()
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	if len(parts) > 32 {
		return ip.String()
	}
	for i := len(parts) - 1; i >= 0; i-- {
		next, e := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if e != nil {
			return ip.String()
		}
		ip = next.Unmap()
		if !m.trustedIP(ip) {
			break
		}
	}
	return ip.String()
}
func (m *Module) challenge(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Sitekey string `json:"sitekey"`
		Origin  string `json:"origin"`
		Action  string `json:"action"`
	}
	if e := appkit.DecodeLimit(w, r, &input, 8192); e != nil {
		return e
	}
	if len(input.Sitekey) > 128 || len(input.Action) > 100 || len(input.Origin) > 512 {
		return appkit.Invalid("验证参数过长")
	}
	site, e := loadSite(r.Context(), m.rt.DB, "sitekey=?", input.Sitekey)
	if e != nil {
		return appkit.Invalid("站点不可用")
	}
	if e = m.active(r.Context(), site); e != nil {
		return e
	}
	if !allowedOrigin(site, input.Origin) {
		return appkit.Forbidden("来源域名不在站点白名单中")
	}
	// For a direct integration Origin must agree. The Euler-hosted iframe may
	// report its parent's origin; siteverify returns hostname for server checking.
	if origin := r.Header.Get("Origin"); origin != "" && origin != input.Origin {
		base, _ := url.Parse(m.rt.PublicURL)
		if base == nil || origin != base.Scheme+"://"+base.Host {
			return appkit.Forbidden("来源不匹配")
		}
	}
	ip := m.clientIP(r)
	now := time.Now().UTC()
	id := appkit.NewID("wac_")
	value := challenge{IP: ip, Origin: input.Origin, Action: input.Action, Batch: site.BatchModeEnabled}
	difficulty, count := site.BaseDifficulty, 1
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		// Preserve 24h statistics while clearing obsolete challenge material.
		if _, e := tx.ExecContext(r.Context(), "DELETE FROM weauth_challenges WHERE site_id=? AND created_at<? AND expires_at<?", site.ID, now.Add(-24*time.Hour).Unix(), now.Unix()); e != nil {
			return e
		}
		var outstanding int
		if e := tx.QueryRowContext(r.Context(), "SELECT count(*) FROM weauth_challenges WHERE site_id=? AND created_at>?", site.ID, now.Add(-time.Minute).Unix()).Scan(&outstanding); e != nil {
			return e
		}
		if outstanding >= 600 {
			return &appkit.Error{Status: 429, Code: "rate_limited", Message: "站点请求过于频繁，请稍后重试"}
		}
		var start int64
		var requests, failures int
		e := tx.QueryRowContext(r.Context(), "SELECT window_start,requests,failures FROM weauth_ip WHERE site_id=? AND ip=?", site.ID, ip).Scan(&start, &requests, &failures)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if start == 0 || now.Unix()-start >= int64(site.Policy.TimeWindowSeconds) {
			start = now.Unix()
			requests = 0
			failures = 0
		}
		if requests >= 120 && now.Unix()-start < 60 {
			return &appkit.Error{Status: 429, Code: "rate_limited", Message: "请求过于频繁，请稍后重试"}
		}
		increments := 0
		if site.Policy.Enabled {
			increments = int(float64(requests)+float64(failures)*site.Policy.FailureWeight) / site.Policy.RequestThreshold
		}
		if site.BatchModeEnabled {
			difficulty = site.BatchDifficulty
			count = min(site.MaxBatchCount, site.MinBatchCount+increments*site.Policy.BatchIncrement)
		} else {
			difficulty = min(site.MaxDifficulty, site.BaseDifficulty+increments*site.Policy.DifficultyIncrement)
		}
		value.Difficulty = difficulty
		for i := 0; i < count; i++ {
			value.Seeds = append(value.Seeds, appkit.NewID(""))
		}
		body, e := json.Marshal(value)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(r.Context(), "INSERT INTO weauth_challenges(id,site_id,body,expires_at,created_at) VALUES(?,?,?,?,?)", id, site.ID, string(body), now.Unix()+int64(site.ChallengeTimeout), now.Unix()); e != nil {
			return e
		}
		if _, e = tx.ExecContext(r.Context(), "INSERT INTO weauth_ip VALUES(?,?,?,?,?,?) ON CONFLICT(site_id,ip) DO UPDATE SET window_start=excluded.window_start,requests=excluded.requests,failures=excluded.failures,last_at=excluded.last_at", site.ID, ip, start, requests+1, failures, now.Unix()); e != nil {
			return e
		}
		if _, e = tx.ExecContext(r.Context(), "DELETE FROM weauth_ip WHERE site_id=? AND last_at<?", site.ID, now.Add(-30*24*time.Hour).Unix()); e != nil {
			return e
		}
		return event(r.Context(), tx, site.ID, 1, 0, 0, 0)
	})
	if e != nil {
		return e
	}
	result := map[string]any{"challenge_id": id, "difficulty": difficulty, "batch_mode": site.BatchModeEnabled, "batch_total": count, "expires_at": now.Add(time.Duration(site.ChallengeTimeout) * time.Second).Format(time.RFC3339)}
	if value.Batch {
		result["challenges"] = value.Seeds
	} else {
		result["challenge"] = value.Seeds[0]
	}
	appkit.JSON(w, 200, result)
	return nil
}
func event(ctx context.Context, tx *sql.Tx, site string, challenges, solved, verifications, success int) error {
	_, e := tx.ExecContext(ctx, "INSERT INTO weauth_events VALUES(?,?,?,?,?,?) ON CONFLICT(site_id,day) DO UPDATE SET challenges=challenges+excluded.challenges,solved=solved+excluded.solved,verifications=verifications+excluded.verifications,success=success+excluded.success", site, time.Now().UTC().Format("2006-01-02"), challenges, solved, verifications, success)
	return e
}

// WeAuth's original SHA256(challenge+nonce), hexadecimal leading-zero rule.
func verifySolution(seed, nonce string, difficulty int) bool {
	return len(nonce) > 0 && len(nonce) <= 128 && difficulty >= 1 && difficulty <= 12 && strings.HasPrefix(digest(seed+nonce), strings.Repeat("0", difficulty))
}
func (m *Module) verify(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		ChallengeID string   `json:"challenge_id"`
		Nonce       string   `json:"nonce"`
		Nonces      []string `json:"nonces"`
		Batch       bool     `json:"batch_mode"`
		Iterations  int64    `json:"iterations"`
		SolveTime   int64    `json:"solve_time_ms"`
	}
	if e := appkit.DecodeLimit(w, r, &input, 16384); e != nil {
		return e
	}
	if len(input.ChallengeID) > 128 || len(input.Nonces) > 20 {
		return appkit.Invalid("验证参数无效")
	}
	var siteID string
	if e := m.rt.DB.QueryRowContext(r.Context(), "SELECT site_id FROM weauth_challenges WHERE id=?", input.ChallengeID).Scan(&siteID); e != nil {
		return appkit.Invalid("验证挑战不存在或已过期")
	}
	site, e := loadSite(r.Context(), m.rt.DB, "id=?", siteID)
	if e != nil {
		return e
	}
	if e = m.active(r.Context(), site); e != nil {
		return e
	}
	token := appkit.NewID("wat_")
	var invalid error
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		var body string
		var expires, solved int64
		if e := tx.QueryRowContext(r.Context(), "SELECT body,expires_at,solved_at FROM weauth_challenges WHERE id=? AND site_id=?", input.ChallengeID, siteID).Scan(&body, &expires, &solved); e != nil {
			return e
		}
		var c challenge
		if e := json.Unmarshal([]byte(body), &c); e != nil {
			return e
		}
		now := time.Now().Unix()
		if solved > 0 {
			return appkit.Conflict("验证挑战已经使用")
		}
		if expires <= now {
			return appkit.Invalid("验证挑战已过期，请重新开始")
		}
		if c.IP != m.clientIP(r) {
			return appkit.Forbidden("验证请求来源已变化，请重新开始")
		}
		nonces := input.Nonces
		if !c.Batch {
			nonces = []string{input.Nonce}
		}
		valid := input.Batch == c.Batch && len(nonces) == len(c.Seeds)
		if valid {
			for i, seed := range c.Seeds {
				if !verifySolution(seed, nonces[i], c.Difficulty) {
					valid = false
					break
				}
			}
		}
		if !valid {
			invalid = appkit.Invalid("工作量证明不正确")
			if _, e := tx.ExecContext(r.Context(), "UPDATE weauth_ip SET failures=failures+1,last_at=? WHERE site_id=? AND ip=?", now, siteID, c.IP); e != nil {
				return e
			}
			return event(r.Context(), tx, siteID, 0, 0, 1, 0)
		}
		res, e := tx.ExecContext(r.Context(), "UPDATE weauth_challenges SET solved_at=?,token_hash=? WHERE id=? AND solved_at=0 AND expires_at>?", now, digest(token), input.ChallengeID, now)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return appkit.Conflict("验证挑战已使用或过期")
		}
		return event(r.Context(), tx, siteID, 0, 1, 1, 1)
	})
	if e != nil {
		return e
	}
	if invalid != nil {
		return invalid
	}
	appkit.JSON(w, 200, map[string]any{"token": token, "expires_in": site.TokenTimeout})
	return nil
}
func (m *Module) consume(ctx context.Context, site Site, token, remoteIP string, scope *appkit.Scope) (map[string]any, error) {
	result := map[string]any{"success": false, "error-codes": []string{"invalid-input-response"}}
	if token == "" || len(token) > 128 {
		return result, nil
	}
	e := m.rt.Transaction(ctx, func(tx *sql.Tx) error {
		var id, body string
		var solved, used int64
		e := tx.QueryRowContext(ctx, "SELECT id,body,solved_at,token_used FROM weauth_challenges WHERE site_id=? AND token_hash=?", site.ID, digest(token)).Scan(&id, &body, &solved, &used)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		if used != 0 || solved == 0 || time.Now().Unix() >= solved+int64(site.TokenTimeout) {
			result["error-codes"] = []string{"timeout-or-duplicate"}
			return nil
		}
		var c challenge
		if e = json.Unmarshal([]byte(body), &c); e != nil {
			return e
		}
		if remoteIP != "" && remoteIP != c.IP {
			return nil
		}
		res, e := tx.ExecContext(ctx, "UPDATE weauth_challenges SET token_used=1 WHERE id=? AND token_used=0", id)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return nil
		}
		origin, _ := url.Parse(c.Origin)
		result = map[string]any{"success": true, "challenge_ts": time.Unix(solved, 0).UTC().Format(time.RFC3339), "hostname": origin.Hostname(), "action": c.Action}
		if scope != nil {
			return m.rt.Audit(ctx, tx, *scope, "token.tested", site.ID, "通过控制台验证并消耗一次性令牌")
		}
		return nil
	})
	return result, e
}
func (m *Module) siteverify(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Secret   string `json:"secret"`
		Token    string `json:"token"`
		Response string `json:"response"`
		RemoteIP string `json:"remoteip"`
	}
	if e := appkit.DecodeLimit(w, r, &input, 8192); e != nil {
		return e
	}
	if input.Token == "" {
		input.Token = input.Response
	}
	if len(input.Secret) > 128 || len(input.Token) > 128 {
		return appkit.Invalid("验证参数过长")
	}
	site, e := loadSite(r.Context(), m.rt.DB, "secret_hash=?", digest(input.Secret))
	if e != nil {
		appkit.JSON(w, 200, map[string]any{"success": false, "error-codes": []string{"invalid-input-secret"}})
		return nil
	}
	if e = m.active(r.Context(), site); e != nil {
		return e
	}
	result, e := m.consume(r.Context(), site, input.Token, input.RemoteIP, nil)
	if e == nil {
		appkit.JSON(w, 200, result)
	}
	return e
}
