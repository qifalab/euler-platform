// Command sqlmigrate applies the platform's DDL to MySQL, exactly once.
//
// # Why this command exists
//
// services/*/sql/V<N>__*.sql is the schema of record (account_db, trade_db,
// resource_db, support_db, metering_db, openapi_meta — 04§6.3), but until this
// command existed nothing executed it: the services ran on in-memory stores
// and the DDL was documentation. sqlmigrate is the execution step — the same
// role `vtctld ApplySchema` plays for the Vitess deployment.
//
// Properties that matter in production:
//
//   - Ordered: files apply in filename order (V1, V2, V3), never in directory
//     order, so a schema's history is reproducible.
//   - Idempotent: a file already applied with the same checksum is skipped.
//     Re-running after a partial failure resumes instead of restarting.
//   - Drift-detecting: a file that was applied and has since changed is an
//     ERROR, not a silent re-apply. Editing history is how two environments
//     end up with different schemas and nobody notices.
//   - Recorded: the applied set lives in each schema's `schema_migration`
//     table (file, checksum, applied_at), so the state is queryable.
//
// Usage (DSN points at the MySQL/Vitess endpoint without a schema; run from
// this directory, which is where the Go module lives — the repository root has
// none, so `go run ./tools/sqlmigrate` from there does not build):
//
//	cd tools/sqlmigrate
//	go run . -dsn 'user:password@tcp(127.0.0.1:3306)/'
//	go run . -probe            # ping + print server version
//	go run . -dry-run          # list what would run
//	go run . -only svc-order   # one schema only
//
// The manifest is found automatically next to this file, and its dir entries
// are resolved against the repository root it discovers by walking up — the
// two anchors differ, so neither can be assumed to be the working directory.
//
// The DSN is also read from SC_DB_DSN, which is the same variable the services
// use to turn persistence on; the migration command appends multiStatements so
// a whole DDL file executes in one round trip (the server parses the file, so
// string literals containing ';' stay correct).
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// migrationManifest is the on-disk manifest (tools/sqlmigrate/migrations.json).
type migrationManifest struct {
	Schemas []manifestSchema          `json:"schemas"`
	Engines map[string]manifestEngine `json:"engines"`
}

type manifestSchema struct {
	Dir    string `json:"dir"`
	Schema string `json:"schema"`
}

type manifestEngine struct {
	Dir    string `json:"dir"`
	Engine string `json:"engine"`
	Note   string `json:"note"`
}

// schemaNameRE guards the identifier we interpolate into CREATE DATABASE:
// schema names come from the committed manifest, but a migration tool that
// concatenates identifiers is exactly where a stray quote becomes an incident.
var schemaNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func main() {
	var (
		dsn      = flag.String("dsn", "", "MySQL/Vitess DSN without a schema (default: $SC_DB_DSN)")
		manifest = flag.String("manifest", "", "migration manifest path (default: tools/sqlmigrate/migrations.json, or migrations.json when run inside this directory)")
		root     = flag.String("root", "", "repository root that manifest dirs are relative to (default: autodetected)")
		only     = flag.String("only", "", "apply only entries whose dir contains this substring")
		dryRun   = flag.Bool("dry-run", false, "list the files that would be applied")
		probe    = flag.Bool("probe", false, "ping the server and print its version, then exit")
		timeout  = flag.Duration("timeout", 60*time.Second, "per-file execution timeout")
	)
	flag.Parse()

	// The Go module lives in this directory while the manifest's dir entries are
	// written relative to the repository root, so both have to be located
	// explicitly: `go run .` only compiles from here (no module at the root),
	// and "services/svc-iam/sql" only resolves from there.
	manifestPath, err := resolveManifest(*manifest)
	if err != nil {
		fatalf("%v", err)
	}
	repoRoot := *root
	if repoRoot == "" {
		if repoRoot = findRepoRoot(filepath.Dir(manifestPath)); repoRoot == "" {
			fatalf("cannot locate the repository root above %s: pass -root", manifestPath)
		}
	}

	raw := strings.TrimSpace(*dsn)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("SC_DB_DSN"))
	}
	if raw == "" {
		fatalf("no DSN: pass -dsn or set SC_DB_DSN")
	}
	baseDSN, err := withMultiStatements(raw)
	if err != nil {
		fatalf("%v", err)
	}

	ctx := context.Background()
	server, err := openServer(ctx, baseDSN)
	if err != nil {
		fatalf("connect: %v", err)
	}
	defer server.Close()

	// DATABASE() is NULL whenever the DSN carries no schema — which is the
	// documented way to run this tool ("DSN ... without a schema"). Scanning
	// it into a plain string fails with "converting NULL to string is
	// unsupported" and aborts every run, probe or not.
	var version string
	var current sql.NullString
	if err := server.QueryRowContext(ctx, "SELECT VERSION(), DATABASE()").Scan(&version, &current); err != nil {
		fatalf("probe server: %v", err)
	}
	fmt.Printf("server: %s (default schema: %q)\n", version, current.String)
	if *probe {
		return
	}

	mf, err := loadManifest(manifestPath)
	if err != nil {
		fatalf("manifest: %v", err)
	}
	// Applied files are recorded by their base name within each schema, so two
	// directories of the SAME schema must not ship files with the same name: the
	// second one would be treated as an already-applied edit of the first and
	// abort the run (observed for real when two dirs both shipped
	// "V2__trade_db_id_sequence.sql"). Different schemas never collide — each
	// carries its own schema_migration table.
	if dupSchema, dupName := duplicateFileNames(mf, repoRoot); dupSchema != "" {
		fatalf("manifest: schema %s has two directories shipping %q; rename one (applied files are recorded by file name)", dupSchema, dupName)
	}

	for name, eng := range mf.Engines {
		fmt.Printf("[skip] %-16s engine=%s — %s\n", name, eng.Engine, eng.Note)
	}

	totalApplied, totalSkipped := 0, 0
	for _, entry := range mf.Schemas {
		if *only != "" && !strings.Contains(entry.Dir, *only) {
			continue
		}
		if !schemaNameRE.MatchString(entry.Schema) {
			fatalf("invalid schema name %q in manifest", entry.Schema)
		}
		applied, skipped, err := applySchema(ctx, server, baseDSN, repoRoot, entry, *dryRun, *timeout)
		if err != nil {
			fatalf("%s: %v", entry.Dir, err)
		}
		totalApplied += applied
		totalSkipped += skipped
		fmt.Printf("[ok]   %-34s schema=%-13s applied=%d skipped=%d\n", entry.Dir, entry.Schema, applied, skipped)
	}

	if *dryRun {
		fmt.Printf("\ndry-run: %d file(s) would be applied, %d already applied\n", totalApplied, totalSkipped)
		return
	}
	fmt.Printf("\nmigrations complete: %d applied, %d already applied\n", totalApplied, totalSkipped)
}

// withMultiStatements forces multiStatements=true (and parseTime/utf8mb4) so a
// whole DDL file is one round trip.
func withMultiStatements(raw string) (string, error) {
	cfg, err := mysql.ParseDSN(raw)
	if err != nil {
		return "", fmt.Errorf("invalid DSN: %w", err)
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.MultiStatements = true
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	if _, ok := cfg.Params["charset"]; !ok {
		cfg.Params["charset"] = "utf8mb4"
	}
	return cfg.FormatDSN(), nil
}

func openServer(ctx context.Context, dsn string) (*dbHandle, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	cfg.DBName = "" // the server-level connection has no default schema yet
	handle, err := openDB(ctx, cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	return handle, nil
}

// resolveManifest locates the migration manifest. No default path works for
// both supported working directories, so the candidates are tried in order:
// the repository root (where the documented `go run ./tools/sqlmigrate` is
// issued — it needs a root module, which this repo does not have) and this
// tool's own directory (the only place the module does compile today).
func resolveManifest(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("manifest: %w", err)
		}
		return explicit, nil
	}
	candidates := []string{
		filepath.Join("tools", "sqlmigrate", "migrations.json"),
		"migrations.json",
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no manifest found (looked for %s); pass -manifest", strings.Join(candidates, ", "))
}

// findRepoRoot walks up from start looking for the repository root, identified
// by the migration targets' parent (services/). Manifest dir entries are
// written relative to that root, so the tool cannot resolve them from its own
// location alone.
func findRepoRoot(start string) string {
	dir, err := filepath.Abs(start)
	if err != nil {
		return ""
	}
	for {
		if info, err := os.Stat(filepath.Join(dir, "services")); err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func loadManifest(path string) (migrationManifest, error) {
	var mf migrationManifest
	raw, err := os.ReadFile(path)
	if err != nil {
		return mf, err
	}
	if err := json.Unmarshal(raw, &mf); err != nil {
		return mf, err
	}
	if len(mf.Schemas) == 0 {
		return mf, errors.New("manifest lists no schemas")
	}
	return mf, nil
}

// applySchema creates the schema if needed, then applies every *.sql file in
// order. Returns (applied, skipped).
func applySchema(ctx context.Context, server *dbHandle, baseDSN, repoRoot string, entry manifestSchema, dryRun bool, timeout time.Duration) (int, int, error) {
	files, err := ddlFiles(filepath.Join(repoRoot, entry.Dir))
	if err != nil {
		return 0, 0, err
	}
	if _, err := server.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS `"+entry.Schema+"` DEFAULT CHARACTER SET utf8mb4"); err != nil {
		return 0, 0, fmt.Errorf("create database %s: %w", entry.Schema, err)
	}

	cfg, err := mysql.ParseDSN(baseDSN)
	if err != nil {
		return 0, 0, err
	}
	cfg.DBName = entry.Schema
	db, err := openDB(ctx, cfg.FormatDSN())
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migration (
  file       VARCHAR(255) NOT NULL PRIMARY KEY,
  checksum   CHAR(64)     NOT NULL,
  applied_at DATETIME(6)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`); err != nil {
		return 0, 0, fmt.Errorf("create schema_migration: %w", err)
	}

	applied, skipped := 0, 0
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			return applied, skipped, err
		}
		sum := sha256.Sum256(body)
		checksum := hex.EncodeToString(sum[:])
		name := filepath.Base(file)

		var priorChecksum string
		err = db.QueryRowContext(ctx, "SELECT checksum FROM schema_migration WHERE file = ?", name).Scan(&priorChecksum)
		switch {
		case err == nil && priorChecksum == checksum:
			skipped++
			continue
		case err == nil && priorChecksum != checksum:
			return applied, skipped, fmt.Errorf(
				"%s was already applied with checksum %s but the file is now %s: schema history was edited; write a NEW V<N> migration instead",
				name, priorChecksum[:12], checksum[:12])
		case !errors.Is(err, errNoRows):
			return applied, skipped, err
		}

		if dryRun {
			fmt.Printf("       would apply %s/%s (%d bytes)\n", entry.Schema, name, len(body))
			applied++
			continue
		}

		fileCtx, cancel := context.WithTimeout(ctx, timeout)
		_, err = db.ExecContext(fileCtx, string(body))
		cancel()
		if err != nil {
			return applied, skipped, fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := db.ExecContext(ctx,
			"INSERT INTO schema_migration (file, checksum, applied_at) VALUES (?, ?, ?)",
			name, checksum, time.Now().UTC()); err != nil {
			return applied, skipped, fmt.Errorf("record %s: %w", name, err)
		}
		applied++
	}
	return applied, skipped, nil
}

// duplicateFileNames reports the first schema in which two directories ship a
// .sql file with the same base name.
func duplicateFileNames(mf migrationManifest, repoRoot string) (dupSchema, dupName string) {
	perSchema := map[string]map[string]string{} // schema → file name → first dir
	for _, entry := range mf.Schemas {
		files, err := ddlFiles(filepath.Join(repoRoot, entry.Dir))
		if err != nil {
			continue // unreadable directories are reported by applySchema
		}
		if perSchema[entry.Schema] == nil {
			perSchema[entry.Schema] = map[string]string{}
		}
		for _, f := range files {
			name := filepath.Base(f)
			if first, taken := perSchema[entry.Schema][name]; taken && first != entry.Dir {
				return entry.Schema, name
			}
			perSchema[entry.Schema][name] = entry.Dir
		}
	}
	return "", ""
}

// ddlFiles returns the *.sql files of a directory in migration order.
func ddlFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".sql") {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s holds no .sql files", dir)
	}
	// V1__… < V2__… < V3__… lexicographically; the V prefix is zero-padded by
	// convention, and a stray non-V file sorts after them (never before).
	sort.Strings(files)
	return files, nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sqlmigrate: "+format+"\n", args...)
	os.Exit(1)
}
