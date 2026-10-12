package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/order"
	"github.com/qifalab/euler-platform/provision"
	"github.com/qifalab/euler-platform/resource"
)

// scriptedDriver is a provision.Driver whose observations the test dictates:
// Apply registers a resource in phase `next` (Provisioning by default), Query
// reports the phase set for it, and every call can be made to fail.
type scriptedDriver struct {
	mu        sync.Mutex
	next      provision.Phase
	phase     map[string]provision.Phase
	applied   map[string]int
	deleted   map[string]int
	queries   int
	applyErr  error
	queryErr  error
	deleteErr error
}

func newScriptedDriver() *scriptedDriver {
	return &scriptedDriver{next: provision.PhaseProvisioning, phase: map[string]provision.Phase{},
		applied: map[string]int{}, deleted: map[string]int{}}
}

func (d *scriptedDriver) Type() provision.DriverType { return provision.DriverMock }

func (d *scriptedDriver) Apply(spec provision.Spec) (provision.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.applied[spec.ResourceID]++
	if d.applyErr != nil {
		return provision.Status{}, d.applyErr
	}
	if _, ok := d.phase[spec.ResourceID]; !ok {
		d.phase[spec.ResourceID] = d.next
	}
	return provision.Status{ResourceID: spec.ResourceID, Phase: d.phase[spec.ResourceID]}, nil
}

func (d *scriptedDriver) Delete(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deleted[id]++
	if d.deleteErr != nil {
		return d.deleteErr
	}
	delete(d.phase, id)
	return nil
}

func (d *scriptedDriver) Query(id string) (provision.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queries++
	if d.queryErr != nil {
		return provision.Status{}, d.queryErr
	}
	ph, ok := d.phase[id]
	if !ok {
		return provision.Status{}, provision.ErrNotFound
	}
	return provision.Status{ResourceID: id, Phase: ph}, nil
}

func (d *scriptedDriver) CollectUsage(string) ([]provision.UsagePoint, error) { return nil, nil }

func (d *scriptedDriver) set(id string, ph provision.Phase) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.phase[id] = ph
}

func (d *scriptedDriver) counts(id string) (applied, deleted int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.applied[id], d.deleted[id]
}

// pollerFixture fulfils one order into a CREATING resource (the driver
// answers Provisioning) and returns its id and a clock the test moves.
type pollerFixture struct {
	s      *lifecycleStore
	d      *scriptedDriver
	resID  string
	now    time.Time
	poller *provisionPoller
}

func newPollerFixture(t *testing.T, s *lifecycleStore, orders acctOrders, o paidOrder, params map[string]any) *pollerFixture {
	t.Helper()
	f := &pollerFixture{s: s, d: newScriptedDriver(), now: time.Now()}
	s.driver = f.d
	s.clock = func() time.Time { return f.now }
	orders.add(100123, o)
	rec := fulfilAs(t, s, 100123, fulfilBodyFor(o, params))
	if rec.Code != 202 {
		t.Fatalf("fulfil: %d %s, want 202 provisioning", rec.Code, rec.Body.String())
	}
	f.resID = decodeFulfil(t, rec).Data.ResourceID
	f.poller = newProvisionPoller(s, time.Second, 30*time.Minute)
	return f
}

func (f *pollerFixture) state(t *testing.T) *resource.Instance {
	t.Helper()
	return mustGet(t, f.s.ledger, f.resID)
}

// O4: a resource the backend finishes after the fulfil answered 202 is
// delivered by the poller — RUNNING, prepaid expiry from the recorded term,
// COMPLETED reported, mirror COMPLETED — without anybody replaying the fulfil.
func TestPollerDeliversWhenReady(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		o := paidIn(501, "euecs", "cn-north-1")
		o.ChargeType, o.Duration, o.DurationUnit = "PREPAID", 1, "MONTH"
		f := newPollerFixture(t, s, orders, o, nil)

		if n := f.poller.tick(context.Background()); n != 0 {
			t.Fatalf("tick while provisioning settled %d", n)
		}
		if got := f.state(t); got.State != resource.StateCreating {
			t.Fatalf("state = %s, want CREATING", got.State)
		}
		f.d.set(f.resID, provision.PhaseReady)
		if n := f.poller.tick(context.Background()); n != 1 {
			t.Fatalf("tick after Ready settled %d, want 1", n)
		}
		got := f.state(t)
		if got.State != resource.StateRunning || got.BillingStart.IsZero() || got.ExpiredAt.IsZero() {
			t.Fatalf("delivered = %+v, want RUNNING with billing start and prepaid expiry", got)
		}
		if want := prepaidExpiry(f.now, 1, "MONTH"); got.ExpiredAt.Sub(want).Abs() > time.Second {
			t.Fatalf("expiry %v, want %v (one month from delivery)", got.ExpiredAt, want)
		}
		reps := reportsOf(s).calls()
		if len(reps) != 1 || reps[0].orderID != 501 || reps[0].rep.Result != fulfilmentCompleted || reps[0].rep.ResourceID != f.resID {
			t.Fatalf("reports = %+v", reps)
		}
		if st := s.mirrorState(501); st != order.StateCompleted {
			t.Fatalf("mirror = %s, want COMPLETED", st)
		}
		// Settled resources are not scanned again.
		if n := f.poller.tick(context.Background()); n != 0 || len(reportsOf(s).calls()) != 1 {
			t.Fatalf("a settled resource was settled again")
		}
	})
}

// O4: a backend that reports Failed is compensated exactly like a failed
// fulfil: reclaim, CREATE_FAILED, FAILED reported (refund).
func TestPollerCompensatesFailed(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		f := newPollerFixture(t, s, orders, paidIn(502, "euecs", "cn-north-1"), nil)
		f.d.set(f.resID, provision.PhaseFailed)
		if n := f.poller.tick(context.Background()); n != 1 {
			t.Fatalf("tick settled %d, want 1", n)
		}
		if got := f.state(t); got.State != resource.StateCreateFailed {
			t.Fatalf("state = %s, want CREATE_FAILED", got.State)
		}
		if _, deleted := f.d.counts(f.resID); deleted != 1 {
			t.Fatalf("deletes = %d, want 1 (the reclaim)", deleted)
		}
		reps := reportsOf(s).calls()
		if len(reps) != 1 || reps[0].rep.Result != fulfilmentFailed {
			t.Fatalf("reports = %+v, want one FAILED", reps)
		}
		if st := s.mirrorState(502); st != order.StateRefunding {
			t.Fatalf("mirror = %s, want REFUNDING", st)
		}
	})
}

// O4: the CREATING timeout compensates a resource still booting past it —
// unless the backend reports it Ready right now (a late delivery is still a
// delivery). A reclaim that fails revokes its claim and is retried.
func TestPollerCreatingTimeout(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		f := newPollerFixture(t, s, orders, paidIn(503, "euecs", "cn-north-1"), nil)
		f.now = f.now.Add(29 * time.Minute)
		if n := f.poller.tick(context.Background()); n != 0 {
			t.Fatalf("compensated before the timeout")
		}
		f.now = f.now.Add(2 * time.Minute)
		f.d.deleteErr = errors.New("rc-compute unavailable")
		if n := f.poller.tick(context.Background()); n != 0 {
			t.Fatalf("settled although the reclaim failed")
		}
		got := f.state(t)
		attrs, _ := s.ledger.Attrs(100123, f.resID)
		if got.State != resource.StateCreating || attrs[attrCompensating] != "" {
			t.Fatalf("after a failed reclaim: %s, claim %q; want CREATING with the claim revoked", got.State, attrs[attrCompensating])
		}
		if len(reportsOf(s).calls()) != 0 {
			t.Fatal("reported before the resource was reclaimed")
		}
		f.d.deleteErr = nil
		if n := f.poller.tick(context.Background()); n != 1 {
			t.Fatalf("retry settled %d, want 1", n)
		}
		if got := f.state(t); got.State != resource.StateCreateFailed {
			t.Fatalf("state = %s, want CREATE_FAILED", got.State)
		}
		if reps := reportsOf(s).calls(); len(reps) != 1 || reps[0].rep.Result != fulfilmentFailed {
			t.Fatalf("reports = %+v", reps)
		}
		if sl, ok := s.ledger.(*sqlLedger); ok {
			var reason string
			if err := sl.db.QueryRow(`SELECT reason FROM resource_state_log WHERE resource_id = ? AND to_status = 'CREATE_FAILED'`,
				f.resID).Scan(&reason); err != nil || reason == "" {
				t.Fatalf("CREATE_FAILED journal reason %q: %v", reason, err)
			}
		}

		// Past the timeout but Ready: delivered, not torn down.
		g := newPollerFixture(t, s, orders, paidIn(504, "euecs", "cn-north-1"), nil)
		g.now = g.now.Add(time.Hour)
		g.d.set(g.resID, provision.PhaseReady)
		if n := g.poller.tick(context.Background()); n != 1 {
			t.Fatalf("late Ready settled %d", n)
		}
		if got := g.state(t); got.State != resource.StateRunning {
			t.Fatalf("late Ready = %s, want RUNNING", got.State)
		}
		if _, deleted := g.d.counts(g.resID); deleted != 0 {
			t.Fatal("a Ready resource was reclaimed for being late")
		}
	})
}

// O4: a backend that lost the resource (an in-memory backend restarted, a
// dispatch went missing) is re-dispatched like a fulfil replay would; a
// transient query error changes nothing.
func TestPollerRedispatchesLostResourceAndToleratesQueryErrors(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		f := newPollerFixture(t, s, orders, paidIn(505, "euecs", "cn-north-1"), map[string]any{"name": "lost"})
		f.d.mu.Lock()
		delete(f.d.phase, f.resID)
		f.d.next = provision.PhaseReady
		f.d.mu.Unlock()
		if n := f.poller.tick(context.Background()); n != 1 {
			t.Fatalf("re-dispatch settled %d, want 1", n)
		}
		if applied, _ := f.d.counts(f.resID); applied != 2 {
			t.Fatalf("applies = %d, want the fulfil's and the re-dispatch", applied)
		}
		if got := f.state(t); got.State != resource.StateRunning {
			t.Fatalf("state = %s", got.State)
		}

		g := newPollerFixture(t, s, orders, paidIn(506, "euecs", "cn-north-1"), nil)
		g.d.queryErr = errors.New("rc-compute: deadline exceeded")
		if n := g.poller.tick(context.Background()); n != 0 {
			t.Fatalf("a query error settled %d", n)
		}
		if got := g.state(t); got.State != resource.StateCreating {
			t.Fatalf("state after a query error = %s", got.State)
		}
	})
}

// O4: several replicas poll the same ledger at once; the resource settles
// once and is reported once.
func TestPollerReplicasSettleOnce(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		f := newPollerFixture(t, s, orders, paidIn(507, "euecs", "cn-north-1"), nil)
		f.d.set(f.resID, provision.PhaseReady)
		replicas := make([]*lifecycleStore, 4)
		for i := range replicas {
			r, _ := newMultiStore()
			r.ledger, r.driver, r.orderSrc, r.clock = s.ledger, f.d, s.orderSrc, s.clock
			r.orderSink = s.orderSink // one svc-order sees every replica's reports
			replicas[i] = r
		}
		var wg sync.WaitGroup
		settled := make([]int, len(replicas))
		for i, r := range replicas {
			wg.Add(1)
			go func(i int, r *lifecycleStore) {
				defer wg.Done()
				settled[i] = newProvisionPoller(r, time.Second, 30*time.Minute).tick(context.Background())
			}(i, r)
		}
		wg.Wait()
		total := 0
		for _, n := range settled {
			total += n
		}
		if total != 1 {
			t.Fatalf("replicas settled %v, want exactly one settle", settled)
		}
		if reps := reportsOf(s).calls(); len(reps) != 1 || reps[0].rep.Result != fulfilmentCompleted {
			t.Fatalf("reports = %+v, want one COMPLETED", reps)
		}
		if got := f.state(t); got.State != resource.StateRunning || got.Version != 2 {
			t.Fatalf("state = %s v%d, want RUNNING v2 (one edge)", got.State, got.Version)
		}
	})
}

// The compensation claim: once one replica claimed a resource's compensation,
// another that sees Ready must not deliver it (the reclaim may be under way);
// it resumes the compensation instead. A settle working from a row read
// before the claim loses its RUNNING edge on the version.
func TestCompensationClaimBlocksDelivery(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		f := newPollerFixture(t, s, orders, paidIn(508, "euecs", "cn-north-1"), nil)
		stale := f.state(t)
		staleAttrs, _ := s.ledger.Attrs(100123, f.resID)

		claimed := f.state(t)
		if err := s.ledger.MarkCompensation(claimed, "replica A: provisioning timed out", true); err != nil {
			t.Fatal(err)
		}
		if claimed.Version != stale.Version+1 {
			t.Fatalf("claim did not bump the version: v%d → v%d", stale.Version, claimed.Version)
		}
		// A claim is not progress: updated_at (what the stuck-resource views
		// key on) does not move.
		if got := f.state(t); !got.UpdatedAt.Equal(stale.UpdatedAt) || got.Version != claimed.Version {
			t.Fatalf("after the claim: updated_at %v → %v, version %d (caller saw %d)",
				stale.UpdatedAt, got.UpdatedAt, got.Version, claimed.Version)
		}
		// Replica B read the row before the claim and now sees Ready.
		ready := provisionObservation{observed: true, status: provision.Status{Phase: provision.PhaseReady}}
		res := s.settleCreating(context.Background(), stale, staleAttrs, ready, "", prepaidTerm{})
		if res.kind == settleDelivered {
			t.Fatal("a settle from a pre-claim read delivered a resource under compensation")
		}
		if got := f.state(t); got.State != resource.StateCreating {
			t.Fatalf("state = %s, want CREATING (claimed)", got.State)
		}
		// Replica C reads after the claim: it resumes the compensation.
		f.d.set(f.resID, provision.PhaseReady)
		if n := f.poller.tick(context.Background()); n != 1 {
			t.Fatalf("tick settled %d", n)
		}
		if got := f.state(t); got.State != resource.StateCreateFailed {
			t.Fatalf("state = %s, want CREATE_FAILED (the claim wins over Ready)", got.State)
		}
		if _, deleted := f.d.counts(f.resID); deleted != 1 {
			t.Fatalf("deletes = %d", deleted)
		}
		// A claim on a row that moved on is refused.
		if err := s.ledger.MarkCompensation(stale, "late", true); err == nil {
			t.Fatal("a stale claim succeeded")
		}
	})
}

func TestPollerRunStopsOnCancel(t *testing.T) {
	s := newTestStore()
	p := newProvisionPoller(s, time.Millisecond, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.run(ctx) }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("poller did not stop on cancel")
	}
}

func TestDurationEnv(t *testing.T) {
	t.Setenv(pollIntervalEnv, "")
	if d, err := durationEnv(pollIntervalEnv, defaultPollInterval); err != nil || d != 15*time.Second {
		t.Fatalf("default = %v %v", d, err)
	}
	t.Setenv(pollIntervalEnv, "2m")
	if d, err := durationEnv(pollIntervalEnv, defaultPollInterval); err != nil || d != 2*time.Minute {
		t.Fatalf("2m = %v %v", d, err)
	}
	for _, bad := range []string{"0s", "-5s", "fast", "15"} {
		t.Setenv(pollIntervalEnv, bad)
		if _, err := durationEnv(pollIntervalEnv, defaultPollInterval); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
