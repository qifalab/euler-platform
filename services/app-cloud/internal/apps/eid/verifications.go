package eid

import (
	"context"
	"database/sql"
	"net/http"
	"strings"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

type VerificationData struct {
	RealName      string `json:"realName"`
	StudentID     string `json:"studentId"`
	IdentityTitle string `json:"identityTitle"`
	IdentityType  string `json:"identityType"`
	ActorName     string `json:"actorName"`
}
type Verification struct {
	ID      string `json:"id"`
	ActorID string `json:"actorId"`
	VerificationData
	Status    string  `json:"status"`
	Version   int64   `json:"version"`
	CreatedAt string  `json:"createdAt"`
	UpdatedAt string  `json:"updatedAt"`
	History   []Event `json:"history,omitempty"`
}
type verificationInput struct {
	RealName      string `json:"realName"`
	StudentID     string `json:"studentId"`
	IdentityTitle string `json:"identityTitle"`
	IdentityType  string `json:"identityType"`
	Version       int64  `json:"version,omitempty"`
}

var identityLevels = map[string]int{"active": 1, "core": 2, "key": 3, "management": 4, "outstanding": 5, "smart_car": 6}

func validateVerification(in verificationInput) (VerificationData, error) {
	d := VerificationData{IdentityType: in.IdentityType}
	var err error
	if _, ok := identityLevels[in.IdentityType]; !ok {
		return d, appkit.Invalid("请选择有效的身份类型")
	}
	d.RealName, err = appkit.Name(in.RealName, 50)
	if err != nil {
		return d, err
	}
	d.StudentID, err = appkit.Name(in.StudentID, 40)
	if err != nil {
		return d, err
	}
	d.IdentityTitle, err = appkit.Name(in.IdentityTitle, 100)
	return d, err
}
func (m *Module) verification(ctx context.Context, s appkit.Scope, id string) (Verification, error) {
	var v Verification
	var b []byte
	e := m.rt.DB.QueryRowContext(ctx, "SELECT id,actor_id,data,status,version,created_at,updated_at FROM eid_verifications WHERE "+scoped+" AND id=?", append(scopeArgs(s), id)...).Scan(&v.ID, &v.ActorID, &b, &v.Status, &v.Version, &v.CreatedAt, &v.UpdatedAt)
	if e != nil {
		return v, e
	}
	e = m.open(b, s.InstallationID+":verification:"+id, &v.VerificationData)
	return v, e
}
func (m *Module) listVerifications(w http.ResponseWriter, r *http.Request, s appkit.Scope, review bool) error {
	page, size := pagination(r)
	where := scoped
	args := scopeArgs(s)
	if !review {
		where += " AND actor_id=?"
		args = append(args, s.ActorID)
	}
	if status := r.URL.Query().Get("status"); status != "" && status != "all" {
		if status != "pending" && status != "approved" && status != "rejected" {
			return appkit.Invalid("无效的状态")
		}
		where += " AND status=?"
		args = append(args, status)
	}
	if actor := r.URL.Query().Get("actorId"); review && actor != "" {
		where += " AND actor_id=?"
		args = append(args, actor)
	}
	var total int
	if e := m.rt.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM eid_verifications WHERE "+where, args...).Scan(&total); e != nil {
		return e
	}
	rows, e := m.rt.DB.QueryContext(r.Context(), "SELECT id,actor_id,data,status,version,created_at,updated_at FROM eid_verifications WHERE "+where+" ORDER BY created_at DESC LIMIT ? OFFSET ?", append(args, size, (page-1)*size)...)
	if e != nil {
		return e
	}
	items := []Verification{}
	for rows.Next() {
		var v Verification
		var b []byte
		if e = rows.Scan(&v.ID, &v.ActorID, &b, &v.Status, &v.Version, &v.CreatedAt, &v.UpdatedAt); e != nil {
			rows.Close()
			return e
		}
		if e = m.open(b, s.InstallationID+":verification:"+v.ID, &v.VerificationData); e != nil {
			rows.Close()
			return e
		}
		items = append(items, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for i := range items {
		items[i].History, e = m.events(r.Context(), s, items[i].ID)
		if e != nil {
			return e
		}
	}
	appkit.JSON(w, 200, map[string]any{"items": items, "total": total, "page": page})
	return nil
}
func (m *Module) myVerifications(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	return m.listVerifications(w, r, s, false)
}
func (m *Module) reviewVerifications(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	return m.listVerifications(w, r, s, true)
}
func (m *Module) applyVerification(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in verificationInput
	if e := appkit.Decode(w, r, &in); e != nil {
		return e
	}
	d, e := validateVerification(in)
	if e != nil {
		return e
	}
	d.ActorName = s.ActorName
	if e = m.ensureMember(r.Context(), s); e != nil {
		return e
	}
	id := appkit.NewID("eva_")
	b, e := m.seal(d, s.InstallationID+":verification:"+id)
	if e != nil {
		return e
	}
	now := appkit.Now()
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		var count int
		if e := tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM eid_verifications WHERE "+scoped+" AND actor_id=? AND status='pending'", append(scopeArgs(s), s.ActorID)...).Scan(&count); e != nil {
			return e
		}
		if count > 0 {
			return appkit.Conflict("已有待审核申请，请等待处理")
		}
		_, e := tx.ExecContext(r.Context(), "INSERT INTO eid_verifications(id,installation_id,tenant_id,project_id,actor_id,data,status,identity_type,created_at,updated_at) VALUES(?,?,?,?,?,?,'pending',?,?,?)", id, s.InstallationID, s.TenantID, s.ProjectID, s.ActorID, b, d.IdentityType, now, now)
		if e != nil {
			return e
		}
		return m.event(r.Context(), tx, s, id, "verification.submit", "", "pending")
	})
	if e != nil {
		return e
	}
	v, e := m.verification(r.Context(), s, id)
	if e != nil {
		return e
	}
	appkit.JSON(w, 201, v)
	return nil
}
func (m *Module) editVerification(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in verificationInput
	if e := appkit.Decode(w, r, &in); e != nil {
		return e
	}
	d, e := validateVerification(in)
	if e != nil {
		return e
	}
	v, e := m.verification(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	if in.Version != v.Version {
		return appkit.Conflict("记录已更新，请刷新后编辑")
	}
	d.ActorName = v.ActorName
	b, e := m.seal(d, s.InstallationID+":verification:"+v.ID)
	if e != nil {
		return e
	}
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		result, e := tx.ExecContext(r.Context(), "UPDATE eid_verifications SET data=?,identity_type=?,version=version+1,updated_at=? WHERE "+scoped+" AND id=? AND version=?", append([]any{b, d.IdentityType, appkit.Now()}, append(scopeArgs(s), v.ID, in.Version)...)...)
		if e != nil {
			return e
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return appkit.Conflict("记录已更新，请刷新后编辑")
		}
		if e = m.syncIdentity(r.Context(), tx, s, v.ActorID); e != nil {
			return e
		}
		return m.event(r.Context(), tx, s, v.ID, "verification.edit", v.Status, v.Status)
	})
	if e != nil {
		return e
	}
	out, e := m.verification(r.Context(), s, v.ID)
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, out)
	return nil
}
func (m *Module) decideVerification(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in struct {
		Status  string `json:"status"`
		Version int64  `json:"version"`
	}
	if e := appkit.Decode(w, r, &in); e != nil {
		return e
	}
	if in.Status != "approved" && in.Status != "rejected" {
		return appkit.Invalid("请选择通过或拒绝")
	}
	v, e := m.verification(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	if in.Version != v.Version {
		return appkit.Conflict("记录已更新，请刷新后审核")
	}
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		result, e := tx.ExecContext(r.Context(), "UPDATE eid_verifications SET status=?,version=version+1,updated_at=? WHERE "+scoped+" AND id=? AND version=?", append([]any{in.Status, appkit.Now()}, append(scopeArgs(s), v.ID, in.Version)...)...)
		if e != nil {
			return e
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return appkit.Conflict("记录已更新，请刷新后审核")
		}
		if e = m.syncIdentity(r.Context(), tx, s, v.ActorID); e != nil {
			return e
		}
		return m.event(r.Context(), tx, s, v.ID, "verification."+in.Status, v.Status, in.Status)
	})
	if e != nil {
		return e
	}
	out, e := m.verification(r.Context(), s, v.ID)
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, out)
	return nil
}
func (m *Module) syncIdentity(ctx context.Context, tx *sql.Tx, s appkit.Scope, actor string) error {
	var id string
	var b []byte
	level := 0
	title := ""
	e := tx.QueryRowContext(ctx, "SELECT id,data FROM eid_verifications WHERE "+scoped+" AND actor_id=? AND status='approved' ORDER BY updated_at DESC,id DESC LIMIT 1", append(scopeArgs(s), actor)...).Scan(&id, &b)
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	if e == nil {
		var d VerificationData
		if e = m.open(b, s.InstallationID+":verification:"+id, &d); e != nil {
			return e
		}
		level = identityLevels[d.IdentityType]
		title = d.IdentityTitle
	}
	_, e = tx.ExecContext(ctx, "UPDATE eid_members SET identity_level=?,identity_title=?,updated_at=? WHERE "+scoped+" AND actor_id=?", append([]any{level, title, appkit.Now()}, append(scopeArgs(s), actor)...)...)
	return e
}
func (m *Module) cards(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	q := r.URL.Query()
	q.Set("status", "approved")
	q.Set("pageSize", "100")
	r.URL.RawQuery = q.Encode()
	return m.myVerifications(w, r, s)
}
func (m *Module) deleteVerification(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	v, e := m.verification(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		_, e := tx.ExecContext(r.Context(), "DELETE FROM eid_verifications WHERE "+scoped+" AND id=?", append(scopeArgs(s), v.ID)...)
		if e != nil {
			return e
		}
		if e = m.syncIdentity(r.Context(), tx, s, v.ActorID); e != nil {
			return e
		}
		return m.event(r.Context(), tx, s, v.ID, "verification.delete", v.Status, "deleted")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 204, nil)
	return nil
}
func isUnique(e error) bool {
	return e != nil && strings.Contains(strings.ToLower(e.Error()), "unique constraint")
}
