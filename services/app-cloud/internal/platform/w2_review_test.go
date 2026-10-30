package platform

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

// freshConnection closes the store's idle connection and returns a newly
// opened one, which is what database/sql does after driver.ErrBadConn.
func freshConnection(t *testing.T, s *Store) *sql.Conn {
	t.Helper()
	ctx := context.Background()
	// A TEMP table exists only on the connection that created it, so its
	// absence below proves the test reached a different connection.
	if _, err := s.db.ExecContext(ctx, `CREATE TEMP TABLE w2_first_connection(id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	s.db.SetMaxIdleConns(0)
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var marker int
	if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_temp_master WHERE name='w2_first_connection'`).Scan(&marker); err != nil || marker != 0 {
		t.Fatalf("test did not reach a new pooled connection: marker=%d err=%v", marker, err)
	}
	return conn
}

func TestFreshPooledConnectionKeepsForeignKeysAndBusyTimeout(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	conn := freshConnection(t, s)
	var foreignKeys, busyTimeout, synchronous int
	var journalMode string
	for _, read := range []struct {
		pragma string
		value  any
	}{{"foreign_keys", &foreignKeys}, {"busy_timeout", &busyTimeout}, {"synchronous", &synchronous}, {"journal_mode", &journalMode}} {
		if err := conn.QueryRowContext(ctx, "PRAGMA "+read.pragma).Scan(read.value); err != nil {
			t.Fatal(err)
		}
	}
	if foreignKeys != 1 || busyTimeout != 5000 || synchronous != 2 || journalMode != "wal" {
		t.Fatalf("new connection lost its settings: foreign_keys=%d busy_timeout=%d synchronous=%d journal_mode=%s", foreignKeys, busyTimeout, synchronous, journalMode)
	}
	// The cascade the settings protect: deleting a user removes its session.
	for _, q := range []string{
		`INSERT INTO users(id,provider,subject,display_name) VALUES('usr_w2','https://issuer.test','w2','W2')`,
		`INSERT INTO sessions(token_hash,user_id,csrf_token,expires_at) VALUES('hash_w2','usr_w2','csrf',0)`,
		`DELETE FROM users WHERE id='usr_w2'`,
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var sessions int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE token_hash='hash_w2'`).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("foreign-key cascade did not run on the new connection: sessions=%d err=%v", sessions, err)
	}
}

func TestInMemoryStoreAppliesConnectionPragmas(t *testing.T) {
	s, err := Open(":memory:", bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var foreignKeys, busyTimeout int
	var journalMode string
	if err = s.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 || busyTimeout != 5000 || journalMode != "memory" {
		t.Fatalf("in-memory store settings: foreign_keys=%d busy_timeout=%d journal_mode=%s", foreignKeys, busyTimeout, journalMode)
	}
}

// countingModule counts how often the platform asks it to build its handler.
type countingModule struct{ builds *atomic.Int32 }

func (countingModule) ID() string                    { return "database" }
func (countingModule) Migrate(context.Context) error { return nil }
func (countingModule) PublicHandler() http.Handler   { return nil }
func (m countingModule) Handler() http.Handler {
	m.builds.Add(1)
	mux := http.NewServeMux()
	appkit.Handle(mux, "GET /databases", "read", func(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
		appkit.JSON(w, 200, map[string]string{"provider": s.Provider})
		return nil
	})
	return mux
}

func TestModuleHandlerIsBuiltOnceForBrowserAndMachineRequests(t *testing.T) {
	s := testStore(t)
	owner := testUser(t, s, "owner")
	tenant := mustTenant(t, s, owner.ID)
	project := mustProject(t, s, owner.ID, tenant.ID)
	if _, err := s.EnableApplication(testContext, owner.ID, tenant.ID, project.ID, "database"); err != nil {
		t.Fatal(err)
	}
	_, secret, err := s.CreateServiceAccount(testContext, owner.ID, tenant.ID, project.ID, ServiceAccountInput{Name: "CI", ExpiresAt: time.Now().Add(time.Hour), Scopes: []ServiceScope{{"database", []string{"read"}}}})
	if err != nil {
		t.Fatal(err)
	}
	module := countingModule{builds: new(atomic.Int32)}
	h := NewHandler(s, actorAuth{owner}, managerFor(t), WithApplications([]appkit.Module{module}, nil))
	path := "/tenants/" + tenant.ID + "/projects/" + project.ID + "/apps/database/databases"
	for i := 0; i < 3; i++ {
		if w := call(h, "GET", "/api/v1"+path, "", true, false); w.Code != 200 || !strings.Contains(w.Body.String(), `"provider":"https://issuer.test"`) {
			t.Fatal("browser request", w.Code, w.Body.String())
		}
		if w := machineCall(h, secret, "GET", "/api/v1/machine"+path, nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"provider":"euler-service-account"`) {
			t.Fatal("machine request", w.Code, w.Body.String())
		}
	}
	if n := module.builds.Load(); n != 1 {
		t.Fatalf("module handler built %d times for 6 requests, want 1", n)
	}
}
