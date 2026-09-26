package eid

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/trust"
)

type ClubData struct {
	RealName  string `json:"realName"`
	ActorName string `json:"actorName"`
	Email     string `json:"email"`
}
type ClubApplication struct {
	ID      string `json:"id"`
	ActorID string `json:"actorId"`
	ClubData
	Status          string  `json:"status"`
	TrustSchemeID   string  `json:"trustSchemeId"`
	TrustVerified   bool    `json:"trustVerified"`
	Version         int64   `json:"version"`
	CreatedAt       string  `json:"createdAt"`
	UpdatedAt       string  `json:"updatedAt"`
	InterviewSentAt string  `json:"interviewSentAt"`
	OfferSentAt     string  `json:"offerSentAt"`
	ConfirmedAt     string  `json:"confirmedAt"`
	History         []Event `json:"history,omitempty"`
}

const clubCols = "id,actor_id,data,status,trust_scheme_id,trust_verified,version,created_at,updated_at,interview_sent_at,offer_sent_at,confirmed_at"

func (m *Module) club(ctx context.Context, s appkit.Scope, id string) (ClubApplication, error) {
	var a ClubApplication
	var b []byte
	e := m.rt.DB.QueryRowContext(ctx, "SELECT "+clubCols+" FROM eid_club WHERE "+scoped+" AND id=?", append(scopeArgs(s), id)...).Scan(&a.ID, &a.ActorID, &b, &a.Status, &a.TrustSchemeID, &a.TrustVerified, &a.Version, &a.CreatedAt, &a.UpdatedAt, &a.InterviewSentAt, &a.OfferSentAt, &a.ConfirmedAt)
	if e != nil {
		return a, e
	}
	e = m.open(b, s.InstallationID+":club:"+id, &a.ClubData)
	return a, e
}
func (m *Module) clubList(ctx context.Context, s appkit.Scope, actor, status string, page, size int) ([]ClubApplication, int, error) {
	where := scoped
	args := scopeArgs(s)
	if actor != "" {
		where += " AND actor_id=?"
		args = append(args, actor)
	}
	if status != "" && status != "all" {
		where += " AND status=?"
		args = append(args, status)
	}
	var total int
	if e := m.rt.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM eid_club WHERE "+where, args...).Scan(&total); e != nil {
		return nil, 0, e
	}
	rows, e := m.rt.DB.QueryContext(ctx, "SELECT "+clubCols+" FROM eid_club WHERE "+where+" ORDER BY created_at DESC LIMIT ? OFFSET ?", append(args, size, (page-1)*size)...)
	if e != nil {
		return nil, 0, e
	}
	items := []ClubApplication{}
	for rows.Next() {
		var a ClubApplication
		var b []byte
		if e = rows.Scan(&a.ID, &a.ActorID, &b, &a.Status, &a.TrustSchemeID, &a.TrustVerified, &a.Version, &a.CreatedAt, &a.UpdatedAt, &a.InterviewSentAt, &a.OfferSentAt, &a.ConfirmedAt); e != nil {
			rows.Close()
			return nil, 0, e
		}
		if e = m.open(b, s.InstallationID+":club:"+a.ID, &a.ClubData); e != nil {
			rows.Close()
			return nil, 0, e
		}
		items = append(items, a)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, 0, e
	}
	for i := range items {
		items[i].History, e = m.events(ctx, s, items[i].ID)
		if e != nil {
			return nil, 0, e
		}
	}
	return items, total, nil
}
func (m *Module) clubOverview(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	cfg, e := m.settings(r.Context(), s)
	if e != nil {
		return e
	}
	page, size := pagination(r)
	items, total, e := m.clubList(r.Context(), s, s.ActorID, "", page, size)
	if e != nil {
		return e
	}
	verified := false
	if cfg.TrustSchemeID != "" {
		verified, e = trust.CheckQualification(r.Context(), m.rt, s, cfg.TrustSchemeID)
		if e != nil {
			return e
		}
	}
	appkit.JSON(w, 200, map[string]any{"items": items, "total": total, "page": page, "clubName": cfg.ClubName, "requireTrust": cfg.RequireTrust, "trustSchemeId": cfg.TrustSchemeID, "trustVerified": verified, "emailVerified": s.EmailVerified})
	return nil
}
func (m *Module) applyClub(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in struct {
		RealName string `json:"realName"`
	}
	if e := appkit.Decode(w, r, &in); e != nil {
		return e
	}
	name, e := appkit.Name(in.RealName, 50)
	if e != nil {
		return e
	}
	if !s.EmailVerified || s.Email == "" {
		return appkit.Invalid("请先在通行证验证邮箱，以接收笔试与录取通知")
	}
	cfg, e := m.settings(r.Context(), s)
	if e != nil {
		return e
	}
	verified := false
	if cfg.TrustSchemeID != "" {
		verified, e = trust.CheckQualification(r.Context(), m.rt, s, cfg.TrustSchemeID)
		if e != nil {
			return e
		}
	}
	if cfg.RequireTrust && !verified {
		return appkit.Forbidden("请先完成本项目要求的 Trust 认证；尚未配置方案时请联系项目管理员")
	}
	if e = m.ensureMember(r.Context(), s); e != nil {
		return e
	}
	id := appkit.NewID("eca_")
	b, e := m.seal(ClubData{RealName: name, ActorName: s.ActorName, Email: s.Email}, s.InstallationID+":club:"+id)
	if e != nil {
		return e
	}
	now := appkit.Now()
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		var n int
		if e := tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM eid_club WHERE "+scoped+" AND actor_id=? AND status!='rejected'", append(scopeArgs(s), s.ActorID)...).Scan(&n); e != nil {
			return e
		}
		if n > 0 {
			return appkit.Conflict("已有进行中的报名；拒绝后可以重新申请")
		}
		_, e := tx.ExecContext(r.Context(), "INSERT INTO eid_club(id,installation_id,tenant_id,project_id,actor_id,data,status,trust_scheme_id,trust_verified,created_at,updated_at) VALUES(?,?,?,?,?,?,'pending',?,?,?,?)", id, s.InstallationID, s.TenantID, s.ProjectID, s.ActorID, b, cfg.TrustSchemeID, verified, now, now)
		if e != nil {
			return e
		}
		return m.event(r.Context(), tx, s, id, "club.submit", "", "pending")
	})
	if e != nil {
		return e
	}
	a, e := m.club(r.Context(), s, id)
	if e != nil {
		return e
	}
	appkit.JSON(w, 201, a)
	return nil
}
func (m *Module) reviewClub(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	page, size := pagination(r)
	items, total, e := m.clubList(r.Context(), s, r.URL.Query().Get("actorId"), r.URL.Query().Get("status"), page, size)
	if e != nil {
		return e
	}
	rows, e := m.rt.DB.QueryContext(r.Context(), "SELECT status,COUNT(*) FROM eid_club WHERE "+scoped+" GROUP BY status", scopeArgs(s)...)
	if e != nil {
		return e
	}
	counts := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if e = rows.Scan(&status, &n); e != nil {
			rows.Close()
			return e
		}
		counts[status] = n
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"items": items, "total": total, "page": page, "counts": counts})
	return nil
}
func (m *Module) decideClub(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in struct {
		Version int64  `json:"version"`
		Status  string `json:"status"`
	}
	if e := appkit.Decode(w, r, &in); e != nil {
		return e
	}
	if in.Status != "rejected" {
		return appkit.Invalid("录取请使用发送录取通知操作")
	}
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	a, e := m.club(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	if a.Version != in.Version {
		return appkit.Conflict("记录已更新，请刷新")
	}
	if a.Status == "offer_confirmed" || a.Status == "rejected" {
		return appkit.Conflict("当前状态不能拒绝")
	}
	e = m.changeClub(r.Context(), s, a, "rejected", "club.reject", "")
	if e != nil {
		return e
	}
	out, e := m.club(r.Context(), s, a.ID)
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, out)
	return nil
}
func (m *Module) changeClub(ctx context.Context, s appkit.Scope, a ClubApplication, status, action, stamp string) error {
	return m.rt.Transaction(ctx, func(tx *sql.Tx) error {
		q := "UPDATE eid_club SET status=?,version=version+1,updated_at=?"
		args := []any{status, appkit.Now()}
		if stamp != "" {
			q += "," + stamp + "=?"
			args = append(args, appkit.Now())
		}
		q += " WHERE " + scoped + " AND id=? AND version=?"
		args = append(args, append(scopeArgs(s), a.ID, a.Version)...)
		result, e := tx.ExecContext(ctx, q, args...)
		if e != nil {
			return e
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return appkit.Conflict("记录已更新，请刷新")
		}
		return m.event(ctx, tx, s, a.ID, action, a.Status, status)
	})
}
func (m *Module) confirmOffer(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in struct {
		Version int64 `json:"version"`
	}
	if e := appkit.Decode(w, r, &in); e != nil {
		return e
	}
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	a, e := m.club(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	if a.ActorID != s.ActorID {
		return appkit.NotFound()
	}
	if a.Status == "offer_confirmed" {
		appkit.JSON(w, 200, a)
		return nil
	}
	if a.Status != "offer_sent" || a.Version != in.Version {
		return appkit.Conflict("录取尚未发出或记录已更新")
	}
	if e = m.changeClub(r.Context(), s, a, "offer_confirmed", "club.confirm", "confirmed_at"); e != nil {
		return e
	}
	out, e := m.club(r.Context(), s, a.ID)
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, out)
	return nil
}
func (m *Module) deleteClub(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	a, e := m.club(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		_, e := tx.ExecContext(r.Context(), "DELETE FROM eid_club WHERE "+scoped+" AND id=?", append(scopeArgs(s), a.ID)...)
		if e != nil {
			return e
		}
		return m.event(r.Context(), tx, s, a.ID, "club.delete", a.Status, "deleted")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 204, nil)
	return nil
}
