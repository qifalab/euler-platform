package database

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestDependencyStatusDistinguishesAbsentConnectedAndFailed(t *testing.T) {
	m := &Module{engines: map[string]engineConfig{}}
	if got := m.DependencyStatus(context.Background()); got["mysql"] != "not_configured" || got["postgresql"] != "not_configured" {
		t.Fatal(got)
	}
	// An actual SQL driver exercises PingContext and a closed-connection failure;
	// real MySQL/PostgreSQL provisioning is covered by the engine integration suite.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	m.engines["mysql"] = engineConfig{Engine: &sqlEngine{db: db, kind: "mysql"}}
	if got := m.DependencyStatus(context.Background()); got["mysql"] != "up" {
		t.Fatal(got)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if got := m.DependencyStatus(context.Background()); got["mysql"] != "down" {
		t.Fatal(got)
	}
	m.engines["postgresql"] = engineConfig{Engine: &fakeEngine{}}
	if got := m.DependencyStatus(context.Background()); got["postgresql"] != "configured_unverified" {
		t.Fatal(got)
	}
}
