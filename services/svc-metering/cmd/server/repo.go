package main

// The aggregate ledger (repo.go). In production the RAW readings live in
// ClickHouse (cloud.metering.usage.raw keeps only 3 days); the durable MySQL
// form is the hourly aggregate — metering_record, upserted by its natural key
// (resource, item, hour) so an aggregation re-run can never inflate a bill
// (03§4.2.5: 聚合任务失败可无限重算). This file makes svc-metering write its
// aggCache through to that table and rebuild the cache from it on restart.
//
// The bill tables (bill_main/bill_detail) belong to svc-billing's ledger store,
// which persists them already; recon/backfill tasks are the batch engine's
// concern (phase 3).

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/starcloud/sc-platform/billing"
	"github.com/starcloud/sc-platform/metering"
	"github.com/starcloud/sc-platform/pricing"
	"github.com/starcloud/sc-platform/storage"
)

// aggRepo persists hourly aggregates.
type aggRepo interface {
	// UpsertHourly writes one aggregate under its deterministic identity: the
	// same (resource, item, hour) — hence the same AggID — updates the row
	// instead of appending a second total.
	UpsertHourly(ctx context.Context, u metering.HourlyUsage) error
	// LoadAll returns every persisted aggregate, so a restarted process can
	// rebuild its aggCache and settle previous hours without the raw records.
	LoadAll(ctx context.Context) ([]metering.HourlyUsage, error)
}

// --- SQL implementation ------------------------------------------------------

type sqlAggRepo struct {
	db *sql.DB
}

var _ aggRepo = (*sqlAggRepo)(nil)

func newSQLAggRepo(_ context.Context, db *sql.DB) *sqlAggRepo {
	return &sqlAggRepo{db: db}
}

// sourceCode maps the DDL's `source` TINYINT (1推送 2拉取 3补算) from the
// domain's BatchID. The concepts are adjacent, not identical — the batch is the
// transport, the source is the collector — and "backfill" is the only value the
// mapping can state with confidence; everything real-time-ish pushes.
func sourceCode(batchID string) int {
	if batchID == metering.BatchBackfill {
		return 3
	}
	return 1
}

// UpsertHourly upserts by uk_res_item_hour. quantity/covered_ratio/windows are
// overwritten on re-run: a late-corrected aggregate must replace the stale one,
// not average with it.
func (r *sqlAggRepo) UpsertHourly(ctx context.Context, u metering.HourlyUsage) error {
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO metering_record
		   (agg_id, account_id, resource_id, resource_type, region, metering_item,
		    metering_hour, quantity, covered_ratio, windows_seen, windows_expected,
		    source, batch_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
		    quantity = VALUES(quantity), covered_ratio = VALUES(covered_ratio),
		    windows_seen = VALUES(windows_seen), windows_expected = VALUES(windows_expected),
		    agg_id = VALUES(agg_id)`,
		u.AggID, u.AccountID, u.ResourceID, u.ResourceType, u.Region, u.MeteringItem,
		u.HourStart.UTC(), u.TotalQuantity.String(), u.CoveredRatio, u.WindowsSeen,
		u.WindowsExpected, sourceCode(u.BatchID), u.BatchID); err != nil {
		return fmt.Errorf("metering: upsert hourly %s: %w", u.AggID, err)
	}
	return nil
}

func (r *sqlAggRepo) LoadAll(ctx context.Context) ([]metering.HourlyUsage, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT agg_id, account_id, resource_id, resource_type, region, metering_item,
		        metering_hour, quantity, covered_ratio, windows_seen, windows_expected, batch_id
		   FROM metering_record
		  ORDER BY metering_hour, resource_id, metering_item`)
	if err != nil {
		return nil, fmt.Errorf("metering: load aggregates: %w", err)
	}
	defer rows.Close()

	out := make([]metering.HourlyUsage, 0, 32)
	for rows.Next() {
		var (
			u        metering.HourlyUsage
			quantity string
			batchID  string
		)
		if err := rows.Scan(&u.AggID, &u.AccountID, &u.ResourceID, &u.ResourceType,
			&u.Region, &u.MeteringItem, &u.HourStart, &quantity, &u.CoveredRatio,
			&u.WindowsSeen, &u.WindowsExpected, &batchID); err != nil {
			return nil, fmt.Errorf("metering: scan aggregate: %w", err)
		}
		q, err := metering.ParseQuantity(quantity)
		if err != nil {
			return nil, fmt.Errorf("metering: aggregate %s quantity %q: %w", u.AggID, quantity, err)
		}
		u.TotalQuantity = q
		u.BatchID = batchID
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("metering: load aggregates: %w", err)
	}
	return out, nil
}

// newAggRepo picks the backend. Persistence is opt-in (pkg-go/storage doc):
// with SC_DB_DSN set the aggregate ledger lives in metering_db, so a restart
// keeps every settled hour; unset, the in-memory cache keeps the demo and
// `go test` dependency-free.
//
// A configured DSN that cannot be reached — or a schema sqlmigrate never
// touched — is a startup failure: the aggregate ledger is the basis every bill
// must reconcile against, and silently dropping it is not a degradation.
func newAggRepo(ctx context.Context) (aggRepo, error) {
	db, ok, err := storage.MustOpenFor(ctx, "metering_db")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	if err := storage.EnsureMigrated(ctx, db, "metering_db"); err != nil {
		return nil, err
	}
	return newSQLAggRepo(ctx, db), nil
}

// newMeteringStoreWith wires a store to an aggregate ledger (nil = in-memory
// only). The raw-record maps and the price/pool demo config stay in memory:
// raw readings belong to ClickHouse in production, and the demo price snapshot
// is process configuration, not ledger state.
func newMeteringStoreWith(agg aggRepo) (*meteringStore, error) {
	s := &meteringStore{
		records:     make(map[string][]metering.UsageRecord),
		accounts:    make(map[string]int64),
		regions:     make(map[string]string),
		types:       make(map[string]string),
		products:    make(map[string]string),
		aggCache:    make(map[string]metering.HourlyUsage),
		unitPrices:  make(map[string]pricing.Amount),
		snapshotIDs: make(map[string]string),
		pools:       make(map[int64][]billing.Pool),
		now:         time.Now,
		agg:         agg,
	}
	if agg == nil {
		s.seed()
		return s, nil
	}
	if err := s.rehydrate(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

// cacheAgg is the single write path into the aggregate cache: memory first,
// then the ledger. A ledger failure is logged, not fatal — the aggregate is
// re-derivable from the raw records still in the cache, and failing the ingest
// that already produced a good reading would punish the collector for a
// database problem.
func (s *meteringStore) cacheAgg(u metering.HourlyUsage) {
	s.aggCache[u.AggID] = u
	if s.agg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.agg.UpsertHourly(ctx, u); err != nil {
		slog.Error("aggregate persist failed", "aggId", u.AggID, "err", err)
	}
}

// rehydrate rebuilds the in-process state from the durable ledger: the
// aggregate cache, plus the resource metadata maps the ownership checks read.
// The product-code map is the one thing an aggregate does not carry; it
// re-registers on the resource's first post-restart ingest, which is the same
// channel that populated it in the first place.
func (s *meteringStore) rehydrate(ctx context.Context) error {
	usage, err := s.agg.LoadAll(ctx)
	if err != nil {
		return err
	}
	for _, u := range usage {
		s.aggCache[u.AggID] = u
		if _, ok := s.accounts[u.ResourceID]; !ok {
			s.accounts[u.ResourceID] = u.AccountID
			s.regions[u.ResourceID] = u.Region
			s.types[u.ResourceID] = u.ResourceType
		}
	}
	slog.Info("metering aggregates rehydrated", "count", len(usage))
	return nil
}
