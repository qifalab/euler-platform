package platform

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	aead cipher.AEAD
	key  []byte
}

// Open uses one serialized SQLite connection, foreign keys and durable WAL.
// The supplied key must persist independently of the database and its backups.
func Open(path string, key []byte) (*Store, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must contain exactly 32 bytes")
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("database path is required")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if err = f.Close(); err != nil {
			return nil, err
		}
		if err = os.Chmod(path, 0600); err != nil {
			return nil, err
		}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	dsn := path
	if path != ":memory:" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		dsn = (&url.URL{Scheme: "file", Path: abs}).String()
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, aead: aead, key: append([]byte(nil), key...)}
	for _, q := range []string{"PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL"} {
		if _, err = db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err = s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.checkKey(); err != nil {
		db.Close()
		return nil, err
	}
	if path != ":memory:" {
		if err = os.Chmod(path, 0600); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}
func (s *Store) Close() error                   { return s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
func (s *Store) migrate() error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var version int
	if e = tx.QueryRow("PRAGMA user_version").Scan(&version); e != nil {
		return e
	}
	if version > 1 {
		return errors.New("database schema is newer than this server")
	}
	_, e = tx.Exec(`
CREATE TABLE IF NOT EXISTS platform_metadata(key TEXT PRIMARY KEY,value BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS users(id TEXT PRIMARY KEY,provider TEXT NOT NULL,subject TEXT NOT NULL,display_name TEXT NOT NULL,email TEXT NOT NULL DEFAULT '',email_verified INTEGER NOT NULL DEFAULT 0,UNIQUE(provider,subject));
CREATE TABLE IF NOT EXISTS sessions(token_hash TEXT PRIMARY KEY,user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,csrf_token TEXT NOT NULL,expires_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS sessions_expiry ON sessions(expires_at);
CREATE TABLE IF NOT EXISTS auth_flows(state_hash TEXT PRIMARY KEY,payload BLOB NOT NULL,expires_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS auth_flows_expiry ON auth_flows(expires_at);
CREATE TABLE IF NOT EXISTS tenants(id TEXT PRIMARY KEY,name TEXT NOT NULL,created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS members(tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,role TEXT NOT NULL CHECK(role IN ('owner','admin','member','viewer')),joined_at INTEGER NOT NULL,PRIMARY KEY(tenant_id,user_id));
CREATE TABLE IF NOT EXISTS projects(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,name TEXT NOT NULL,created_at INTEGER NOT NULL,UNIQUE(tenant_id,id));
CREATE TABLE IF NOT EXISTS project_members(tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,user_id TEXT NOT NULL,role TEXT NOT NULL CHECK(role IN ('admin','member','viewer')),joined_at INTEGER NOT NULL,PRIMARY KEY(project_id,user_id),FOREIGN KEY(tenant_id,project_id) REFERENCES projects(tenant_id,id) ON DELETE CASCADE,FOREIGN KEY(tenant_id,user_id) REFERENCES members(tenant_id,user_id) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS invitations(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,token_hash TEXT NOT NULL UNIQUE,role TEXT NOT NULL CHECK(role IN ('admin','member','viewer')),created_by TEXT NOT NULL REFERENCES users(id),expires_at INTEGER NOT NULL,used_at INTEGER,revoked_at INTEGER);
CREATE TABLE IF NOT EXISTS installations(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,application_id TEXT NOT NULL,status TEXT NOT NULL CHECK(status IN ('enabled','disabled')),created_at INTEGER NOT NULL,UNIQUE(project_id,application_id),FOREIGN KEY(tenant_id,project_id) REFERENCES projects(tenant_id,id) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS connections(installation_id TEXT PRIMARY KEY REFERENCES installations(id) ON DELETE CASCADE,application_id TEXT NOT NULL,base_url TEXT NOT NULL,credential BLOB NOT NULL,credential_fingerprint TEXT,external_account_id TEXT,updated_at INTEGER NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS connection_credential_unique ON connections(application_id,credential_fingerprint) WHERE credential_fingerprint IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS connection_owner_unique ON connections(application_id,base_url,external_account_id) WHERE external_account_id IS NOT NULL;
CREATE TABLE IF NOT EXISTS resource_bindings(installation_id TEXT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,resource_id TEXT NOT NULL,name TEXT NOT NULL,type TEXT NOT NULL,status TEXT NOT NULL DEFAULT '',url TEXT NOT NULL DEFAULT '',PRIMARY KEY(installation_id,resource_id));
CREATE TABLE IF NOT EXISTS operations(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,installation_id TEXT NOT NULL REFERENCES installations(id),actor_id TEXT NOT NULL,action TEXT NOT NULL,resource_id TEXT NOT NULL DEFAULT '',status TEXT NOT NULL CHECK(status IN ('pending','completed','uncertain')),created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS audit(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,project_id TEXT NOT NULL DEFAULT '',actor_id TEXT NOT NULL,action TEXT NOT NULL,target_id TEXT NOT NULL,summary TEXT NOT NULL,created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS audit_tenant_time ON audit(tenant_id,created_at DESC,id);
CREATE TABLE IF NOT EXISTS app_grants(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,application_id TEXT NOT NULL,user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,permission TEXT NOT NULL CHECK(permission IN ('review','admin')),granted_by TEXT NOT NULL,created_at INTEGER NOT NULL,UNIQUE(tenant_id,project_id,application_id,user_id,permission),FOREIGN KEY(tenant_id,project_id) REFERENCES projects(tenant_id,id) ON DELETE CASCADE,FOREIGN KEY(tenant_id,user_id) REFERENCES members(tenant_id,user_id) ON DELETE CASCADE);
PRAGMA user_version=1;`)
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) checkKey() error {
	return s.write(context.Background(), func(tx *sql.Tx) error {
		var blob []byte
		err := tx.QueryRow("SELECT value FROM platform_metadata WHERE key='encryption_check'").Scan(&blob)
		if errors.Is(err, sql.ErrNoRows) {
			blob, err = s.encrypt("Euler application cloud key check v1", "metadata:encryption_check")
			if err != nil {
				return err
			}
			_, err = tx.Exec("INSERT INTO platform_metadata VALUES('encryption_check',?)", blob)
			return err
		}
		if err != nil {
			return err
		}
		plain, err := s.decrypt(blob, "metadata:encryption_check")
		if err != nil || plain != "Euler application cloud key check v1" {
			return errors.New("encryption key does not match this database")
		}
		return nil
	})
}
func newID(prefix string) string {
	b := make([]byte, 18)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}
func tokenHash(s string) string   { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func stamp() int64                { return time.Now().UTC().UnixMilli() }
func fromStamp(n int64) time.Time { return time.UnixMilli(n).UTC() }
func cleanName(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 120 {
		return "", fmt.Errorf("%w: name must be 1–120 bytes", ErrInvalid)
	}
	return v, nil
}
func (s *Store) encrypt(v, aad string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, e := rand.Read(nonce); e != nil {
		return nil, e
	}
	return s.aead.Seal(nonce, nonce, []byte(v), []byte(aad)), nil
}
func (s *Store) decrypt(v []byte, aad string) (string, error) {
	n := s.aead.NonceSize()
	if len(v) < n {
		return "", errors.New("invalid encrypted record")
	}
	p, e := s.aead.Open(nil, v[:n], v[n:], []byte(aad))
	return string(p), e
}
func (s *Store) fingerprint(v string) string {
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte("credential\x00" + v))
	return hex.EncodeToString(h.Sum(nil))
}
func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = fn(tx); e != nil {
		return e
	}
	return tx.Commit()
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func tenantRole(ctx context.Context, q queryer, user, tenant string) (string, error) {
	var role string
	e := q.QueryRowContext(ctx, "SELECT role FROM members WHERE tenant_id=? AND user_id=?", tenant, user).Scan(&role)
	if errors.Is(e, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, e
}
func admin(role string) bool { return role == "owner" || role == "admin" }
func projectRole(ctx context.Context, q queryer, user, tenant, project string) (string, error) {
	r, e := tenantRole(ctx, q, user, tenant)
	if e != nil {
		return "", e
	}
	var id string
	if e = q.QueryRowContext(ctx, "SELECT id FROM projects WHERE id=? AND tenant_id=?", project, tenant).Scan(&id); errors.Is(e, sql.ErrNoRows) {
		return "", ErrNotFound
	} else if e != nil {
		return "", e
	}
	if admin(r) {
		return "admin", nil
	}
	teamViewer := r == "viewer"
	e = q.QueryRowContext(ctx, "SELECT role FROM project_members WHERE tenant_id=? AND project_id=? AND user_id=?", tenant, project, user).Scan(&r)
	if errors.Is(e, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if e == nil && teamViewer {
		return "viewer", nil
	}
	return r, e
}
func requireProject(ctx context.Context, q queryer, user, tenant, project, level string) error {
	r, e := projectRole(ctx, q, user, tenant, project)
	if e != nil {
		return e
	}
	if level == "admin" && r != "admin" || level == "write" && r == "viewer" {
		return ErrForbidden
	}
	return nil
}
func audit(ctx context.Context, tx *sql.Tx, user, tenant, project, action, target, summary string) error {
	_, e := tx.ExecContext(ctx, "INSERT INTO audit VALUES(?,?,?,?,?,?,?,?)", newID("evt_"), tenant, project, user, action, target, summary, stamp())
	return e
}
