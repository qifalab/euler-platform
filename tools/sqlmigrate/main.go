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
//   - Ordered: files apply in version order — the numeric V<N> (V2 before
//     V10; V1_2 / V1.2 compare part by part), then file name for equal
//     versions — never in directory or plain lexicographic order, so a
//     schema's history is reproducible. Names outside V<N>__ sort last.
//   - Idempotent: a file already applied with the same checksum is skipped.
//     Re-running after a failure resumes at the first unrecorded file.
//   - Drift-detecting: a file that was applied and has since changed is an
//     ERROR, not a silent re-apply. Editing history is how two environments
//     end up with different schemas and nobody notices.
//   - Line-ending neutral: the checksum is SHA-256 over the file with CRLF
//     normalised to LF (the repo holds both, and a checkout can flip them),
//     so only a content change counts as an edit. Rows recorded by earlier
//     versions (raw-byte SHA-256) are still accepted.
//   - Recorded: the applied set lives in each schema's `schema_migration`
//     table (file, checksum, applied_at), so the state is queryable.
//   - Read-only dry run: -dry-run reads the recorded state but writes
//     nothing (no CREATE DATABASE, no schema_migration table).
//
// # Partial failure needs manual repair
//
// A file is recorded only after ALL of its statements succeeded, but a file is
// not atomic: MySQL commits every DDL statement implicitly, so when statement
// k of a multi-statement file fails, statements 1..k-1 stay applied while the
// file stays unrecorded. Re-running executes the file from the top again and
// typically fails on the first statement that is not re-runnable (CREATE TABLE
// without IF NOT EXISTS, ADD COLUMN, ...). The tool cannot tell which
// statements took effect, so an operator must repair it:
//
//  1. compare the schema with the file to see which statements took effect;
//  2. either undo them and fix the cause, then re-run sqlmigrate; or finish
//     the remaining statements by hand and record the file:
//     INSERT INTO schema_migration (file, checksum, applied_at)
//     VALUES ('<file>', '<checksum printed in the error>', UTC_TIMESTAMP(6));
//
// Writing migrations so every statement is re-runnable (IF NOT EXISTS, the
// information_schema-guarded ALTERs used by the existing V<N> files) makes a
// plain re-run the repair.
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
// The DSN is also read from EULER_DB_DSN, which is the same variable the services
// use to turn persistence on; the migration command appends multiStatements so
// a whole DDL file executes in one round trip (the server parses the file, so
// string literals containing ';' stay correct).
package main

import (
	"bytes"
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
	"strconv"
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
		dsn      = flag.String("dsn", "", "MySQL/Vitess DSN without a schema (default: $EULER_DB_DSN)")
		manifest = flag.String("manifest", "", "migration manifest path (default: tools/sqlmigrate/migrations.json, or migrations.json when run inside this directory)")
		root     = flag.String("root", "", "repository root that manifest dirs are relative to (default: autodetected)")
		only     = flag.String("only", "", "apply only entries whose dir contains this substring")
		dryRun   = flag.Bool("dry-run", false, "list the files that would be applied (read-only: creates no schema or table)")
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
		raw = strings.TrimSpace(os.Getenv("EULER_DB_DSN"))
	}
	if raw == "" {
		fatalf("no DSN: pass -dsn or set EULER_DB_DSN")
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
// order. Returns (applied, skipped). With dryRun it only plans (planSchema).
func applySchema(ctx context.Context, server *dbHandle, baseDSN, repoRoot string, entry manifestSchema, dryRun bool, timeout time.Duration) (int, int, error) {
	files, err := ddlFiles(filepath.Join(repoRoot, entry.Dir))
	if err != nil {
		return 0, 0, err
	}
	if dryRun {
		return planSchema(ctx, server, entry, files)
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
		checksum := fileChecksum(body)
		name := filepath.Base(file)

		var priorChecksum string
		err = db.QueryRowContext(ctx, "SELECT checksum FROM schema_migration WHERE file = ?", name).Scan(&priorChecksum)
		switch {
		case err == nil && checksumMatches(priorChecksum, body):
			skipped++
			continue
		case err == nil:
			return applied, skipped, editedError(name, priorChecksum, checksum)
		case !errors.Is(err, errNoRows):
			return applied, skipped, err
		}

		fileCtx, cancel := context.WithTimeout(ctx, timeout)
		_, err = db.ExecContext(fileCtx, string(body))
		cancel()
		if err != nil {
			return applied, skipped, fmt.Errorf(
				"apply %s: %w — the file is NOT recorded, but its statements before the failing one may already be committed (MySQL DDL auto-commits): repair by hand before re-running (see \"Partial failure\" in the sqlmigrate doc; checksum to record: %s)",
				name, err, checksum)
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

// planSchema is -dry-run: it reports what applySchema would do using only
// reads — no CREATE DATABASE, no schema_migration table, no connection that
// names the (possibly missing) schema — so it is safe against production with
// a read-only account. Recorded checksums are still verified, so a dry run
// surfaces an edited migration exactly like a real run.
func planSchema(ctx context.Context, server *dbHandle, entry manifestSchema, files []string) (int, int, error) {
	var tables int
	if err := server.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'schema_migration'",
		entry.Schema).Scan(&tables); err != nil {
		return 0, 0, fmt.Errorf("read migration state of %s: %w", entry.Schema, err)
	}
	applied, skipped := 0, 0
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			return applied, skipped, err
		}
		name := filepath.Base(file)
		if tables > 0 {
			var priorChecksum string
			err = server.QueryRowContext(ctx,
				"SELECT checksum FROM `"+entry.Schema+"`.schema_migration WHERE file = ?", name).Scan(&priorChecksum)
			switch {
			case err == nil && checksumMatches(priorChecksum, body):
				skipped++
				continue
			case err == nil:
				return applied, skipped, editedError(name, priorChecksum, fileChecksum(body))
			case !errors.Is(err, errNoRows):
				return applied, skipped, err
			}
		}
		fmt.Printf("       would apply %s/%s (%d bytes)\n", entry.Schema, name, len(body))
		applied++
	}
	return applied, skipped, nil
}

func editedError(name, prior, current string) error {
	return fmt.Errorf(
		"%s was already applied with checksum %s but the file is now %s: schema history was edited; write a NEW V<N> migration instead",
		name, shortSum(prior), shortSum(current))
}

func shortSum(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// fileChecksum is the checksum recorded for a migration: SHA-256 over the
// file with CRLF normalised to LF. The repository holds both CRLF and LF .sql
// files and a checkout (core.autocrlf, editors) can flip a file's endings; a
// raw-byte checksum then reported an untouched, already-applied file as
// "schema history was edited" and blocked every later migration.
func fileChecksum(body []byte) string {
	return sha256Hex(normalizeNewlines(body))
}

// checksumMatches reports whether a recorded checksum belongs to this file's
// content: the normalised checksum, or the legacy raw-byte SHA-256 earlier
// versions recorded — of these exact bytes or of the same content with the
// other line ending (applied as CRLF, now checked out as LF, or vice versa).
func checksumMatches(recorded string, body []byte) bool {
	recorded = strings.ToLower(strings.TrimSpace(recorded))
	lf := normalizeNewlines(body)
	crlf := bytes.ReplaceAll(lf, []byte("\n"), []byte("\r\n"))
	for _, candidate := range [][]byte{lf, body, crlf} {
		if recorded == sha256Hex(candidate) {
			return true
		}
	}
	return false
}

func normalizeNewlines(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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
	// Version order, not string order: sort.Strings put V10__ before V2__.
	sort.SliceStable(files, func(i, j int) bool { return migrationLess(files[i], files[j]) })
	return files, nil
}

// versionRE matches the V<version>__ prefix; the version is dot- or
// underscore-separated numeric parts (V2, V1_2, V1.2).
var versionRE = regexp.MustCompile(`^[Vv](\d+(?:[._]\d+)*)__`)

// migrationVersion parses a file name's version: "V10__x.sql" → [10],
// "V1_2__x.sql" → [1 2]. ok is false outside the V<N>__ convention.
func migrationVersion(name string) (version []int, ok bool) {
	m := versionRE.FindStringSubmatch(name)
	if m == nil {
		return nil, false
	}
	for _, part := range strings.FieldsFunc(m[1], func(r rune) bool { return r == '.' || r == '_' }) {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		version = append(version, n)
	}
	return version, true
}

// migrationLess orders migration files: versioned files by numeric version
// (part by part; a shorter equal prefix first), equal versions by file name,
// and files outside the V<N>__ convention after every versioned one.
func migrationLess(a, b string) bool {
	na, nb := filepath.Base(a), filepath.Base(b)
	va, okA := migrationVersion(na)
	vb, okB := migrationVersion(nb)
	if okA != okB {
		return okA
	}
	if okA {
		for i := 0; i < len(va) && i < len(vb); i++ {
			if va[i] != vb[i] {
				return va[i] < vb[i]
			}
		}
		if len(va) != len(vb) {
			return len(va) < len(vb)
		}
	}
	return na < nb
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sqlmigrate: "+format+"\n", args...)
	os.Exit(1)
}
