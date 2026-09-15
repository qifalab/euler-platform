package quota

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/storage"
	"github.com/qifalab/euler-platform/storage/sqltest"
)

// clock is a mutable test clock: the Manager reads it through a closure, so a
// test can move time forward (to expire a reservation) without sleeping.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

const sqlAcct = int64(100123)

// newSQLQuota wires a Manager to the real MySQL store.
//
// The DDL applied is services/svc-quota/sql — quota_definition / quota_usage /
// quota_token live there (support_db), which is where the service writes.
func newSQLQuota(t *testing.T) (*SQLStore, *sql.DB, *Manager, *clock) {
	t.Helper()
	db := sqltest.Open(t, sqltest.Services("svc-quota"))
	store := NewSQLStore(db)
	c := &clock{t: base}
	var seq int64
	m := NewManager(store, c.now, func() string {
		seq++
		return fmt.Sprintf("qt-%d", seq)
	})
	return store, db, m, c
}

// seedDefinition sets a quota rule the way an operator's seed data would:
// directly through SQL, not through the store under test.
//
// It replaces an existing row rather than failing on it: the V3 migration seeds
// the two production definitions, so a test that wants different values has to
// overwrite them — the scratch schema is the test's to define, and a duplicate-key
// error here would say nothing about the code under test.
func seedDefinition(t *testing.T, db *sql.DB, code, product string, defValue int, scope string, adjustable int) {
	t.Helper()
	err := storage.Tx(context.Background(), db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM quota_definition WHERE quota_code = ?`, code); err != nil {
			return err
		}
		_, err := tx.Exec(
			`INSERT INTO quota_definition (quota_code, product_code, default_value, scope, adjustable)
			 VALUES (?, ?, ?, ?, ?)`,
			code, product, defValue, scope, adjustable)
		return err
	})
	if err != nil {
		t.Fatalf("seed definition %s: %v", code, err)
	}
}

func TestSQLStoreDefinitionRoundTrip(t *testing.T) {
	store, db, _, _ := newSQLQuota(t)

	if _, err := store.GetDefinition("quota_missing"); !errors.Is(err, ErrUnknownQuota) {
		t.Fatalf("unknown code = %v, want ErrUnknownQuota", err)
	}

	seedDefinition(t, db, "quota_euecs_instance", "euecs", 20, "REGION", 1)
	def, err := store.GetDefinition("quota_euecs_instance")
	if err != nil {
		t.Fatal(err)
	}
	if def.ProductCode != "euecs" || def.DefaultValue != 20 || def.Scope != ScopeRegion || !def.Adjustable {
		t.Fatalf("definition round trip = %+v", def)
	}

	seedDefinition(t, db, "quota_euoss_bucket", "euoss", 100, "GLOBAL", 0)
	def, err = store.GetDefinition("quota_euoss_bucket")
	if err != nil {
		t.Fatal(err)
	}
	if def.Scope != ScopeGlobal || def.Adjustable {
		t.Fatalf("global definition = %+v, want GLOBAL and not adjustable", def)
	}

	// A scope the domain does not know is schema drift; guessing at it would
	// silently change whether the counter is per-region.
	seedDefinition(t, db, "quota_weird", "euoss", 5, "CONTINENT", 1)
	if _, err := store.GetDefinition("quota_weird"); err == nil {
		t.Fatal("an unknown scope must be rejected, not guessed")
	}
}

func TestSQLStoreUsageCreateAndRead(t *testing.T) {
	store, _, _, _ := newSQLQuota(t)

	// No row yet: version 0 and zeros, so the Manager can lazily build it from
	// the definition.
	u, err := store.GetUsage(sqlAcct, "quota_euecs_instance", "cn-north-1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Version != 0 || u.Used != 0 || u.Occupying != 0 {
		t.Fatalf("absent usage = %+v, want zeroed with version 0", u)
	}

	u.Occupying = 3
	u.HardLimit = 20
	if err := store.UpdateUsage(u, 0); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetUsage(sqlAcct, "quota_euecs_instance", "cn-north-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Occupying != 3 || got.HardLimit != 20 || got.Version != 1 {
		t.Fatalf("after create = %+v, want occupying 3 hard 20 version 1", got)
	}
}

// TestSQLStoreUpdatesSeededVersionZeroRow is why UpdateUsage tries the guarded
// UPDATE before inserting: a usage row that SQL seeded (an account set up ahead
// of its first reservation) sits at version 0, and on the ledger's "version 0
// means absent" convention its first reserve would collide with itself.
func TestSQLStoreUpdatesSeededVersionZeroRow(t *testing.T) {
	store, db, _, _ := newSQLQuota(t)

	if _, err := db.Exec(
		`INSERT INTO quota_usage (account_id, quota_code, region, used, occupying, hard_limit, version)
		 VALUES (?, ?, ?, ?, ?, ?, 0)`,
		sqlAcct, "quota_euecs_instance", "cn-north-1", 5, 0, 20); err != nil {
		t.Fatal(err)
	}

	if err := store.UpdateUsage(Usage{
		AccountID: sqlAcct, QuotaCode: "quota_euecs_instance", Region: "cn-north-1",
		Used: 5, Occupying: 2, HardLimit: 20,
	}, 0); err != nil {
		t.Fatalf("first reserve against a seeded row failed: %v", err)
	}

	got, err := store.GetUsage(sqlAcct, "quota_euecs_instance", "cn-north-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Used != 5 || got.Occupying != 2 || got.Version != 1 {
		t.Fatalf("usage = %+v, want 5/2 version 1", got)
	}
	var rows int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM quota_usage WHERE account_id = ? AND quota_code = ? AND region = ?`,
		sqlAcct, "quota_euecs_instance", "cn-north-1").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("the seeded row was duplicated: %d rows", rows)
	}
}

func TestSQLStoreVersionConflictDetected(t *testing.T) {
	store, _, _, _ := newSQLQuota(t)

	u := Usage{AccountID: sqlAcct, QuotaCode: "quota_euecs_instance", Region: "cn-north-1", HardLimit: 20, Occupying: 1}
	if err := store.UpdateUsage(u, 0); err != nil {
		t.Fatal(err)
	}

	// A writer holding the pre-create version must lose: this is what stops two
	// concurrent orders from both reserving the last slot.
	u.Occupying = 2
	err := store.UpdateUsage(u, 0)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale update = %v, want ErrVersionConflict", err)
	}
	if !errors.Is(err, storage.ErrVersionConflict) {
		t.Fatalf("conflict invisible to storage.WithVersionRetry: %v", err)
	}

	got, err := store.GetUsage(sqlAcct, "quota_euecs_instance", "cn-north-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Occupying != 1 || got.Version != 1 {
		t.Fatalf("rejected write moved the counter: %+v", got)
	}
}

func TestSQLStoreTokenLifecycle(t *testing.T) {
	store, _, _, _ := newSQLQuota(t)

	if _, err := store.GetToken("qt-absent"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("missing token = %v, want ErrTokenNotFound", err)
	}

	tok := Token{
		TokenID: "qt-1", AccountID: sqlAcct, QuotaCode: "quota_euecs_instance",
		Region: "cn-north-1", Amount: 3, BizKey: "ord-1",
		ExpiresAt: base.Add(DefaultTokenTTL), CreatedAt: base,
	}
	if err := store.PutToken(tok); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetToken("qt-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != tok {
		t.Fatalf("token round trip = %+v, want %+v", got, tok)
	}

	// Claim: the first delete wins, the second reports that it lost.
	if err := store.DeleteToken("qt-1"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := store.DeleteToken("qt-1"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("second delete = %v, want ErrTokenNotFound (claim semantics)", err)
	}

	// PutToken is an upsert (it restores a token whose conversion failed).
	if err := store.PutToken(tok); err != nil {
		t.Fatal(err)
	}
	restored := tok
	restored.Amount = 5
	if err := store.PutToken(restored); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, err = store.GetToken("qt-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount != 5 {
		t.Fatalf("token = %+v, want amount 5", got)
	}
}

func TestSQLStoreListExpiredTokens(t *testing.T) {
	store, _, _, _ := newSQLQuota(t)

	put := func(id string, created time.Time, expires time.Time) {
		t.Helper()
		if err := store.PutToken(Token{
			TokenID: id, AccountID: sqlAcct, QuotaCode: "quota_euecs_instance",
			Region: "cn-north-1", Amount: 1, BizKey: "ord", ExpiresAt: expires, CreatedAt: created,
		}); err != nil {
			t.Fatal(err)
		}
	}
	put("qt-live", base, base.Add(DefaultTokenTTL))
	put("qt-old", base.Add(-2*time.Hour), base.Add(-time.Hour))
	put("qt-older", base.Add(-3*time.Hour), base.Add(-2*time.Hour))

	tokens, err := store.ListExpiredTokens(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 {
		t.Fatalf("expired tokens = %d, want 2", len(tokens))
	}
	if tokens[0].TokenID != "qt-older" || tokens[1].TokenID != "qt-old" {
		t.Fatalf("expired tokens = %s, %s: want oldest first", tokens[0].TokenID, tokens[1].TokenID)
	}
}

// TestSQLStoreOccupyCommitDescribe drives the two-phase protocol end to end
// against the real schema, including the counter arithmetic the UI reads.
func TestSQLStoreOccupyCommitDescribe(t *testing.T) {
	store, db, m, _ := newSQLQuota(t)
	seedDefinition(t, db, "quota_euecs_instance", "euecs", 20, "REGION", 1)

	tok, err := m.CheckAndOccupy(sqlAcct, "quota_euecs_instance", "cn-north-1", 3, "ord-1")
	if err != nil {
		t.Fatal(err)
	}
	u, err := m.Describe(sqlAcct, "quota_euecs_instance", "cn-north-1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Occupying != 3 || u.Used != 0 || u.Available() != 17 {
		t.Fatalf("after occupy = %+v, want occupying 3 available 17", u)
	}

	if err := m.CommitOccupy(tok.TokenID); err != nil {
		t.Fatal(err)
	}
	u, err = m.Describe(sqlAcct, "quota_euecs_instance", "cn-north-1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Used != 3 || u.Occupying != 0 || u.Available() != 17 {
		t.Fatalf("after commit = %+v, want used 3 occupying 0", u)
	}
	if _, err := store.GetToken(tok.TokenID); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("committed token still present: %v", err)
	}
}

// TestSQLStoreConcurrentOccupyCannotOversell is the in-memory suite's oversell
// test on real MySQL: the last slot is guarded by a row lock, not a mutex.
func TestSQLStoreConcurrentOccupyCannotOversell(t *testing.T) {
	_, db, m, _ := newSQLQuota(t)
	seedDefinition(t, db, "quota_euecs_instance", "euecs", 1, "REGION", 1)

	const goroutines = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created []Token
		errs    []error
	)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			tok, err := m.CheckAndOccupy(sqlAcct, "quota_euecs_instance", "cn-north-1", 1, fmt.Sprintf("ord-%d", i))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			created = append(created, tok)
		}(i)
	}
	close(start)
	wg.Wait()

	if len(created) != 1 {
		t.Fatalf("%d of %d occupies succeeded against a limit of 1 (errors: %v)", len(created), goroutines, errs)
	}
	u, err := m.Describe(sqlAcct, "quota_euecs_instance", "cn-north-1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Occupying != 1 || u.Used != 0 {
		t.Fatalf("usage = %+v, want exactly one slot occupied", u)
	}
	// Every loser must have failed for a legitimate reason, not a driver error.
	for _, err := range errs {
		if !errors.Is(err, ErrQuotaExceeded) && !errors.Is(err, ErrVersionConflict) {
			t.Fatalf("unexpected failure: %v", err)
		}
	}
}

// TestSQLStoreSweepReturnsCapacity: a reservation the saga abandoned must come
// back on its own, or customers are refused while the capacity sits idle.
func TestSQLStoreSweepReturnsCapacity(t *testing.T) {
	store, db, m, c := newSQLQuota(t)
	seedDefinition(t, db, "quota_euecs_instance", "euecs", 2, "REGION", 1)

	tok, err := m.CheckAndOccupy(sqlAcct, "quota_euecs_instance", "cn-north-1", 2, "ord-1")
	if err != nil {
		t.Fatal(err)
	}

	c.t = base.Add(DefaultTokenTTL + time.Minute)
	swept, err := m.SweepExpired()
	if err != nil {
		t.Fatal(err)
	}
	if swept != 1 {
		t.Fatalf("swept %d tokens, want 1", swept)
	}
	u, err := m.Describe(sqlAcct, "quota_euecs_instance", "cn-north-1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Occupying != 0 || u.Available() != 2 {
		t.Fatalf("after sweep = %+v, want the capacity back", u)
	}
	if _, err := store.GetToken(tok.TokenID); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("swept token still present: %v", err)
	}

	// Committing a sweep-ed reservation must not double-count it: the token was
	// claimed first, so the commit cannot convert capacity the pool already has
	// back.
	if err := m.CommitOccupy(tok.TokenID); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("commit after sweep = %v, want ErrTokenNotFound", err)
	}
	u, err = m.Describe(sqlAcct, "quota_euecs_instance", "cn-north-1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Used != 0 || u.Occupying != 0 {
		t.Fatalf("commit after sweep moved usage: %+v", u)
	}
}

// TestSQLStoreReleaseIsIdempotent pins the saga-compensation contract: releasing
// an already-released reservation succeeds, including in the window where a
// sweep claims the token between the read and the delete.
func TestSQLStoreReleaseIsIdempotent(t *testing.T) {
	_, db, m, _ := newSQLQuota(t)
	seedDefinition(t, db, "quota_euecs_instance", "euecs", 5, "REGION", 1)

	tok, err := m.CheckAndOccupy(sqlAcct, "quota_euecs_instance", "cn-north-1", 2, "ord-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ReleaseOccupy(tok.TokenID); err != nil {
		t.Fatal(err)
	}
	if err := m.ReleaseOccupy(tok.TokenID); err != nil {
		t.Fatalf("second release = %v, want nil", err)
	}
	u, err := m.Describe(sqlAcct, "quota_euecs_instance", "cn-north-1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Occupying != 0 {
		t.Fatalf("usage = %+v, want no occupancy", u)
	}
}

// TestSQLStoreGlobalScopeUsesStarRegion guards the normalization the Manager
// documents: a GLOBAL quota occupied under "*" must be released under "*" too,
// or the two rows drift apart and the released capacity is never usable.
func TestSQLStoreGlobalScopeUsesStarRegion(t *testing.T) {
	store, db, m, _ := newSQLQuota(t)
	seedDefinition(t, db, "quota_euoss_bucket", "euoss", 3, "GLOBAL", 0)

	tok, err := m.CheckAndOccupy(sqlAcct, "quota_euoss_bucket", "cn-north-1", 3, "ord-1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Region != "*" {
		t.Fatalf("global token region = %q, want *", tok.Region)
	}
	if err := m.CommitOccupy(tok.TokenID); err != nil {
		t.Fatal(err)
	}
	// The counter must live on the "*" row, not on a cn-north-1 row.
	u, err := store.GetUsage(sqlAcct, "quota_euoss_bucket", "*")
	if err != nil {
		t.Fatal(err)
	}
	if u.Used != 3 {
		t.Fatalf("global usage = %+v, want used 3 under *", u)
	}
	regional, err := store.GetUsage(sqlAcct, "quota_euoss_bucket", "cn-north-1")
	if err != nil {
		t.Fatal(err)
	}
	if regional.Used != 0 {
		t.Fatalf("a region row was created for a global quota: %+v", regional)
	}
}
