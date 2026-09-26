// Package statistics rebuilds the ECloud Statistics feature set for Euler.
// Functional reference: ctipscn/ecloud-statistics, Apache-2.0; see SOURCE.md.
package statistics

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

type Module struct{ rt *appkit.Runtime }

func New(rt *appkit.Runtime) *Module { return &Module{rt: rt} }
func (*Module) ID() string           { return "statistics" }
func (m *Module) Migrate(ctx context.Context) error {
	_, err := m.rt.DB.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS statistics_sites (
 id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, project_id TEXT NOT NULL, installation_id TEXT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
 name TEXT NOT NULL, domains TEXT NOT NULL, public_id TEXT NOT NULL UNIQUE, enabled INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS statistics_sites_scope ON statistics_sites(tenant_id,project_id,installation_id);
CREATE TABLE IF NOT EXISTS statistics_views (
 id INTEGER PRIMARY KEY AUTOINCREMENT, site_id TEXT NOT NULL REFERENCES statistics_sites(id) ON DELETE CASCADE,
 visitor_hash TEXT NOT NULL, ip_hash TEXT NOT NULL, url TEXT NOT NULL, referrer TEXT NOT NULL, title TEXT NOT NULL,
 resolution TEXT NOT NULL, language TEXT NOT NULL, user_agent TEXT NOT NULL, visited_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS statistics_views_site_time ON statistics_views(site_id,visited_at);
CREATE INDEX IF NOT EXISTS statistics_views_site_url ON statistics_views(site_id,url);
CREATE INDEX IF NOT EXISTS statistics_views_rate ON statistics_views(site_id,ip_hash,visited_at);
`)
	return err
}

type site struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Domains   []string `json:"domains"`
	PublicID  string   `json:"publicId"`
	Enabled   bool     `json:"enabled"`
	CreatedAt string   `json:"createdAt"`
}
type scanner interface{ Scan(...any) error }

func scanSite(row scanner) (site, error) {
	var s site
	var domains string
	err := row.Scan(&s.ID, &s.Name, &domains, &s.PublicID, &s.Enabled, &s.CreatedAt)
	if err == nil {
		err = json.Unmarshal([]byte(domains), &s.Domains)
	}
	return s, err
}

const columns = "s.id,s.name,s.domains,s.public_id,s.enabled,s.created_at"

func (m *Module) getSite(ctx context.Context, s appkit.Scope, id string) (site, error) {
	return scanSite(m.rt.DB.QueryRowContext(ctx, "SELECT "+columns+" FROM statistics_sites s WHERE s.id=? AND s.tenant_id=? AND s.project_id=? AND s.installation_id=?", id, s.TenantID, s.ProjectID, s.InstallationID))
}
func (m *Module) publicSite(ctx context.Context, id string) (site, error) {
	return scanSite(m.rt.DB.QueryRowContext(ctx, "SELECT "+columns+" FROM statistics_sites s JOIN installations i ON i.id=s.installation_id AND i.tenant_id=s.tenant_id AND i.project_id=s.project_id WHERE s.public_id=? AND s.enabled=1 AND i.status='enabled' AND i.application_id='statistics'", id))
}
func (m *Module) Handler() http.Handler {
	x := http.NewServeMux()
	appkit.Handle(x, "GET /sites", "read", m.list)
	appkit.Handle(x, "POST /sites", "manage", m.create)
	appkit.Handle(x, "PATCH /sites/{id}", "manage", m.update)
	appkit.Handle(x, "GET /sites/{id}/report", "read", m.report)
	appkit.Handle(x, "GET /sites/{id}/integration", "read", m.integration)
	return x
}
func (m *Module) list(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	rows, e := m.rt.DB.QueryContext(r.Context(), "SELECT "+columns+" FROM statistics_sites s WHERE s.tenant_id=? AND s.project_id=? AND s.installation_id=? ORDER BY s.created_at DESC", s.TenantID, s.ProjectID, s.InstallationID)
	if e != nil {
		return e
	}
	defer rows.Close()
	result := []site{}
	for rows.Next() {
		v, e := scanSite(rows)
		if e != nil {
			return e
		}
		result = append(result, v)
	}
	if e = rows.Err(); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"sites": result})
	return nil
}

type siteInput struct {
	Name    string   `json:"name"`
	Domains []string `json:"domains"`
	Enabled *bool    `json:"enabled"`
}

func validateSite(v *siteInput) error {
	var e error
	v.Name, e = appkit.Name(v.Name, 100)
	if e != nil {
		return e
	}
	if len(v.Domains) < 1 || len(v.Domains) > 30 {
		return appkit.Invalid("请配置 1–30 个精确域名（可含端口，不支持通配符）")
	}
	seen := map[string]bool{}
	out := []string{}
	for _, d := range v.Domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if strings.ContainsAny(d, "/@?#* \\:") { // Ports are accepted after URL parsing below.
			if strings.ContainsAny(d, "/@?#* \\") {
				return appkit.Invalid("域名不含协议、路径或通配符")
			}
		}
		u, e := url.Parse("https://" + d)
		if e != nil || u.Host != d || u.Hostname() == "" || strings.Contains(d, "..") {
			return appkit.Invalid("域名格式无效")
		}
		if p := u.Port(); p != "" {
			n, e := strconv.Atoi(p)
			if e != nil || n < 1 || n > 65535 {
				return appkit.Invalid("域名端口无效")
			}
		}
		if !seen[d] {
			out = append(out, d)
			seen[d] = true
		}
	}
	v.Domains = out
	return nil
}
func (m *Module) create(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in siteInput
	if e := appkit.Decode(w, r, &in); e != nil {
		return e
	}
	if e := validateSite(&in); e != nil {
		return e
	}
	v := site{ID: appkit.NewID("site_"), Name: in.Name, Domains: in.Domains, PublicID: appkit.NewID("pub_"), Enabled: true, CreatedAt: appkit.Now()}
	if in.Enabled != nil {
		v.Enabled = *in.Enabled
	}
	domains, _ := json.Marshal(v.Domains)
	e := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		_, e := tx.ExecContext(r.Context(), "INSERT INTO statistics_sites VALUES(?,?,?,?,?,?,?,?,?)", v.ID, s.TenantID, s.ProjectID, s.InstallationID, v.Name, string(domains), v.PublicID, v.Enabled, v.CreatedAt)
		if e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "site.created", v.ID, "创建统计站点")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 201, v)
	return nil
}
func (m *Module) update(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	v, e := m.getSite(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	var in siteInput
	if e = appkit.Decode(w, r, &in); e != nil {
		return e
	}
	if e = validateSite(&in); e != nil {
		return e
	}
	v.Name = in.Name
	v.Domains = in.Domains
	if in.Enabled != nil {
		v.Enabled = *in.Enabled
	}
	domains, _ := json.Marshal(v.Domains)
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		_, e := tx.ExecContext(r.Context(), "UPDATE statistics_sites SET name=?,domains=?,enabled=? WHERE id=? AND tenant_id=? AND project_id=? AND installation_id=?", v.Name, string(domains), v.Enabled, v.ID, s.TenantID, s.ProjectID, s.InstallationID)
		if e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "site.updated", v.ID, "更新统计站点设置")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, v)
	return nil
}
func pageParam(v string, def, max int) int {
	n, e := strconv.Atoi(v)
	if e != nil || n < 1 {
		return def
	}
	if n > max {
		return max
	}
	return n
}
func (m *Module) report(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	v, e := m.getSite(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	page := pageParam(r.URL.Query().Get("page"), 1, 1000000)
	size := pageParam(r.URL.Query().Get("pageSize"), 20, 100)
	var pv, uv, total int
	e = m.rt.DB.QueryRowContext(r.Context(), "SELECT COUNT(*),COUNT(DISTINCT visitor_hash),COUNT(DISTINCT url) FROM statistics_views WHERE site_id=?", v.ID).Scan(&pv, &uv, &total)
	if e != nil {
		return e
	}
	pages := (total + size - 1) / size
	if pages > 0 && page > pages {
		page = pages
	}
	rows, e := m.rt.DB.QueryContext(r.Context(), "SELECT url,COUNT(*),COUNT(DISTINCT visitor_hash),MAX(visited_at) FROM statistics_views WHERE site_id=? GROUP BY url ORDER BY COUNT(*) DESC,url ASC LIMIT ? OFFSET ?", v.ID, size, (page-1)*size)
	if e != nil {
		return e
	}
	defer rows.Close()
	type ranking struct {
		URL            string `json:"url"`
		Count          int    `json:"count"`
		UniqueVisitors int    `json:"uniqueVisitors"`
		LastVisit      string `json:"lastVisit"`
	}
	ranks := []ranking{}
	for rows.Next() {
		var v ranking
		if e = rows.Scan(&v.URL, &v.Count, &v.UniqueVisitors, &v.LastVisit); e != nil {
			return e
		}
		ranks = append(ranks, v)
	}
	if e = rows.Err(); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"pageViews": pv, "uniqueVisitors": uv, "topPages": ranks, "pagination": map[string]int{"currentPage": page, "pageSize": size, "total": total, "totalPages": pages}, "refreshedAt": appkit.Now()})
	return nil
}
func (m *Module) publicBase() string {
	return strings.TrimRight(m.rt.PublicURL, "/") + "/public/statistics"
}
func (m *Module) integration(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	v, e := m.getSite(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	base := m.publicBase() + "/sites/" + v.PublicID
	appkit.JSON(w, 200, map[string]string{"trackerURL": base + "/tracker.js", "widgetURL": base + "/widget.js", "helperURL": base + "/helper.js", "collectURL": base + "/collect", "pageViewsURL": base + "/page-views", "note": "仅配置域名可上报；URL 查询参数和片段会移除，访客与 IP 仅保存站点隔离的 HMAC 摘要。"})
	return nil
}
func (m *Module) PublicHandler() http.Handler {
	x := http.NewServeMux()
	wrap := func(fn func(http.ResponseWriter, *http.Request, site) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			s, e := m.publicSite(r.Context(), r.PathValue("publicID"))
			if e == nil {
				e = fn(w, r, s)
			}
			if e != nil {
				appkit.RespondError(w, e)
			}
		}
	}
	x.HandleFunc("POST /sites/{publicID}/collect", wrap(m.collect))
	x.HandleFunc("OPTIONS /sites/{publicID}/collect", wrap(func(w http.ResponseWriter, r *http.Request, s site) error {
		if e := origin(w, r, s, true); e != nil {
			return e
		}
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.WriteHeader(204)
		return nil
	}))
	x.HandleFunc("GET /sites/{publicID}/page-views", wrap(m.pageViews))
	x.HandleFunc("GET /sites/{publicID}/tracker.js", wrap(m.tracker))
	x.HandleFunc("GET /sites/{publicID}/widget.js", wrap(m.widget))
	x.HandleFunc("GET /sites/{publicID}/helper.js", wrap(m.helper))
	return x
}
func matches(s site, host string) bool {
	for _, d := range s.Domains {
		if strings.EqualFold(d, host) {
			return true
		}
	}
	return false
}
func origin(w http.ResponseWriter, r *http.Request, s site, required bool) error {
	o := r.Header.Get("Origin")
	if o == "" && !required {
		return nil
	}
	u, e := url.Parse(o)
	if e != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || !matches(s, u.Host) || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return appkit.Forbidden("来源域名未获此站点授权")
	}
	w.Header().Set("Access-Control-Allow-Origin", o)
	w.Header().Add("Vary", "Origin")
	return nil
}
func cleanURL(raw string) (string, error) {
	if len(raw) > 4096 {
		return "", appkit.Invalid("页面地址过长")
	}
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", appkit.Invalid("页面地址无效")
	}
	u.Host = strings.ToLower(u.Host)
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}
func (m *Module) digest(purpose, value string) string {
	h := hmac.New(sha256.New, m.rt.DeriveKey("statistics/"+purpose))
	h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil))
}
func (m *Module) collect(w http.ResponseWriter, r *http.Request, s site) error {
	if e := origin(w, r, s, true); e != nil {
		return e
	}
	var in struct {
		VisitorID  string `json:"visitor_id"`
		URL        string `json:"url"`
		Referrer   string `json:"referrer"`
		Title      string `json:"page_title"`
		Resolution string `json:"screen_resolution"`
		Language   string `json:"browser_language"`
	}
	if e := appkit.DecodeLimit(w, r, &in, 16<<10); e != nil {
		return e
	}
	if len(in.VisitorID) < 8 || len(in.VisitorID) > 128 || len(in.Title) > 1000 || len(in.Resolution) > 40 || len(in.Language) > 80 {
		return appkit.Invalid("统计字段长度无效")
	}
	clean, e := cleanURL(in.URL)
	if e != nil {
		return e
	}
	u, _ := url.Parse(clean)
	o, _ := url.Parse(r.Header.Get("Origin"))
	if !matches(s, u.Host) || !strings.EqualFold(o.Host, u.Host) || o.Scheme != u.Scheme {
		return appkit.Forbidden("页面地址与来源不一致")
	}
	ref := ""
	if in.Referrer != "" {
		ref, _ = cleanURL(in.Referrer)
	}
	ip, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		ip = r.RemoteAddr
	}
	ipHash := m.digest(s.ID+"/ip", ip)
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		var enabled, rate int
		if e := tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM statistics_sites s JOIN installations i ON i.id=s.installation_id WHERE s.id=? AND s.enabled=1 AND i.status='enabled'", s.ID).Scan(&enabled); e != nil {
			return e
		}
		if enabled == 0 {
			return appkit.NotFound()
		}
		if e := tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM statistics_views WHERE site_id=? AND ip_hash=? AND visited_at>?", s.ID, ipHash, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)).Scan(&rate); e != nil {
			return e
		}
		if rate >= 120 {
			return &appkit.Error{Status: 429, Code: "rate_limited", Message: "上报过于频繁，请稍后重试"}
		}
		_, e := tx.ExecContext(r.Context(), "INSERT INTO statistics_views(site_id,visitor_hash,ip_hash,url,referrer,title,resolution,language,user_agent,visited_at) VALUES(?,?,?,?,?,?,?,?,?,?)", s.ID, m.digest(s.ID+"/visitor", in.VisitorID), ipHash, clean, ref, in.Title, in.Resolution, in.Language, truncate(r.UserAgent(), 512), appkit.Now())
		return e
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 202, map[string]bool{"success": true})
	return nil
}
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
func (m *Module) pageViews(w http.ResponseWriter, r *http.Request, s site) error {
	if e := origin(w, r, s, false); e != nil {
		return e
	}
	clean, e := cleanURL(r.URL.Query().Get("url"))
	if e != nil {
		return e
	}
	u, _ := url.Parse(clean)
	if !matches(s, u.Host) {
		return appkit.Forbidden("页面不属于此站点")
	}
	var count int
	e = m.rt.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM statistics_views WHERE site_id=? AND url=?", s.ID, clean).Scan(&count)
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]int{"views": count})
	return nil
}
func javascript(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, code)
}
func (m *Module) tracker(w http.ResponseWriter, r *http.Request, s site) error {
	javascript(w, `(()=>{'use strict';const script=document.currentScript,base=new URL('.',script.src).href,key='euler-statistics-`+s.PublicID+`';let visitor;try{visitor=localStorage.getItem(key);if(!visitor){visitor=crypto.randomUUID();localStorage.setItem(key,visitor)}}catch(_){visitor=crypto.randomUUID()}const clean=v=>{try{const u=new URL(v);u.search='';u.hash='';return u.href}catch(_){return ''}};const collect=async(attempt=0)=>{try{const response=await fetch(base+'collect',{method:'POST',headers:{'Content-Type':'application/json'},credentials:'omit',keepalive:true,body:JSON.stringify({visitor_id:visitor,url:clean(location.href),referrer:clean(document.referrer),page_title:document.title.slice(0,250),screen_resolution:screen.width+'x'+screen.height,browser_language:navigator.language})});if(!response.ok&&response.status>=500)throw Error('temporary')}catch(_){if(attempt<3)setTimeout(()=>collect(attempt+1),1000*(attempt+1))}};if(document.readyState==='complete')collect();else addEventListener('load',()=>collect(),{once:true});window.eulerTrackPage=()=>collect()})();`)
	return nil
}
func (m *Module) widget(w http.ResponseWriter, r *http.Request, s site) error {
	javascript(w, `(()=>{'use strict';const base=new URL('.',document.currentScript.src).href;const init=()=>{let el=document.getElementById('page-view-counter');if(!el){el=document.createElement('span');el.id='page-view-counter';el.style.cssText='position:fixed;right:16px;bottom:16px;padding:8px 12px;border-radius:20px;background:#12233b;color:white;font:13px system-ui;z-index:1000';document.body.append(el)}const update=async()=>{try{const u=new URL(location.href);u.search='';u.hash='';const r=await fetch(base+'page-views?url='+encodeURIComponent(u.href),{credentials:'omit'});if(r.ok){const d=await r.json();el.textContent='浏览 '+d.views+' 次'}}catch(_){}};update();setInterval(update,60000)};if(document.readyState==='loading')addEventListener('DOMContentLoaded',init,{once:true});else init()})();`)
	return nil
}
func (m *Module) helper(w http.ResponseWriter, r *http.Request, s site) error {
	javascript(w, `(()=>{'use strict';const base=new URL('.',document.currentScript.src).href;window.getPageViews=async callback=>{let views=0;try{const u=new URL(location.href);u.search='';u.hash='';const r=await fetch(base+'page-views?url='+encodeURIComponent(u.href),{credentials:'omit'});if(r.ok)views=(await r.json()).views}catch(_){}if(typeof callback==='function')callback(views);return views}})();`)
	return nil
}
