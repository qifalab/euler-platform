package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestMigrationOrderIsNumeric(t *testing.T) {
	names := []string{
		"zz_notes.sql", "V10__ten.sql", "V2__two_b.sql", "V1__one.sql",
		"V2__two_a.sql", "V1_10__one_ten.sql", "V1_2__one_two.sql", "V9__nine.sql",
	}
	sort.SliceStable(names, func(i, j int) bool { return migrationLess(names[i], names[j]) })
	want := []string{
		"V1__one.sql", "V1_2__one_two.sql", "V1_10__one_ten.sql",
		"V2__two_a.sql", "V2__two_b.sql", "V9__nine.sql", "V10__ten.sql",
		"zz_notes.sql",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("order = %v\nwant    %v", names, want)
	}
}

func TestDDLFilesSortsByVersion(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"V10__c.sql", "V2__b.sql", "V1__a.sql", "README.md"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("SELECT 1;"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := ddlFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		got = append(got, filepath.Base(f))
	}
	if want := []string{"V1__a.sql", "V2__b.sql", "V10__c.sql"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ddlFiles = %v, want %v", got, want)
	}
}

func rawSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestChecksumIgnoresLineEndings(t *testing.T) {
	lf := []byte("CREATE TABLE t (\n  id INT\n);\n")
	crlf := []byte(strings.ReplaceAll(string(lf), "\n", "\r\n"))
	if fileChecksum(lf) != fileChecksum(crlf) {
		t.Fatal("CRLF and LF copies of one migration must share a checksum")
	}
	if fileChecksum(lf) != rawSum(lf) {
		t.Fatal("an LF file's checksum must equal its raw SHA-256")
	}
	// Legacy rows hold the raw-byte SHA-256 of whatever was checked out.
	for name, recorded := range map[string]string{
		"normalised":      fileChecksum(crlf),
		"legacy raw CRLF": rawSum(crlf),
		"legacy raw LF":   rawSum(lf),
		"upper-case hex":  strings.ToUpper(rawSum(crlf)),
	} {
		for _, body := range [][]byte{lf, crlf} {
			if !checksumMatches(recorded, body) {
				t.Errorf("%s checksum not accepted for %q", name, body)
			}
		}
	}
	edited := []byte("CREATE TABLE t (\n  id BIGINT\n);\n")
	if checksumMatches(fileChecksum(lf), edited) || checksumMatches(rawSum(crlf), edited) {
		t.Fatal("a content change must not match the recorded checksum")
	}
}

// TestDryRunWritesNothing runs against a real server when EULER_DB_DSN is set
// (tools: $HOME/withdb.sh); it is skipped otherwise.
func TestDryRunWritesNothing(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("EULER_DB_DSN"))
	if raw == "" {
		t.Skip("EULER_DB_DSN not set")
	}
	ctx := context.Background()
	dsn, err := withMultiStatements(raw)
	if err != nil {
		t.Fatal(err)
	}
	server, err := openServer(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	schema := fmt.Sprintf("sqlmigrate_test_%d", os.Getpid())
	drop := func() { _, _ = server.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+schema+"`") }
	drop()
	defer drop()

	root := t.TempDir()
	dir := filepath.Join(root, "sql")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("V1__t.sql", "CREATE TABLE IF NOT EXISTS t (id INT);\r\n")
	write("V10__u.sql", "CREATE TABLE IF NOT EXISTS u (id INT);\n")
	write("V2__t_col.sql", "ALTER TABLE t ADD COLUMN v INT;\n")
	entry := manifestSchema{Dir: "sql", Schema: schema}

	schemaExists := func() bool {
		var n int
		if err := server.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", schema).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}

	applied, skipped, err := applySchema(ctx, server, dsn, root, entry, true, time.Minute)
	if err != nil || applied != 3 || skipped != 0 {
		t.Fatalf("dry run on a missing schema = (%d, %d, %v), want (3, 0, nil)", applied, skipped, err)
	}
	if schemaExists() {
		t.Fatal("-dry-run created the schema")
	}

	// Real run: V2 (ALTER on t) only works if V1 ran before it and V10 after.
	if applied, skipped, err = applySchema(ctx, server, dsn, root, entry, false, time.Minute); err != nil || applied != 3 {
		t.Fatalf("apply = (%d, %d, %v), want 3 applied", applied, skipped, err)
	}
	var order string
	if err := server.QueryRowContext(ctx,
		"SELECT GROUP_CONCAT(file ORDER BY applied_at, file SEPARATOR ',') FROM `"+schema+"`.schema_migration").Scan(&order); err != nil {
		t.Fatal(err)
	}
	if order != "V1__t.sql,V2__t_col.sql,V10__u.sql" {
		t.Fatalf("applied order = %s", order)
	}

	// Second run (and dry run) are no-ops.
	for _, dry := range []bool{false, true} {
		if applied, skipped, err = applySchema(ctx, server, dsn, root, entry, dry, time.Minute); err != nil || applied != 0 || skipped != 3 {
			t.Fatalf("re-run (dry=%v) = (%d, %d, %v), want (0, 3, nil)", dry, applied, skipped, err)
		}
	}

	// A legacy raw-byte checksum of the CRLF checkout still matches after
	// the file is converted to LF.
	if _, err := server.ExecContext(ctx, "UPDATE `"+schema+"`.schema_migration SET checksum = ? WHERE file = 'V1__t.sql'",
		rawSum([]byte("CREATE TABLE IF NOT EXISTS t (id INT);\r\n"))); err != nil {
		t.Fatal(err)
	}
	write("V1__t.sql", "CREATE TABLE IF NOT EXISTS t (id INT);\n")
	if applied, skipped, err = applySchema(ctx, server, dsn, root, entry, false, time.Minute); err != nil || applied != 0 || skipped != 3 {
		t.Fatalf("after CRLF->LF = (%d, %d, %v), want (0, 3, nil)", applied, skipped, err)
	}

	// A real edit is still drift, in a dry run too.
	write("V1__t.sql", "CREATE TABLE IF NOT EXISTS t (id BIGINT);\n")
	for _, dry := range []bool{true, false} {
		if _, _, err = applySchema(ctx, server, dsn, root, entry, dry, time.Minute); err == nil || !strings.Contains(err.Error(), "schema history was edited") {
			t.Fatalf("edited file (dry=%v): err = %v, want drift error", dry, err)
		}
	}
}
