package database

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// TestRealEngines is required in the integration CI job, which supplies both
// disposable service DSNs. No original product or production database is used.
// Locally, an absent service is an explicit skip rather than a fake success.
func TestRealEngines(t *testing.T) {
	for _, kind := range []string{"mysql", "postgresql"} {
		t.Run(kind, func(t *testing.T) {
			key := "EULER_TEST_MYSQL_DSN"
			deploymentKey := "EULER_DATABASE_MYSQL_DSN"
			if kind == "postgresql" {
				key = "EULER_TEST_POSTGRES_DSN"
				deploymentKey = "EULER_DATABASE_POSTGRES_DSN"
			}
			dsn := os.Getenv(key)
			if dsn == "" {
				t.Skip(key + " is not configured; real engine verification runs in integration CI")
			}
			t.Setenv("EULER_DATABASE_MYSQL_DSN", "")
			t.Setenv("EULER_DATABASE_POSTGRES_DSN", "")
			t.Setenv(deploymentKey, dsn)
			m, s, _, _ := databaseFixture(t)
			configs, e := readEngines()
			if e != nil {
				t.Fatal(e)
			}
			m.engines = configs
			engine := m.engines[kind].Engine
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			resource := createDatabase(t, m, s, kind)
			t.Cleanup(func() {
				cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
				defer done()
				if e := engine.Drop(cleanup, resource.Database, resource.Username); e != nil {
					t.Errorf("cleanup primary test database: %v", e)
				}
			})
			credentials := func() string {
				w := databaseCall(m.Handler(), &s, "GET", "/databases/"+resource.ID+"/credentials", nil)
				databaseStatus(t, w, 200)
				return decodeResponse[struct {
					Password string `json:"password"`
				}](t, w).Password
			}
			password := credentials()
			openUser := func(database, user, password string) *sql.DB {
				var client *sql.DB
				if kind == "mysql" {
					cfg, e := mysql.ParseDSN(dsn)
					if e != nil {
						t.Fatal("invalid integration MySQL configuration")
					}
					cfg.User = user
					cfg.Passwd = password
					cfg.DBName = database
					cfg.MultiStatements = false
					cfg.Timeout = 5 * time.Second
					cfg.ReadTimeout = 5 * time.Second
					cfg.WriteTimeout = 5 * time.Second
					client, e = sql.Open("mysql", cfg.FormatDSN())
					if e != nil {
						t.Fatal(e)
					}
				} else {
					cfg, e := pgx.ParseConfig(dsn)
					if e != nil {
						t.Fatal("invalid integration PostgreSQL configuration")
					}
					cfg.User = user
					cfg.Password = password
					cfg.Database = database
					cfg.ConnectTimeout = 5 * time.Second
					client = stdlib.OpenDB(*cfg)
				}
				client.SetMaxOpenConns(1)
				t.Cleanup(func() { client.Close() })
				return client
			}
			client := openUser(resource.Database, resource.Username, password)
			if e = client.PingContext(ctx); e != nil {
				t.Fatalf("project login cannot connect: %v", e)
			}
			if _, e = client.ExecContext(ctx, "CREATE TABLE euler_contract (id INTEGER PRIMARY KEY,value VARCHAR(80))"); e != nil {
				t.Fatalf("project login cannot create its own table: %v", e)
			}
			if _, e = client.ExecContext(ctx, "INSERT INTO euler_contract(id,value) VALUES(1,'before-readonly')"); e != nil {
				t.Fatalf("project login cannot write: %v", e)
			}
			var value string
			if e = client.QueryRowContext(ctx, "SELECT value FROM euler_contract WHERE id=1").Scan(&value); e != nil || value != "before-readonly" {
				t.Fatalf("real readback failed: %v", e)
			}
			if size, e := engine.Size(ctx, resource.Database); e != nil || size <= 0 {
				t.Fatalf("real size was not measured: size=%d error=%v", size, e)
			}
			// Independent project/database: a valid login must not become a server-wide
			// user capable of connecting to another project's database.
			otherScope := s
			otherScope.ProjectID = "separate-project"
			otherScope.InstallationID = "separate-installation"
			other := createDatabase(t, m, otherScope, kind)
			t.Cleanup(func() {
				cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
				defer done()
				if e := engine.Drop(cleanup, other.Database, other.Username); e != nil {
					t.Errorf("cleanup second test database: %v", e)
				}
			})
			cross := openUser(other.Database, resource.Username, password)
			if e = cross.PingContext(ctx); e == nil {
				t.Fatal("database login connected to another project's database")
			}
			cross.Close()
			if kind == "postgresql" {
				var super, createDB, createRole, ownerMember bool
				if e = client.QueryRowContext(ctx, "SELECT rolsuper,rolcreatedb,rolcreaterole FROM pg_roles WHERE rolname=current_user").Scan(&super, &createDB, &createRole); e != nil {
					t.Fatal(e)
				}
				if super || createDB || createRole {
					t.Fatal("project login received server administration rights")
				}
				if e = client.QueryRowContext(ctx, "SELECT pg_has_role(current_user,$1,'MEMBER')", resource.Database+"_owner").Scan(&ownerMember); e != nil {
					t.Fatal(e)
				}
				if ownerMember {
					t.Fatal("project login can assume the privileged owner role")
				}
				if _, e = client.ExecContext(ctx, "CREATE FUNCTION euler_security_identity() RETURNS text LANGUAGE sql SECURITY DEFINER AS $$ SELECT current_user::text $$"); e != nil {
					t.Fatalf("cannot create project-owned test function: %v", e)
				}
				if _, e = client.ExecContext(ctx, "CREATE PROCEDURE euler_attempt_write() LANGUAGE sql SECURITY DEFINER AS $$ INSERT INTO euler_contract(id,value) VALUES(6,'procedure') $$"); e != nil {
					t.Fatalf("cannot create project-owned test procedure: %v", e)
				}
			}
			// Pin an existing client session before revocation. Testing only a fresh
			// login would miss grants cached by already-connected database users.
			existing, e := client.Conn(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer existing.Close()
			if kind == "mysql" {
				m.ledger.base.MySQLMB = 0
			} else {
				m.ledger.base.PostgresMB = 0
			}
			databaseStatus(t, databaseCall(m.Handler(), &s, "POST", "/admin/check-quota", nil), 200)
			if _, e = existing.ExecContext(ctx, "INSERT INTO euler_contract(id,value) VALUES(2,'existing-session-bypass')"); e == nil {
				t.Error("an existing session retained write access after quota revocation")
			}
			existing.Close()
			client.Close()
			readonly := openUser(resource.Database, resource.Username, password)
			if e = readonly.QueryRowContext(ctx, "SELECT value FROM euler_contract WHERE id=1").Scan(&value); e != nil {
				t.Fatalf("read-only account cannot read: %v", e)
			}
			if kind == "postgresql" {
				if _, e = readonly.ExecContext(ctx, "SET default_transaction_read_only=off"); e != nil {
					t.Fatal(e)
				}
				if e = readonly.QueryRowContext(ctx, "SELECT euler_security_identity()").Scan(&value); e == nil {
					t.Error("read-only project can invoke a transferred SECURITY DEFINER function")
				}
				if _, e = readonly.ExecContext(ctx, "CALL euler_attempt_write()"); e == nil {
					t.Error("read-only project can invoke a transferred SECURITY DEFINER procedure")
				}
			}
			for _, statement := range []string{"INSERT INTO euler_contract(id,value) VALUES(3,'new-session-bypass')", "UPDATE euler_contract SET value='modified' WHERE id=1", "CREATE TABLE euler_forbidden (id INTEGER)", "ALTER TABLE euler_contract ADD COLUMN blocked INTEGER"} {
				if _, e = readonly.ExecContext(ctx, statement); e == nil {
					t.Errorf("read-only login executed forbidden statement: %s", statement)
				}
			}
			// Restoring a package/base allowance must re-enable ordinary usage.
			if kind == "mysql" {
				m.ledger.base.MySQLMB = 100
			} else {
				m.ledger.base.PostgresMB = 100
			}
			databaseStatus(t, databaseCall(m.Handler(), &s, "POST", "/admin/check-quota", nil), 200)
			readonly.Close()
			restored := openUser(resource.Database, resource.Username, password)
			if _, e = restored.ExecContext(ctx, "INSERT INTO euler_contract(id,value) VALUES(4,'restored')"); e != nil {
				t.Fatalf("writes were not restored: %v", e)
			}
			if _, e = restored.ExecContext(ctx, "CREATE TABLE euler_restored (id INTEGER)"); e != nil {
				t.Fatalf("table creation was not restored: %v", e)
			}
			if _, e = restored.ExecContext(ctx, "ALTER TABLE euler_contract ADD COLUMN restored INTEGER"); e != nil {
				t.Fatalf("ownership/ALTER of original table was not restored: %v", e)
			}
			if _, e = restored.ExecContext(ctx, "DROP TABLE euler_restored"); e != nil {
				t.Fatalf("DROP of project-owned table was not restored: %v", e)
			}
			if kind == "postgresql" {
				if e = restored.QueryRowContext(ctx, "SELECT euler_security_identity()").Scan(&value); e != nil {
					t.Fatalf("project function execution was not restored: %v", e)
				}
				if value != resource.Username {
					t.Fatal("SECURITY DEFINER function now executes with a different, privileged identity")
				}
				if _, e = restored.ExecContext(ctx, "CALL euler_attempt_write()"); e != nil {
					t.Fatalf("procedure ownership/execution was not restored: %v", e)
				}
			}
			// Rotate through Euler, then verify actual engine authentication. Existing
			// sessions may remain open after a password change; old *new* logins must fail.
			databaseStatus(t, databaseCall(m.Handler(), &s, "POST", "/databases/"+resource.ID+"/password", nil), 200)
			newPassword := credentials()
			if newPassword == password {
				t.Fatal("password rotation returned the prior password")
			}
			restored.Close()
			oldLogin := openUser(resource.Database, resource.Username, password)
			if e = oldLogin.PingContext(ctx); e == nil {
				t.Error("old password still authenticates a new connection")
			}
			oldLogin.Close()
			newLogin := openUser(resource.Database, resource.Username, newPassword)
			if e = newLogin.PingContext(ctx); e != nil {
				t.Fatalf("new password cannot authenticate: %v", e)
			}
			if _, e = newLogin.ExecContext(ctx, "INSERT INTO euler_contract(id,value) VALUES(5,'rotated')"); e != nil {
				t.Fatalf("rotated account cannot write: %v", e)
			}
			newLogin.Close()
			databaseStatus(t, databaseCall(m.Handler(), &s, "DELETE", "/databases/"+resource.ID, nil), 200)
			databaseStatus(t, databaseCall(m.Handler(), &otherScope, "DELETE", "/databases/"+other.ID, nil), 200)
			deleted := openUser(resource.Database, resource.Username, newPassword)
			if e = deleted.PingContext(ctx); e == nil {
				t.Fatal("deleted database/user still accepts new connections")
			}
			deleted.Close()
			var completed int
			if e = m.rt.DB.QueryRow("SELECT COUNT(*) FROM database_operations WHERE resource_id=? AND state='completed'", resource.ID).Scan(&completed); e != nil || completed < 5 {
				t.Fatalf("external operations not durably tracked: count=%d error=%v", completed, e)
			}
		})
	}
}
