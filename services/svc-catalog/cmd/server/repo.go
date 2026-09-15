package main

// The catalogue loader (repo.go). The quote path (询价) reads four tables —
// t_product / t_sku / t_pricing_rule / t_promo_policy — and this file decides
// where they come from: the in-memory seed when persistence is off, trade_db
// when it is on. Read-only by design: the catalogue is written by ops (the seed
// SQL, the ops back-office), never by this service, and t_pricing_rule is
// append-only (01§12.3 — 价格可追溯、不可改写 是资损防控底线).

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/starcloud/sc-platform/pricing"
	"github.com/starcloud/sc-platform/storage"
)

// catalogData is one loaded snapshot of the quote-path catalogue.
type catalogData struct {
	Products []product
	SKUs     []sku
	Rules    []pricing.PricingRule
	Promos   []pricing.Promotion
}

// catalogRepo loads the quote-path tables.
type catalogRepo interface {
	Load(ctx context.Context) (catalogData, error)
}

// --- in-memory implementation ------------------------------------------------

// memCatalogRepo serves the seed lists. It exists so `go test` and the demo run
// without a database; the data is identical in shape to what the DB loader
// returns (and where the two drift, the DB wins — the V4 migration corrected
// the seed's placement columns to match this list).
type memCatalogRepo struct{}

func newMemCatalogRepo() *memCatalogRepo {
	return &memCatalogRepo{}
}

func (r *memCatalogRepo) Load(_ context.Context) (catalogData, error) {
	return catalogData{
		Products: seedProducts(),
		SKUs:     seedSKUs(),
		Rules:    seedRules(),
		Promos:   seedPromos(),
	}, nil
}

// --- SQL implementation ------------------------------------------------------

type sqlCatalogRepo struct {
	db *sql.DB
}

var _ catalogRepo = (*sqlCatalogRepo)(nil)

func newSQLCatalogRepo(_ context.Context, db *sql.DB) *sqlCatalogRepo {
	return &sqlCatalogRepo{db: db}
}

// Load reads the four quote-path tables. A schema that holds no products is an
// error here rather than a silently empty catalogue: the failure a startup
// guard reports ("seed never ran") is fixable in seconds, while an empty
// catalogue discovered by the first customer's 404 is not.
func (r *sqlCatalogRepo) Load(ctx context.Context) (catalogData, error) {
	var data catalogData
	var err error

	if data.Products, err = r.loadProducts(ctx); err != nil {
		return data, err
	}
	if data.SKUs, err = r.loadSKUs(ctx); err != nil {
		return data, err
	}
	if data.Rules, err = r.loadRules(ctx); err != nil {
		return data, err
	}
	if data.Promos, err = r.loadPromos(ctx); err != nil {
		return data, err
	}
	return data, nil
}

func (r *sqlCatalogRepo) loadProducts(ctx context.Context) ([]product, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT product_code, product_name, category, description, resource_type,
		        region_scope, cross_az, status, owner_team
		   FROM t_product
		  ORDER BY product_code`)
	if err != nil {
		return nil, fmt.Errorf("catalog: load products: %w", err)
	}
	defer rows.Close()

	out := make([]product, 0, 16)
	for rows.Next() {
		var (
			p               product
			desc, ownerTeam sql.NullString
		)
		if err := rows.Scan(&p.ProductCode, &p.ProductName, &p.Category, &desc,
			&p.ResourceType, &p.RegionScope, &p.CrossAZ, &p.Status, &ownerTeam); err != nil {
			return nil, fmt.Errorf("catalog: scan product: %w", err)
		}
		p.Description = desc.String
		p.OwnerTeam = ownerTeam.String
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *sqlCatalogRepo) loadSKUs(ctx context.Context) ([]sku, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT sku_code, product_code, charge_type, spec_json, status
		   FROM t_sku
		  ORDER BY sku_code`)
	if err != nil {
		return nil, fmt.Errorf("catalog: load skus: %w", err)
	}
	defer rows.Close()

	out := make([]sku, 0, 32)
	for rows.Next() {
		var (
			s      sku
			status int // TINYINT in the schema; the domain renders it as a string
		)
		if err := rows.Scan(&s.SKUCode, &s.ProductCode, &s.ChargeType, &s.SpecJSON, &status); err != nil {
			return nil, fmt.Errorf("catalog: scan sku: %w", err)
		}
		s.Status = fmt.Sprintf("%d", status)
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *sqlCatalogRepo) loadRules(ctx context.Context) ([]pricing.PricingRule, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT rule_id, sku_code, region_id, duration_unit, list_price, customer_level,
		        effective_from, effective_to
		   FROM t_pricing_rule
		  ORDER BY rule_id`)
	if err != nil {
		return nil, fmt.Errorf("catalog: load pricing rules: %w", err)
	}
	defer rows.Close()

	out := make([]pricing.PricingRule, 0, 32)
	for rows.Next() {
		var (
			rule        pricing.PricingRule
			listPrice   string // DECIMAL(12,6): parsed into Amount, never a float
			level       string
			effectiveTo sql.NullTime
		)
		if err := rows.Scan(&rule.RuleID, &rule.SKUCode, &rule.RegionID, &rule.DurationUnit,
			&listPrice, &level, &rule.EffectiveFrom, &effectiveTo); err != nil {
			return nil, fmt.Errorf("catalog: scan pricing rule: %w", err)
		}
		price, err := pricing.ParseAmount(listPrice)
		if err != nil {
			return nil, fmt.Errorf("catalog: rule %d list price %q: %w", rule.RuleID, listPrice, err)
		}
		rule.ListPrice = price
		rule.CustomerLevel = level
		if effectiveTo.Valid {
			rule.EffectiveTo = effectiveTo.Time
		} // zero otherwise: the domain's open-ended marker
		out = append(out, rule)
	}
	return out, rows.Err()
}

func (r *sqlCatalogRepo) loadPromos(ctx context.Context) ([]pricing.Promotion, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT promo_id, promo_type, scope_type, scope_ref, rate_bp, fixed_price,
		        user_tag, start_at, end_at
		   FROM t_promo_policy
		  WHERE status = 1
		  ORDER BY promo_id`)
	if err != nil {
		return nil, fmt.Errorf("catalog: load promotions: %w", err)
	}
	defer rows.Close()

	out := make([]pricing.Promotion, 0, 8)
	for rows.Next() {
		var (
			promo      pricing.Promotion
			rateBp     sql.NullInt64
			fixedPrice sql.NullString
		)
		if err := rows.Scan(&promo.PromoID, &promo.PromoType, &promo.ScopeType, &promo.ScopeRef,
			&rateBp, &fixedPrice, &promo.UserTag, &promo.StartAt, &promo.EndAt); err != nil {
			return nil, fmt.Errorf("catalog: scan promotion: %w", err)
		}
		if rateBp.Valid {
			promo.RateBasisPoints = rateBp.Int64
		}
		if fixedPrice.Valid {
			price, err := pricing.ParseAmount(fixedPrice.String)
			if err != nil {
				return nil, fmt.Errorf("catalog: promo %s fixed price %q: %w", promo.PromoID, fixedPrice.String, err)
			}
			promo.FixedPrice = price
		}
		out = append(out, promo)
	}
	return out, rows.Err()
}

// validateCatalog rejects a catalogue that cannot serve a quote. Promotions may
// legitimately be empty (no active campaign); products, SKUs and pricing rules
// may not — an empty set there means the seed never ran, and the quote path
// would answer every request with "规格不存在", which reads like an outage.
func validateCatalog(data catalogData) error {
	switch {
	case len(data.Products) == 0:
		return fmt.Errorf("catalog: no products loaded (did the seed run?)")
	case len(data.SKUs) == 0:
		return fmt.Errorf("catalog: no SKUs loaded (did the seed run?)")
	case len(data.Rules) == 0:
		return fmt.Errorf("catalog: no pricing rules loaded (did the seed run?)")
	}
	return nil
}

// newCatalogRepo picks the backend. Persistence is opt-in (pkg-go/storage doc):
// with SC_DB_DSN set the catalogue is loaded from trade_db — ops edits prices
// and placements in the database and a restart picks them up; unset, the seed
// lists keep the demo and `go test` dependency-free.
//
// The region/zone, image and category inventories have no DDL tables: they are
// platform inventory metadata, static per release, and both modes serve them
// from the same seed. A configured DSN that cannot be reached — or a schema
// whose seed never ran — is a startup failure: an empty catalogue is an outage,
// not a degraded mode.
func newCatalogRepo(ctx context.Context) (catalogRepo, error) {
	db, ok, err := storage.MustOpenFor(ctx, "trade_db")
	if err != nil {
		return nil, err
	}
	if !ok {
		return newMemCatalogRepo(), nil
	}
	if err := storage.EnsureMigrated(ctx, db, "trade_db"); err != nil {
		return nil, err
	}
	return newSQLCatalogRepo(ctx, db), nil
}

// newCatalogStoreFrom loads the catalogue and builds the store; every startup
// with a DSN takes this path.
func newCatalogStoreFrom(ctx context.Context, repo catalogRepo) (*catalogStore, error) {
	data, err := repo.Load(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateCatalog(data); err != nil {
		return nil, err
	}
	return newCatalogStoreWith(data), nil
}

// persistentCatalogRepo reports whether a repo reads trade_db, for the startup
// log.
func persistentCatalogRepo(r catalogRepo) bool {
	_, ok := r.(*sqlCatalogRepo)
	return ok
}

// loadDeadline bounds one catalogue load in main().
const loadDeadline = 30 * time.Second
