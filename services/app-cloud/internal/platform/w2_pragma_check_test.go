package platform

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// The startup read-back must refuse a connection the DSN did not configure.
func TestVerifyConnectionPragmasRejectsUnconfiguredConnection(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = verifyConnectionPragmas(db); err == nil || !strings.Contains(err.Error(), "foreign_keys=0") {
		t.Fatalf("unconfigured connection accepted: %v", err)
	}
}
