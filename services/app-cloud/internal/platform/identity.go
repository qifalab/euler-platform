package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/identity"
)

var _ identity.SessionStore = (*Store)(nil)

func (s *Store) UpsertIdentity(ctx context.Context, v identity.ExternalIdentity) (identity.Principal, error) {
	if strings.TrimSpace(v.Provider) == "" || strings.TrimSpace(v.Subject) == "" {
		return identity.Principal{}, ErrInvalid
	}
	var p identity.Principal
	e := s.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `INSERT INTO users(id,provider,subject,display_name,email,email_verified) VALUES(?,?,?,?,?,?) ON CONFLICT(provider,subject) DO UPDATE SET display_name=excluded.display_name,email=excluded.email,email_verified=excluded.email_verified`, newID("usr_"), v.Provider, v.Subject, v.Name, v.Email, v.EmailVerified)
		if e != nil {
			return e
		}
		return tx.QueryRowContext(ctx, "SELECT id,provider,subject,display_name,email,email_verified FROM users WHERE provider=? AND subject=?", v.Provider, v.Subject).Scan(&p.ID, &p.Provider, &p.Subject, &p.Name, &p.Email, &p.EmailVerified)
	})
	return p, e
}
func (s *Store) GetPrincipal(ctx context.Context, id string) (identity.Principal, error) {
	var p identity.Principal
	e := s.db.QueryRowContext(ctx, "SELECT id,provider,subject,display_name,email,email_verified FROM users WHERE id=?", id).Scan(&p.ID, &p.Provider, &p.Subject, &p.Name, &p.Email, &p.EmailVerified)
	if errors.Is(e, sql.ErrNoRows) {
		e = identity.ErrNotFound
	}
	return p, e
}
func (s *Store) PutSession(ctx context.Context, v identity.Session) error {
	if v.TokenHash == "" || v.CSRFToken == "" || !v.ExpiresAt.After(time.Now()) {
		return ErrInvalid
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at<=?", stamp()); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, "INSERT INTO sessions(token_hash,user_id,csrf_token,expires_at) VALUES(?,?,?,?)", v.TokenHash, v.UserID, v.CSRFToken, v.ExpiresAt.UnixMilli())
		return e
	})
}
func (s *Store) GetSession(ctx context.Context, hash string) (identity.Session, error) {
	var v identity.Session
	var n int64
	e := s.db.QueryRowContext(ctx, "SELECT token_hash,user_id,csrf_token,expires_at FROM sessions WHERE token_hash=? AND expires_at>?", hash, stamp()).Scan(&v.TokenHash, &v.UserID, &v.CSRFToken, &n)
	if errors.Is(e, sql.ErrNoRows) {
		e = identity.ErrNotFound
	}
	v.ExpiresAt = fromStamp(n)
	return v, e
}
func (s *Store) DeleteSession(ctx context.Context, hash string) error {
	_, e := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash=?", hash)
	return e
}
func (s *Store) PutAuthFlow(ctx context.Context, v identity.AuthFlow) error {
	if v.StateHash == "" || !v.ExpiresAt.After(time.Now()) {
		return ErrInvalid
	}
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	enc, e := s.encrypt(string(b), "authflow:"+v.StateHash)
	if e != nil {
		return e
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(ctx, "DELETE FROM auth_flows WHERE expires_at<=?", stamp()); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, "INSERT INTO auth_flows VALUES(?,?,?)", v.StateHash, enc, v.ExpiresAt.UnixMilli())
		return e
	})
}
func (s *Store) ConsumeAuthFlow(ctx context.Context, hash string) (identity.AuthFlow, error) {
	var flow identity.AuthFlow
	var data []byte
	var exp int64
	e := s.write(ctx, func(tx *sql.Tx) error {
		e := tx.QueryRowContext(ctx, "SELECT payload,expires_at FROM auth_flows WHERE state_hash=?", hash).Scan(&data, &exp)
		if errors.Is(e, sql.ErrNoRows) {
			return identity.ErrNotFound
		}
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "DELETE FROM auth_flows WHERE state_hash=?", hash)
		return e
	})
	if e != nil {
		return flow, e
	}
	if exp <= stamp() {
		return flow, identity.ErrNotFound
	}
	plain, e := s.decrypt(data, "authflow:"+hash)
	if e != nil {
		return flow, fmt.Errorf("read authentication flow: %w", e)
	}
	e = json.Unmarshal([]byte(plain), &flow)
	return flow, e
}
