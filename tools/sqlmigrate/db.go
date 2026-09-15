package main

import (
	"context"
	"database/sql"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// errNoRows is the driver's "no such row" sentinel, aliased so the apply loop
// reads as a state machine (new file / unchanged file / edited file).
var errNoRows = sql.ErrNoRows

// dbHandle hides the pool from the migration logic: the migrator is a
// short-lived single-threaded process, so the tuning is minimal by design.
type dbHandle struct{ *sql.DB }

func openDB(ctx context.Context, dsn string) (*dbHandle, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &dbHandle{db}, nil
}
