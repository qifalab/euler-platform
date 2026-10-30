// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestFreshPooledConnectionKeepsForeignKeysAndBusyTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		dsn  func(path string) string
	}{
		{"path", func(path string) string { return path }},
		{"file URI with query", func(path string) string { return "file:" + path + "?mode=rwc" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, err := Open(ctx, tc.dsn(filepath.Join(t.TempDir(), "pragma.sqlite")))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			// A TEMP table exists only on the connection that created it, so its
			// absence below proves the reads run on a newly opened connection, as
			// database/sql opens after driver.ErrBadConn.
			if _, err = s.db.ExecContext(ctx, `CREATE TEMP TABLE w2_first_connection(id INTEGER)`); err != nil {
				t.Fatal(err)
			}
			s.db.SetMaxIdleConns(0)
			conn, err := s.db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			var marker, foreignKeys, busyTimeout, synchronous int
			var journalMode string
			if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_temp_master WHERE name='w2_first_connection'`).Scan(&marker); err != nil || marker != 0 {
				t.Fatalf("test did not reach a new pooled connection: marker=%d err=%v", marker, err)
			}
			for _, read := range []struct {
				pragma string
				value  any
			}{{"foreign_keys", &foreignKeys}, {"busy_timeout", &busyTimeout}, {"synchronous", &synchronous}, {"journal_mode", &journalMode}} {
				if err = conn.QueryRowContext(ctx, "PRAGMA "+read.pragma).Scan(read.value); err != nil {
					t.Fatal(err)
				}
			}
			if foreignKeys != 1 || busyTimeout != 5000 || synchronous != 1 || journalMode != "wal" {
				t.Fatalf("new connection lost its settings: foreign_keys=%d busy_timeout=%d synchronous=%d journal_mode=%s", foreignKeys, busyTimeout, synchronous, journalMode)
			}
			// The cascade the settings protect: deleting a device removes its token.
			for _, q := range []string{
				`INSERT INTO devices(id,name,hostname,os,arch,agent_version,identity_key,status,enrolled_at,created_at,updated_at) VALUES('dev_w2','w2','host','linux','amd64','test','','offline','t','t','t')`,
				`INSERT INTO agent_tokens(id,device_id,token_hash,created_at) VALUES('agt_w2','dev_w2','hash_w2','t')`,
				`DELETE FROM devices WHERE id='dev_w2'`,
			} {
				if _, err = conn.ExecContext(ctx, q); err != nil {
					t.Fatal(err)
				}
			}
			var tokens int
			if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM agent_tokens WHERE id='agt_w2'`).Scan(&tokens); err != nil || tokens != 0 {
				t.Fatalf("foreign-key cascade did not run on the new connection: tokens=%d err=%v", tokens, err)
			}
		})
	}
}

func TestOpenInMemoryAppliesConnectionPragmas(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var foreignKeys, busyTimeout int
	var journalMode string
	if err = s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 || busyTimeout != 5000 || journalMode != "memory" {
		t.Fatalf("in-memory store settings: foreign_keys=%d busy_timeout=%d journal_mode=%s", foreignKeys, busyTimeout, journalMode)
	}
}

// A caller DSN that overrides a protected setting would leave every new
// connection without it; Open must refuse it instead of serving.
func TestOpenRejectsDSNThatOverridesConnectionSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "override.sqlite")
	s, err := Open(context.Background(), path+"?_foreign_keys=0")
	if err == nil {
		_ = s.Close()
		t.Fatal("Open accepted a DSN that disables foreign keys")
	}
	if !strings.Contains(err.Error(), "foreign_keys=0") {
		t.Fatalf("unexpected error: %v", err)
	}
}
