package platform

import (
	"context"
	"database/sql"
	"errors"
)

// BeginResourceOperation commits the authorization decision before contacting
// an external product. Pending records survive crashes and can be reconciled;
// an ambiguous network result is never reported as a fabricated local success.
func (s *Store) BeginResourceOperation(ctx context.Context, user, tenant, project, id, action, resource string) (string, error) {
	if action != "create" && action != "delete" {
		return "", ErrInvalid
	}
	op := newID("op_")
	err := s.write(ctx, func(tx *sql.Tx) error {
		if e := requireProject(ctx, tx, user, tenant, project, "write"); e != nil {
			return e
		}
		v, e := installation(ctx, tx, tenant, project, id)
		if e != nil {
			return e
		}
		if v.Status != "enabled" {
			return ErrConflict
		}
		if action == "delete" {
			var existing string
			if e = tx.QueryRowContext(ctx, "SELECT resource_id FROM resource_bindings WHERE installation_id=? AND resource_id=?", id, resource).Scan(&existing); errors.Is(e, sql.ErrNoRows) {
				return ErrNotFound
			} else if e != nil {
				return e
			}
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO operations VALUES(?,?,?,?,?,?,?,?,?)", op, tenant, project, id, user, action, resource, "pending", stamp())
		if e != nil {
			return e
		}
		return audit(ctx, tx, user, tenant, project, "resource."+action+"_requested", op, "Authorized remote resource operation")
	})
	return op, err
}

// CompleteResourceOperation records an operation authorized earlier. It does
// not grant a new operation after revocation and must use an independent short
// context so a client disconnect cannot discard an already-real upstream result.
func (s *Store) CompleteResourceOperation(ctx context.Context, op string, result *Resource) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var tenant, project, id, user, action, resource, status string
		e := tx.QueryRowContext(ctx, "SELECT tenant_id,project_id,installation_id,actor_id,action,resource_id,status FROM operations WHERE id=?", op).Scan(&tenant, &project, &id, &user, &action, &resource, &status)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrNotFound
		}
		if e != nil {
			return e
		}
		if status == "completed" {
			return nil
		}
		if status != "pending" {
			return ErrConflict
		}
		if action == "create" {
			if result == nil || result.ID == "" {
				return ErrInvalid
			}
			resource = result.ID
			_, e = tx.ExecContext(ctx, "INSERT INTO resource_bindings VALUES(?,?,?,?,?,?) ON CONFLICT(installation_id,resource_id) DO UPDATE SET name=excluded.name,type=excluded.type,status=excluded.status,url=excluded.url", id, result.ID, result.Name, result.Type, result.Status, result.URL)
		} else {
			_, e = tx.ExecContext(ctx, "DELETE FROM resource_bindings WHERE installation_id=? AND resource_id=?", id, resource)
		}
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE operations SET status='completed',resource_id=? WHERE id=?", resource, op); e != nil {
			return e
		}
		return audit(ctx, tx, user, tenant, project, "resource."+action+"_completed", resource, "Recorded confirmed remote operation result")
	})
}
func (s *Store) MarkOperationUncertain(ctx context.Context, op string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var tenant, project, user string
		e := tx.QueryRowContext(ctx, "SELECT tenant_id,project_id,actor_id FROM operations WHERE id=? AND status='pending'", op).Scan(&tenant, &project, &user)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrNotFound
		}
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE operations SET status='uncertain' WHERE id=?", op); e != nil {
			return e
		}
		return audit(ctx, tx, user, tenant, project, "resource.operation_uncertain", op, "Upstream result not confirmed; inspect the product before retrying")
	})
}
