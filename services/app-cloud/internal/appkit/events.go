package appkit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Event carries identifiers and state only. Materials, credentials and private
// profile fields stay in their owning application. Emit uses the business
// transaction: a rolled-back approval can never grant an entitlement.
type Event struct {
	ID         string `json:"id"`
	TenantID   string `json:"tenantId"`
	ProjectID  string `json:"projectId"`
	Source     string `json:"source"`
	SubjectID  string `json:"subjectId"`
	ResourceID string `json:"resourceId"`
	FilterID   string `json:"filterId"`
	Status     string `json:"status"`
	Version    string `json:"version"`
}

func EnsureEvents(ctx context.Context, q Execer) error {
	_, err := q.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS automation_events(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,source TEXT NOT NULL,filter_id TEXT NOT NULL,body TEXT NOT NULL,created_at INTEGER NOT NULL,dispatched_at INTEGER NOT NULL DEFAULT 0); CREATE INDEX IF NOT EXISTS automation_event_pending ON automation_events(dispatched_at,created_at);`)
	return err
}

func Emit(ctx context.Context, tx Execer, event Event) error {
	if event.TenantID == "" || event.ProjectID == "" || event.ResourceID == "" || event.Source == "" || event.Version == "" {
		return Invalid("事件缺少业务标识")
	}
	if err := EnsureEvents(ctx, tx); err != nil {
		return err
	}
	event.ID = fmt.Sprintf("%s:%s:%s:%s:%s", event.Source, event.ProjectID, event.ResourceID, event.Version, event.SubjectID)
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO automation_events(id,tenant_id,project_id,source,filter_id,body,created_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, event.ID, event.TenantID, event.ProjectID, event.Source, event.FilterID, string(body), time.Now().UnixMilli())
	return err
}
