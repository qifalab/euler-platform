package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Engine is also the seam used by deterministic orchestration tests. Production
// engines execute DDL against administrator-configured MySQL and PostgreSQL.
type Engine interface {
	Create(context.Context, string, string, string) error
	Drop(context.Context, string, string) error
	Rotate(context.Context, string, string, string) error
	Size(context.Context, string) (int64, error)
	ReadOnly(context.Context, string, string, bool) error
	Repair(context.Context, string, string) error
}
type engineConfig struct {
	Engine  Engine
	Host    string
	Port    int
	ToolURL string
}
type sqlEngine struct {
	db   *sql.DB
	kind string
	pg   *pgx.ConnConfig
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{1,62}$`)

func ident(value string) (string, error) {
	if !identifier.MatchString(value) {
		return "", errors.New("invalid generated identifier")
	}
	return value, nil
}
func literal(s string) string   { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
func mysqlName(s string) string { return "`" + s + "`" }
func pgName(s string) string    { return `"` + s + `"` }
func envInt(key string, fallback int) int {
	n, e := strconv.Atoi(os.Getenv(key))
	if e != nil || n < 0 {
		return fallback
	}
	return n
}
func readEngines() (map[string]engineConfig, error) {
	out := map[string]engineConfig{}
	if dsn := os.Getenv("EULER_DATABASE_MYSQL_DSN"); dsn != "" {
		cfg, e := mysql.ParseDSN(dsn)
		if e != nil {
			return nil, errors.New("invalid EULER_DATABASE_MYSQL_DSN")
		}
		cfg.MultiStatements = false
		db, e := sql.Open("mysql", cfg.FormatDSN())
		if e != nil {
			return nil, e
		}
		db.SetMaxOpenConns(4)
		host, port, _ := net.SplitHostPort(cfg.Addr)
		p, _ := strconv.Atoi(port)
		if p == 0 {
			p = 3306
		}
		if h := os.Getenv("EULER_DATABASE_MYSQL_PUBLIC_HOST"); h != "" {
			host = h
		}
		p = envInt("EULER_DATABASE_MYSQL_PUBLIC_PORT", p)
		out["mysql"] = engineConfig{&sqlEngine{db: db, kind: "mysql"}, host, p, os.Getenv("EULER_DATABASE_PHPMYADMIN_URL")}
	}
	if dsn := os.Getenv("EULER_DATABASE_POSTGRES_DSN"); dsn != "" {
		cfg, e := pgx.ParseConfig(dsn)
		if e != nil {
			return nil, errors.New("invalid EULER_DATABASE_POSTGRES_DSN")
		}
		db := stdlib.OpenDB(*cfg)
		db.SetMaxOpenConns(4)
		host := cfg.Host
		if h := os.Getenv("EULER_DATABASE_POSTGRES_PUBLIC_HOST"); h != "" {
			host = h
		}
		out["postgresql"] = engineConfig{&sqlEngine{db: db, kind: "postgresql", pg: cfg}, host, envInt("EULER_DATABASE_POSTGRES_PUBLIC_PORT", int(cfg.Port)), ""}
	}
	for _, key := range []string{"EULER_DATABASE_PHPMYADMIN_URL", "EULER_DATABASE_ADMINER_URL"} {
		if value := os.Getenv(key); value != "" {
			u, e := url.Parse(value)
			if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
				return nil, fmt.Errorf("invalid %s", key)
			}
		}
	}
	return out, nil
}
func (e *sqlEngine) Create(ctx context.Context, db, user, password string) error {
	if _, err := ident(db); err != nil {
		return err
	}
	if _, err := ident(user); err != nil {
		return err
	}
	if e.kind == "mysql" {
		if _, err := e.db.ExecContext(ctx, "CREATE DATABASE "+mysqlName(db)+" CHARACTER SET utf8mb4"); err != nil {
			return err
		}
		if _, err := e.db.ExecContext(ctx, "CREATE USER "+literal(user)+"@'%' IDENTIFIED BY "+literal(password)); err != nil {
			return err
		}
		_, err := e.db.ExecContext(ctx, "GRANT ALL PRIVILEGES ON "+mysqlName(db)+".* TO "+literal(user)+"@'%'")
		return err
	}
	if _, err := e.db.ExecContext(ctx, "CREATE ROLE "+pgName(user)+" LOGIN PASSWORD "+literal(password)); err != nil {
		return err
	}
	if _, err := e.db.ExecContext(ctx, "CREATE ROLE "+pgName(db+"_owner")+" NOLOGIN"); err != nil {
		return err
	}
	// The service owns the database; the project login never receives server-wide
	// role/database creation or membership in a privileged owner role.
	if _, err := e.db.ExecContext(ctx, "CREATE DATABASE "+pgName(db)+" OWNER "+pgName(db+"_owner")); err != nil {
		return err
	}
	return e.ReadOnly(ctx, db, user, false)
}
func (e *sqlEngine) Drop(ctx context.Context, db, user string) error {
	if _, err := ident(db); err != nil {
		return err
	}
	if _, err := ident(user); err != nil {
		return err
	}
	if e.kind == "mysql" {
		if _, err := e.db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+mysqlName(db)); err != nil {
			return err
		}
		_, err := e.db.ExecContext(ctx, "DROP USER IF EXISTS "+literal(user)+"@'%'")
		return err
	}
	if _, err := e.db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+pgName(db)+" WITH (FORCE)"); err != nil {
		return err
	}
	if _, err := e.db.ExecContext(ctx, "DROP ROLE IF EXISTS "+pgName(user)); err != nil {
		return err
	}
	_, err := e.db.ExecContext(ctx, "DROP ROLE IF EXISTS "+pgName(db+"_owner"))
	return err
}
func (e *sqlEngine) Rotate(ctx context.Context, db, user, password string) error {
	if _, err := ident(user); err != nil {
		return err
	}
	q := "ALTER ROLE " + pgName(user) + " PASSWORD " + literal(password)
	if e.kind == "mysql" {
		q = "ALTER USER " + literal(user) + "@'%' IDENTIFIED BY " + literal(password)
	}
	_, err := e.db.ExecContext(ctx, q)
	if err != nil {
		return err
	}
	return e.disconnect(ctx, db, user)
}
func (e *sqlEngine) Size(ctx context.Context, db string) (int64, error) {
	var size int64
	var err error
	if e.kind == "mysql" {
		err = e.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(data_length+index_length),0) FROM information_schema.tables WHERE table_schema=?", db).Scan(&size)
	} else {
		err = e.db.QueryRowContext(ctx, "SELECT pg_database_size($1)", db).Scan(&size)
	}
	return size, err
}
func (e *sqlEngine) ReadOnly(ctx context.Context, db, user string, readOnly bool) error {
	if _, err := ident(db); err != nil {
		return err
	}
	if _, err := ident(user); err != nil {
		return err
	}
	if e.kind == "mysql" {
		if _, err := e.db.ExecContext(ctx, "REVOKE ALL PRIVILEGES ON "+mysqlName(db)+".* FROM "+literal(user)+"@'%'"); err != nil {
			return err
		}
		priv := "ALL PRIVILEGES"
		if readOnly {
			priv = "SELECT, SHOW VIEW"
		}
		if _, err := e.db.ExecContext(ctx, "GRANT "+priv+" ON "+mysqlName(db)+".* TO "+literal(user)+"@'%'"); err != nil {
			return err
		}
		// MySQL may cache database privileges until the next USE. Terminate this
		// project's sessions so an already connected client cannot keep writing.
		return e.disconnect(ctx, db, user)
	}
	cfg := e.pg.Copy()
	cfg.Database = db
	conn := stdlib.OpenDB(*cfg)
	defer conn.Close()
	// Transfer login-owned objects before revoking writes. Merely setting
	// default_transaction_read_only is not a security boundary: users can unset it.
	commands := []string{"REVOKE ALL ON DATABASE " + pgName(db) + " FROM PUBLIC", "GRANT CONNECT ON DATABASE " + pgName(db) + " TO " + pgName(user)}
	if readOnly {
		commands = append(commands, "REASSIGN OWNED BY "+pgName(user)+" TO "+pgName(db+"_owner"), "REVOKE CREATE,TEMPORARY ON DATABASE "+pgName(db)+" FROM "+pgName(user))
	} else {
		commands = append(commands, "GRANT TEMPORARY ON DATABASE "+pgName(db)+" TO "+pgName(user))
	}
	for _, q := range commands {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	rows, err := conn.QueryContext(ctx, "SELECT nspname FROM pg_namespace WHERE nspname NOT LIKE 'pg_%' AND nspname <> 'information_schema'")
	if err != nil {
		return err
	}
	var schemas []string
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		schemas = append(schemas, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, schema := range schemas {
		sn := `"` + strings.ReplaceAll(schema, `"`, `""`) + `"`
		commands = []string{"REVOKE ALL ON SCHEMA " + sn + " FROM PUBLIC", "REVOKE ALL ON ALL TABLES IN SCHEMA " + sn + " FROM " + pgName(user), "REVOKE ALL ON ALL SEQUENCES IN SCHEMA " + sn + " FROM " + pgName(user), "REVOKE ALL ON SCHEMA " + sn + " FROM " + pgName(user)}
		if readOnly {
			commands = append(commands, "GRANT USAGE ON SCHEMA "+sn+" TO "+pgName(user), "GRANT SELECT ON ALL TABLES IN SCHEMA "+sn+" TO "+pgName(user), "REVOKE EXECUTE ON ALL ROUTINES IN SCHEMA "+sn+" FROM PUBLIC,"+pgName(user))
		} else {
			commands = append(commands, "GRANT USAGE,CREATE ON SCHEMA "+sn+" TO "+pgName(user), "GRANT ALL ON ALL TABLES IN SCHEMA "+sn+" TO "+pgName(user), "GRANT ALL ON ALL SEQUENCES IN SCHEMA "+sn+" TO "+pgName(user), "GRANT EXECUTE ON ALL ROUTINES IN SCHEMA "+sn+" TO "+pgName(user))
		}
		for _, q := range commands {
			if _, err = conn.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if !readOnly {
		if err := restoreObjectOwners(ctx, conn, db+"_owner", user); err != nil {
			return err
		}
	}
	return e.disconnect(ctx, db, user)
}
func (e *sqlEngine) Repair(ctx context.Context, db, user string) error {
	return e.ReadOnly(ctx, db, user, false)
}

func (e *sqlEngine) disconnect(ctx context.Context, db, user string) error {
	if e.kind == "postgresql" {
		_, err := e.db.ExecContext(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename=$1 AND datname=$2 AND pid<>pg_backend_pid()", user, db)
		return err
	}
	rows, err := e.db.QueryContext(ctx, "SELECT ID FROM information_schema.PROCESSLIST WHERE USER=?", user)
	if err != nil {
		return err
	}
	var ids []uint64
	for rows.Next() {
		var id uint64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = e.db.ExecContext(ctx, "KILL CONNECTION "+strconv.FormatUint(id, 10)); err != nil {
			var mysqlErr *mysql.MySQLError
			if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1094 {
				return err
			}
		}
	}
	return nil
}

// Transfer only project-owned schema objects back after thawing. REASSIGN OWNED
// in this direction would also hand over database ownership and must not be used.
func restoreObjectOwners(ctx context.Context, conn *sql.DB, owner, user string) error {
	queries := []string{
		`SELECT 'ALTER ' || CASE c.relkind WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW' WHEN 'S' THEN 'SEQUENCE' WHEN 'f' THEN 'FOREIGN TABLE' ELSE 'TABLE' END || ' ' || quote_ident(n.nspname)||'.'||quote_ident(c.relname)||' OWNER TO '||quote_ident($2) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_roles r ON r.oid=c.relowner WHERE r.rolname=$1 AND c.relkind IN ('r','p','v','m','S','f') AND n.nspname NOT LIKE 'pg_%' AND n.nspname<>'information_schema' ORDER BY CASE WHEN c.relkind='S' THEN 1 ELSE 0 END`,
		`SELECT 'ALTER ' || CASE p.prokind WHEN 'p' THEN 'PROCEDURE' WHEN 'a' THEN 'AGGREGATE' ELSE 'FUNCTION' END || ' ' || quote_ident(n.nspname)||'.'||quote_ident(p.proname)||'('||pg_get_function_identity_arguments(p.oid)||') OWNER TO '||quote_ident($2) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace JOIN pg_roles r ON r.oid=p.proowner WHERE r.rolname=$1 AND n.nspname NOT LIKE 'pg_%' AND n.nspname<>'information_schema'`,
		`SELECT 'ALTER TYPE ' || quote_ident(n.nspname)||'.'||quote_ident(t.typname)||' OWNER TO '||quote_ident($2) FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace JOIN pg_roles r ON r.oid=t.typowner WHERE r.rolname=$1 AND t.typrelid=0 AND t.typelem=0 AND t.typtype IN ('e','d','r') AND n.nspname NOT LIKE 'pg_%' AND n.nspname<>'information_schema'`,
	}
	for _, q := range queries {
		rows, err := conn.QueryContext(ctx, q, owner, user)
		if err != nil {
			return err
		}
		var commands []string
		for rows.Next() {
			var command string
			if err = rows.Scan(&command); err != nil {
				rows.Close()
				return err
			}
			commands = append(commands, command)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, command := range commands {
			if _, err = conn.ExecContext(ctx, command); err != nil {
				return err
			}
		}
	}
	return nil
}
