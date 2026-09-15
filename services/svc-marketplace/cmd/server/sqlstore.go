package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/storage"
)

// sqlStore is the MySQL-backed Store over trade_db's marketplace_listing and
// marketplace_settlement (09§5.2 M-10).
//
// # Ids come from the 号段 allocator, not a process counter
//
// V1 describes both ids as "服务内序列". Kept as a process-local counter — which is
// what the in-memory store uses — a restart starts issuing 1 again and the first
// new listing (or settlement) collides with a committed row. The settlement case is
// the expensive one: a lost settlement is a partner's share of an order.
//
// # The unique indexes are the arbiters, not the code above them
//
// uk_order makes "one settlement per order" a database fact, so a retried
// settlement cannot pay a partner twice even if two processes race; the listing
// path relies on the primary key the same way. Both are reported as the errors the
// in-memory store returns for the same situations, so the handlers do not need to
// know which backend they are talking to.
type sqlStore struct {
	db            *sql.DB
	listingIDs    func() int64
	settlementIDs func() int64
}

var _ Store = (*sqlStore)(nil)

// statementTimeout bounds one store call; the port has no context parameter.
const statementTimeout = 10 * time.Second

// newSQLStore wires the store to its two id sequences (seeded by trade_db V2).
func newSQLStore(ctx context.Context, db *sql.DB) (*sqlStore, error) {
	listings, err := storage.OpenSequence(ctx, db, "marketplace_listing", 1000)
	if err != nil {
		return nil, err
	}
	settlements, err := storage.OpenSequence(ctx, db, "marketplace_settlement", 1000)
	if err != nil {
		return nil, err
	}
	return &sqlStore{
		db:            db,
		listingIDs:    listings.NextFunc(),
		settlementIDs: settlements.NextFunc(),
	}, nil
}

// newStore picks the backend. Persistence is opt-in (pkg-go/storage doc): with
// EULER_DB_DSN set, listings and settlements live in trade_db; unset, the in-memory
// store keeps the demo and `go test` dependency-free.
func newStore(ctx context.Context) (Store, error) {
	db, ok, err := storage.MustOpenFor(ctx, "trade_db")
	if err != nil {
		return nil, err
	}
	if !ok {
		return newMemoryStore(), nil
	}
	if err := storage.EnsureMigrated(ctx, db, "trade_db"); err != nil {
		return nil, err
	}
	return newSQLStore(ctx, db)
}

// persistentStore reports whether a store is backed by MySQL, for the startup log.
func persistentStore(s Store) bool {
	_, ok := s.(*sqlStore)
	return ok
}

const listingColumns = `listing_id, partner_id, name, category, openapi_url,
	status, partner_rate_bps, created_at, updated_at`

const settlementColumns = `settlement_id, order_id, listing_id, partner_id,
	gross_micro, partner_rate_bps, partner_micro, platform_micro, settled_at`

// CreateListing inserts a listing, issuing an id when the caller did not supply
// one (the in-memory store's contract).
func (s *sqlStore) CreateListing(l *Listing) error {
	if l.ListingID == 0 {
		l.ListingID = s.listingIDs()
	}
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO marketplace_listing
		   (listing_id, partner_id, name, category, openapi_url, status, partner_rate_bps,
		    created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ListingID, l.PartnerID, l.Name, string(l.Category), l.OpenAPIURL,
		string(l.Status), l.PartnerRateBps, l.CreatedAt.UTC(), l.UpdatedAt.UTC()); err != nil {
		if storage.IsDuplicateKey(err) {
			return fmt.Errorf("marketplace: duplicate listing id %d", l.ListingID)
		}
		return fmt.Errorf("marketplace: create listing %d: %w", l.ListingID, err)
	}
	return nil
}

func (s *sqlStore) GetListing(id int64) (*Listing, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	l, err := scanListing(s.db.QueryRowContext(ctx,
		`SELECT `+listingColumns+` FROM marketplace_listing WHERE listing_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("marketplace: listing %d not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("marketplace: get listing %d: %w", id, err)
	}
	return l, nil
}

// ListApproved returns the customer-facing catalogue for a category.
func (s *sqlStore) ListApproved(category ListingCategory) ([]*Listing, error) {
	return s.ListByStatus(StatusApproved, category)
}

// ListByStatus returns listings in one review state, newest id last.
func (s *sqlStore) ListByStatus(status ListingStatus, category ListingCategory) ([]*Listing, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	// The two filters are built as one statement with a bound category; an empty
	// category means "all categories", matching the in-memory store.
	query := `SELECT ` + listingColumns + ` FROM marketplace_listing WHERE status = ?`
	args := []any{string(status)}
	if category != "" {
		query += ` AND category = ?`
		args = append(args, string(category))
	}
	query += ` ORDER BY listing_id`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("marketplace: list listings: %w", err)
	}
	defer rows.Close()

	out := make([]*Listing, 0, 8)
	for rows.Next() {
		l, err := scanListing(rows)
		if err != nil {
			return nil, fmt.Errorf("marketplace: scan listing: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("marketplace: list listings: %w", err)
	}
	return out, nil
}

// UpdateListingStatus performs one review transition under the row lock, refusing
// a transition whose "from" no longer holds — the same guard the in-memory store
// applies, so two concurrent approvals cannot both succeed.
func (s *sqlStore) UpdateListingStatus(id int64, from, to ListingStatus) (*Listing, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	err := storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		var current string
		err := tx.QueryRowContext(ctx,
			`SELECT status FROM marketplace_listing WHERE listing_id = ? FOR UPDATE`, id).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("marketplace: listing %d not found", id)
		}
		if err != nil {
			return fmt.Errorf("marketplace: lock listing %d: %w", id, err)
		}
		if ListingStatus(current) != from {
			return fmt.Errorf("marketplace: listing %d is %s, not %s", id, current, from)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE marketplace_listing SET status = ?, updated_at = ? WHERE listing_id = ?`,
			string(to), time.Now().UTC(), id); err != nil {
			return fmt.Errorf("marketplace: update listing %d: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetListing(id)
}

// CreateSettlement writes an immutable split record. uk_order makes the order the
// idempotency key: a replay is refused rather than paid twice.
func (s *sqlStore) CreateSettlement(r *SettlementRecord) error {
	if r.SettlementID == 0 {
		r.SettlementID = s.settlementIDs()
	}
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO marketplace_settlement
		   (settlement_id, order_id, listing_id, partner_id, gross_micro,
		    partner_rate_bps, partner_micro, platform_micro, settled_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.SettlementID, r.OrderID, r.ListingID, r.PartnerID, r.Gross.String(),
		r.PartnerRateBps, r.Partner.String(), r.Platform.String(), r.SettledAt.UTC()); err != nil {
		if storage.IsDuplicateKey(err) {
			return fmt.Errorf("marketplace: order %s already settled", r.OrderID)
		}
		return fmt.Errorf("marketplace: create settlement %d: %w", r.SettlementID, err)
	}
	return nil
}

// GetSettlementByOrder answers the idempotency question the settle handler asks
// before doing any work.
func (s *sqlStore) GetSettlementByOrder(orderID string) (*SettlementRecord, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	r, err := scanSettlement(s.db.QueryRowContext(ctx,
		`SELECT `+settlementColumns+` FROM marketplace_settlement WHERE order_id = ?`, orderID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("marketplace: get settlement for order %s: %w", orderID, err)
	}
	return r, true, nil
}

// rowScanner is the shared shape of *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanListing(sc rowScanner) (*Listing, error) {
	var (
		l        Listing
		category string
		status   string
	)
	if err := sc.Scan(&l.ListingID, &l.PartnerID, &l.Name, &category, &l.OpenAPIURL,
		&status, &l.PartnerRateBps, &l.CreatedAt, &l.UpdatedAt); err != nil {
		return nil, err
	}
	l.Category = ListingCategory(category)
	l.Status = ListingStatus(status)
	return &l, nil
}

func scanSettlement(sc rowScanner) (*SettlementRecord, error) {
	var (
		r                        SettlementRecord
		gross, partner, platform string
	)
	if err := sc.Scan(&r.SettlementID, &r.OrderID, &r.ListingID, &r.PartnerID,
		&gross, &r.PartnerRateBps, &partner, &platform, &r.SettledAt); err != nil {
		return nil, err
	}
	var err error
	if r.Gross, err = pricing.ParseAmount(gross); err != nil {
		return nil, fmt.Errorf("settlement %d gross: %w", r.SettlementID, err)
	}
	if r.Partner, err = pricing.ParseAmount(partner); err != nil {
		return nil, fmt.Errorf("settlement %d partner: %w", r.SettlementID, err)
	}
	if r.Platform, err = pricing.ParseAmount(platform); err != nil {
		return nil, fmt.Errorf("settlement %d platform: %w", r.SettlementID, err)
	}
	return &r, nil
}
