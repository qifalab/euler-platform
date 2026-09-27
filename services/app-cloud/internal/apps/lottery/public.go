package lottery

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/weauth"
	qrcode "github.com/skip2/go-qrcode"
)

func (m *Module) publicRoom(ctx context.Context, q queryer, token string) (room, error) {
	if len(token) < 20 || len(token) > 100 {
		return room{}, appkit.NotFound()
	}
	return scanRoom(q.QueryRowContext(ctx, "SELECT "+roomColumns+" FROM lottery_rooms r JOIN installations i ON i.id=r.installation_id AND i.tenant_id=r.tenant_id AND i.project_id=r.project_id WHERE r.token_hash=? AND i.status='enabled' AND i.application_id='lottery'", hash(token)))
}
func (m *Module) PublicHandler() http.Handler {
	x := http.NewServeMux()
	wrap := func(fn func(http.ResponseWriter, *http.Request, room) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			v, e := m.publicRoom(r.Context(), m.rt.DB, r.PathValue("token"))
			if e == nil {
				e = fn(w, r, v)
			}
			if e != nil {
				appkit.RespondError(w, e)
			}
		}
	}
	x.HandleFunc("GET /join/{token}", wrap(m.signupPage))
	x.HandleFunc("GET /join/{token}/info", wrap(m.signupInfo))
	x.HandleFunc("GET /join/{token}/qr.png", wrap(m.qrCode))
	x.HandleFunc("POST /join/{token}/register", wrap(m.register))
	return x
}
func (m *Module) signupInfo(w http.ResponseWriter, r *http.Request, v room) error {
	if e := roomCounts(r.Context(), m.rt.DB, &v); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"name": v.Name, "description": v.Description, "status": v.Status, "totalUsers": v.TotalUsers, "currentWinners": v.CurrentWinners})
	return nil
}
func (m *Module) qrCode(w http.ResponseWriter, r *http.Request, v room) error {
	target := strings.TrimRight(m.rt.PublicURL, "/") + "/public/lottery/join/" + r.PathValue("token")
	if !strings.HasPrefix(target, "https://") && !strings.HasPrefix(target, "http://") {
		return appkit.Unavailable("请先配置欧拉公开访问地址")
	}
	png, e := qrcode.Encode(target, qrcode.Medium, 320)
	if e != nil {
		return e
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, e = w.Write(png)
	return e
}
func (m *Module) register(w http.ResponseWriter, r *http.Request, v room) error {
	// Same-origin browser signup only. The unguessable invitation is a narrow
	// capability, never a management session. Do not enable wildcard CORS here.
	if o := r.Header.Get("Origin"); o != "" {
		expected, e := url.Parse(m.rt.PublicURL)
		actual, ae := url.Parse(o)
		if e != nil || ae != nil || actual.Scheme != expected.Scheme || !strings.EqualFold(actual.Host, expected.Host) || actual.Path != "" || actual.RawQuery != "" || actual.Fragment != "" {
			return appkit.Forbidden("请从活动报名页面提交")
		}
	}
	var body signupInput
	if e := appkit.DecodeLimit(w, r, &body, 4096); e != nil {
		return e
	}
	in := participantInput{Name: body.Name, Department: body.Department}
	if e := validateParticipant(&in, true); e != nil {
		return e
	}
	ip, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		ip = r.RemoteAddr
	}
	h := hmac.New(sha256.New, m.rt.DeriveKey("lottery/signup-rate"))
	fmt.Fprint(h, v.ID, "/", ip)
	client := hex.EncodeToString(h.Sum(nil))
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		// Recheck token and enabled installation inside the write transaction;
		// rotation, closure or app disabling cannot race a successful signup.
		current, e := m.publicRoom(r.Context(), tx, r.PathValue("token"))
		if e != nil {
			return e
		}
		if current.Status != "open" {
			return appkit.Conflict("活动已暂停报名")
		}
		p, err := policy(r.Context(), tx, v.ID)
		if err != nil {
			return err
		}
		if p.RequireLogin {
			return appkit.Forbidden("本活动需要登录欧拉并验证本人资格")
		}
		if p.WeAuthSiteID != "" {
			var tenant, project string
			if err = tx.QueryRowContext(r.Context(), "SELECT tenant_id,project_id FROM lottery_rooms WHERE id=?", v.ID).Scan(&tenant, &project); err != nil {
				return err
			}
			if err = weauth.ConsumeForApplication(r.Context(), tx, m.rt, r, tenant, project, p.WeAuthSiteID, "lottery:"+v.ID, body.WeAuthToken); err != nil {
				return err
			}
		}

		var count int
		if e = tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM lottery_participants WHERE room_id=? AND client_hash=? AND created_at>?", v.ID, client, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)).Scan(&count); e != nil {
			return e
		}
		if count >= 20 {
			return &appkit.Error{Status: 429, Code: "rate_limited", Message: "报名过于频繁，请稍后重试"}
		}
		_, e = insertParticipant(r.Context(), tx, v.ID, in, "public", client)
		return e
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 201, map[string]bool{"success": true})
	return nil
}

var signupTemplate = template.Must(template.New("signup").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="referrer" content="no-referrer"><title>{{.Name}} · 欧拉活动报名</title>
<style nonce="{{.Nonce}}">*{box-sizing:border-box}body{margin:0;font:16px/1.6 system-ui,sans-serif;background:radial-gradient(ellipse at 10% 0%,#ffe4ef,transparent 60%),radial-gradient(ellipse at 100% 40%,#dceaff,transparent 70%),#f8f9fc;color:#202944;min-height:100vh;padding:32px 20px}main{max-width:480px;margin:6vh auto;background:#fff;border:1px solid #e8eaf4;border-radius:24px;padding:32px;box-shadow:0 20px 60px #33466c12}.brand{color:#6554cf;font-size:13px;letter-spacing:2px;font-weight:700}h1{font-size:28px;line-height:1.3;margin:20px 0 12px}.muted{color:#75809a}label{display:block;font-weight:600;margin-top:20px}input{font:inherit;width:100%;padding:12px;border:1px solid #ced5e4;border-radius:10px;margin-top:6px}input:focus{outline:3px solid #7466ee33;border-color:#7662ea}button{font:inherit;font-weight:600;cursor:pointer;width:100%;border:0;border-radius:12px;background:linear-gradient(100deg,#6758d9,#9a58d9);color:white;padding:14px;margin-top:24px}button:disabled{opacity:.55;cursor:wait}#notice{margin-top:18px;white-space:pre-wrap}#notice[data-error=true]{color:#be2044}.stats{padding:14px;background:#f3f5fc;border-radius:12px;font-size:14px;margin:20px 0}footer{text-align:center;font-size:12px;margin-top:24px;color:#75809a}@media(prefers-reduced-motion:reduce){*{animation:none!important}}</style></head><body><main>
<div class="brand">EULER · 活动报名</div><h1>{{.Name}}</h1><p class="muted">{{.Description}}</p><div class="stats">已有 {{.TotalUsers}} 人报名 · {{.CurrentWinners}} 次中奖</div>
{{if .Open}}{{if .RequireLogin}}<p>本活动需要登录欧拉，并满足主办方设置的资格条件。</p><p><a href="{{.LoginURL}}">登录并报名</a></p><p class="muted">需已有当前团队与项目访问权限；此链接不会授予管理权限。</p>{{else}}<form id="signup"><label for="name">姓名</label><input id="name" name="name" required minlength="2" maxlength="20" autocomplete="name" placeholder="请输入真实姓名"><label for="department">部门 <span class="muted">（可选）</span></label><input id="department" name="department" maxlength="50" autocomplete="organization" placeholder="便于现场识别">{{if .Sitekey}}<iframe id="weauth" title="WeAuth 人机验证" src="{{.WidgetURL}}" width="340" height="114" style="border:0;max-width:100%"></iframe>{{end}}<button id="submit" type="submit">确认报名</button></form><div id="notice" role="status" aria-live="polite"></div><button id="again" hidden type="button">继续为其他人报名</button>
{{end}}{{else}}<p role="status">活动已暂停报名，请联系现场工作人员。</p>{{end}}<footer>报名链接仅用于本场活动，不提供活动管理权限。</footer></main>
<script nonce="{{.Nonce}}">(()=>{const form=document.getElementById('signup');if(!form)return;let weauthToken='';const widget=document.getElementById('weauth');window.addEventListener('message',event=>{if(widget&&event.source===widget.contentWindow&&event.origin===location.origin&&event.data?.type==='euler-weauth'){weauthToken=event.data.event==='success'?event.data.token:''}});const button=document.getElementById('submit'),notice=document.getElementById('notice'),again=document.getElementById('again');form.addEventListener('submit',async event=>{event.preventDefault();button.disabled=true;notice.textContent='正在提交…';notice.dataset.error='false';try{const response=await fetch(location.pathname.replace(/\/$/,'')+'/register',{method:'POST',credentials:'omit',headers:{'Content-Type':'application/json'},body:JSON.stringify({name:form.elements.name.value.trim(),department:form.elements.department.value.trim(),weauthToken})});const data=await response.json();if(!response.ok)throw Error(data.error?.message||'报名失败，请稍后重试');form.hidden=true;notice.textContent='🎉 报名成功，祝您好运！';again.hidden=false;form.reset()}catch(error){notice.textContent=error.message;notice.dataset.error='true'}finally{button.disabled=false}});again.addEventListener('click',()=>{weauthToken='';widget?.contentWindow?.postMessage({type:'euler-weauth-control',action:'reset'},location.origin);form.hidden=false;again.hidden=true;notice.textContent='';document.getElementById('name').focus()})})();</script></body></html>`))

func (m *Module) signupPage(w http.ResponseWriter, r *http.Request, v room) error {
	if e := roomCounts(r.Context(), m.rt.DB, &v); e != nil {
		return e
	}
	nonce := appkit.NewID("")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'nonce-"+nonce+"'; script-src 'nonce-"+nonce+"'; frame-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	p, err := policy(r.Context(), m.rt.DB, v.ID)
	if err != nil {
		return err
	}
	var tenant, project, sitekey string
	if err = m.rt.DB.QueryRowContext(r.Context(), "SELECT tenant_id,project_id FROM lottery_rooms WHERE id=?", v.ID).Scan(&tenant, &project); err != nil {
		return err
	}
	if p.WeAuthSiteID != "" {
		if err = m.rt.DB.QueryRowContext(r.Context(), "SELECT sitekey FROM weauth_sites WHERE id=? AND tenant_id=? AND project_id=?", p.WeAuthSiteID, tenant, project).Scan(&sitekey); err != nil {
			return err
		}
	}
	query := url.Values{"tenantId": {tenant}, "projectId": {project}, "signup": {v.ID}}
	widget := url.Values{"sitekey": {sitekey}, "origin": {strings.TrimRight(m.rt.PublicURL, "/")}, "action": {"lottery:" + v.ID}}
	return signupTemplate.Execute(w, struct {
		Name, Description, Nonce     string
		TotalUsers, CurrentWinners   int
		Open, RequireLogin           bool
		LoginURL, Sitekey, WidgetURL string
	}{v.Name, v.Description, nonce, v.TotalUsers, v.CurrentWinners, v.Status == "open", p.RequireLogin, "/apps/lottery?" + query.Encode(), sitekey, "/public/weauth/widget.html?" + widget.Encode()})
}
