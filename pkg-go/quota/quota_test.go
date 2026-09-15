package quota

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

var base = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

// memStore is an in-memory Store. Production uses MySQL sharded by account_id
// with Redis as a read accelerator; the optimistic lock lives in both.
type memStore struct {
	mu           sync.Mutex
	defs         map[string]Definition
	usage        map[string]Usage
	tokens       map[string]Token
	failPutToken bool
}

func newStore() *memStore {
	return &memStore{
		defs:   make(map[string]Definition),
		usage:  make(map[string]Usage),
		tokens: make(map[string]Token),
	}
}

func usageKey(accountID int64, quotaCode, region string) string {
	return fmt.Sprintf("%d|%s|%s", accountID, quotaCode, region)
}

func (m *memStore) GetDefinition(code string) (Definition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.defs[code]
	if !ok {
		return Definition{}, ErrUnknownQuota
	}
	return d, nil
}

func (m *memStore) GetUsage(accountID int64, quotaCode, region string) (Usage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.usage[usageKey(accountID, quotaCode, region)]
	if !ok {
		return Usage{AccountID: accountID, QuotaCode: quotaCode, Region: region}, nil
	}
	return u, nil
}

func (m *memStore) UpdateUsage(u Usage, expectedVersion int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := usageKey(u.AccountID, u.QuotaCode, u.Region)
	cur, ok := m.usage[k]
	curVersion := 0
	if ok {
		curVersion = cur.Version
	}
	if curVersion != expectedVersion {
		return ErrVersionConflict
	}
	u.Version = curVersion + 1
	m.usage[k] = u
	return nil
}

func (m *memStore) PutToken(t Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failPutToken {
		return fmt.Errorf("memStore: simulated PutToken failure")
	}
	m.tokens[t.TokenID] = t
	return nil
}

func (m *memStore) GetToken(id string) (Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[id]
	if !ok {
		return Token{}, ErrTokenNotFound
	}
	return t, nil
}

func (m *memStore) DeleteToken(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tokens, id)
	return nil
}

func (m *memStore) ListExpiredTokens(now time.Time) ([]Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Token
	for _, t := range m.tokens {
		if t.Expired(now) {
			out = append(out, t)
		}
	}
	SortTokens(out)
	return out, nil
}

func newManager(now time.Time) (*Manager, *memStore) {
	store := newStore()
	store.defs["quota_euecs_instance"] = Definition{
		QuotaCode:    "quota_euecs_instance",
		ProductCode:  "euecs",
		DefaultValue: 20,
		Scope:        ScopeRegion,
		Adjustable:   true,
	}
	store.defs["quota_euoss_bucket"] = Definition{
		QuotaCode:    "quota_euoss_bucket",
		ProductCode:  "euoss",
		DefaultValue: 100,
		Scope:        ScopeGlobal,
	}
	var seq int64
	return NewManager(store, func() time.Time { return now }, func() string {
		seq++
		return fmt.Sprintf("qt-%d", seq)
	}), store
}

const acct = int64(100123)

// --- Two-phase protocol ---

func TestOccupyThenCommit(t *testing.T) {
	m, _ := newManager(base)

	tok, err := m.CheckAndOccupy(acct, "quota_euecs_instance", "cn-north-1", 3, "order-9001")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := m.Describe(acct, "quota_euecs_instance", "cn-north-1")
	if u.Occupying != 3 || u.Used != 0 {
		t.Fatalf("after occupy: used=%d occupying=%d, want 0/3", u.Used, u.Occupying)
	}
	// Reserved capacity is NOT available to anyone else — this is what stops
	// two concurrent orders both claiming the last slot.
	if u.Available() != 17 {
		t.Fatalf("available = %d, want 17", u.Available())
	}

	if err := m.CommitOccupy(tok.TokenID); err != nil {
		t.Fatal(err)
	}
	u, _ = m.Describe(acct, "quota_euecs_instance", "cn-north-1")
	if u.Used != 3 || u.Occupying != 0 {
		t.Fatalf("after commit: used=%d occupying=%d, want 3/0", u.Used, u.Occupying)
	}
}

// TestPutTokenFailureRollsBackQuota guards the rollback path when storing the
// reservation token fails after the quota was already occupied: capacity must
// be returned to the pool, otherwise it is leaked forever.
func TestPutTokenFailureRollsBackQuota(t *testing.T) {
	m, store := newManager(base)
	store.failPutToken = true

	if _, err := m.CheckAndOccupy(acct, "quota_euecs_instance", "cn-north-1", 3, "order-fail"); err == nil {
		t.Fatal("CheckAndOccupy should fail when PutToken fails")
	}

	u, _ := m.Describe(acct, "quota_euecs_instance", "cn-north-1")
	if u.Occupying != 0 || u.Used != 0 {
		t.Fatalf("after failed occupy: used=%d occupying=%d, want 0/0 (capacity must not leak)", u.Used, u.Occupying)
	}
	if u.Available() != 20 {
		t.Fatalf("available = %d, want 20 (full quota restored)", u.Available())
	}
	if len(store.tokens) != 0 {
		t.Fatalf("token stored despite PutToken failure: %d tokens", len(store.tokens))
	}
}

func TestOccupyThenRelease(t *testing.T) {
	m, _ := newManager(base)

	tok, err := m.CheckAndOccupy(acct, "quota_euecs_instance", "cn-north-1", 5, "order-9002")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ReleaseOccupy(tok.TokenID); err != nil {
		t.Fatal(err)
	}

	u, _ := m.Describe(acct, "quota_euecs_instance", "cn-north-1")
	if u.Occupying != 0 || u.Used != 0 {
		t.Fatalf("after release: used=%d occupying=%d, want 0/0", u.Used, u.Occupying)
	}
	if u.Available() != 20 {
		t.Fatalf("available = %d, want the full 20 back", u.Available())
	}
}

// TestReleaseIsIdempotent covers saga compensation: it retries, and an error
// on an already-released token would turn a completed rollback into a false
// escalation.
func TestReleaseIsIdempotent(t *testing.T) {
	m, _ := newManager(base)
	tok, _ := m.CheckAndOccupy(acct, "quota_euecs_instance", "cn-north-1", 2, "order-1")

	if err := m.ReleaseOccupy(tok.TokenID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := m.ReleaseOccupy(tok.TokenID); err != nil {
			t.Fatalf("repeat release %d must succeed, got %v", i, err)
		}
	}
	if err := m.ReleaseOccupy("qt-nonexistent"); err != nil {
		t.Fatalf("releasing an unknown token must succeed, got %v", err)
	}
}

// --- Exhaustion ---

func TestQuotaExceededRejected(t *testing.T) {
	m, _ := newManager(base)
	if _, err := m.CheckAndOccupy(acct, "quota_euecs_instance", "cn-north-1", 20, "order-1"); err != nil {
		t.Fatal(err)
	}
	_, err := m.CheckAndOccupy(acct, "quota_euecs_instance", "cn-north-1", 1, "order-2")
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected ErrQuotaExceeded, got %v", err)
	}
}

// TestInFlightReservationsCountAgainstLimit is the anti-oversell property:
// ignoring in-flight reservations is exactly how two orders both get the last
// slot.
func TestInFlightReservationsCountAgainstLimit(t *testing.T) {
	m, _ := newManager(base)
	// Reserve everything but do not commit.
	if _, err := m.CheckAndOccupy(acct, "quota_euecs_instance", "cn-north-1", 20, "order-1"); err != nil {
		t.Fatal(err)
	}
	u, _ := m.Describe(acct, "quota_euecs_instance", "cn-north-1")
	if u.Used != 0 {
		t.Fatal("nothing is committed yet")
	}
	if u.Available() != 0 {
		t.Fatalf("available = %d, want 0 — uncommitted reservations still consume capacity", u.Available())
	}
}

// --- TTL sweeping ---

// TestExpiredTokenSweptBack is what bounds a saga that died between occupy and
// commit. Without it, capacity that is neither used nor sellable accumulates
// silently: customers are refused while hardware sits idle.
func TestExpiredTokenSweptBack(t *testing.T) {
	m, _ := newManager(base)
	if _, err := m.CheckAndOccupy(acct, "quota_euecs_instance", "cn-north-1", 8, "order-abandoned"); err != nil {
		t.Fatal(err)
	}

	// Nothing to sweep while the token is live.
	swept, err := m.SweepExpired()
	if err != nil {
		t.Fatal(err)
	}
	if swept != 0 {
		t.Fatalf("swept %d live tokens", swept)
	}

	// Past the TTL, the reservation comes back.
	later, store := newManagerFrom(m, base.Add(16*time.Minute))
	swept, err = later.SweepExpired()
	if err != nil {
		t.Fatal(err)
	}
	if swept != 1 {
		t.Fatalf("swept = %d, want 1", swept)
	}
	u, _ := store.GetUsage(acct, "quota_euecs_instance", "cn-north-1")
	if u.Occupying != 0 {
		t.Fatalf("occupying = %d after sweep, want 0", u.Occupying)
	}
	if u.Available() != 20 {
		t.Fatalf("available = %d, want the full 20 recovered", u.Available())
	}
}

// newManagerFrom rebuilds a Manager over the same store at a later time.
func newManagerFrom(m *Manager, at time.Time) (*Manager, *memStore) {
	store := m.store.(*memStore)
	var seq int64
	return NewManager(store, func() time.Time { return at }, func() string {
		seq++
		return fmt.Sprintf("qt-late-%d", seq)
	}), store
}

// TestExpiredTokenCannotCommit prevents double-counting: the sweeper already
// returned that capacity to the pool.
func TestExpiredTokenCannotCommit(t *testing.T) {
	m, _ := newManager(base)
	tok, _ := m.CheckAndOccupy(acct, "quota_euecs_instance", "cn-north-1", 3, "order-slow")

	late, _ := newManagerFrom(m, base.Add(16*time.Minute))
	if err := late.CommitOccupy(tok.TokenID); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expected ErrTokenExpired, got %v", err)
	}
}

// --- Concurrency ---

// TestConcurrentOccupyCannotOversell is the core guarantee: the optimistic
// lock must let at most the available count through, however many callers race.
func TestConcurrentOccupyCannotOversell(t *testing.T) {
	m, store := newManager(base)

	const goroutines = 50
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
	)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			if _, err := m.CheckAndOccupy(acct, "quota_euecs_instance", "cn-north-1", 1,
				fmt.Sprintf("order-%d", idx)); err == nil {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if granted > 20 {
		t.Fatalf("%d reservations granted against a limit of 20 — capacity was oversold", granted)
	}
	u, _ := store.GetUsage(acct, "quota_euecs_instance", "cn-north-1")
	if u.Occupying != granted {
		t.Fatalf("counter says %d occupying but %d were granted", u.Occupying, granted)
	}
	if u.Occupying > u.HardLimit && u.HardLimit > 0 {
		t.Fatalf("occupying %d exceeds limit %d", u.Occupying, u.HardLimit)
	}
	t.Logf("%d/%d concurrent reservations granted, counter consistent", granted, goroutines)
}

// --- Scope ---

func TestGlobalScopeIgnoresRegion(t *testing.T) {
	// A global quota counts across regions; passing a region must not create
	// per-region buckets that each allow the full limit.
	m, _ := newManager(base)
	if _, err := m.CheckAndOccupy(acct, "quota_euoss_bucket", "cn-north-1", 60, "o1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CheckAndOccupy(acct, "quota_euoss_bucket", "cn-east-1", 60, "o2"); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("a global quota must not reset per region, got %v", err)
	}
}

func TestRegionScopeIsIndependent(t *testing.T) {
	m, _ := newManager(base)
	if _, err := m.CheckAndOccupy(acct, "quota_euecs_instance", "cn-north-1", 20, "o1"); err != nil {
		t.Fatal(err)
	}
	// A different region has its own capacity — that is what REGION scope means.
	if _, err := m.CheckAndOccupy(acct, "quota_euecs_instance", "cn-east-1", 20, "o2"); err != nil {
		t.Fatalf("region-scoped quota should be independent per region: %v", err)
	}
}

// --- Warning threshold ---

func TestWarnThreshold(t *testing.T) {
	cases := []struct {
		used, occupying, limit int
		want                   bool
	}{
		{15, 0, 20, false}, // 75%
		{16, 0, 20, true},  // 80%
		{14, 2, 20, true},  // 80% counting in-flight
		{20, 0, 20, true},  // 100%
		{0, 0, 0, false},   // no limit configured
	}
	for _, c := range cases {
		u := Usage{Used: c.used, Occupying: c.occupying, HardLimit: c.limit}
		if got := ShouldWarn(u); got != c.want {
			t.Errorf("used=%d occupying=%d limit=%d: warn=%v, want %v",
				c.used, c.occupying, c.limit, got, c.want)
		}
	}
}

// --- Reconciliation ---

// TestReconcileAgainstResourceLedger covers 03§8.5: quota is a derived
// counter, and when a counter disagrees with the thing it counts, the thing
// is right.
func TestReconcileAgainstResourceLedger(t *testing.T) {
	m, _ := newManager(base)
	tok, _ := m.CheckAndOccupy(acct, "quota_euecs_instance", "cn-north-1", 5, "o1")
	_ = m.CommitOccupy(tok.TokenID)

	// The resource ledger says only 3 instances actually exist.
	res, err := m.Reconcile(acct, "quota_euecs_instance", "cn-north-1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Drifted() || res.Drift != 2 {
		t.Fatalf("drift = %d, want 2", res.Drift)
	}

	if err := m.Correct(acct, "quota_euecs_instance", "cn-north-1", 3); err != nil {
		t.Fatal(err)
	}
	u, _ := m.Describe(acct, "quota_euecs_instance", "cn-north-1")
	if u.Used != 3 {
		t.Fatalf("after correction used = %d, want 3", u.Used)
	}

	after, _ := m.Reconcile(acct, "quota_euecs_instance", "cn-north-1", 3)
	if after.Drifted() {
		t.Fatal("counter should agree with the ledger after correction")
	}
}

// --- Release on resource deletion ---

func TestReleaseCommittedOnResourceRelease(t *testing.T) {
	m, _ := newManager(base)
	tok, _ := m.CheckAndOccupy(acct, "quota_euecs_instance", "cn-north-1", 4, "o1")
	_ = m.CommitOccupy(tok.TokenID)

	if err := m.ReleaseCommitted(acct, "quota_euecs_instance", "cn-north-1", 4); err != nil {
		t.Fatal(err)
	}
	u, _ := m.Describe(acct, "quota_euecs_instance", "cn-north-1")
	if u.Used != 0 {
		t.Fatalf("used = %d after release, want 0", u.Used)
	}
}

func TestCountersNeverGoNegative(t *testing.T) {
	// A double release, or a release racing the sweeper, must clamp rather
	// than produce a negative counter that would grant phantom capacity.
	m, _ := newManager(base)
	if err := m.ReleaseCommitted(acct, "quota_euecs_instance", "cn-north-1", 5); err != nil {
		t.Fatal(err)
	}
	u, _ := m.Describe(acct, "quota_euecs_instance", "cn-north-1")
	if u.Used < 0 || u.Occupying < 0 {
		t.Fatalf("negative counter: used=%d occupying=%d", u.Used, u.Occupying)
	}
	if u.Available() > u.HardLimit {
		t.Fatalf("available %d exceeds the limit %d — phantom capacity", u.Available(), u.HardLimit)
	}
}

func TestInvalidAmountRejected(t *testing.T) {
	m, _ := newManager(base)
	for _, n := range []int{0, -1} {
		if _, err := m.CheckAndOccupy(acct, "quota_euecs_instance", "cn-north-1", n, "o"); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("amount %d: expected ErrInvalidAmount, got %v", n, err)
		}
	}
}

func TestUnknownQuotaRejected(t *testing.T) {
	m, _ := newManager(base)
	if _, err := m.CheckAndOccupy(acct, "quota_nonexistent", "cn-north-1", 1, "o"); !errors.Is(err, ErrUnknownQuota) {
		t.Fatalf("expected ErrUnknownQuota, got %v", err)
	}
}
