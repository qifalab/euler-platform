package eid

import (
	"context"
	"database/sql"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/trust"
)

type Settings struct {
	ClubName         string `json:"clubName"`
	TrustSchemeID    string `json:"trustSchemeId"`
	RequireTrust     bool   `json:"requireTrust"`
	InterviewSubject string `json:"interviewSubject"`
	InterviewBody    string `json:"interviewBody"`
	OfferSubject     string `json:"offerSubject"`
	OfferBody        string `json:"offerBody"`
	SMTPAddress      string `json:"smtpAddress"`
	SMTPFrom         string `json:"smtpFrom"`
	SMTPUsername     string `json:"smtpUsername"`
	SMTPPassword     string `json:"smtpPassword,omitempty"`
	SMTPImplicitTLS  bool   `json:"smtpImplicitTLS"`
	WecomURL         string `json:"wecomUrl,omitempty"`
	FeishuURL        string `json:"feishuUrl,omitempty"`
}

func defaults() Settings {
	return Settings{ClubName: "团队招募", RequireTrust: true, InterviewSubject: "笔试通知", InterviewBody: "你好，{{name}}：\n你的报名已进入笔试阶段。请留意团队后续安排。\n查看进度：{{url}}", OfferSubject: "录取通知", OfferBody: "你好，{{name}}：\n恭喜你获得录取资格。请登录欧拉确认接受录取：\n{{url}}", SMTPAddress: os.Getenv("EULER_EID_SMTP_ADDRESS"), SMTPFrom: os.Getenv("EULER_EID_SMTP_FROM"), SMTPUsername: os.Getenv("EULER_EID_SMTP_USERNAME"), SMTPPassword: os.Getenv("EULER_EID_SMTP_PASSWORD"), SMTPImplicitTLS: os.Getenv("EULER_EID_SMTP_IMPLICIT_TLS") == "true", WecomURL: os.Getenv("EULER_EID_WECOM_URL"), FeishuURL: os.Getenv("EULER_EID_FEISHU_URL")}
}

type settingsReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (m *Module) settings(ctx context.Context, s appkit.Scope) (Settings, error) {
	return m.readSettings(ctx, m.rt.DB, s)
}
func (m *Module) readSettings(ctx context.Context, reader settingsReader, s appkit.Scope) (Settings, error) {
	var b []byte
	e := reader.QueryRowContext(ctx, "SELECT data FROM eid_settings WHERE "+scoped, scopeArgs(s)...).Scan(&b)
	if e == sql.ErrNoRows {
		return defaults(), nil
	}
	if e != nil {
		return Settings{}, e
	}
	var cfg Settings
	e = m.open(b, s.InstallationID+":settings", &cfg)
	return cfg, e
}
func redacted(cfg Settings) map[string]any {
	return map[string]any{"clubName": cfg.ClubName, "trustSchemeId": cfg.TrustSchemeID, "requireTrust": cfg.RequireTrust, "interviewSubject": cfg.InterviewSubject, "interviewBody": cfg.InterviewBody, "offerSubject": cfg.OfferSubject, "offerBody": cfg.OfferBody, "smtpAddress": cfg.SMTPAddress, "smtpFrom": cfg.SMTPFrom, "smtpUsername": cfg.SMTPUsername, "smtpImplicitTLS": cfg.SMTPImplicitTLS, "smtpPasswordConfigured": cfg.SMTPPassword != "", "wecomConfigured": cfg.WecomURL != "", "feishuConfigured": cfg.FeishuURL != ""}
}
func (m *Module) getSettings(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	cfg, e := m.settings(r.Context(), s)
	if e != nil {
		return e
	}
	schemes, e := trust.ListAvailableSchemes(r.Context(), m.rt, s)
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"settings": redacted(cfg), "schemes": schemes})
	return nil
}
func validWebhook(raw, provider string) error {
	if raw == "" {
		return nil
	}
	u, e := url.Parse(raw)
	host := "qyapi.weixin.qq.com"
	if provider == "feishu" {
		host = "open.feishu.cn"
	}
	if e != nil || u.Scheme != "https" || u.Host != host || u.User != nil || u.Fragment != "" {
		return appkit.Invalid("通知地址必须是对应平台的官方 HTTPS 机器人地址")
	}
	if provider == "wecom" && !strings.HasPrefix(u.Path, "/cgi-bin/webhook/send") {
		return appkit.Invalid("无效的企业微信机器人地址")
	}
	if provider == "feishu" && !strings.HasPrefix(u.Path, "/open-apis/bot/v2/hook/") {
		return appkit.Invalid("无效的飞书机器人地址")
	}
	return nil
}
func validateSettings(cfg *Settings) error {
	var e error
	cfg.ClubName, e = appkit.Name(cfg.ClubName, 100)
	if e != nil {
		return e
	}
	cfg.InterviewSubject, e = appkit.Name(cfg.InterviewSubject, 150)
	if e != nil {
		return e
	}
	cfg.OfferSubject, e = appkit.Name(cfg.OfferSubject, 150)
	if e != nil {
		return e
	}
	if strings.ContainsAny(cfg.InterviewSubject+cfg.OfferSubject, "\r\n") {
		return appkit.Invalid("邮件主题不能包含换行")
	}
	cfg.InterviewBody, e = appkit.Name(cfg.InterviewBody, 12000)
	if e != nil {
		return e
	}
	cfg.OfferBody, e = appkit.Name(cfg.OfferBody, 12000)
	if e != nil {
		return e
	}
	if len(cfg.SMTPPassword) > 2048 || len(cfg.SMTPUsername) > 320 {
		return appkit.Invalid("邮件凭据过长")
	}
	if cfg.SMTPAddress != "" {
		host, port, e := net.SplitHostPort(cfg.SMTPAddress)
		if e != nil || host == "" || strings.ContainsAny(host, "/\\\r\n") {
			return appkit.Invalid("SMTP 地址应为 host:port")
		}
		p, e := strconv.Atoi(port)
		if e != nil || p < 1 || p > 65535 {
			return appkit.Invalid("SMTP 端口无效")
		}
		addr, e := mail.ParseAddress(cfg.SMTPFrom)
		if e != nil || strings.ContainsAny(cfg.SMTPFrom, "\r\n") {
			return appkit.Invalid("请填写有效的发件人邮箱")
		}
		cfg.SMTPFrom = addr.Address
	}
	if e = validWebhook(cfg.WecomURL, "wecom"); e != nil {
		return e
	}
	return validWebhook(cfg.FeishuURL, "feishu")
}
func (m *Module) putSettings(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in struct {
		Settings
		ClearSMTPPassword bool `json:"clearSMTPPassword"`
		ClearWecom        bool `json:"clearWecom"`
		ClearFeishu       bool `json:"clearFeishu"`
	}
	if e := appkit.Decode(w, r, &in); e != nil {
		return e
	}
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	old, e := m.settings(r.Context(), s)
	if e != nil {
		return e
	}
	if in.SMTPAddress != old.SMTPAddress || in.SMTPFrom != old.SMTPFrom || in.SMTPUsername != old.SMTPUsername || in.SMTPImplicitTLS != old.SMTPImplicitTLS || in.SMTPPassword != "" || in.WecomURL != "" || in.FeishuURL != "" || in.ClearSMTPPassword || in.ClearWecom || in.ClearFeishu {
		if e = s.Require("secrets"); e != nil {
			return e
		}
	}
	// A saved or deployment-default password belongs to its original SMTP
	// destination and principal. Never forward it to a project-selected endpoint.
	smtpTargetChanged := in.SMTPAddress != old.SMTPAddress || in.SMTPUsername != old.SMTPUsername || in.SMTPImplicitTLS != old.SMTPImplicitTLS
	if smtpTargetChanged && old.SMTPPassword != "" && in.SMTPPassword == "" && !in.ClearSMTPPassword {
		return appkit.Invalid("更改 SMTP 服务器、用户名或 TLS 方式时，请重新输入密码或明确勾选清除密码")
	}
	if in.SMTPPassword == "" && !smtpTargetChanged {
		in.SMTPPassword = old.SMTPPassword
	}
	if in.WecomURL == "" {
		in.WecomURL = old.WecomURL
	}
	if in.FeishuURL == "" {
		in.FeishuURL = old.FeishuURL
	}
	if in.ClearSMTPPassword {
		in.SMTPPassword = ""
	}
	if in.ClearWecom {
		in.WecomURL = ""
	}
	if in.ClearFeishu {
		in.FeishuURL = ""
	}
	if e = validateSettings(&in.Settings); e != nil {
		return e
	}
	if in.TrustSchemeID != "" {
		schemes, e := trust.ListAvailableSchemes(r.Context(), m.rt, s)
		if e != nil {
			return e
		}
		found := false
		for _, scheme := range schemes {
			if scheme.ID == in.TrustSchemeID {
				found = true
			}
		}
		if !found {
			return appkit.Invalid("请选择本项目已启用的 Trust 认证方案")
		}
	}
	b, e := m.seal(in.Settings, s.InstallationID+":settings")
	if e != nil {
		return e
	}
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		_, e := tx.ExecContext(r.Context(), "INSERT INTO eid_settings(installation_id,tenant_id,project_id,data,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(installation_id) DO UPDATE SET data=excluded.data,updated_at=excluded.updated_at", s.InstallationID, s.TenantID, s.ProjectID, b, appkit.Now())
		if e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "settings.update", s.InstallationID, "更新社团流程与通知配置")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, redacted(in.Settings))
	return nil
}
