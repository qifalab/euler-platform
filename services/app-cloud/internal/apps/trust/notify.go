package trust

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type noticePayload struct {
	Scheme       string `json:"scheme"`
	Action       string `json:"action"`
	Status       string `json:"status"`
	SubmissionID string `json:"submissionId"`
}
type Notice struct {
	ID           string `json:"id"`
	SubmissionID string `json:"submissionId"`
	Channel      string `json:"channel"`
	Status       string `json:"status"`
	Attempts     int    `json:"attempts"`
	LastError    string `json:"lastError"`
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
}

func webhook(channel string) string {
	if channel == "wecom" {
		return os.Getenv("EULER_NOTIFY_WECOM_URL")
	}
	if channel == "feishu" {
		return os.Getenv("EULER_NOTIFY_FEISHU_URL")
	}
	return ""
}
func (m *Module) enqueue(ctx context.Context, tx *sql.Tx, s appkit.Scope, submissionID, scheme, action, status string) error {
	for _, channel := range []string{"wecom", "feishu"} {
		if webhook(channel) == "" {
			continue
		}
		id := appkit.NewID("tn_")
		b, e := m.seal(noticePayload{Scheme: scheme, Action: action, Status: status, SubmissionID: submissionID}, "notice:"+id)
		if e != nil {
			return e
		}
		now := appkit.Now()
		_, e = tx.ExecContext(ctx, "INSERT INTO trust_notifications VALUES(?,?,?,?,?,?,?,?,?,?,?)", id, s.TenantID, s.ProjectID, submissionID, channel, b, "pending", 0, "", now, now)
		if e != nil {
			return e
		}
	}
	return nil
}
func (m *Module) deliverFor(ctx context.Context, s appkit.Scope, submissionID string) {
	rows, e := m.rt.DB.QueryContext(ctx, "SELECT id FROM trust_notifications WHERE tenant_id=? AND project_id=? AND submission_id=? AND status='pending'", s.TenantID, s.ProjectID, submissionID)
	if e != nil {
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		m.deliver(context.WithoutCancel(ctx), s, id)
	}
}
func (m *Module) deliver(ctx context.Context, s appkit.Scope, id string) {
	ctx, cancel := context.WithTimeout(ctx, 7*time.Second)
	defer cancel()
	now := appkit.Now()
	result, e := m.rt.DB.ExecContext(ctx, "UPDATE trust_notifications SET status='sending',attempts=attempts+1,updated_at=? WHERE id=? AND tenant_id=? AND project_id=? AND status='pending'", now, id, s.TenantID, s.ProjectID)
	if e != nil {
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return
	}
	status, reason := "failed", "通知配置无效"
	defer func() {
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_, _ = m.rt.DB.ExecContext(persist, "UPDATE trust_notifications SET status=?,last_error=?,updated_at=? WHERE id=? AND tenant_id=? AND project_id=?", status, reason, appkit.Now(), id, s.TenantID, s.ProjectID)
	}()
	var channel string
	var b []byte
	if e = m.rt.DB.QueryRowContext(ctx, "SELECT channel,payload FROM trust_notifications WHERE id=? AND tenant_id=? AND project_id=?", id, s.TenantID, s.ProjectID).Scan(&channel, &b); e != nil {
		return
	}
	var p noticePayload
	if m.open(b, "notice:"+id, &p) != nil {
		return
	}
	raw := webhook(channel)
	u, e := url.Parse(raw)
	if e != nil || u.User != nil || u.Host == "" || u.Fragment != "" {
		return
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || (u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()))) {
		return
	}
	labels := map[string]string{"submit": "新认证提交", "resubmit": "认证重新提交", "approve": "认证已通过", "reject": "认证已拒绝", "pending": "待审核", "approved": "已通过", "rejected": "未通过"}
	text := "【Euler Trust】" + labels[p.Action] + "\n认证方案：" + p.Scheme + "\n申请编号：" + p.SubmissionID + "\n状态：" + labels[p.Status]
	var payload any
	if channel == "wecom" {
		payload = map[string]any{"msgtype": "text", "text": map[string]string{"content": text}}
	} else {
		payload = map[string]any{"msg_type": "text", "content": map[string]string{"text": text}}
	}
	body, _ := json.Marshal(payload)
	req, e := http.NewRequestWithContext(ctx, "POST", raw, bytes.NewReader(body))
	if e != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, e := client.Do(req)
	if e != nil {
		status = "unknown"
		reason = "通知结果未知，请核实后手动重试"
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reason = "通知服务拒绝请求"
		return
	}
	response, e := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if e != nil || len(response) > 65536 {
		status = "unknown"
		reason = "通知服务返回无效响应"
		return
	}
	var out struct {
		Code    *int `json:"code"`
		ErrCode *int `json:"errcode"`
	}
	if json.Unmarshal(response, &out) != nil {
		status = "unknown"
		reason = "通知服务返回无效响应"
		return
	}
	code := out.Code
	if channel == "wecom" {
		code = out.ErrCode
	}
	if code == nil {
		status = "unknown"
		reason = "通知服务没有返回确认结果"
		return
	}
	if *code != 0 {
		reason = "通知服务未接受消息"
		return
	}
	status = "sent"
	reason = ""
}
func (m *Module) notifications(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	rows, e := m.rt.DB.QueryContext(r.Context(), "SELECT id,submission_id,channel,status,attempts,last_error,created_at,updated_at FROM trust_notifications WHERE tenant_id=? AND project_id=? ORDER BY created_at DESC LIMIT 200", s.TenantID, s.ProjectID)
	if e != nil {
		return e
	}
	defer rows.Close()
	items := []Notice{}
	for rows.Next() {
		var n Notice
		if e = rows.Scan(&n.ID, &n.SubmissionID, &n.Channel, &n.Status, &n.Attempts, &n.LastError, &n.CreatedAt, &n.UpdatedAt); e != nil {
			return e
		}
		items = append(items, n)
	}
	if e = rows.Err(); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"items": items, "channels": map[string]bool{"wecom": strings.TrimSpace(webhook("wecom")) != "", "feishu": strings.TrimSpace(webhook("feishu")) != ""}})
	return nil
}
func (m *Module) retryNotification(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	id := r.PathValue("id")
	e := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		result, e := tx.ExecContext(r.Context(), "UPDATE trust_notifications SET status='pending',updated_at=? WHERE id=? AND tenant_id=? AND project_id=? AND (status IN ('failed','unknown','pending') OR (status='sending' AND updated_at<?))", appkit.Now(), id, s.TenantID, s.ProjectID, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano))
		if e != nil {
			return e
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return appkit.Conflict("通知已成功或仍在发送")
		}
		return m.rt.Audit(r.Context(), tx, s, "notification.retry", id, "手动重试生命周期通知")
	})
	if e != nil {
		return e
	}
	m.deliver(context.WithoutCancel(r.Context()), s, id)
	appkit.JSON(w, 200, map[string]bool{"processed": true})
	return nil
}
