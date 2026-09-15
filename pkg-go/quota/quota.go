// Package quota implements two-phase quota reservation
// (03-backend-services.md §4.3.2).
//
// # Why two phases
//
// Provisioning is asynchronous and can fail minutes after the order is placed.
// A single-phase "decrement on order" leaks capacity whenever fulfilment
// fails; a "decrement on success" oversells, because two concurrent orders
// both see free capacity that only one of them will get.
//
// So: CheckAndOccupy reserves and returns a token, then the fulfilment saga
// either Commits (the resource exists) or Releases (it did not). Both failure
// directions are bounded — an uncommitted reservation expires on its own.
//
// # The TTL is the load-bearing part
//
// A reservation whose saga crashed between occupy and commit would otherwise
// hold capacity forever, and quota that is neither used nor sellable is the
// worst of both worlds: customers are refused while the hardware sits idle.
// Every token carries an expiry, and expired tokens are swept back
// automatically (03§4.3.2: 占用令牌带 TTL 防止悬挂).
//
// # DB is authoritative, Redis is an accelerator
//
// 03§4.3.2 is explicit: the DB optimistic lock (version column) decides;
// Redis only speeds up the hot read path. A Redis-authoritative counter loses
// its state on failover, and quota that resets on a cache restart is quota
// that can be spent twice.
package quota

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// DefaultTokenTTL is how long a reservation survives without being committed
// (03§4.3.2). Fifteen minutes matches the CREATING timeout in the resource
// lifecycle, so a reservation outlives the provisioning attempt it belongs to
// but not by much.
const DefaultTokenTTL = 15 * time.Minute

// Scope is the dimension a quota applies to.
type Scope string

const (
	// ScopeGlobal counts across all regions.
	ScopeGlobal Scope = "GLOBAL"
	// ScopeRegion counts per region — the usual case, since capacity is
	// physically per-region.
	ScopeRegion Scope = "REGION"
)

// Definition is a quota rule (quota_definition).
type Definition struct {
	QuotaCode    string // quota_euecs_instance
	ProductCode  string
	DefaultValue int
	Scope        Scope
	Adjustable   bool
}

// Usage is an account's consumption of one quota (quota_usage).
type Usage struct {
	AccountID int64 // shard key
	QuotaCode string
	Region    string // "*" for global scope
	// Used is committed consumption: resources that exist.
	Used int
	// Occupying is reserved-but-not-committed: in-flight orders.
	Occupying int
	// HardLimit is the account's effective ceiling, defaulting to the
	// definition's DefaultValue.
	HardLimit int
	Version   int
}

// Available returns how much can still be reserved. Both committed and
// in-flight consumption count against the limit — ignoring in-flight
// reservations is precisely how oversell happens.
func (u Usage) Available() int {
	avail := u.HardLimit - u.Used - u.Occupying
	if avail < 0 {
		return 0
	}
	return avail
}

// Token is a reservation handle. The saga carries it from occupy to
// commit-or-release.
type Token struct {
	TokenID   string
	AccountID int64
	QuotaCode string
	Region    string
	Amount    int
	// BizKey ties the reservation to what asked for it, so a leaked token can
	// be traced to the order that abandoned it.
	BizKey    string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Expired reports whether the reservation has timed out.
func (t Token) Expired(now time.Time) bool {
	return !now.Before(t.ExpiresAt)
}

// Errors.
var (
	ErrQuotaExceeded   = errors.New("quota: exceeded")
	ErrTokenNotFound   = errors.New("quota: reservation token not found")
	ErrTokenExpired    = errors.New("quota: reservation token expired")
	ErrVersionConflict = errors.New("quota: version conflict (concurrent update)")
	ErrUnknownQuota    = errors.New("quota: no such quota code")
	ErrInvalidAmount   = errors.New("quota: amount must be positive")
)

// Store persists definitions, usage and tokens. Production backs this with
// MySQL (sharded by account_id) plus a Redis read cache.
type Store interface {
	GetDefinition(quotaCode string) (Definition, error)
	GetUsage(accountID int64, quotaCode, region string) (Usage, error)
	// UpdateUsage applies the change under the optimistic lock.
	UpdateUsage(u Usage, expectedVersion int) error
	PutToken(t Token) error
	GetToken(tokenID string) (Token, error)
	DeleteToken(tokenID string) error
	// ListExpiredTokens returns tokens past their TTL, for the sweeper.
	ListExpiredTokens(now time.Time) ([]Token, error)
}

// Manager implements the two-phase protocol.
type Manager struct {
	store    Store
	now      func() time.Time
	tokenTTL time.Duration
	newToken func() string
}

// NewManager builds a Manager.
func NewManager(store Store, now func() time.Time, newToken func() string) *Manager {
	if now == nil {
		now = time.Now
	}
	if newToken == nil {
		var seq int64
		newToken = func() string {
			seq++
			return fmt.Sprintf("qt-%d", seq)
		}
	}
	return &Manager{store: store, now: now, tokenTTL: DefaultTokenTTL, newToken: newToken}
}

// SetTokenTTL overrides the reservation lifetime.
func (m *Manager) SetTokenTTL(d time.Duration) { m.tokenTTL = d }

// normalizeRegion maps the region to "*" for GLOBAL-scoped quotas, mirroring
// CheckAndOccupy. Every path that reads or writes usage must apply the same
// normalization, or a GLOBAL quota occupied under "*" is released under
// "cn-north-1" and the two rows drift apart forever.
func (m *Manager) normalizeRegion(quotaCode, region string) (string, error) {
	def, err := m.store.GetDefinition(quotaCode)
	if err != nil {
		return "", err
	}
	if def.Scope == ScopeGlobal {
		return "*", nil
	}
	return region, nil
}

// CheckAndOccupy reserves capacity and returns a token (phase 1).
//
// The check and the reservation happen under one optimistic-lock update, so
// two concurrent callers cannot both pass the check against the same starting
// version. The loser gets ErrVersionConflict and retries against fresh state,
// where it will correctly see less availability.
func (m *Manager) CheckAndOccupy(accountID int64, quotaCode, region string, amount int, bizKey string) (Token, error) {
	if amount <= 0 {
		return Token{}, fmt.Errorf("%w: %d", ErrInvalidAmount, amount)
	}
	def, err := m.store.GetDefinition(quotaCode)
	if err != nil {
		return Token{}, err
	}
	if def.Scope == ScopeGlobal {
		region = "*"
	}

	u, err := m.store.GetUsage(accountID, quotaCode, region)
	if err != nil {
		return Token{}, err
	}
	if u.HardLimit == 0 {
		u.HardLimit = def.DefaultValue
		u.AccountID = accountID
		u.QuotaCode = quotaCode
		u.Region = region
	}

	if u.Available() < amount {
		return Token{}, fmt.Errorf("%w: %s available %d, requested %d",
			ErrQuotaExceeded, quotaCode, u.Available(), amount)
	}

	now := m.now()
	updated := u
	updated.Occupying += amount
	if err := m.store.UpdateUsage(updated, u.Version); err != nil {
		return Token{}, err
	}

	tok := Token{
		TokenID:   m.newToken(),
		AccountID: accountID,
		QuotaCode: quotaCode,
		Region:    region,
		Amount:    amount,
		BizKey:    bizKey,
		ExpiresAt: now.Add(m.tokenTTL),
		CreatedAt: now,
	}
	if err := m.store.PutToken(tok); err != nil {
		// Roll the reservation back rather than leaving capacity held by a
		// token nobody has. The successful UpdateUsage above advanced the
		// stored row to u.Version+1, so the rollback must guard on that
		// post-increment version or the optimistic-lock check will fail and
		// the capacity stays reserved forever.
		rollback := updated
		rollback.Occupying -= amount
		_ = m.store.UpdateUsage(rollback, u.Version+1)
		return Token{}, err
	}
	return tok, nil
}

// CommitOccupy converts a reservation into committed usage (phase 2, success).
//
// The token is CLAIMED (deleted) before the usage row moves: commit races the
// expiry sweeper, and if both read the token and then both update usage, the
// same reservation is counted twice — the sweeper returns it to the pool while
// the commit converts it to Used, overselling by the token amount. Deleting
// first makes exactly one of the two racers own the token; if the usage update
// then fails, the token is restored so the reservation is not lost.
func (m *Manager) CommitOccupy(tokenID string) error {
	tok, err := m.store.GetToken(tokenID)
	if err != nil {
		return err
	}
	// An expired token has already been swept (or is about to be): its capacity
	// went back to the pool, so committing it now would double-count.
	if tok.Expired(m.now()) {
		return fmt.Errorf("%w: %s expired at %v", ErrTokenExpired, tokenID, tok.ExpiresAt)
	}

	// Claim the token. If the sweeper (or a concurrent commit) got here first,
	// the delete fails and this commit must not touch usage.
	if err := m.store.DeleteToken(tokenID); err != nil {
		return err
	}

	u, err := m.store.GetUsage(tok.AccountID, tok.QuotaCode, tok.Region)
	if err != nil {
		_ = m.store.PutToken(tok) // restore the claim; the reservation still stands
		return err
	}
	updated := u
	updated.Occupying -= tok.Amount
	updated.Used += tok.Amount
	if updated.Occupying < 0 {
		updated.Occupying = 0
	}
	if err := m.store.UpdateUsage(updated, u.Version); err != nil {
		_ = m.store.PutToken(tok)
		return err
	}
	return nil
}

// ReleaseOccupy returns a reservation to the pool (phase 2, failure).
//
// Releasing an unknown or already-released token SUCCEEDS: this runs as saga
// compensation, which retries, and an error would turn a completed rollback
// into a false escalation.
func (m *Manager) ReleaseOccupy(tokenID string) error {
	tok, err := m.store.GetToken(tokenID)
	if errors.Is(err, ErrTokenNotFound) {
		return nil // already released or swept
	}
	if err != nil {
		return err
	}

	u, err := m.store.GetUsage(tok.AccountID, tok.QuotaCode, tok.Region)
	if err != nil {
		return err
	}
	updated := u
	updated.Occupying -= tok.Amount
	if updated.Occupying < 0 {
		updated.Occupying = 0
	}
	if err := m.store.UpdateUsage(updated, u.Version); err != nil {
		return err
	}
	// The delete also has to tolerate an already-claimed token: a commit or the
	// expiry sweep may have claimed it between the GetToken above and here, and
	// this function's contract is that releasing an unknown or already-released
	// token SUCCEEDS (it runs as saga compensation, which retries).
	if err := m.store.DeleteToken(tokenID); err != nil && !errors.Is(err, ErrTokenNotFound) {
		return err
	}
	return nil
}

// ReleaseCommitted returns committed capacity when a resource is released.
func (m *Manager) ReleaseCommitted(accountID int64, quotaCode, region string, amount int) error {
	if amount <= 0 {
		return fmt.Errorf("%w: %d", ErrInvalidAmount, amount)
	}
	region, err := m.normalizeRegion(quotaCode, region)
	if err != nil {
		return err
	}
	u, err := m.store.GetUsage(accountID, quotaCode, region)
	if err != nil {
		return err
	}
	updated := u
	updated.Used -= amount
	if updated.Used < 0 {
		updated.Used = 0
	}
	return m.store.UpdateUsage(updated, u.Version)
}

// SweepExpired returns expired reservations to the pool.
//
// This is what bounds the damage from a saga that died between occupy and
// commit. Without it, capacity that is neither used nor sellable accumulates
// silently: customers are refused while the hardware sits idle, and nothing in
// the system reports a problem.
func (m *Manager) SweepExpired() (int, error) {
	now := m.now()
	expired, err := m.store.ListExpiredTokens(now)
	if err != nil {
		return 0, err
	}
	swept := 0
	for _, tok := range expired {
		// Claim the token FIRST: sweep races CommitOccupy, and updating usage
		// before owning the token lets a commit convert the same reservation to
		// Used after the sweeper already returned it — plus a failed delete
		// would leave the token behind to be swept (and decremented) again.
		if err := m.store.DeleteToken(tok.TokenID); err != nil {
			continue // someone else claimed it (commit or a concurrent sweep)
		}
		u, err := m.store.GetUsage(tok.AccountID, tok.QuotaCode, tok.Region)
		if err != nil {
			_ = m.store.PutToken(tok) // give it back; next sweep retries
			continue
		}
		updated := u
		updated.Occupying -= tok.Amount
		if updated.Occupying < 0 {
			updated.Occupying = 0
		}
		if err := m.store.UpdateUsage(updated, u.Version); err != nil {
			_ = m.store.PutToken(tok)
			continue
		}
		swept++
	}
	return swept, nil
}

// Describe returns the account's quota position.
func (m *Manager) Describe(accountID int64, quotaCode, region string) (Usage, error) {
	def, err := m.store.GetDefinition(quotaCode)
	if err != nil {
		return Usage{}, err
	}
	if def.Scope == ScopeGlobal {
		region = "*"
	}
	u, err := m.store.GetUsage(accountID, quotaCode, region)
	if err != nil {
		return Usage{}, err
	}
	if u.HardLimit == 0 {
		u.HardLimit = def.DefaultValue
		u.AccountID = accountID
		u.QuotaCode = quotaCode
		u.Region = region
	}
	return u, nil
}

// WarnThreshold is the usage fraction at which a quota warning fires
// (cloud.resource.quota.event, 03§7: 用量超 80% 预警).
const WarnThreshold = 0.8

// ShouldWarn reports whether usage has crossed the warning threshold.
//
// Warning before the wall is reached is the difference between a customer
// planning an increase and a customer discovering the limit mid-incident.
func ShouldWarn(u Usage) bool {
	if u.HardLimit <= 0 {
		return false
	}
	consumed := float64(u.Used+u.Occupying) / float64(u.HardLimit)
	return consumed >= WarnThreshold
}

// Reconcile compares recorded usage against the actual resource count
// (03§8.5 配额↔资源对账). The resource ledger wins: quota is a derived
// counter, and when a counter disagrees with the thing it counts, the thing is
// right.
type ReconcileResult struct {
	AccountID    int64
	QuotaCode    string
	Region       string
	RecordedUsed int
	ActualCount  int
	Drift        int
}

// Drifted reports whether the counter disagrees with reality.
func (r ReconcileResult) Drifted() bool { return r.Drift != 0 }

// Reconcile checks the counter against the real resource count and returns
// the correction needed.
func (m *Manager) Reconcile(accountID int64, quotaCode, region string, actualCount int) (ReconcileResult, error) {
	region, err := m.normalizeRegion(quotaCode, region)
	if err != nil {
		return ReconcileResult{}, err
	}
	u, err := m.store.GetUsage(accountID, quotaCode, region)
	if err != nil {
		return ReconcileResult{}, err
	}
	return ReconcileResult{
		AccountID:    accountID,
		QuotaCode:    quotaCode,
		Region:       region,
		RecordedUsed: u.Used,
		ActualCount:  actualCount,
		Drift:        u.Used - actualCount,
	}, nil
}

// Correct forces the counter to match the resource ledger.
func (m *Manager) Correct(accountID int64, quotaCode, region string, actualCount int) error {
	region, err := m.normalizeRegion(quotaCode, region)
	if err != nil {
		return err
	}
	u, err := m.store.GetUsage(accountID, quotaCode, region)
	if err != nil {
		return err
	}
	updated := u
	updated.Used = actualCount
	return m.store.UpdateUsage(updated, u.Version)
}

// SortTokens orders tokens by creation time, for deterministic sweep output.
func SortTokens(tokens []Token) {
	sort.SliceStable(tokens, func(i, j int) bool {
		return tokens[i].CreatedAt.Before(tokens[j].CreatedAt)
	})
}
