package billing

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/metering"
	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/storage"
	"github.com/qifalab/euler-platform/storage/sqltest"
)

// newSQLSettleStore runs against the schema of record: settle_charge and
// settle_lock live in services/svc-billing/sql (V4).
func newSQLSettleStore(t *testing.T) *SQLSettleStore {
	t.Helper()
	return NewSQLSettleStore(sqltest.Open(t, sqltest.Services("svc-billing")))
}

// settledCharge is a final charge with micro-unit amounts and a nanosecond
// timestamp: both must survive the store at the column precision.
func settledCharge(acct int64, chargeID, period string, at time.Time) Charge {
	hour, _ := time.Parse("2006-01", period)
	return Charge{
		ChargeID: chargeID, AccountID: acct, ResourceID: "res-" + chargeID, ProductCode: "euecs",
		MeteringItem: "cpu_core_hour", BillPeriod: period, BillingCycle: hour.Add(5 * time.Hour),
		Quantity: metering.MustParseQuantity("1.999998"), UnitPrice: amt("0.250001"),
		PretaxAmount: amt("0.5"), PayAmount: amt("0.123457"), Shortfall: amt("0.076543"),
		Deductions: []Deduction{
			{Source: SourceResourcePack, Ref: "pk-1", Amount: amt("0.2")},
			{Source: SourceCoupon, Ref: "cp-1", Amount: amt("0.1")},
			{Source: SourceBalance, Amount: amt("0.123457")},
		},
		SnapshotID: "snap-1", CoveredRatio: 97,
		SettledAt: at,
	}
}

// runSettleStoreContract is the behaviour both stores must share.
func runSettleStoreContract(t *testing.T, s SettleStore) {
	const acct = int64(7001)
	if _, ok, err := s.GetSettle(acct, "chg-a"); err != nil || ok {
		t.Fatalf("absent record: ok=%v err=%v", ok, err)
	}
	if err := s.SaveSettle(&SettleRecord{AccountID: acct}); !errors.Is(err, ErrInvalidSettleRecord) {
		t.Fatalf("empty charge id = %v, want ErrInvalidSettleRecord", err)
	}

	// 1. The pack intent is written before any pack moves.
	rec := SettleRecord{AccountID: acct, ChargeID: "chg-a",
		PackIntent: map[string]pricing.Amount{"pk-1": amt("0.2"), "pk-2": amt("0.000001")}}
	if err := s.SaveSettle(&rec); err != nil {
		t.Fatal(err)
	}
	if rec.Version != 1 {
		t.Fatalf("created version = %d, want 1", rec.Version)
	}
	dup := SettleRecord{AccountID: acct, ChargeID: "chg-a"}
	if err := s.SaveSettle(&dup); !errors.Is(err, ErrSettleConflict) || !errors.Is(err, storage.ErrVersionConflict) {
		t.Fatalf("second create = %v, want ErrSettleConflict (and storage.ErrVersionConflict)", err)
	}
	got, ok, err := s.GetSettle(acct, "chg-a")
	if err != nil || !ok || !reflect.DeepEqual(got, rec) {
		t.Fatalf("planned record round trip:\n got %+v\nwant %+v (ok=%v err=%v)", got, rec, ok, err)
	}

	// 2. Pack phase done: what each pack covered is pinned.
	rec.PackTaken = map[string]pricing.Amount{"pk-1": amt("0.2"), "pk-2": 0}
	rec.PacksDone = true
	stale := rec
	if err := s.SaveSettle(&rec); err != nil || rec.Version != 2 {
		t.Fatalf("plan update: v=%d err=%v", rec.Version, err)
	}
	if err := s.SaveSettle(&stale); !errors.Is(err, ErrSettleConflict) {
		t.Fatalf("stale update = %v, want ErrSettleConflict", err)
	}

	// 3. Settled: the final charge, at the stored precision.
	rec.Settled = true
	rec.CashPaid = amt("0.123457")
	rec.Charge = settledCharge(acct, "chg-a", "2026-08", time.Date(2026, 8, 3, 4, 5, 6, 123456789, time.FixedZone("CST", 8*3600)))
	if err := s.SaveSettle(&rec); err != nil || rec.Version != 3 {
		t.Fatalf("settle: v=%d err=%v", rec.Version, err)
	}
	if want := time.Date(2026, 8, 2, 20, 5, 6, 123456000, time.UTC); !rec.Charge.SettledAt.Equal(want) || rec.Charge.SettledAt.Location() != time.UTC {
		t.Fatalf("SaveSettle must normalise SettledAt to UTC microseconds, got %v", rec.Charge.SettledAt)
	}
	got, ok, err = s.GetSettle(acct, "chg-a")
	if err != nil || !ok || !reflect.DeepEqual(got, rec) {
		t.Fatalf("settled record round trip:\n got %+v\nwant %+v (ok=%v err=%v)", got, rec, ok, err)
	}
	// A settled record is final: no writer may change it again.
	again := rec
	again.Charge.PayAmount = amt("99")
	if err := s.SaveSettle(&again); !errors.Is(err, ErrSettleConflict) {
		t.Fatalf("update of a settled record = %v, want ErrSettleConflict", err)
	}

	// 4. Bills read settled charges only, per account and period.
	other := SettleRecord{AccountID: acct, ChargeID: "chg-b", Settled: true,
		Charge: settledCharge(acct, "chg-b", "2026-08", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))}
	sep := SettleRecord{AccountID: acct, ChargeID: "chg-c", Settled: true,
		Charge: settledCharge(acct, "chg-c", "2026-09", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))}
	foreign := SettleRecord{AccountID: acct + 1, ChargeID: "chg-d", Settled: true,
		Charge: settledCharge(acct+1, "chg-d", "2026-08", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))}
	planned := SettleRecord{AccountID: acct, ChargeID: "chg-e", PackIntent: map[string]pricing.Amount{"pk-1": amt("1")}}
	for _, r := range []*SettleRecord{&other, &sep, &foreign, &planned} {
		if err := s.SaveSettle(r); err != nil {
			t.Fatal(err)
		}
	}
	aug, err := s.ListCharges(acct, "2026-08")
	if err != nil {
		t.Fatal(err)
	}
	if len(aug) != 2 || aug[0].ChargeID != "chg-b" || aug[1].ChargeID != "chg-a" {
		t.Fatalf("August charges = %+v, want chg-b then chg-a (settlement order)", aug)
	}
	if !reflect.DeepEqual(aug[1], rec.Charge) {
		t.Fatalf("listed charge:\n got %+v\nwant %+v", aug[1], rec.Charge)
	}
	all, err := s.ListCharges(acct, "")
	if err != nil || len(all) != 3 {
		t.Fatalf("all periods = %d charges (err %v), want 3", len(all), err)
	}
	bill := Summarize(acct, "2026-08", aug)
	if bill.ChargeCount != 2 || bill.TotalAmount != amt("1") || bill.IncompleteCharges != 2 || bill.UnreconciledCount != 0 {
		t.Fatalf("summary over stored charges = %+v", bill)
	}
}

func TestMemorySettleStoreContract(t *testing.T) {
	runSettleStoreContract(t, NewMemorySettleStore())
}

func TestSQLSettleStoreContract(t *testing.T) {
	runSettleStoreContract(t, newSQLSettleStore(t))
}

// TestSQLSettleStoreParityWithMemoryStore runs one script through both stores
// and compares everything they return.
func TestSQLSettleStoreParityWithMemoryStore(t *testing.T) {
	stores := []SettleStore{NewMemorySettleStore(), newSQLSettleStore(t)}
	type snapshot struct {
		recs    []SettleRecord
		charges []Charge
	}
	var snaps []snapshot
	for _, s := range stores {
		var recs []SettleRecord
		for i, id := range []string{"chg-p1", "chg-p2", "chg-p3"} {
			rec := SettleRecord{AccountID: 9001, ChargeID: id,
				PackIntent: map[string]pricing.Amount{"pk-" + id: amt("0.3")}}
			if err := s.SaveSettle(&rec); err != nil {
				t.Fatal(err)
			}
			if i == 2 {
				recs = append(recs, rec) // left mid-plan
				continue
			}
			rec.PacksDone = true
			rec.PackTaken = map[string]pricing.Amount{"pk-" + id: amt("0.3")}
			rec.Settled = true
			rec.Charge = settledCharge(9001, id, "2026-07", time.Date(2026, 7, 9, 1, 2, 3, int(i)*1000, time.UTC))
			rec.Charge.Deductions = nil // a fully unpaid line has no deductions
			if err := s.SaveSettle(&rec); err != nil {
				t.Fatal(err)
			}
			recs = append(recs, rec)
		}
		var read []SettleRecord
		for _, r := range recs {
			got, ok, err := s.GetSettle(r.AccountID, r.ChargeID)
			if err != nil || !ok {
				t.Fatalf("get %s: ok=%v err=%v", r.ChargeID, ok, err)
			}
			read = append(read, got)
		}
		charges, err := s.ListCharges(9001, "2026-07")
		if err != nil {
			t.Fatal(err)
		}
		snaps = append(snaps, snapshot{recs: read, charges: charges})
	}
	if !reflect.DeepEqual(snaps[0], snaps[1]) {
		t.Fatalf("stores disagree:\nmemory %+v\n   sql %+v", snaps[0], snaps[1])
	}
}

// runLockContract: the account lock admits one holder at a time, other
// accounts are independent, and a waiter gives up when its context ends.
func runLockContract(t *testing.T, a, b SettleStore) {
	ctx := context.Background()
	var inside, maxInside int32
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		s := a
		if i%2 == 1 {
			s = b // a second process sharing the same store
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := s.LockAccount(ctx, 42)
			if err != nil {
				t.Error(err)
				return
			}
			n := atomic.AddInt32(&inside, 1)
			for {
				m := atomic.LoadInt32(&maxInside)
				if n <= m || atomic.CompareAndSwapInt32(&maxInside, m, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&inside, -1)
			release()
			release() // idempotent
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Fatalf("%d holders inside the account lock at once, want 1", maxInside)
	}

	release, err := a.LockAccount(ctx, 43)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	other, err := b.LockAccount(ctx, 44) // another account is not blocked
	if err != nil {
		t.Fatal(err)
	}
	other()
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if r, err := b.LockAccount(short, 43); err == nil {
		r()
		t.Fatal("a second holder took a held account lock")
	}
}

func TestMemorySettleStoreLock(t *testing.T) {
	s := NewMemorySettleStore()
	runLockContract(t, s, s)
}

// Two SQLSettleStore instances on one database are two replicas: the row lock
// must serialise them, not the in-process queue.
func TestSQLSettleStoreLockAcrossReplicas(t *testing.T) {
	a := newSQLSettleStore(t)
	b := NewSQLSettleStore(a.db)
	runLockContract(t, a, b)
}
