package lottery

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/weauth"
)

type signupPolicy struct {
	RequireLogin  bool   `json:"requireLogin"`
	TrustSchemeID string `json:"trustSchemeId"`
	RequireEID    bool   `json:"requireEid"`
	WeAuthSiteID  string `json:"weauthSiteId"`
	Sitekey       string `json:"sitekey,omitempty"`
}

func policy(ctx context.Context, q queryer, id string) (signupPolicy, error) {
	var p signupPolicy
	err := q.QueryRowContext(ctx, "SELECT require_login,trust_scheme_id,require_eid,weauth_site_id FROM lottery_signup_policies WHERE room_id=?", id).Scan(&p.RequireLogin, &p.TrustSchemeID, &p.RequireEID, &p.WeAuthSiteID)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return p, err
}
func (m *Module) getSignupPolicy(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	room, err := scopedRoom(r.Context(), m.rt.DB, s, r.PathValue("id"))
	if err != nil {
		return err
	}
	p, err := policy(r.Context(), m.rt.DB, room.ID)
	if err != nil {
		return err
	}
	if p.WeAuthSiteID != "" {
		if err = m.rt.DB.QueryRowContext(r.Context(), "SELECT sitekey FROM weauth_sites WHERE id=? AND tenant_id=? AND project_id=?", p.WeAuthSiteID, s.TenantID, s.ProjectID).Scan(&p.Sitekey); err != nil {
			return err
		}
	}
	appkit.JSON(w, 200, p)
	return nil
}
func (m *Module) saveSignupPolicy(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var p signupPolicy
	if err := appkit.Decode(w, r, &p); err != nil {
		return err
	}
	if p.TrustSchemeID != "" || p.RequireEID {
		p.RequireLogin = true
	}
	p.Sitekey = ""
	err := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		room, err := scopedRoom(r.Context(), tx, s, r.PathValue("id"))
		if err != nil {
			return err
		}
		check := func(app, query, id string) error {
			var n int
			if err := tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM installations WHERE tenant_id=? AND project_id=? AND application_id=? AND status='enabled'", s.TenantID, s.ProjectID, app).Scan(&n); err != nil {
				return err
			}
			if n != 1 {
				return appkit.Invalid("请先启用资格或人机验证应用")
			}
			if query != "" {
				if err := tx.QueryRowContext(r.Context(), query, id, s.TenantID, s.ProjectID).Scan(&n); err != nil {
					return err
				}
				if n != 1 {
					return appkit.Invalid("所选方案或站点不属于当前项目")
				}
			}
			return nil
		}
		if p.TrustSchemeID != "" {
			if err = check("trust", "SELECT COUNT(*) FROM trust_schemes WHERE id=? AND tenant_id=? AND project_id=? AND status='active'", p.TrustSchemeID); err != nil {
				return err
			}
		}
		if p.RequireEID {
			if err = check("eid", "", ""); err != nil {
				return err
			}
		}
		if p.WeAuthSiteID != "" {
			if err = check("weauth", "SELECT COUNT(*) FROM weauth_sites WHERE id=? AND tenant_id=? AND project_id=?", p.WeAuthSiteID); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(r.Context(), "INSERT INTO lottery_signup_policies(room_id,require_login,trust_scheme_id,require_eid,weauth_site_id) VALUES(?,?,?,?,?) ON CONFLICT(room_id) DO UPDATE SET require_login=excluded.require_login,trust_scheme_id=excluded.trust_scheme_id,require_eid=excluded.require_eid,weauth_site_id=excluded.weauth_site_id", room.ID, p.RequireLogin, p.TrustSchemeID, p.RequireEID, p.WeAuthSiteID)
		if err != nil {
			return err
		}
		return m.rt.Audit(r.Context(), tx, s, "signup.policy", room.ID, "更新活动报名资格与人机验证要求")
	})
	if err == nil {
		appkit.JSON(w, 200, p)
	}
	return err
}

type signupInput struct {
	Name        string `json:"name"`
	Department  string `json:"department"`
	WeAuthToken string `json:"weauthToken"`
}

func (m *Module) checkEligibility(ctx context.Context, tx *sql.Tx, s appkit.Scope, p signupPolicy) error {
	enabled := func(app string) error {
		var yes bool
		err := tx.QueryRowContext(ctx, "SELECT status='enabled' FROM installations WHERE tenant_id=? AND project_id=? AND application_id=?", s.TenantID, s.ProjectID, app).Scan(&yes)
		if err != nil || !yes {
			return appkit.Forbidden("报名资格来源应用已停用")
		}
		return nil
	}
	if p.TrustSchemeID != "" {
		if err := enabled("trust"); err != nil {
			return err
		}
		var n int
		err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM trust_submissions s JOIN trust_schemes c ON c.id=s.scheme_id WHERE s.tenant_id=? AND s.project_id=? AND s.actor_id=? AND s.scheme_id=? AND s.status='approved' AND c.status='active'", s.TenantID, s.ProjectID, s.ActorID, p.TrustSchemeID).Scan(&n)
		if err != nil {
			return err
		}
		if n == 0 {
			return appkit.Forbidden("请先通过活动要求的 Trust 认证")
		}
	}
	if p.RequireEID {
		if err := enabled("eid"); err != nil {
			return err
		}
		var n int
		err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM eid_club WHERE tenant_id=? AND project_id=? AND actor_id=? AND status='offer_confirmed'", s.TenantID, s.ProjectID, s.ActorID).Scan(&n)
		if err != nil {
			return err
		}
		if n == 0 {
			return appkit.Forbidden("本活动要求已确认的 EID 成员资格")
		}
	}
	return nil
}
func (m *Module) authenticatedSignup(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in signupInput
	if err := appkit.DecodeLimit(w, r, &in, 4096); err != nil {
		return err
	}
	person := participantInput{Name: in.Name, Department: in.Department}
	if err := validateParticipant(&person, true); err != nil {
		return err
	}
	var out participant
	err := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		v, err := scopedRoom(r.Context(), tx, s, r.PathValue("id"))
		if err != nil {
			return err
		}
		if v.Status != "open" {
			return appkit.Conflict("活动已暂停报名")
		}
		p, err := policy(r.Context(), tx, v.ID)
		if err != nil {
			return err
		}
		if err = m.checkEligibility(r.Context(), tx, s, p); err != nil {
			return err
		}
		var exists int
		if err = tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM lottery_identities WHERE room_id=? AND actor_id=?", v.ID, s.ActorID).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			return appkit.Conflict("您已报名此活动")
		}
		if p.WeAuthSiteID != "" {
			if err = weauth.ConsumeForApplication(r.Context(), tx, m.rt, r, s.TenantID, s.ProjectID, p.WeAuthSiteID, "lottery:"+v.ID, in.WeAuthToken); err != nil {
				return err
			}
		}
		out, err = insertParticipant(r.Context(), tx, v.ID, person, "authenticated", "")
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(r.Context(), "INSERT INTO lottery_identities(participant_id,room_id,actor_id) VALUES(?,?,?)", out.ID, v.ID, s.ActorID)
		if err != nil {
			return err
		}
		return m.rt.Audit(r.Context(), tx, s, "participant.self_registered", out.ID, "通过当前资格及防刷校验完成实名绑定报名")
	})
	if err == nil {
		appkit.JSON(w, 201, out)
	}
	return err
}
