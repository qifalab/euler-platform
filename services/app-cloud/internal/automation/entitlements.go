package automation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"time"
)

// This adapter speaks only to Euler's two resource-package ledgers. It keeps
// their immutable quota snapshot and expiry semantics, adds a workflow receipt,
// and writes the same application audit in the caller's atomic transaction.
// No dynamic table name can come from outside the fixed allow-list.
func ledgerPrefix(app string) (string, error) {
	if app == "database" || app == "storage" {
		return app, nil
	}
	return "", appkit.Invalid("不支持此资源包应用")
}
func template(ctx context.Context, tx *sql.Tx, r Rule) (name, quota string, days int, err error) {
	prefix, err := ledgerPrefix(r.TargetApp)
	if err != nil {
		return "", "", 0, err
	}
	var active bool
	err = tx.QueryRowContext(ctx, "SELECT name,quota,days,active FROM "+prefix+"_templates WHERE id=? AND tenant_id=? AND project_id=?", r.TemplateID, r.TenantID, r.TargetProjectID).Scan(&name, &quota, &days, &active)
	if err != nil {
		return
	}
	if !active || days < 1 || days > 36500 || !json.Valid([]byte(quota)) {
		err = appkit.Conflict("资源包模板已停用或无效")
	}
	return
}
func validateTemplate(ctx context.Context, tx *sql.Tx, r Rule) error {
	_, _, _, err := template(ctx, tx, r)
	return err
}
func (e *Engine) grant(ctx context.Context, tx *sql.Tx, s appkit.Scope, r Rule, event appkit.Event) error {
	var id string
	err := tx.QueryRowContext(ctx, "SELECT id FROM automation_entitlements WHERE rule_id=? AND subject_id=?", r.ID, event.SubjectID).Scan(&id)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	name, quota, days, err := template(ctx, tx, r)
	if err != nil {
		return err
	}
	prefix, _ := ledgerPrefix(r.TargetApp)
	id = appkit.NewID("auto_pkg_")
	now := time.Now().UTC()
	expiry := now.Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, "INSERT INTO "+prefix+"_grants(id,tenant_id,project_id,name,quota,expires_at,created_at,code_id) VALUES(?,?,?,?,?,?,?,NULL)", id, r.TenantID, r.TargetProjectID, name, quota, expiry, now.Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO automation_entitlements(id,rule_id,subject_id,grant_id,target_app,tenant_id,project_id,source_resource,created_at) VALUES(?,?,?,?,?,?,?,?,?)", id, r.ID, event.SubjectID, id, r.TargetApp, r.TenantID, r.TargetProjectID, event.ResourceID, now.UnixMilli())
	if err != nil {
		return err
	}
	return e.rt.Audit(ctx, tx, s, "package.automation_granted", id, "按明确联动规则授予项目资源包；单人单规则一次，保留模板有效期")
}
func (e *Engine) revoke(ctx context.Context, tx *sql.Tx, s appkit.Scope, r Rule, event appkit.Event) error {
	prefix, err := ledgerPrefix(r.TargetApp)
	if err != nil {
		return err
	}
	var id string
	err = tx.QueryRowContext(ctx, "SELECT grant_id FROM automation_entitlements WHERE rule_id=? AND subject_id=? AND revoked_at=0", r.ID, event.SubjectID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE "+prefix+"_grants SET expires_at=? WHERE id=? AND tenant_id=? AND project_id=?", appkit.Now(), id, r.TenantID, r.TargetProjectID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE automation_entitlements SET revoked_at=? WHERE rule_id=? AND subject_id=?", time.Now().UnixMilli(), r.ID, event.SubjectID)
	if err != nil {
		return err
	}
	return e.rt.Audit(ctx, tx, s, "package.automation_revoked", id, "来源资格已失效，终止联动资源包；不删除已有数据库或文件")
}
