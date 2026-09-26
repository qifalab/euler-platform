package eid

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

type ProfileData struct {
	Email         string `json:"email"`
	EmailVerified bool   `json:"emailVerified"`
	Bio           string `json:"bio"`
	Phone         string `json:"phone"`
	Avatar        string `json:"avatar"`
}
type Member struct {
	ActorID string `json:"actorId"`
	Name    string `json:"name"`
	ProfileData
	IdentityLevel int    `json:"identityLevel"`
	IdentityTitle string `json:"identityTitle"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

func (m *Module) ensureMember(ctx context.Context, s appkit.Scope) error {
	return m.rt.Transaction(ctx, func(tx *sql.Tx) error {
		return m.ensureMemberTx(ctx, tx, s)
	})
}

// Refresh identity-provider fields without overwriting concurrent profile edits.
// Every read and write must retain the same database transaction.
func (m *Module) ensureMemberTx(ctx context.Context, tx *sql.Tx, s appkit.Scope) error {
	var b []byte
	err := tx.QueryRowContext(ctx, "SELECT data FROM eid_members WHERE "+scoped+" AND actor_id=?", append(scopeArgs(s), s.ActorID)...).Scan(&b)
	p := ProfileData{Email: s.Email, EmailVerified: s.EmailVerified}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		if e := m.open(b, s.InstallationID+":member:"+s.ActorID, &p); e != nil {
			return e
		}
		p.Email = s.Email
		p.EmailVerified = s.EmailVerified
	}
	enc, err := m.seal(p, s.InstallationID+":member:"+s.ActorID)
	if err != nil {
		return err
	}
	now := appkit.Now()
	_, err = tx.ExecContext(ctx, "INSERT INTO eid_members(installation_id,tenant_id,project_id,actor_id,display_name,data,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(installation_id,actor_id) DO UPDATE SET display_name=excluded.display_name,data=excluded.data,updated_at=excluded.updated_at", s.InstallationID, s.TenantID, s.ProjectID, s.ActorID, s.ActorName, enc, now, now)
	return err
}

type memberQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (m *Module) member(ctx context.Context, s appkit.Scope, id string) (Member, error) {
	return m.memberWith(ctx, m.rt.DB, s, id)
}
func (m *Module) memberWith(ctx context.Context, q memberQuerier, s appkit.Scope, id string) (Member, error) {
	var p Member
	var b []byte
	err := q.QueryRowContext(ctx, "SELECT actor_id,display_name,data,identity_level,identity_title,created_at,updated_at FROM eid_members WHERE "+scoped+" AND actor_id=?", append(scopeArgs(s), id)...).Scan(&p.ActorID, &p.Name, &b, &p.IdentityLevel, &p.IdentityTitle, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	err = m.open(b, s.InstallationID+":member:"+id, &p.ProfileData)
	return p, err
}
func (m *Module) profile(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	if err := m.ensureMember(r.Context(), s); err != nil {
		return err
	}
	p, err := m.member(r.Context(), s, s.ActorID)
	if err != nil {
		return err
	}
	appkit.JSON(w, 200, p)
	return nil
}
func validateProfile(p *ProfileData) error {
	var err error
	p.Bio, err = clean(p.Bio, 2000)
	if err != nil {
		return err
	}
	p.Phone, err = clean(p.Phone, 40)
	if err != nil {
		return err
	}
	p.Avatar, err = clean(p.Avatar, 2048)
	if err != nil {
		return err
	}
	if p.Avatar != "" {
		u, e := url.Parse(p.Avatar)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return appkit.Invalid("头像必须是 HTTPS 图片地址")
		}
	}
	return nil
}
func (m *Module) updateProfile(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in struct {
		Bio    string `json:"bio"`
		Phone  string `json:"phone"`
		Avatar string `json:"avatar"`
	}
	if err := appkit.Decode(w, r, &in); err != nil {
		return err
	}
	var p Member
	err := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		if e := m.ensureMemberTx(r.Context(), tx, s); e != nil {
			return e
		}
		var e error
		p, e = m.memberWith(r.Context(), tx, s, s.ActorID)
		if e != nil {
			return e
		}
		p.Bio = in.Bio
		p.Phone = in.Phone
		p.Avatar = in.Avatar
		if e = validateProfile(&p.ProfileData); e != nil {
			return e
		}
		enc, e := m.seal(p.ProfileData, s.InstallationID+":member:"+s.ActorID)
		if e != nil {
			return e
		}
		p.UpdatedAt = appkit.Now()
		_, e = tx.ExecContext(r.Context(), "UPDATE eid_members SET data=?,updated_at=? WHERE "+scoped+" AND actor_id=?", append([]any{enc, p.UpdatedAt}, append(scopeArgs(s), s.ActorID)...)...)
		if e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "profile.update", s.ActorID, "更新本人会员资料")
	})
	if err != nil {
		return err
	}
	appkit.JSON(w, 200, p)
	return nil
}
func pagination(r *http.Request) (int, int) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	size, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	if size < 1 || size > 100 {
		size = 25
	}
	return page, size
}
func (m *Module) members(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	page, size := pagination(r)
	q := "%" + strings.TrimSpace(r.URL.Query().Get("search")) + "%"
	args := append(scopeArgs(s), q, q)
	where := scoped + " AND (display_name LIKE ? OR actor_id LIKE ?)"
	var total int
	if err := m.rt.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM eid_members WHERE "+where, args...).Scan(&total); err != nil {
		return err
	}
	rows, err := m.rt.DB.QueryContext(r.Context(), "SELECT actor_id,display_name,data,identity_level,identity_title,created_at,updated_at FROM eid_members WHERE "+where+" ORDER BY created_at DESC LIMIT ? OFFSET ?", append(args, size, (page-1)*size)...)
	if err != nil {
		return err
	}
	items := []Member{}
	for rows.Next() {
		var p Member
		var b []byte
		if err = rows.Scan(&p.ActorID, &p.Name, &b, &p.IdentityLevel, &p.IdentityTitle, &p.CreatedAt, &p.UpdatedAt); err != nil {
			rows.Close()
			return err
		}
		if err = m.open(b, s.InstallationID+":member:"+p.ActorID, &p.ProfileData); err != nil {
			rows.Close()
			return err
		}
		items = append(items, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	appkit.JSON(w, 200, map[string]any{"items": items, "total": total, "page": page})
	return nil
}
func (m *Module) editMember(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in struct {
		Bio           string `json:"bio"`
		Phone         string `json:"phone"`
		Avatar        string `json:"avatar"`
		IdentityLevel int    `json:"identityLevel"`
		IdentityTitle string `json:"identityTitle"`
	}
	if err := appkit.Decode(w, r, &in); err != nil {
		return err
	}
	if in.IdentityLevel < 0 || in.IdentityLevel > 6 {
		return appkit.Invalid("无效的成员资格类型")
	}
	title, err := clean(in.IdentityTitle, 100)
	if err != nil {
		return err
	}
	err = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		p, e := m.memberWith(r.Context(), tx, s, r.PathValue("actorID"))
		if e != nil {
			return e
		}
		if in.IdentityLevel != p.IdentityLevel || title != p.IdentityTitle {
			if e = s.Require("review"); e != nil {
				return e
			}
		}
		p.Bio = in.Bio
		p.Phone = in.Phone
		p.Avatar = in.Avatar
		if e = validateProfile(&p.ProfileData); e != nil {
			return e
		}
		enc, e := m.seal(p.ProfileData, s.InstallationID+":member:"+p.ActorID)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(r.Context(), "UPDATE eid_members SET data=?,identity_level=?,identity_title=?,updated_at=? WHERE "+scoped+" AND actor_id=?", append([]any{enc, in.IdentityLevel, title, appkit.Now()}, append(scopeArgs(s), p.ActorID)...)...)
		if e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "member.update", p.ActorID, "运营管理员更新会员业务资料")
	})
	if err != nil {
		return err
	}
	appkit.JSON(w, 200, map[string]bool{"updated": true})
	return nil
}
func (m *Module) deleteMember(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	id := r.PathValue("actorID")
	if _, err := m.member(r.Context(), s, id); err != nil {
		return err
	}
	err := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		for _, table := range []string{"eid_verifications", "eid_club", "eid_members"} {
			if _, e := tx.ExecContext(r.Context(), "DELETE FROM "+table+" WHERE "+scoped+" AND actor_id=?", append(scopeArgs(s), id)...); e != nil {
				return e
			}
		}
		return m.rt.Audit(r.Context(), tx, s, "member.delete", id, "删除本应用会员业务记录，保留平台账号与审计")
	})
	if err != nil {
		return err
	}
	appkit.JSON(w, 204, nil)
	return nil
}
