package weauth

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

func (m *Module) Handler() http.Handler {
	mux := http.NewServeMux()
	appkit.Handle(mux, "GET /sites", "read", m.list)
	appkit.Handle(mux, "POST /sites", "write", m.create)
	appkit.Handle(mux, "GET /sites/{id}", "read", func(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
		site, e := m.scoped(r, s)
		if e == nil {
			appkit.JSON(w, 200, site)
		}
		return e
	})
	appkit.Handle(mux, "PUT /sites/{id}", "write", func(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
		site, e := m.scoped(r, s)
		if e != nil {
			return e
		}
		input := site.Settings
		if e = appkit.Decode(w, r, &input); e != nil {
			return e
		}
		if e = validateSettings(&input); e != nil {
			return e
		}
		site.Settings = input
		if e = m.save(r.Context(), s, site, "site.updated"); e == nil {
			appkit.JSON(w, 200, site)
		}
		return e
	})
	appkit.Handle(mux, "DELETE /sites/{id}", "manage", m.remove)
	appkit.Handle(mux, "GET /sites/{id}/domains", "read", func(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
		site, e := m.scoped(r, s)
		if e == nil {
			appkit.JSON(w, 200, map[string]any{"items": site.Domains})
		}
		return e
	})
	appkit.Handle(mux, "POST /sites/{id}/domains", "write", m.changeDomain)
	appkit.Handle(mux, "DELETE /sites/{id}/domains/{domain}", "write", m.changeDomain)
	appkit.Handle(mux, "GET /sites/{id}/secret", "secrets", m.secret)
	appkit.Handle(mux, "POST /sites/{id}/regenerate-secret", "secrets", m.secret)
	appkit.Handle(mux, "GET /sites/{id}/ip-policy", "read", func(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
		site, e := m.scoped(r, s)
		if e == nil {
			appkit.JSON(w, 200, site.Policy)
		}
		return e
	})
	appkit.Handle(mux, "PUT /sites/{id}/ip-policy", "manage", func(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
		site, e := m.scoped(r, s)
		if e != nil {
			return e
		}
		input := site.Policy
		if e = appkit.Decode(w, r, &input); e != nil {
			return e
		}
		if e = validatePolicy(input); e != nil {
			return e
		}
		site.Policy = input
		if e = m.save(r.Context(), s, site, "policy.updated"); e == nil {
			appkit.JSON(w, 200, site.Policy)
		}
		return e
	})
	appkit.Handle(mux, "GET /sites/{id}/ip-records", "manage", m.records)
	appkit.Handle(mux, "DELETE /sites/{id}/ip-records/{ip}", "manage", m.records)
	appkit.Handle(mux, "GET /sites/{id}/stats", "read", m.stats)
	appkit.Handle(mux, "POST /sites/{id}/test-token", "secrets", m.testToken)
	return mux
}
func (m *Module) list(w http.ResponseWriter, r *http.Request, scope appkit.Scope) error {
	rows, e := m.rt.DB.QueryContext(r.Context(), "SELECT id,sitekey,body,created_at FROM weauth_sites WHERE tenant_id=? AND project_id=? AND installation_id=? ORDER BY created_at DESC", scope.TenantID, scope.ProjectID, scope.InstallationID)
	if e != nil {
		return e
	}
	defer rows.Close()
	items := []Site{}
	for rows.Next() {
		var site Site
		var body string
		if e = rows.Scan(&site.ID, &site.Sitekey, &body, &site.CreatedAt); e != nil {
			return e
		}
		if e = json.Unmarshal([]byte(body), &site); e != nil {
			return e
		}
		items = append(items, site)
	}
	if e = rows.Err(); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"items": items})
	return nil
}
func (m *Module) create(w http.ResponseWriter, r *http.Request, scope appkit.Scope) error {
	site := defaults()
	input := struct {
		Settings
		Domains []string `json:"domains"`
	}{Settings: site.Settings}
	if e := appkit.Decode(w, r, &input); e != nil {
		return e
	}
	if e := validateSettings(&input.Settings); e != nil {
		return e
	}
	if len(input.Domains) > 100 {
		return appkit.Invalid("最多允许 100 个域名")
	}
	for _, value := range input.Domains {
		d, e := domain(value)
		if e != nil {
			return e
		}
		if !slices.Contains(site.Domains, d) {
			site.Domains = append(site.Domains, d)
		}
	}
	site.Settings = input.Settings
	site.ID = appkit.NewID("was_")
	site.Sitekey = appkit.NewID("wa_")
	site.CreatedAt = appkit.Now()
	site.TenantID = scope.TenantID
	site.ProjectID = scope.ProjectID
	site.InstallationID = scope.InstallationID
	secret := appkit.NewID("was_secret_")
	if m.rt.Encrypt == nil {
		return appkit.Unavailable("密钥加密尚未配置")
	}
	encrypted, e := m.rt.Encrypt(secret, "weauth:"+site.ID)
	if e != nil {
		return e
	}
	body, e := json.Marshal(site)
	if e != nil {
		return e
	}
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		var count int
		if e := tx.QueryRowContext(r.Context(), "SELECT count(*) FROM weauth_sites WHERE installation_id=?", scope.InstallationID).Scan(&count); e != nil {
			return e
		}
		if count >= 1000 {
			return appkit.Conflict("每个项目最多创建 1000 个验证站点")
		}
		_, e := tx.ExecContext(r.Context(), "INSERT INTO weauth_sites VALUES(?,?,?,?,?,?,?,?,?)", site.ID, scope.TenantID, scope.ProjectID, scope.InstallationID, site.Sitekey, encrypted, digest(secret), string(body), site.CreatedAt)
		if e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, scope, "site.created", site.ID, "创建人机验证站点")
	})
	if e == nil {
		appkit.JSON(w, 201, site)
	}
	return e
}
func (m *Module) remove(w http.ResponseWriter, r *http.Request, scope appkit.Scope) error {
	site, e := m.scoped(r, scope)
	if e != nil {
		return e
	}
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		for _, table := range []string{"weauth_ip", "weauth_challenges", "weauth_events"} {
			if _, e := tx.ExecContext(r.Context(), "DELETE FROM "+table+" WHERE site_id=?", site.ID); e != nil {
				return e
			}
		}
		res, e := tx.ExecContext(r.Context(), "DELETE FROM weauth_sites WHERE id=? AND tenant_id=? AND project_id=? AND installation_id=?", site.ID, scope.TenantID, scope.ProjectID, scope.InstallationID)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return appkit.NotFound()
		}
		return m.rt.Audit(r.Context(), tx, scope, "site.deleted", site.ID, "删除人机验证站点及验证记录")
	})
	if e == nil {
		appkit.JSON(w, 204, nil)
	}
	return e
}
func (m *Module) changeDomain(w http.ResponseWriter, r *http.Request, scope appkit.Scope) error {
	site, e := m.scoped(r, scope)
	if e != nil {
		return e
	}
	value := r.PathValue("domain")
	if r.Method == "POST" {
		var input struct {
			Domain string `json:"domain"`
		}
		if e = appkit.Decode(w, r, &input); e != nil {
			return e
		}
		value = input.Domain
	}
	value, e = domain(value)
	if e != nil {
		return e
	}
	if r.Method == "POST" {
		if len(site.Domains) >= 100 {
			return appkit.Invalid("最多允许 100 个域名")
		}
		if slices.Contains(site.Domains, value) {
			return appkit.Conflict("域名已存在")
		}
		site.Domains = append(site.Domains, value)
	} else {
		index := slices.Index(site.Domains, value)
		if index < 0 {
			return appkit.NotFound()
		}
		site.Domains = slices.Delete(site.Domains, index, index+1)
	}
	if e = m.save(r.Context(), scope, site, "domain.updated"); e == nil {
		appkit.JSON(w, 200, map[string]any{"items": site.Domains})
	}
	return e
}
func (m *Module) secret(w http.ResponseWriter, r *http.Request, scope appkit.Scope) error {
	site, e := m.scoped(r, scope)
	if e != nil {
		return e
	}
	var secret string
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		var encrypted []byte
		if e := tx.QueryRowContext(r.Context(), "SELECT secret FROM weauth_sites WHERE id=? AND tenant_id=? AND project_id=? AND installation_id=?", site.ID, scope.TenantID, scope.ProjectID, scope.InstallationID).Scan(&encrypted); e != nil {
			return e
		}
		action := "secret.viewed"
		if r.Method == "POST" {
			var input struct {
				Confirm bool `json:"confirm"`
			}
			if e := appkit.Decode(w, r, &input); e != nil {
				return e
			}
			if !input.Confirm {
				return appkit.Invalid("请确认旧密钥将立即失效")
			}
			if m.rt.Encrypt == nil {
				return appkit.Unavailable("密钥加密尚未配置")
			}
			secret = appkit.NewID("was_secret_")
			cipher, e := m.rt.Encrypt(secret, "weauth:"+site.ID)
			if e != nil {
				return e
			}
			if _, e = tx.ExecContext(r.Context(), "UPDATE weauth_sites SET secret=?,secret_hash=? WHERE id=?", cipher, digest(secret), site.ID); e != nil {
				return e
			}
			action = "secret.rotated"
		} else {
			if m.rt.Decrypt == nil {
				return appkit.Unavailable("密钥解密尚未配置")
			}
			var e error
			secret, e = m.rt.Decrypt(encrypted, "weauth:"+site.ID)
			if e != nil {
				return e
			}
		}
		return m.rt.Audit(r.Context(), tx, scope, action, site.ID, "访问站点服务端验证密钥")
	})
	if e == nil {
		appkit.JSON(w, 200, map[string]string{"secret": secret})
	}
	return e
}

type IPRecord struct {
	IP          string `json:"ip"`
	WindowStart int64  `json:"windowStart"`
	Requests    int    `json:"requests"`
	Failures    int    `json:"failures"`
	LastAt      int64  `json:"lastAt"`
}

func (m *Module) records(w http.ResponseWriter, r *http.Request, scope appkit.Scope) error {
	site, e := m.scoped(r, scope)
	if e != nil {
		return e
	}
	if r.Method == "DELETE" {
		e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
			res, e := tx.ExecContext(r.Context(), "DELETE FROM weauth_ip WHERE site_id=? AND ip=?", site.ID, r.PathValue("ip"))
			if e != nil {
				return e
			}
			n, _ := res.RowsAffected()
			if n == 0 {
				return appkit.NotFound()
			}
			return m.rt.Audit(r.Context(), tx, scope, "ip.reset", site.ID, "清除指定 IP 的风控计数")
		})
		if e == nil {
			appkit.JSON(w, 204, nil)
		}
		return e
	}
	rows, e := m.rt.DB.QueryContext(r.Context(), "SELECT ip,window_start,requests,failures,last_at FROM weauth_ip WHERE site_id=? ORDER BY last_at DESC LIMIT 1000", site.ID)
	if e != nil {
		return e
	}
	defer rows.Close()
	items := []IPRecord{}
	for rows.Next() {
		var v IPRecord
		if e = rows.Scan(&v.IP, &v.WindowStart, &v.Requests, &v.Failures, &v.LastAt); e != nil {
			return e
		}
		items = append(items, v)
	}
	if e = rows.Err(); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"items": items, "limit": 1000})
	return nil
}
func (m *Module) stats(w http.ResponseWriter, r *http.Request, scope appkit.Scope) error {
	site, e := m.scoped(r, scope)
	if e != nil {
		return e
	}
	var total, solved, verified, success, recent, recentSolved int64
	e = m.rt.DB.QueryRowContext(r.Context(), "SELECT COALESCE(SUM(challenges),0),COALESCE(SUM(solved),0),COALESCE(SUM(verifications),0),COALESCE(SUM(success),0) FROM weauth_events WHERE site_id=?", site.ID).Scan(&total, &solved, &verified, &success)
	if e != nil {
		return e
	}
	e = m.rt.DB.QueryRowContext(r.Context(), "SELECT COUNT(*),COALESCE(SUM(CASE WHEN solved_at>0 THEN 1 ELSE 0 END),0) FROM weauth_challenges WHERE site_id=? AND created_at>?", site.ID, time.Now().Add(-24*time.Hour).Unix()).Scan(&recent, &recentSolved)
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"totalChallenges": total, "solvedChallenges": solved, "totalVerifications": verified, "successVerifications": success, "last24hChallenges": recent, "last24hSolved": recentSolved})
	return nil
}
func (m *Module) testToken(w http.ResponseWriter, r *http.Request, scope appkit.Scope) error {
	site, e := m.scoped(r, scope)
	if e != nil {
		return e
	}
	if e = m.active(r.Context(), site); e != nil {
		return e
	}
	var input struct {
		Token string `json:"token"`
	}
	if e = appkit.Decode(w, r, &input); e != nil {
		return e
	}
	result, e := m.consume(r.Context(), site, input.Token, "", &scope)
	if e == nil {
		appkit.JSON(w, 200, result)
	}
	return e
}
