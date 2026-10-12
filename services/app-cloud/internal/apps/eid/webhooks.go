package eid

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

// queueWebhooks writes an encrypted outbox in the business transaction. A crash
// after commit can leave a visible queued job, never an unrecorded notification.
func (m *Module) queueWebhooks(ctx context.Context, tx *sql.Tx, s appkit.Scope, event, target string) error {
	cfg, err := m.readSettings(ctx, tx, s)
	if err != nil {
		return err
	}
	for _, destination := range []struct{ provider, url string }{{"wecom", cfg.WecomURL}, {"feishu", cfg.FeishuURL}} {
		if destination.url == "" {
			continue
		}
		id := appkit.NewID("ent_")
		data := notificationData{Recipient: destination.url, Subject: event, Body: "欧拉成员服务：" + event + "\n记录：" + target + "\n" + m.applicationLink(s)}
		enc, err := m.seal(data, s.InstallationID+":notification:"+id)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO eid_notifications(id,installation_id,tenant_id,project_id,target_id,actor_id,kind,data,state,idempotency_key,created_at) VALUES(?,?,?,?,?,?,?,?,'queued',?,?)", id, s.InstallationID, s.TenantID, s.ProjectID, target, s.ActorID, "webhook_"+destination.provider, enc, id, appkit.Now()); err != nil {
			return err
		}
	}
	return nil
}

// flushWebhooks runs after commit with cancellation detached from the browser.
// Jobs claimed before a process crash remain sending and are never auto-retried.
func (m *Module) flushWebhooks(parent context.Context, s appkit.Scope) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 25*time.Second)
	defer cancel()
	rows, err := m.rt.DB.QueryContext(ctx, "SELECT id FROM eid_notifications WHERE "+scoped+" AND state='queued' AND kind LIKE 'webhook_%' ORDER BY created_at LIMIT 25", scopeArgs(s)...)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err != nil {
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		_ = m.deliverWebhook(ctx, s, id)
	}
}

func (m *Module) deliverWebhook(ctx context.Context, s appkit.Scope, id string) error {
	var kind, target string
	var encrypted []byte
	claimed := false
	err := m.rt.Transaction(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "SELECT kind,target_id,data FROM eid_notifications WHERE "+scoped+" AND id=? AND state='queued' AND kind LIKE 'webhook_%'", append(scopeArgs(s), id)...).Scan(&kind, &target, &encrypted); err != nil {
			if err == sql.ErrNoRows {
				return nil
			}
			return err
		}
		result, err := tx.ExecContext(ctx, "UPDATE eid_notifications SET state='sending' WHERE "+scoped+" AND id=? AND state='queued'", append(scopeArgs(s), id)...)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		claimed = n == 1
		return nil
	})
	if err != nil || !claimed {
		return err
	}
	var data notificationData
	sendErr := m.open(encrypted, s.InstallationID+":notification:"+id, &data)
	if sendErr == nil {
		sendErr = m.postWebhook(ctx, strings.TrimPrefix(kind, "webhook_"), data.Recipient, data.Body)
	}
	state, message := "sent", ""
	if sendErr != nil {
		state = "failed"
		message = "机器人通知未确认送达，请核对渠道记录与配置后重试"
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return m.rt.Transaction(finish, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(finish, "UPDATE eid_notifications SET state=?,error=?,completed_at=? WHERE "+scoped+" AND id=? AND state='sending'", append([]any{state, message, appkit.Now()}, append(scopeArgs(s), id)...)...); err != nil {
			return err
		}
		return m.rt.Audit(finish, tx, s, "notification."+state, target, "记录机器人通知投递结果")
	})
}

func (m *Module) postWebhook(ctx context.Context, provider, endpoint, content string) error {
	if err := validWebhook(endpoint, provider); err != nil {
		return err
	}
	var payload any
	if provider == "wecom" {
		payload = map[string]any{"msgtype": "text", "text": map[string]string{"content": content}}
	} else {
		payload = map[string]any{"msg_type": "text", "content": map[string]string{"text": content}}
	}
	body, _ := json.Marshal(payload)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := m.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var result struct {
		ErrCode *int `json:"errcode"`
		Code    *int `json:"code"`
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&result)
	confirmed := provider == "wecom" && result.ErrCode != nil && *result.ErrCode == 0 || provider == "feishu" && result.Code != nil && *result.Code == 0
	if response.StatusCode < 200 || response.StatusCode > 299 || err != nil || !confirmed {
		return errors.New("notification not acknowledged")
	}
	return nil
}

func (m *Module) retryWebhook(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in struct {
		Confirmed bool `json:"confirmed"`
	}
	if err := appkit.Decode(w, r, &in); err != nil {
		return err
	}
	if !in.Confirmed {
		return appkit.Invalid("请先核对渠道记录，确认重试可能造成重复消息")
	}
	id := r.PathValue("id")
	err := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		var kind, state string
		var encrypted []byte
		if err := tx.QueryRowContext(r.Context(), "SELECT kind,state,data FROM eid_notifications WHERE "+scoped+" AND id=?", append(scopeArgs(s), id)...).Scan(&kind, &state, &encrypted); err != nil {
			return err
		}
		if !strings.HasPrefix(kind, "webhook_") {
			return appkit.Invalid("邮件请在招募审核中核对并重发")
		}
		if state != "failed" && state != "queued" {
			return appkit.Conflict("该通知已经发送或结果尚未确定，请先核对渠道记录")
		}
		cfg, err := m.readSettings(r.Context(), tx, s)
		if err != nil {
			return err
		}
		var data notificationData
		if err = m.open(encrypted, s.InstallationID+":notification:"+id, &data); err != nil {
			return err
		}
		data.Recipient = cfg.WecomURL
		if kind == "webhook_feishu" {
			data.Recipient = cfg.FeishuURL
		}
		if data.Recipient == "" {
			return appkit.Conflict("当前渠道已停用，请先配置通知地址")
		}
		encrypted, err = m.seal(data, s.InstallationID+":notification:"+id)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(r.Context(), "UPDATE eid_notifications SET state='queued',data=?,error='',completed_at='' WHERE "+scoped+" AND id=?", append([]any{encrypted}, append(scopeArgs(s), id)...)...); err != nil {
			return err
		}
		return m.rt.Audit(r.Context(), tx, s, "notification.retry", id, "明确确认后重试机器人通知")
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
	defer cancel()
	if err = m.deliverWebhook(ctx, s, id); err != nil {
		return err
	}
	result, err := m.notificationByKey(ctx, s, id)
	if err != nil {
		return err
	}
	appkit.JSON(w, 200, result)
	return nil
}
