package platform

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *Store) Tenants(ctx context.Context, user string) ([]Tenant, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT t.id,t.name,m.role,t.created_at FROM tenants t JOIN members m ON m.tenant_id=t.id WHERE m.user_id=? ORDER BY t.created_at,t.id", user)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Tenant{}
	for rows.Next() {
		var t Tenant
		var n int64
		if e = rows.Scan(&t.ID, &t.Name, &t.Role, &n); e != nil {
			return nil, e
		}
		t.CreatedAt = fromStamp(n)
		out = append(out, t)
	}
	return out, rows.Err()
}
func (s *Store) CreateTenant(ctx context.Context, user, name string) (Tenant, error) {
	name, e := cleanName(name)
	if e != nil {
		return Tenant{}, e
	}
	t := Tenant{ID: newID("ten_"), Name: name, Role: "owner", CreatedAt: time.Now().UTC()}
	e = s.write(ctx, func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(ctx, "INSERT INTO tenants VALUES(?,?,?)", t.ID, t.Name, t.CreatedAt.UnixMilli()); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, "INSERT INTO members VALUES(?,?,?,?)", t.ID, user, "owner", t.CreatedAt.UnixMilli()); e != nil {
			return e
		}
		return audit(ctx, tx, user, t.ID, "", "tenant.created", t.ID, "Created team")
	})
	return t, e
}
func (s *Store) Members(ctx context.Context, user, tenant string) ([]Member, error) {
	if _, e := tenantRole(ctx, s.db, user, tenant); e != nil {
		return nil, e
	}
	rows, e := s.db.QueryContext(ctx, "SELECT m.user_id,u.display_name,m.role,m.joined_at FROM members m JOIN users u ON u.id=m.user_id WHERE tenant_id=? ORDER BY joined_at,user_id", tenant)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	return scanMembers(rows)
}
func scanMembers(rows *sql.Rows) ([]Member, error) {
	out := []Member{}
	for rows.Next() {
		var m Member
		var n int64
		if e := rows.Scan(&m.UserID, &m.DisplayName, &m.Role, &n); e != nil {
			return nil, e
		}
		m.JoinedAt = fromStamp(n)
		out = append(out, m)
	}
	return out, rows.Err()
}
func validRole(r string, owner bool) bool {
	return r == "admin" || r == "member" || r == "viewer" || (owner && r == "owner")
}
func (s *Store) ChangeMember(ctx context.Context, user, tenant, target, role string, remove bool) error {
	if !remove && !validRole(role, true) {
		return ErrInvalid
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		actorRole, e := tenantRole(ctx, tx, user, tenant)
		if e != nil {
			return e
		}
		if !admin(actorRole) {
			return ErrForbidden
		}
		old, e := tenantRole(ctx, tx, target, tenant)
		if e != nil {
			return e
		}
		if actorRole != "owner" && (old == "owner" || role == "owner") {
			return ErrForbidden
		}
		if old == "owner" && (remove || role != "owner") {
			var n int
			if e = tx.QueryRowContext(ctx, "SELECT count(*) FROM members WHERE tenant_id=? AND role='owner'", tenant).Scan(&n); e != nil {
				return e
			}
			if n <= 1 {
				return fmt.Errorf("%w: the final owner cannot be removed or demoted", ErrConflict)
			}
		}
		act := "member.role_changed"
		if remove {
			act = "member.removed"
			_, e = tx.ExecContext(ctx, "DELETE FROM members WHERE tenant_id=? AND user_id=?", tenant, target)
		} else {
			_, e = tx.ExecContext(ctx, "UPDATE members SET role=? WHERE tenant_id=? AND user_id=?", role, tenant, target)
		}
		if e != nil {
			return e
		}
		return audit(ctx, tx, user, tenant, "", act, target, "Updated team membership")
	})
}
func (s *Store) CreateInvitation(ctx context.Context, user, tenant, role string) (Invitation, error) {
	if !validRole(role, false) {
		return Invitation{}, ErrInvalid
	}
	v := Invitation{ID: newID("inv_"), Role: role, Token: newID(""), ExpiresAt: time.Now().UTC().Add(7 * 24 * time.Hour)}
	e := s.write(ctx, func(tx *sql.Tx) error {
		r, e := tenantRole(ctx, tx, user, tenant)
		if e != nil {
			return e
		}
		if !admin(r) {
			return ErrForbidden
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO invitations(id,tenant_id,token_hash,role,created_by,expires_at) VALUES(?,?,?,?,?,?)", v.ID, tenant, tokenHash(v.Token), role, user, v.ExpiresAt.UnixMilli())
		if e != nil {
			return e
		}
		return audit(ctx, tx, user, tenant, "", "invitation.created", v.ID, "Created single-use invitation")
	})
	if e != nil {
		return Invitation{}, e
	}
	return v, nil
}
func (s *Store) Invitations(ctx context.Context, user, tenant string) ([]Invitation, error) {
	r, e := tenantRole(ctx, s.db, user, tenant)
	if e != nil {
		return nil, e
	}
	if !admin(r) {
		return nil, ErrForbidden
	}
	rows, e := s.db.QueryContext(ctx, "SELECT id,role,expires_at FROM invitations WHERE tenant_id=? AND used_at IS NULL AND revoked_at IS NULL AND expires_at>? ORDER BY expires_at", tenant, stamp())
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Invitation{}
	for rows.Next() {
		var v Invitation
		var n int64
		if e = rows.Scan(&v.ID, &v.Role, &n); e != nil {
			return nil, e
		}
		v.ExpiresAt = fromStamp(n)
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) RevokeInvitation(ctx context.Context, user, tenant, id string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		r, e := tenantRole(ctx, tx, user, tenant)
		if e != nil {
			return e
		}
		if !admin(r) {
			return ErrForbidden
		}
		res, e := tx.ExecContext(ctx, "UPDATE invitations SET revoked_at=? WHERE id=? AND tenant_id=? AND used_at IS NULL AND revoked_at IS NULL", stamp(), id, tenant)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrNotFound
		}
		return audit(ctx, tx, user, tenant, "", "invitation.revoked", id, "Revoked invitation")
	})
}
func (s *Store) AcceptInvitation(ctx context.Context, user, token string) (Tenant, error) {
	var t Tenant
	e := s.write(ctx, func(tx *sql.Tx) error {
		var id, role, tenant string
		var exp int64
		e := tx.QueryRowContext(ctx, "SELECT id,tenant_id,role,expires_at FROM invitations WHERE token_hash=? AND used_at IS NULL AND revoked_at IS NULL", tokenHash(token)).Scan(&id, &tenant, &role, &exp)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrNotFound
		}
		if e != nil {
			return e
		}
		if exp <= stamp() {
			return ErrNotFound
		}
		// An invitation grants initial membership only; it never elevates an existing member.
		_, e = tx.ExecContext(ctx, "INSERT INTO members(tenant_id,user_id,role,joined_at) VALUES(?,?,?,?) ON CONFLICT(tenant_id,user_id) DO NOTHING", tenant, user, role, stamp())
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE invitations SET used_at=? WHERE id=?", stamp(), id); e != nil {
			return e
		}
		var n int64
		if e = tx.QueryRowContext(ctx, "SELECT t.id,t.name,m.role,t.created_at FROM tenants t JOIN members m ON m.tenant_id=t.id WHERE t.id=? AND m.user_id=?", tenant, user).Scan(&t.ID, &t.Name, &t.Role, &n); e != nil {
			return e
		}
		t.CreatedAt = fromStamp(n)
		return audit(ctx, tx, user, tenant, "", "invitation.accepted", id, "Joined team by invitation")
	})
	return t, e
}
func (s *Store) Projects(ctx context.Context, user, tenant string) ([]Project, error) {
	r, e := tenantRole(ctx, s.db, user, tenant)
	if e != nil {
		return nil, e
	}
	var rows *sql.Rows
	if admin(r) {
		rows, e = s.db.QueryContext(ctx, "SELECT id,tenant_id,name,'admin',created_at FROM projects WHERE tenant_id=? ORDER BY created_at,id", tenant)
	} else {
		rows, e = s.db.QueryContext(ctx, "SELECT p.id,p.tenant_id,p.name,CASE WHEN ?='viewer' THEN 'viewer' ELSE m.role END,p.created_at FROM projects p JOIN project_members m ON p.id=m.project_id WHERE p.tenant_id=? AND m.user_id=? ORDER BY p.created_at,p.id", r, tenant, user)
	}
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		var p Project
		var n int64
		if e = rows.Scan(&p.ID, &p.TenantID, &p.Name, &p.Role, &n); e != nil {
			return nil, e
		}
		p.CreatedAt = fromStamp(n)
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) CreateProject(ctx context.Context, user, tenant, name string) (Project, error) {
	name, e := cleanName(name)
	if e != nil {
		return Project{}, e
	}
	p := Project{ID: newID("prj_"), TenantID: tenant, Name: name, Role: "admin", CreatedAt: time.Now().UTC()}
	e = s.write(ctx, func(tx *sql.Tx) error {
		r, e := tenantRole(ctx, tx, user, tenant)
		if e != nil {
			return e
		}
		if !admin(r) {
			return ErrForbidden
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO projects VALUES(?,?,?,?)", p.ID, tenant, name, p.CreatedAt.UnixMilli())
		if e != nil {
			return e
		}
		return audit(ctx, tx, user, tenant, p.ID, "project.created", p.ID, "Created project")
	})
	return p, e
}
func (s *Store) ProjectMembers(ctx context.Context, user, tenant, project string) ([]Member, error) {
	if e := requireProject(ctx, s.db, user, tenant, project, "read"); e != nil {
		return nil, e
	}
	rows, e := s.db.QueryContext(ctx, "SELECT pm.user_id,u.display_name,pm.role,pm.joined_at FROM project_members pm JOIN users u ON u.id=pm.user_id WHERE pm.tenant_id=? AND pm.project_id=? ORDER BY pm.joined_at", tenant, project)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	return scanMembers(rows)
}
func (s *Store) SetProjectMember(ctx context.Context, user, tenant, project, target, role string, remove bool) error {
	if !remove && !validRole(role, false) {
		return ErrInvalid
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if e := requireProject(ctx, tx, user, tenant, project, "admin"); e != nil {
			return e
		}
		targetRole, e := tenantRole(ctx, tx, target, tenant)
		if e != nil {
			return e
		}
		if !remove && targetRole == "viewer" && role != "viewer" {
			return ErrInvalid
		}
		act := "project.member_granted"
		if remove {
			act = "project.member_removed"
			_, e = tx.ExecContext(ctx, "DELETE FROM project_members WHERE project_id=? AND user_id=?", project, target)
			if e == nil {
				_, e = tx.ExecContext(ctx, "DELETE FROM app_grants WHERE tenant_id=? AND project_id=? AND user_id=?", tenant, project, target)
			}
		} else {
			_, e = tx.ExecContext(ctx, "INSERT INTO project_members VALUES(?,?,?,?,?) ON CONFLICT(project_id,user_id) DO UPDATE SET role=excluded.role", tenant, project, target, role, stamp())
		}
		if e != nil {
			return e
		}
		return audit(ctx, tx, user, tenant, project, act, target, "Updated project membership")
	})
}
func (s *Store) Audit(ctx context.Context, user, tenant, project string, limit int, before ...string) ([]AuditEvent, error) {
	r, e := tenantRole(ctx, s.db, user, tenant)
	if e != nil {
		return nil, e
	}
	if !admin(r) {
		if project == "" {
			return nil, ErrForbidden
		}
		if e = requireProject(ctx, s.db, user, tenant, project, "admin"); e != nil {
			return nil, e
		}
	} else if project != "" {
		if e = requireProject(ctx, s.db, user, tenant, project, "read"); e != nil {
			return nil, e
		}
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	q := "SELECT id,actor_id,action,target_id,project_id,created_at,summary FROM audit WHERE tenant_id=?"
	args := []any{tenant}
	if project != "" {
		q += " AND project_id=?"
		args = append(args, project)
	}
	if len(before) > 0 && before[0] != "" {
		var ts int64
		cursorQuery := "SELECT created_at FROM audit WHERE id=? AND tenant_id=?"
		cursorArgs := []any{before[0], tenant}
		if project != "" {
			cursorQuery += " AND project_id=?"
			cursorArgs = append(cursorArgs, project)
		}
		if e = s.db.QueryRowContext(ctx, cursorQuery, cursorArgs...).Scan(&ts); errors.Is(e, sql.ErrNoRows) {
			return nil, ErrNotFound
		} else if e != nil {
			return nil, e
		}
		q += " AND (created_at<? OR (created_at=? AND id<?))"
		args = append(args, ts, ts, before[0])
	}
	q += " ORDER BY created_at DESC,id DESC LIMIT ?"
	args = append(args, limit)
	rows, e := s.db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []AuditEvent{}
	for rows.Next() {
		var v AuditEvent
		var n int64
		if e = rows.Scan(&v.ID, &v.ActorID, &v.Action, &v.TargetID, &v.ProjectID, &n, &v.Summary); e != nil {
			return nil, e
		}
		v.CreatedAt = fromStamp(n)
		out = append(out, v)
	}
	return out, rows.Err()
}
