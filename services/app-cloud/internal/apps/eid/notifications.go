package eid

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/trust"
)

type Notification struct {
	ID          string `json:"id"`
	TargetID    string `json:"targetId"`
	ActorID     string `json:"actorId"`
	Kind        string `json:"kind"`
	State       string `json:"state"`
	Error       string `json:"error"`
	CreatedAt   string `json:"createdAt"`
	CompletedAt string `json:"completedAt"`
}
type notificationData struct {
	Recipient string `json:"recipient"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
}

func (m *Module) notifications(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	page, size := pagination(r)
	args := scopeArgs(s)
	where := scoped
	if target := r.URL.Query().Get("targetId"); target != "" {
		where += " AND target_id=?"
		args = append(args, target)
	}
	var total int
	if e := m.rt.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM eid_notifications WHERE "+where, args...).Scan(&total); e != nil {
		return e
	}
	rows, e := m.rt.DB.QueryContext(r.Context(), "SELECT id,target_id,actor_id,kind,state,error,created_at,completed_at FROM eid_notifications WHERE "+where+" ORDER BY created_at DESC LIMIT ? OFFSET ?", append(args, size, (page-1)*size)...)
	if e != nil {
		return e
	}
	defer rows.Close()
	items := []Notification{}
	for rows.Next() {
		var n Notification
		if e = rows.Scan(&n.ID, &n.TargetID, &n.ActorID, &n.Kind, &n.State, &n.Error, &n.CreatedAt, &n.CompletedAt); e != nil {
			return e
		}
		items = append(items, n)
	}
	if e = rows.Err(); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"items": items, "total": total, "page": page})
	return nil
}
func (m *Module) notificationByKey(ctx context.Context, s appkit.Scope, key string) (Notification, error) {
	var n Notification
	e := m.rt.DB.QueryRowContext(ctx, "SELECT id,target_id,actor_id,kind,state,error,created_at,completed_at FROM eid_notifications WHERE "+scoped+" AND idempotency_key=?", append(scopeArgs(s), key)...).Scan(&n.ID, &n.TargetID, &n.ActorID, &n.Kind, &n.State, &n.Error, &n.CreatedAt, &n.CompletedAt)
	return n, e
}
func (m *Module) recordNotification(ctx context.Context, s appkit.Scope, id, target, kind, key string, data notificationData) error {
	b, e := m.seal(data, s.InstallationID+":notification:"+id)
	if e != nil {
		return e
	}
	return m.rt.Transaction(ctx, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "INSERT INTO eid_notifications(id,installation_id,tenant_id,project_id,target_id,actor_id,kind,data,state,idempotency_key,created_at) VALUES(?,?,?,?,?,?,?,?,'sending',?,?)", id, s.InstallationID, s.TenantID, s.ProjectID, target, s.ActorID, kind, b, key, appkit.Now())
		if e != nil {
			return e
		}
		return m.rt.Audit(ctx, tx, s, "notification.intent", target, "已记录通知发送意图")
	})
}
func (m *Module) sendClubNotification(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in struct {
		Kind           string `json:"kind"`
		Resend         bool   `json:"resend"`
		Version        int64  `json:"version"`
		IdempotencyKey string `json:"idempotencyKey"`
	}
	if e := appkit.Decode(w, r, &in); e != nil {
		return e
	}
	if in.Kind != "interview" && in.Kind != "offer" {
		return appkit.Invalid("无效的通知类型")
	}
	key, e := appkit.Name(in.IdempotencyKey, 120)
	if e != nil {
		return e
	}
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	kind := in.Kind
	if in.Resend {
		kind = "resend_" + kind
	}
	existing, e := m.notificationByKey(r.Context(), s, key)
	if e == nil {
		if existing.TargetID != r.PathValue("id") || existing.Kind != kind {
			return appkit.Conflict("该请求编号已用于其他操作")
		}
		appkit.JSON(w, 200, existing)
		return nil
	}
	if e != sql.ErrNoRows {
		return e
	}
	a, e := m.club(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	if a.Version != in.Version {
		return appkit.Conflict("申请已更新，请刷新")
	}
	expected := "pending"
	next := "interview_sent"
	stamp := "interview_sent_at"
	if in.Kind == "offer" {
		expected = "interview_sent"
		next = "offer_sent"
		stamp = "offer_sent_at"
	}
	if in.Resend {
		expected = next
		stamp = ""
	}
	if a.Status != expected {
		return appkit.Conflict("当前申请状态不允许此通知；重发只适用于已经发送的阶段")
	}
	cfg, e := m.settings(r.Context(), s)
	if e != nil {
		return e
	}
	if cfg.SMTPAddress == "" || cfg.SMTPFrom == "" {
		return appkit.Unavailable("项目尚未配置邮件服务，请先在流程设置中配置 SMTP")
	}
	if e = validateSettings(&cfg); e != nil {
		return e
	}
	if in.Kind == "offer" && cfg.RequireTrust {
		actorScope := s
		actorScope.ActorID = a.ActorID
		ok, e := trust.CheckQualification(r.Context(), m.rt, actorScope, cfg.TrustSchemeID)
		if e != nil {
			return e
		}
		if !ok {
			return appkit.Conflict("申请人的 Trust 认证尚未通过或已改变，不能发送录取通知")
		}
	}
	link := m.applicationLink(s)
	replace := strings.NewReplacer("{{name}}", a.RealName, "{{url}}", link, "{{club}}", cfg.ClubName)
	subject, body := cfg.InterviewSubject, cfg.InterviewBody
	if in.Kind == "offer" {
		subject, body = cfg.OfferSubject, cfg.OfferBody
	}
	subject = replace.Replace(subject)
	body = replace.Replace(body)
	id := appkit.NewID("ent_")
	if e = m.recordNotification(r.Context(), s, id, a.ID, kind, key, notificationData{Recipient: a.Email, Subject: subject, Body: body}); e != nil {
		return e
	}
	sendErr := m.mailer(r.Context(), cfg, a.Email, subject, body)
	finish, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if sendErr != nil {
		state := "failed"
		message := "邮件未发送，请检查配置后人工重试"
		var ambiguous *uncertainDelivery
		if errors.As(sendErr, &ambiguous) {
			state = "uncertain"
			message = "邮件投递结果未知，请先核对收件箱或邮件服务记录，避免重复发送"
		}
		e = m.rt.Transaction(finish, func(tx *sql.Tx) error {
			_, e := tx.ExecContext(finish, "UPDATE eid_notifications SET state=?,error=?,completed_at=? WHERE id=? AND "+scoped, append([]any{state, message, appkit.Now(), id}, scopeArgs(s)...)...)
			if e != nil {
				return e
			}
			return m.rt.Audit(finish, tx, s, "notification."+state, a.ID, "通知未确认投递，申请状态保持不变")
		})
		if e != nil {
			return e
		}
		n, e := m.notificationByKey(finish, s, key)
		if e != nil {
			return e
		}
		appkit.JSON(w, 200, n)
		return nil
	}
	e = m.rt.Transaction(finish, func(tx *sql.Tx) error {
		q := "UPDATE eid_club SET status=?,version=version+1,updated_at=?"
		args := []any{next, appkit.Now()}
		if stamp != "" {
			q += "," + stamp + "=?"
			args = append(args, appkit.Now())
		}
		q += " WHERE " + scoped + " AND id=? AND version=?"
		result, e := tx.ExecContext(finish, q, append(args, append(scopeArgs(s), a.ID, a.Version)...)...)
		if e != nil {
			return e
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return appkit.Conflict("邮件已发送但申请发生变化，请核对通知历史")
		}
		_, e = tx.ExecContext(finish, "UPDATE eid_notifications SET state='sent',completed_at=? WHERE id=? AND "+scoped, append([]any{appkit.Now(), id}, scopeArgs(s)...)...)
		if e != nil {
			return e
		}
		return m.event(finish, tx, s, a.ID, "club."+kind, a.Status, next)
	})
	if e != nil {
		return appkit.Unavailable("邮件已投递，但记录更新尚未确认；请核对通知历史，勿重复发送")
	}
	n, e := m.notificationByKey(finish, s, key)
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, n)
	return nil
}
func (m *Module) applicationLink(s appkit.Scope) string {
	return strings.TrimRight(m.rt.PublicURL, "/") + "/apps/eid?tenantId=" + url.QueryEscape(s.TenantID) + "&projectId=" + url.QueryEscape(s.ProjectID) + "&tab=club"
}

type uncertainDelivery struct{ err error }

func (e *uncertainDelivery) Error() string { return "delivery confirmation unavailable" }
func (m *Module) sendSMTP(ctx context.Context, cfg Settings, to, subject, body string) error {
	recipient, e := mail.ParseAddress(to)
	if e != nil || strings.ContainsAny(to+subject, "\r\n") {
		return errors.New("invalid recipient or subject")
	}
	from, e := mail.ParseAddress(cfg.SMTPFrom)
	if e != nil {
		return errors.New("invalid sender")
	}
	host, _, e := net.SplitHostPort(cfg.SMTPAddress)
	if e != nil {
		return e
	}
	addrs, e := net.DefaultResolver.LookupIPAddr(ctx, host)
	if e != nil {
		return e
	}
	if len(addrs) == 0 {
		return errors.New("SMTP address unavailable")
	}
	allowLocal := os.Getenv("EULER_EID_ALLOW_LOCAL_SMTP") == "true"
	if !allowLocal {
		for _, a := range addrs {
			if !a.IP.IsGlobalUnicast() || a.IP.IsPrivate() || a.IP.IsLoopback() || a.IP.IsLinkLocalUnicast() {
				return errors.New("private SMTP endpoint requires deployment opt-in")
			}
		}
	}
	_, port, _ := net.SplitHostPort(cfg.SMTPAddress)
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, e := dialer.DialContext(ctx, "tcp", net.JoinHostPort(addrs[0].IP.String(), port))
	if e != nil {
		return e
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	tlsConfig := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if m.smtpTLSConfig != nil {
		tlsConfig = m.smtpTLSConfig(host)
	}
	if cfg.SMTPImplicitTLS {
		tlsConn := tls.Client(conn, tlsConfig)
		if e = tlsConn.HandshakeContext(ctx); e != nil {
			return e
		}
		conn = tlsConn
	}
	client, e := smtp.NewClient(conn, host)
	if e != nil {
		return e
	}
	defer client.Close()
	if !cfg.SMTPImplicitTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("SMTP STARTTLS required")
		}
		if e = client.StartTLS(tlsConfig); e != nil {
			return e
		}
	}
	if cfg.SMTPUsername != "" {
		if e = client.Auth(smtp.PlainAuth("", cfg.SMTPUsername, cfg.SMTPPassword, host)); e != nil {
			return e
		}
	}
	if e = client.Mail(from.Address); e != nil {
		return e
	}
	if e = client.Rcpt(recipient.Address); e != nil {
		return e
	}
	writer, e := client.Data()
	if e != nil {
		return e
	}
	message := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n%s", from.Address, recipient.Address, mime.QEncoding.Encode("UTF-8", subject), strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	if _, e = io.WriteString(writer, message); e != nil {
		return &uncertainDelivery{e}
	}
	if e = writer.Close(); e != nil {
		return &uncertainDelivery{e}
	}
	_ = client.Quit()
	return nil
}
