// Package sqltest gives the SQL-backed stores a real MySQL to test against,
// without making the unit suite depend on one.
//
// # Why a harness instead of a fake
//
// A store that is only ever tested against a hand-written fake proves the fake
// agrees with the store, which is nothing. The things that actually break a SQL
// store are the ones a fake cannot have: a column that does not exist, a
// DECIMAL that loses precision on the round trip, a unique index that does not
// arbitrate the race the code believes it arbitrates, a CHECK constraint that
// rejects a value the domain thinks is fine. So the tests run against a real
// MySQL and, crucially, against the schema of record: the DDL directory passed
// to Open is the one sqlmigrate applies in production, not a copy that drifts.
//
// # Hermetic by default
//
// Without SC_DB_DSN — the same variable that turns persistence on for a service
// — every test that calls Open is SKIPPED. `go test ./...` therefore stays
// runnable on a machine with no database, which is what the platform's
// "persistence is opt-in" design requires (storage package doc).
//
// With SC_DB_DSN set, each test gets its own throwaway schema named
// sc_it_<test>_<nanos>, applies the DDL directories it asked for, and drops the
// schema again on cleanup. SQTEST_KEEP=1 keeps it for inspection. The DSN user
// needs CREATE/DROP DATABASE (the local dev account has it).
package sqltest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/starcloud/sc-platform/storage"
)

// Services names the DDL directory of one service, relative to the repository
// root (e.g. Services("svc-payment") is services/svc-payment/sql).
//
// Tests name the service whose tables they exercise rather than a path of their
// own, so the tables under test are the ones the migrator will have created in
// production.
func Services(service string) string {
	return filepath.Join("services", service, "sql")
}

// Open skips t unless SC_DB_DSN is set, then returns a pool bound to a fresh
// schema with ddlDirs (repository-relative, see Services) applied in filename
// order — the order sqlmigrate uses.
func Open(t *testing.T, ddlDirs ...string) *sql.DB {
	t.Helper()

	raw := strings.TrimSpace(os.Getenv(storage.DSNEnv))
	if raw == "" {
		t.Skipf("%s is not set: SQL-store tests need a real MySQL (see package sqltest)", storage.DSNEnv)
	}
	cfg, err := mysql.ParseDSN(raw)
	if err != nil {
		t.Fatalf("sqltest: invalid %s: %v", storage.DSNEnv, err)
	}
	// One DSN, two pools: a server-level pool that owns the schema's lifetime,
	// and the test pool bound to it. The DDL is applied with multiStatements so
	// a whole file is one round trip, exactly as the migrator does it.
	cfg.DBName = ""
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.MultiStatements = true
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	if _, ok := cfg.Params["charset"]; !ok {
		cfg.Params["charset"] = "utf8mb4"
	}

	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatalf("sqltest: open admin pool: %v", err)
	}
	// Bounded pools on purpose: `go test ./...` runs packages in parallel, and a
	// harness that opens connections per test as fast as the scheduler allows
	// exhausts the server's max_connections — the failure then looks like a bug in
	// whatever store happened to run last. Two connections manage the schema's
	// lifetime; the test pool gets a handful, which is all a single test needs.
	admin.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = admin.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("sqltest: connect: %v", err)
	}

	root, err := repoRoot()
	if err != nil {
		t.Fatalf("sqltest: %v", err)
	}

	schema := scratchSchemaName(t.Name())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+schema+"` DEFAULT CHARACTER SET utf8mb4"); err != nil {
		t.Fatalf("sqltest: create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if os.Getenv("SQTEST_KEEP") == "1" {
			t.Logf("sqltest: SQTEST_KEEP=1, leaving schema %s in place", schema)
			return
		}
		dropCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := admin.ExecContext(dropCtx, "DROP DATABASE IF EXISTS `"+schema+"`"); err != nil {
			t.Errorf("sqltest: drop schema %s: %v", schema, err)
		}
	})

	cfg.DBName = schema
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatalf("sqltest: open %s: %v", schema, err)
	}
	db.SetMaxOpenConns(6)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("sqltest: connect to %s: %v", schema, err)
	}

	for _, rel := range ddlDirs {
		applyDir(t, ctx, db, filepath.Join(root, rel))
	}
	return db
}

// applyDir runs every *.sql in dir, in filename order (V1 < V2 < V3).
func applyDir(t *testing.T, ctx context.Context, db *sql.DB, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("sqltest: read DDL dir %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".sql") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	if len(files) == 0 {
		t.Fatalf("sqltest: %s holds no .sql files", dir)
	}
	sort.Strings(files)
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("sqltest: read %s: %v", file, err)
		}
		if _, err := db.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("sqltest: apply %s: %v", filepath.Base(file), err)
		}
	}
}

// repoRoot walks up from the working directory (the package under test) to the
// repository root, identified by holding both services/ and pkg-go/. The DDL
// directories are written relative to it.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if isDir(filepath.Join(dir, "services")) && isDir(filepath.Join(dir, "pkg-go")) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("repository root not found above %s (it holds services/ and pkg-go/)", dir)
		}
		dir = parent
	}
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// scratchSchemaName derives a MySQL-safe schema name from the test name: lower
// case, alphanumeric and underscore, starting with a letter.
//
// The length cap is load-bearing: MySQL rejects identifiers longer than 64
// characters (Error 1059), and the suffix is a 19-digit nanosecond timestamp, so
// the name derived from t.Name() has to leave room for it. A long Go test name
// is not a reason for the harness itself to fail.
func scratchSchemaName(testName string) string {
	const (
		maxIdentifier = 64
		prefix        = "sc_it_"
	)
	// prefix + name + "_" + nanoseconds must fit.
	budget := maxIdentifier - len(prefix) - 1 - 20

	var b strings.Builder
	for _, r := range strings.ToLower(testName) {
		if b.Len() >= budget {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '/' || r == '-':
			b.WriteByte('_')
		}
	}
	return fmt.Sprintf("%s%s_%d", prefix, b.String(), time.Now().UnixNano())
}
