package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/storage"
)

// ErrRuleConflict is returned when a Save loses the optimistic lock: the row
// moved between the caller's read and its write (two console tabs editing the
// same rule). Handlers map it to 409.
var ErrRuleConflict = errors.New("monitor: rule version conflict")

// ruleRepo is the persistence boundary for alert rules (alert_rule,
// 03§4.4.1/§4.4.5). svc-monitor writes, alert-engine evaluates from the same
// rows — which is exactly why the rules cannot live in a process map: an
// engine restart must not forget what a tenant asked to be alerted about, and
// a monitor restart must not forget a rule the engine is mid-evaluation on.
//
// The natural key (uk_acc_rule: account, product, resource_type, metric) is the
// write-side idempotency contract: one rule per metric per tenant, "create
// again" updates the existing rule rather than duplicating the alerting.
type ruleRepo interface {
	List(acct int64) ([]*AlertRule, error)
	// Get returns the rule for (account, id), or nil when either half misses —
	// a rule id is not globally unique knowledge, it is scoped to the shard.
	Get(acct, id int64) (*AlertRule, error)
	// ByNaturalKey returns the row uk_acc_rule covers, or nil.
	ByNaturalKey(acct int64, product, rtype, metric string) (*AlertRule, error)
	// Create inserts a new rule, assigning RuleID. Duplicate natural keys are
	// impossible through this path only if the caller checked ByNaturalKey
	// first; the unique key backs them up with an error.
	Create(rule *AlertRule) error
	// Save applies the caller's edit under the optimistic lock: the UPDATE
	// matches only at expectedVersion, and zero matches is ErrRuleConflict.
	Save(rule *AlertRule, expectedVersion int) error
	Delete(acct, id int64) error
}

// --- in-memory implementation ------------------------------------------------

// memRuleRepo has value semantics, matching the SQL repo: Get/List/ByNaturalKey
// hand out copies and Save copies in. Handing out the internal pointer would let
// a handler's `rule.Version++` mutate the stored row BEFORE the guard runs, and
// the write would then fight its own optimistic lock.
type memRuleRepo struct {
	mu    sync.RWMutex
	rules map[int64]AlertRule
	seq   int64
}

func newMemRuleRepo() *memRuleRepo {
	return &memRuleRepo{rules: make(map[int64]AlertRule)}
}

func (s *memRuleRepo) List(acct int64) ([]*AlertRule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*AlertRule, 0)
	for _, r := range s.rules {
		if r.AccountID == acct {
			copied := r
			out = append(out, &copied)
		}
	}
	return out, nil
}

func (s *memRuleRepo) Get(acct, id int64) (*AlertRule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.rules[id]
	if !ok || r.AccountID != acct {
		return nil, nil
	}
	return &r, nil
}

func (s *memRuleRepo) ByNaturalKey(acct int64, product, rtype, metric string) (*AlertRule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.rules {
		if r.AccountID == acct && r.ProductCode == product && r.ResourceType == rtype && r.Metric == metric {
			copied := r
			return &copied, nil
		}
	}
	return nil, nil
}

func (s *memRuleRepo) Create(rule *AlertRule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	rule.RuleID = s.seq
	s.rules[rule.RuleID] = *rule
	return nil
}

func (s *memRuleRepo) Save(rule *AlertRule, expectedVersion int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.rules[rule.RuleID]
	if !ok || stored.AccountID != rule.AccountID || stored.Version != expectedVersion {
		return ErrRuleConflict
	}
	s.rules[rule.RuleID] = *rule
	return nil
}

func (s *memRuleRepo) Delete(acct, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rules[id]
	if !ok || r.AccountID != acct {
		return nil // deleting an unknown rule is already the desired state
	}
	delete(s.rules, id)
	return nil
}

// --- SQL implementation ------------------------------------------------------

type sqlRuleRepo struct {
	db  *sql.DB
	ids *storage.Sequence // rule_id is user-visible (returned by the API)
}

var _ ruleRepo = (*sqlRuleRepo)(nil)

// newSQLRuleRepo wires the repo to support_db. rule_id has no AUTO_INCREMENT
// (it is a domain-visible id), so it comes from the 号段 allocator, which seeds
// its own row in id_sequence on first use.
func newSQLRuleRepo(ctx context.Context, db *sql.DB) (*sqlRuleRepo, error) {
	ids, err := storage.OpenSequence(ctx, db, "alert_rule", 1000)
	if err != nil {
		return nil, err
	}
	return &sqlRuleRepo{db: db, ids: ids}, nil
}

const ruleColumns = `rule_id, account_id, product_code, resource_type, metric, threshold,
	comparison_operator, period, eval_periods, notification_channels_json, status,
	version, created_at, updated_at`

func marshalChannels(ch []string) string {
	b, err := json.Marshal(ch)
	if err != nil || len(ch) == 0 {
		// The column is NOT NULL JSON; "[]" is the honest empty set.
		return "[]"
	}
	return string(b)
}

func (s *sqlRuleRepo) List(acct int64) ([]*AlertRule, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+ruleColumns+` FROM alert_rule WHERE account_id = ? ORDER BY created_at DESC, rule_id`, acct)
	if err != nil {
		return nil, fmt.Errorf("monitor: list rules for account %d: %w", acct, err)
	}
	defer rows.Close()

	out := make([]*AlertRule, 0, 8)
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, fmt.Errorf("monitor: scan rule for account %d: %w", acct, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("monitor: list rules for account %d: %w", acct, err)
	}
	return out, nil
}

func (s *sqlRuleRepo) Get(acct, id int64) (*AlertRule, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r, err := scanRule(s.db.QueryRowContext(ctx,
		`SELECT `+ruleColumns+` FROM alert_rule WHERE rule_id = ? AND account_id = ?`, id, acct))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("monitor: get rule %d: %w", id, err)
	}
	return r, nil
}

func (s *sqlRuleRepo) ByNaturalKey(acct int64, product, rtype, metric string) (*AlertRule, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r, err := scanRule(s.db.QueryRowContext(ctx,
		`SELECT `+ruleColumns+` FROM alert_rule
		  WHERE account_id = ? AND product_code = ? AND resource_type = ? AND metric = ?`,
		acct, product, rtype, metric))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("monitor: natural key lookup %d/%s/%s/%s: %w", acct, product, rtype, metric, err)
	}
	return r, nil
}

func (s *sqlRuleRepo) Create(rule *AlertRule) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	id, err := s.ids.Next(ctx)
	if err != nil {
		return fmt.Errorf("monitor: allocate rule id: %w", err)
	}
	rule.RuleID = id
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO alert_rule
		   (rule_id, account_id, product_code, resource_type, metric, threshold,
		    comparison_operator, period, eval_periods, notification_channels_json, status,
		    version, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rule.RuleID, rule.AccountID, rule.ProductCode, rule.ResourceType, rule.Metric, rule.Threshold,
		rule.ComparisonOperator, rule.Period, rule.EvalPeriods, marshalChannels(rule.NotificationChannels),
		rule.Status, rule.Version, rule.CreatedAt.UTC(), rule.UpdatedAt.UTC()); err != nil {
		if storage.IsDuplicateKey(err) {
			// uk_acc_rule: the caller's ByNaturalKey check lost a race. The
			// handler re-reads and takes the update path instead.
			return ErrRuleConflict
		}
		return fmt.Errorf("monitor: create rule: %w", err)
	}
	return nil
}

func (s *sqlRuleRepo) Save(rule *AlertRule, expectedVersion int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := s.db.ExecContext(ctx,
		`UPDATE alert_rule
		    SET threshold = ?, comparison_operator = ?, period = ?, eval_periods = ?,
		        notification_channels_json = ?, status = ?, version = ?, updated_at = ?
		  WHERE rule_id = ? AND account_id = ? AND version = ?`,
		rule.Threshold, rule.ComparisonOperator, rule.Period, rule.EvalPeriods,
		marshalChannels(rule.NotificationChannels), rule.Status, rule.Version, rule.UpdatedAt.UTC(),
		rule.RuleID, rule.AccountID, expectedVersion)
	if err != nil {
		return fmt.Errorf("monitor: save rule %d: %w", rule.RuleID, err)
	}
	if err := storage.Affected(res, nil); err != nil {
		return fmt.Errorf("monitor: save rule %d: %w", rule.RuleID, errors.Join(ErrRuleConflict, err))
	}
	return nil
}

func (s *sqlRuleRepo) Delete(acct, id int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM alert_rule WHERE rule_id = ? AND account_id = ?`, id, acct); err != nil {
		return fmt.Errorf("monitor: delete rule %d: %w", id, err)
	}
	return nil
}

func scanRule(sc interface{ Scan(...any) error }) (*AlertRule, error) {
	var (
		r        AlertRule
		channels string
	)
	if err := sc.Scan(&r.RuleID, &r.AccountID, &r.ProductCode, &r.ResourceType, &r.Metric,
		&r.Threshold, &r.ComparisonOperator, &r.Period, &r.EvalPeriods, &channels,
		&r.Status, &r.Version, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(channels), &r.NotificationChannels); err != nil {
		return nil, fmt.Errorf("monitor: rule %d channels: %w", r.RuleID, err)
	}
	return &r, nil
}

// newRuleRepo picks the backend. Persistence is opt-in (pkg-go/storage doc):
// with EULER_DB_DSN set, rules live in support_db so alert-engine and monitor
// restarts agree on what tenants asked for; unset, the in-memory repo keeps the
// demo and `go test` dependency-free.
//
// A configured DSN that cannot be reached — or a schema sqlmigrate never
// touched — is a startup failure: silently dropping every tenant's alerting
// because a row cannot be written is not an acceptable degradation for the one
// service whose job is to wake people up.
func newRuleRepo(ctx context.Context) (ruleRepo, error) {
	db, ok, err := storage.MustOpenFor(ctx, "support_db")
	if err != nil {
		return nil, err
	}
	if !ok {
		return newMemRuleRepo(), nil
	}
	if err := storage.EnsureMigrated(ctx, db, "support_db"); err != nil {
		return nil, err
	}
	return newSQLRuleRepo(ctx, db)
}

// persistentRuleRepo reports whether a repo is backed by MySQL, for the startup
// log.
func persistentRuleRepo(r ruleRepo) bool {
	_, ok := r.(*sqlRuleRepo)
	return ok
}
