package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	mysql "github.com/go-sql-driver/mysql"
)

// sessionDriver is a fake database/sql driver that records which connection
// ran each statement, because the stats-expiry override is a per-session
// setting: it has no effect on a SELECT that runs on another connection.
type sessionDriver struct {
	mu     sync.Mutex
	opened int
	log    []string // "<connection>:<statement>"
	setErr error    // result of the SET statement
}
type sessionConn struct {
	d  *sessionDriver
	id int
}
type sizeRows struct{ done bool }

func (d *sessionDriver) Open(string) (driver.Conn, error) { return d.Connect(context.Background()) }
func (d *sessionDriver) Connect(context.Context) (driver.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.opened++
	return &sessionConn{d: d, id: d.opened}, nil
}
func (d *sessionDriver) Driver() driver.Driver { return d }
func (d *sessionDriver) statements() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.log...)
}
func (c *sessionConn) record(query string) {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	c.d.log = append(c.d.log, fmt.Sprintf("%d:%s", c.id, query))
}
func (c *sessionConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (c *sessionConn) Close() error                        { return nil }
func (c *sessionConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }
func (c *sessionConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.record(query)
	if strings.HasPrefix(query, "SET ") && c.d.setErr != nil {
		return nil, c.d.setErr
	}
	return driver.RowsAffected(0), nil
}
func (c *sessionConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.record(query)
	return &sizeRows{}, nil
}
func (r *sizeRows) Columns() []string { return []string{"size"} }
func (r *sizeRows) Close() error      { return nil }
func (r *sizeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = int64(4096)
	return nil
}

// MySQL 8 caches information_schema.tables sizes for 24 h by default, so the
// 5-minute quota check could leave an over-quota database writable for a day.
func TestMySQLSizeReadsCurrentStatisticsOnOneSession(t *testing.T) {
	const set = "SET SESSION information_schema_stats_expiry=0"
	for _, tc := range []struct {
		name    string
		setErr  error
		wantErr bool
	}{
		{"current statistics", nil, false},
		{"server without the variable", &mysql.MySQLError{Number: 1193, Message: "Unknown system variable"}, false},
		{"refused setting", &mysql.MySQLError{Number: 1227, Message: "Access denied"}, true},
		{"lost connection", errors.New("connection reset"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &sessionDriver{setErr: tc.setErr}
			db := sql.OpenDB(d)
			defer db.Close()
			// Pool calls outside a pinned connection each get a new session, so
			// a SET and a SELECT on different sessions are told apart.
			db.SetMaxIdleConns(0)
			e := &sqlEngine{db: db, kind: "mysql"}
			size, err := e.Size(context.Background(), "e_project")
			log := d.statements()
			if len(log) == 0 || log[0] != "1:"+set {
				t.Fatalf("size read did not disable the statistics cache first: %+q", log)
			}
			if tc.wantErr {
				if err == nil || len(log) != 1 {
					t.Fatalf("SET failure must be returned without reading a size: size=%d err=%v log=%+q", size, err, log)
				}
				return
			}
			if err != nil || size != 4096 {
				t.Fatalf("size=%d err=%v", size, err)
			}
			if len(log) != 2 || !strings.HasPrefix(log[1], "1:SELECT COALESCE(SUM(data_length+index_length),0) FROM information_schema.tables") {
				t.Fatalf("size query must follow the SET on the same session: %+q", log)
			}
		})
	}
	t.Run("postgresql", func(t *testing.T) {
		d := &sessionDriver{}
		db := sql.OpenDB(d)
		defer db.Close()
		e := &sqlEngine{db: db, kind: "postgresql"}
		if size, err := e.Size(context.Background(), "e_project"); err != nil || size != 4096 {
			t.Fatalf("size=%d err=%v", size, err)
		}
		if log := d.statements(); len(log) != 1 || log[0] != "1:SELECT pg_database_size($1)" {
			t.Fatalf("PostgreSQL size must be one pg_database_size query: %+q", log)
		}
	})
}
