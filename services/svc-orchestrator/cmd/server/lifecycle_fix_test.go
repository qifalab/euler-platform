package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/order"
	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/provision"
	"github.com/qifalab/euler-platform/resource"
)

// --- helpers -----------------------------------------------------------------

const seededECS = "euecs-cn-north-1-01-a1b2c3d4"  // RUNNING, PREPAID
const seededVPC = "euvpc-cn-north-1-01-vpc0a1b2c" // RUNNING, POSTPAID

type fulfilBody struct {
	Code string `json:"Code"`
	Data struct {
		ResourceID   string `json:"resourceId"`
		State        string `json:"state"`
		OrderState   string `json:"orderState"`
		BillingStart string `json:"billingStart"`
		ExpiredAt    string `json:"expiredAt"`
	} `json:"Data"`
}

func decodeFulfil(t *testing.T, rec *httptest.ResponseRecorder) fulfilBody {
	t.Helper()
	var b fulfilBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return b
}

func mustGet(t *testing.T, l resourceLedger, id string) *resource.Instance {
	t.Helper()
	inst, err := l.Get(100123, id)
	if err != nil || inst == nil {
		t.Fatalf("get %s: %v %v", id, inst, err)
	}
	return inst
}

func doRelease(t *testing.T, s *lifecycleStore, resID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/orchestrator/resources/"+resID+"/release", nil)
	req.SetPathValue("id", resID)
	req.Header.Set(accountIDHeader, "100123")
	rec := httptest.NewRecorder()
	s.handleRelease(rec, req)
	return rec
}

// putInstance creates an instance and walks it to state through legal edges.
func putInstance(t *testing.T, l resourceLedger, id string, ct resource.ChargeType, path ...resource.State) *resource.Instance {
	t.Helper()
	now := time.Now()
	inst := &resource.Instance{
		ResourceID: id, AccountID: 100123, ProductCode: "euecs", ResourceType: "instance",
		Region: "cn-north-1", ChargeType: ct, State: resource.StateCreating,
		SpecCode: "euecs.s2.large", Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := l.Create(inst); err != nil {
		t.Fatal(err)
	}
	for _, to := range path {
		if err := l.Transition(inst, to, "test setup"); err != nil {
			t.Fatalf("setup %s → %s: %v", inst.State, to, err)
		}
	}
	return inst
}

// --- O1: start/restart only from their own states ----------------------------

func TestStartRequiresStoppedRestartRequiresRunning(t *testing.T) {
	s := newTestStore()
	creating := putInstance(t, s.ledger, "euecs-cn-north-1-07-00000101", resource.ChargePostpaid)
	locked := putInstance(t, s.ledger, "euecs-cn-north-1-07-00000102", resource.ChargePostpaid,
		resource.StateRunning, resource.StateLocked)
	upgrading := putInstance(t, s.ledger, "euecs-cn-north-1-07-00000103", resource.ChargePostpaid,
		resource.StateRunning, resource.StateUpgrading)
	for _, inst := range []*resource.Instance{creating, locked, upgrading} {
		for _, action := range []string{"start", "restart"} {
			if rec := doAction(t, s, inst.ResourceID, `{"action":"`+action+`"}`); rec.Code != 409 {
				t.Fatalf("%s from %s: want 409, got %d %s", action, inst.State, rec.Code, rec.Body.String())
			}
			if got := mustGet(t, s.ledger, inst.ResourceID); got.State != inst.State {
				t.Fatalf("%s from %s moved the resource to %s", action, inst.State, got.State)
			}
		}
	}

	// The legal paths still work: restart from RUNNING, start from STOPPED,
	// and restart refuses a STOPPED instance (that would be a start).
	if rec := doAction(t, s, seededECS, `{"action":"restart"}`); rec.Code != 200 {
		t.Fatalf("restart RUNNING: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doAction(t, s, seededECS, `{"action":"stop"}`); rec.Code != 200 {
		t.Fatalf("stop: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doAction(t, s, seededECS, `{"action":"restart"}`); rec.Code != 409 {
		t.Fatalf("restart STOPPED: want 409, got %d", rec.Code)
	}
	if rec := doAction(t, s, seededECS, `{"action":"start"}`); rec.Code != 200 {
		t.Fatalf("start STOPPED: %d %s", rec.Code, rec.Body.String())
	}
	if got := mustGet(t, s.ledger, seededECS); got.State != resource.StateRunning {
		t.Fatalf("state = %s, want RUNNING", got.State)
	}
}

// --- O2: a failed fulfil must not wedge the order mirror -----------------------

// flakyLedger fails the first createFails Creates, the first runningFails
// transitions to RUNNING and the first createFailedFails transitions to
// CREATE_FAILED.
type flakyLedger struct {
	resourceLedger
	mu                sync.Mutex
	createFails       int
	runningFails      int
	createFailedFails int
}

func (f *flakyLedger) Create(inst *resource.Instance, opts ...createOption) error {
	f.mu.Lock()
	fail := f.createFails > 0
	if fail {
		f.createFails--
	}
	f.mu.Unlock()
	if fail {
		return errors.New("Error 2013: Lost connection to MySQL server during query")
	}
	return f.resourceLedger.Create(inst, opts...)
}

func (f *flakyLedger) Transition(inst *resource.Instance, to resource.State, reason string) error {
	f.mu.Lock()
	var fail bool
	switch {
	case to == resource.StateRunning && f.runningFails > 0:
		f.runningFails--
		fail = true
	case to == resource.StateCreateFailed && f.createFailedFails > 0:
		f.createFailedFails--
		fail = true
	}
	f.mu.Unlock()
	if fail {
		return errors.New("Error 2013: Lost connection to MySQL server during query")
	}
	return f.resourceLedger.Transition(inst, to, reason)
}

func TestFulfilRetryAfterLedgerCreateFailure(t *testing.T) {
	s := newTestStore(paid(31, "euecs", "euecs.c1"))
	s.ledger = &flakyLedger{resourceLedger: s.ledger, createFails: 1}
	if rec := doFulfill(t, s, fulfillRequest{OrderID: 31, ProductCode: "euecs"}); rec.Code != 500 {
		t.Fatalf("first fulfil: want 500, got %d %s", rec.Code, rec.Body.String())
	}
	rec := doFulfill(t, s, fulfillRequest{OrderID: 31, ProductCode: "euecs"})
	if rec.Code != 200 {
		t.Fatalf("retry after a failed create: want 200, got %d %s (mirror stuck in FULFILLING?)", rec.Code, rec.Body.String())
	}
	if b := decodeFulfil(t, rec); b.Data.State != "RUNNING" || b.Data.OrderState != "COMPLETED" {
		t.Fatalf("retry result = %+v", b.Data)
	}
}

func TestFulfilRetryAfterRunningTransitionFailure(t *testing.T) {
	s := newTestStore(paid(32, "euecs", "euecs.c1"))
	s.ledger = &flakyLedger{resourceLedger: s.ledger, runningFails: 1}
	if rec := doFulfill(t, s, fulfillRequest{OrderID: 32, ProductCode: "euecs"}); rec.Code != 409 {
		t.Fatalf("first fulfil: want 409, got %d %s", rec.Code, rec.Body.String())
	}
	rec := doFulfill(t, s, fulfillRequest{OrderID: 32, ProductCode: "euecs"})
	if rec.Code != 200 {
		t.Fatalf("retry: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	b := decodeFulfil(t, rec)
	if b.Data.State != "RUNNING" || b.Data.OrderState != "COMPLETED" {
		t.Fatalf("retry did not finish the saga: %+v", b.Data)
	}
	if n := resourceCount(t, s); n != 3 { // two seeds + this order
		t.Fatalf("resources = %d, want 3", n)
	}
}

// --- O3: a release whose driver Delete failed can be retried ------------------

func TestReleaseRetryAfterDeleteFailure(t *testing.T) {
	s := newTestStore(paid(33, "euecs", "euecs.c1"))
	md := provision.NewMockDriver(nil)
	s.driver = md
	b := decodeFulfil(t, doFulfill(t, s, fulfillRequest{OrderID: 33, ProductCode: "euecs"}))
	resID := b.Data.ResourceID

	md.FailDelete = func(string) error { return errors.New("rc-compute unavailable") }
	if rec := doRelease(t, s, resID); rec.Code != 500 {
		t.Fatalf("release with failing driver: want 500, got %d %s", rec.Code, rec.Body.String())
	}
	if got := mustGet(t, s.ledger, resID); got.State != resource.StateReleasing {
		t.Fatalf("state = %s, want RELEASING", got.State)
	}
	md.FailDelete = nil
	if rec := doRelease(t, s, resID); rec.Code != 200 {
		t.Fatalf("release retry: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	if got := mustGet(t, s.ledger, resID); got.State != resource.StateReleased {
		t.Fatalf("state = %s, want RELEASED", got.State)
	}
}

// --- O4: compensation reclaims; provisioning is not failure --------------------

func TestFulfilCompensationDeletesAndRefunds(t *testing.T) {
	s := newTestStore(paid(34, "euecs", "euecs.c1"))
	md := provision.NewMockDriver(nil)
	md.FailApply = func(string) error { return errors.New("quota exhausted in rc-compute") }
	var deleted []string
	md.FailDelete = func(id string) error { deleted = append(deleted, id); return nil }
	s.driver = md

	rec := doFulfill(t, s, fulfillRequest{OrderID: 34, ProductCode: "euecs"})
	if rec.Code != 500 {
		t.Fatalf("want 500, got %d %s", rec.Code, rec.Body.String())
	}
	resID := fulfilResourceID("euecs", "cn-north-1", 100123, 34)
	if len(deleted) != 1 || deleted[0] != resID {
		t.Fatalf("compensation must reclaim via driver.Delete: deleted %v", deleted)
	}
	if got := mustGet(t, s.ledger, resID); got.State != resource.StateCreateFailed {
		t.Fatalf("state = %s, want CREATE_FAILED", got.State)
	}
	if o := s.orders[34]; o.State != order.StateRefunding {
		t.Fatalf("order = %s, want REFUNDING", o.State)
	}
	// A replay must not tell the console the instance was created.
	if rec := doFulfill(t, s, fulfillRequest{OrderID: 34, ProductCode: "euecs"}); rec.Code != 409 {
		t.Fatalf("replay of a failed fulfil: want 409, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestFulfilCompensationKeepsCreatingWhenDeleteFails(t *testing.T) {
	s := newTestStore(paid(35, "euecs", "euecs.c1"))
	md := provision.NewMockDriver(nil)
	md.FailApply = func(string) error { return errors.New("rc-compute timeout") }
	md.FailDelete = func(string) error { return errors.New("rc-compute timeout") }
	s.driver = md

	if rec := doFulfill(t, s, fulfillRequest{OrderID: 35, ProductCode: "euecs"}); rec.Code != 500 {
		t.Fatalf("want 500, got %d %s", rec.Code, rec.Body.String())
	}
	resID := fulfilResourceID("euecs", "cn-north-1", 100123, 35)
	if got := mustGet(t, s.ledger, resID); got.State != resource.StateCreating {
		t.Fatalf("unreclaimed resource marked %s; want it kept CREATING for retry", got.State)
	}
	if o := s.orders[35]; o.State != order.StateFulfilling {
		t.Fatalf("order = %s: must not refund while the resource may still exist", o.State)
	}

	// The backend recovers: the retry re-drives the saga to completion.
	md.FailApply, md.FailDelete = nil, nil
	rec := doFulfill(t, s, fulfillRequest{OrderID: 35, ProductCode: "euecs"})
	if rec.Code != 200 {
		t.Fatalf("retry: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	if b := decodeFulfil(t, rec); b.Data.ResourceID != resID || b.Data.State != "RUNNING" || b.Data.OrderState != "COMPLETED" {
		t.Fatalf("retry result = %+v", b.Data)
	}
}

// L4: the compensation reclaimed the resource but its CREATE_FAILED edge did
// not land. The mirror still went REFUNDING while the row stayed CREATING, so
// every replay answered 202 "provisioning" and FAILED never reached svc-order.
// The mirror now stays FULFILLING until the ledger records the failure, and a
// replay resumes the compensation instead of provisioning the reclaimed
// resource again.
func TestFulfilReplayResumesCompensationAfterCreateFailedTransitionFailure(t *testing.T) {
	s := newTestStore(paid(37, "euecs", "euecs.c1"))
	s.ledger = &flakyLedger{resourceLedger: s.ledger, createFailedFails: 1}
	md := provision.NewMockDriver(nil)
	md.FailApply = func(string) error { return errors.New("quota exhausted in rc-compute") }
	var deleted int
	md.FailDelete = func(string) error { deleted++; return nil }
	s.driver = md
	resID := fulfilResourceID("euecs", "cn-north-1", 100123, 37)

	rec := doFulfill(t, s, fulfillRequest{OrderID: 37, ProductCode: "euecs"})
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "Provision.Failed") {
		t.Fatalf("first fulfil: %d %s, want 500 Provision.Failed", rec.Code, rec.Body.String())
	}
	if got := mustGet(t, s.ledger, resID); got.State != resource.StateCreating || deleted != 1 {
		t.Fatalf("after a failed CREATE_FAILED edge: state %s, %d deletes; want CREATING, 1", got.State, deleted)
	}
	if o := s.orders[37]; o.State != order.StateFulfilling {
		t.Fatalf("mirror = %s, want FULFILLING until the ledger records the failure", o.State)
	}
	if got := reportsOf(s).calls(); len(got) != 0 {
		t.Fatalf("reports = %+v, want none before the resource settled", got)
	}

	// The backend would now provision fine: the replay must not re-apply the
	// reclaimed resource, and must not answer 202.
	var applied int
	md.FailApply = func(string) error { applied++; return nil }
	rec = doFulfill(t, s, fulfillRequest{OrderID: 37, ProductCode: "euecs"})
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "Provision.Failed") {
		t.Fatalf("replay: %d %s, want the compensation's 500 Provision.Failed", rec.Code, rec.Body.String())
	}
	if applied != 0 {
		t.Fatalf("replay re-applied the reclaimed resource %d time(s)", applied)
	}
	if got := mustGet(t, s.ledger, resID); got.State != resource.StateCreateFailed {
		t.Fatalf("state after replay = %s, want CREATE_FAILED", got.State)
	}
	if o := s.orders[37]; o.State != order.StateRefunding {
		t.Fatalf("mirror after replay = %s, want REFUNDING", o.State)
	}
	got := reportsOf(s).calls()
	if len(got) != 1 || got[0].orderID != 37 || got[0].rep.Result != fulfilmentFailed {
		t.Fatalf("reports = %+v, want one FAILED for order 37", got)
	}
	if deleted != 2 {
		t.Fatalf("deletes = %d, want the replay to repeat the (idempotent) reclaim once", deleted)
	}

	// From here on it is an ordinary settled failure.
	if rec := doFulfill(t, s, fulfillRequest{OrderID: 37, ProductCode: "euecs"}); rec.Code != 409 {
		t.Fatalf("second replay: %d %s, want 409", rec.Code, rec.Body.String())
	}
	if got := reportsOf(s).calls(); len(got) != 2 || got[1].rep.Result != fulfilmentFailed {
		t.Fatalf("reports = %+v, want FAILED re-sent", got)
	}
}

func TestFulfilProvisioningIsAcceptedNotFailed(t *testing.T) {
	s := newTestStore(paid(36, "euecs", "euecs.c1"))
	md := provision.NewMockDriver(nil)
	md.ProvisioningDelay = 2
	var deleted int
	md.FailDelete = func(string) error { deleted++; return nil }
	s.driver = md

	rec := doFulfill(t, s, fulfillRequest{OrderID: 36, ProductCode: "euecs"})
	if rec.Code != 202 {
		t.Fatalf("still provisioning: want 202, got %d %s", rec.Code, rec.Body.String())
	}
	b := decodeFulfil(t, rec)
	// The console SDK treats any 2xx with Code OK as success.
	if b.Code != "OK" || b.Data.State != "CREATING" || b.Data.OrderState != "FULFILLING" || b.Data.BillingStart != "" {
		t.Fatalf("202 body = %+v", b)
	}
	if deleted != 0 {
		t.Fatal("a resource still booting was torn down")
	}
	// A replay while it is still booting is again 202, not a second resource.
	if rec := doFulfill(t, s, fulfillRequest{OrderID: 36, ProductCode: "euecs"}); rec.Code != 202 {
		t.Fatalf("replay while provisioning: want 202, got %d", rec.Code)
	}
	// Once the backend reports Ready, a retry completes the saga.
	for i := 0; i < 2; i++ {
		_, _ = md.Query(b.Data.ResourceID)
	}
	rec = doFulfill(t, s, fulfillRequest{OrderID: 36, ProductCode: "euecs"})
	if rec.Code != 200 {
		t.Fatalf("retry after Ready: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	if b := decodeFulfil(t, rec); b.Data.State != "RUNNING" || b.Data.OrderState != "COMPLETED" || b.Data.BillingStart == "" {
		t.Fatalf("after Ready = %+v", b.Data)
	}
}

// --- O5: one order, one resource, whichever replica wins ----------------------

// hidingLedger hides existing rows from the first two ByOrder calls — the
// pre-checks (lock-free, then under the store lock) a second replica runs
// before the first replica's insert is visible to it.
type hidingLedger struct {
	resourceLedger
	mu     sync.Mutex
	hidden int
}

func (h *hidingLedger) ByOrder(acct, orderID int64) (*resource.Instance, error) {
	h.mu.Lock()
	hide := h.hidden < 2
	h.hidden++
	h.mu.Unlock()
	if hide {
		return nil, nil
	}
	return h.resourceLedger.ByOrder(acct, orderID)
}

func TestConcurrentFulfilOnTwoReplicasCreatesOneResource(t *testing.T) {
	shared := newMemLedger()
	replicaA := newTestStore(paid(41, "euecs", "euecs.c1"))
	replicaA.ledger = shared
	replicaB := newTestStore(paid(40, "euecs", "euecs.c1"), paid(41, "euecs", "euecs.c1"))
	replicaB.ledger = shared

	// Replica B has already fulfilled another order (a per-process counter
	// would now be ahead of replica A's).
	if rec := doFulfill(t, replicaB, fulfillRequest{OrderID: 40, ProductCode: "euecs"}); rec.Code != 200 {
		t.Fatalf("order 40: %d %s", rec.Code, rec.Body.String())
	}
	a := decodeFulfil(t, doFulfill(t, replicaA, fulfillRequest{OrderID: 41, ProductCode: "euecs"}))

	replicaB.ledger = &hidingLedger{resourceLedger: shared}
	rec := doFulfill(t, replicaB, fulfillRequest{OrderID: 41, ProductCode: "euecs"})
	if rec.Code != 200 {
		t.Fatalf("losing replica: want 200 with the winner's resource, got %d %s", rec.Code, rec.Body.String())
	}
	if b := decodeFulfil(t, rec); b.Data.ResourceID != a.Data.ResourceID {
		t.Fatalf("replica B served %q, want the winner %q", b.Data.ResourceID, a.Data.ResourceID)
	}
	list, _ := shared.List(100123)
	n := 0
	for _, inst := range list {
		if inst.OrderID == 41 {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("order 41 produced %d resources, want 1", n)
	}
}

func TestFulfilResourceIDIsDerivedFromOrder(t *testing.T) {
	id := fulfilResourceID("euecs", "cn-north-1", 100123, 42)
	if id != fulfilResourceID("euecs", "cn-north-1", 100123, 42) {
		t.Fatal("resource id is not a function of the order")
	}
	if id == fulfilResourceID("euecs", "cn-north-1", 100123, 43) {
		t.Fatal("two orders share a resource id")
	}
	if !strings.HasPrefix(id, "euecs-cn-north-1-") || !strings.HasSuffix(id, "-0000002a") {
		t.Fatalf("id %q does not follow {product}-{region}-{shard}-{hex8}", id)
	}
}

func TestMemLedgerOneResourcePerOrder(t *testing.T) {
	l := newMemLedger()
	if err := l.Create(newInstance("euecs-cn-north-1-01-00000050", 50)); err != nil {
		t.Fatal(err)
	}
	if err := l.Create(newInstance("euecs-cn-north-1-01-00000051", 50)); err == nil {
		t.Fatal("a second resource for one order was accepted")
	}
}

// --- O6 / O8: the in-memory ledger hands out snapshots, in SQL order -----------

func TestMemLedgerReturnsCopies(t *testing.T) {
	l := newMemLedger()
	inst := newInstance("euecs-cn-north-1-01-00000060", 60)
	if err := l.Create(inst); err != nil {
		t.Fatal(err)
	}
	inst.State = resource.StateReleased // the caller's struct is not the stored one
	got := mustGet(t, l, inst.ResourceID)
	if got.State != resource.StateCreating {
		t.Fatalf("Create stored the caller's pointer: %s", got.State)
	}
	got.SpecCode = "euecs.c8.huge"
	if again := mustGet(t, l, inst.ResourceID); again.SpecCode != "euecs.s2.large" {
		t.Fatalf("Get returned the stored pointer: %s", again.SpecCode)
	}
	list, _ := l.List(100123)
	list[0].State = resource.StateReleased
	if again := mustGet(t, l, inst.ResourceID); again.State != resource.StateCreating {
		t.Fatal("List returned stored pointers")
	}
	byOrder, _ := l.ByOrder(100123, 60)
	byOrder.State = resource.StateReleased
	if again := mustGet(t, l, inst.ResourceID); again.State != resource.StateCreating {
		t.Fatal("ByOrder returned the stored pointer")
	}
}

func TestMemLedgerTransitionKeepsMachineStamps(t *testing.T) {
	l := newMemLedger()
	inst := putInstance(t, l, "euecs-cn-north-1-01-00000061", resource.ChargePostpaid, resource.StateRunning)
	stale := mustGet(t, l, inst.ResourceID) // LockedAt zero
	if err := l.Transition(inst, resource.StateLocked, "arrears"); err != nil {
		t.Fatal(err)
	}
	if inst.LockedAt.IsZero() || mustGet(t, l, inst.ResourceID).LockedAt.IsZero() {
		t.Fatal("LockedAt stamped by the machine was overwritten with the caller's zero value")
	}
	// A stale view cannot move the row (the SQL ledger's version guard).
	if err := l.Transition(stale, resource.StateStopped, "stale"); err == nil {
		t.Fatal("a stale transition succeeded")
	}
	if err := l.Transition(inst, resource.StateReleasing, "retention over"); err != nil {
		t.Fatal(err)
	}
	if err := l.Transition(inst, resource.StateReleased, "released"); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, l, inst.ResourceID); got.ReleasedAt.IsZero() || got.LockedAt.IsZero() {
		t.Fatalf("machine stamps lost: locked %v released %v", got.LockedAt, got.ReleasedAt)
	}
}

// Run with -race: readers used to get the stored pointer and read it after
// the lock was released while Transition wrote it.
func TestMemLedgerConcurrentReadsAndTransitions(t *testing.T) {
	l := newMemLedger()
	inst := putInstance(t, l, "euecs-cn-north-1-01-00000062", resource.ChargePostpaid, resource.StateRunning)
	id := inst.ResourceID // inst itself belongs to the writer goroutine
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			to := resource.StateStopped
			if inst.State == resource.StateStopped {
				to = resource.StateRunning
			}
			if err := l.Transition(inst, to, "flip"); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if got, _ := l.Get(100123, id); got != nil {
				_ = got.State
				_ = got.Version
			}
			list, _ := l.List(100123)
			for _, x := range list {
				_ = x.State
			}
		}
	}()
	wg.Wait()
}

func TestMemLedgerListOrderMatchesSQL(t *testing.T) {
	l := newMemLedger()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i, spec := range []struct {
		id string
		at time.Time
	}{
		{"euecs-cn-north-1-01-0000000b", base},
		{"euecs-cn-north-1-01-0000000c", base.Add(time.Hour)},
		{"euecs-cn-north-1-01-0000000a", base},
	} {
		inst := newInstance(spec.id, int64(70+i))
		inst.CreatedAt = spec.at
		if err := l.Create(inst); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"euecs-cn-north-1-01-0000000c", "euecs-cn-north-1-01-0000000a", "euecs-cn-north-1-01-0000000b"}
	for round := 0; round < 5; round++ { // map iteration order varies per call
		list, _ := l.List(100123)
		for i, inst := range list {
			if inst.ResourceID != want[i] {
				t.Fatalf("List order = %v, want created_at DESC, resource_id", ids(list))
			}
		}
	}
}

func ids(list []*resource.Instance) []string {
	out := make([]string, len(list))
	for i, inst := range list {
		out[i] = inst.ResourceID
	}
	return out
}

// --- O9 / O10: upgrade persists the spec and keeps the charge type -------------

func TestUpgradePersistsSpecInEveryLedger(t *testing.T) {
	for name, ledger := range map[string]func(t *testing.T) resourceLedger{
		"memory": func(*testing.T) resourceLedger { return newMemLedger() },
		"mysql":  func(t *testing.T) resourceLedger { return newSQLTestLedger(t) },
	} {
		t.Run(name, func(t *testing.T) {
			s := newStoreWith(ledger(t))
			s.orderSrc = fakeOrders{}
			inst := putInstance(t, s.ledger, "euecs-cn-north-1-01-00000080", resource.ChargePostpaid, resource.StateRunning)
			rec := doAction(t, s, inst.ResourceID, `{"action":"upgrade","specCode":"euecs.s2.xlarge.postpaid"}`)
			if rec.Code != 200 {
				t.Fatalf("upgrade: %d %s", rec.Code, rec.Body.String())
			}
			got := mustGet(t, s.ledger, inst.ResourceID)
			if got.SpecCode != "euecs.s2.xlarge.postpaid" || got.State != resource.StateRunning {
				t.Fatalf("after upgrade the ledger holds %s/%s, want euecs.s2.xlarge.postpaid/RUNNING", got.SpecCode, got.State)
			}
		})
	}
}

func TestUpgradeRejectsSKUOfOtherChargeType(t *testing.T) {
	s := newTestStore()
	if rec := doAction(t, s, seededVPC, `{"action":"upgrade","specCode":"euvpc.premium.prepaid"}`); rec.Code != 400 {
		t.Fatalf("postpaid instance → prepaid SKU: want 400, got %d %s", rec.Code, rec.Body.String())
	}
	if got := mustGet(t, s.ledger, seededVPC); got.SpecCode != "euvpc.standard" || got.State != resource.StateRunning {
		t.Fatalf("refused upgrade changed the instance: %+v", got)
	}
	if rec := doAction(t, s, seededVPC, `{"action":"upgrade","specCode":"euvpc.premium.postpaid"}`); rec.Code != 200 {
		t.Fatalf("postpaid SKU: want 200, got %d %s", rec.Code, rec.Body.String())
	}
}

// --- O11: expiry and unset times ----------------------------------------------

func TestInstanceToMapTimes(t *testing.T) {
	inst := newInstance("euecs-cn-north-1-01-00000090", 90)
	m := instanceToMap(inst)
	if m["billingStart"] != "" || m["expiredAt"] != "" {
		t.Fatalf("unset times rendered as %q / %q, want empty", m["billingStart"], m["expiredAt"])
	}
	inst.BillingStart = testClock
	inst.ExpiredAt = testClock.AddDate(0, 1, 0)
	m = instanceToMap(inst)
	if m["billingStart"] != "2026-09-15T14:00:00Z" || m["expiredAt"] != "2026-10-15T14:00:00Z" {
		t.Fatalf("times = %q / %q", m["billingStart"], m["expiredAt"])
	}
}

func TestPrepaidExpiry(t *testing.T) {
	jan31 := time.Date(2026, 1, 31, 10, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		n    int64
		unit pricing.DurationUnit
		want time.Time
	}{
		{1, pricing.DurationMonth, time.Date(2026, 2, 28, 10, 30, 0, 0, time.UTC)},
		{3, pricing.DurationMonth, time.Date(2026, 4, 30, 10, 30, 0, 0, time.UTC)},
		{13, pricing.DurationMonth, time.Date(2027, 2, 28, 10, 30, 0, 0, time.UTC)},
		{1, pricing.DurationYear, time.Date(2027, 1, 31, 10, 30, 0, 0, time.UTC)},
		{36, pricing.DurationHour, jan31.Add(36 * time.Hour)},
		{0, pricing.DurationMonth, time.Time{}},
		{3, pricing.DurationUsage, time.Time{}},
		{3, "", time.Time{}},
		{1 << 40, pricing.DurationHour, time.Time{}},
	} {
		if got := prepaidExpiry(jan31, c.n, c.unit); !got.Equal(c.want) {
			t.Errorf("%d %s: got %v, want %v", c.n, c.unit, got, c.want)
		}
	}
}

func TestFulfilPrepaidSetsExpiredAt(t *testing.T) {
	prepaid := paid(91, "euecs", "euecs.c1")
	prepaid.Quantity, prepaid.Duration, prepaid.DurationUnit = 1, 3, pricing.DurationMonth
	postpaid := paid(92, "euecs", "euecs.c1.post")
	postpaid.ChargeType = "POSTPAID"
	legacy := paid(93, "euecs", "euecs.c1") // svc-order without the term fields
	s := newTestStore(prepaid, postpaid, legacy)

	b := decodeFulfil(t, doFulfill(t, s, fulfillRequest{OrderID: 91, ProductCode: "euecs"}))
	inst := mustGet(t, s.ledger, b.Data.ResourceID)
	if inst.ExpiredAt.IsZero() {
		t.Fatal("prepaid resource created without ExpiredAt")
	}
	if want := addMonthsClamped(inst.BillingStart, 3); inst.ExpiredAt.Sub(want).Abs() > 5*time.Second {
		t.Fatalf("ExpiredAt = %v, want ≈ billing start + 3 months (%v)", inst.ExpiredAt, want)
	}
	if b.Data.ExpiredAt == "" || instanceToMap(inst)["expiredAt"] == "" {
		t.Fatal("expiredAt missing from the API output")
	}

	for _, id := range []int64{92, 93} {
		b := decodeFulfil(t, doFulfill(t, s, fulfillRequest{OrderID: id, ProductCode: "euecs"}))
		if b.Data.ExpiredAt != "" || !mustGet(t, s.ledger, b.Data.ResourceID).ExpiredAt.IsZero() {
			t.Fatalf("order %d: unexpected expiry %q", id, b.Data.ExpiredAt)
		}
	}
}

func TestHTTPOrderSourceReadsTerm(t *testing.T) {
	withTerm := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := map[string]any{
			"orderId": 78, "orderNo": "SO78", "state": "PAID", "productCode": "euecs",
			"skuCode": "euecs.c1", "regionId": "cn-north-1", "chargeType": "PREPAID", "payableAmount": "64.8",
		}
		if withTerm {
			data["quantity"], data["duration"], data["durationUnit"] = 2, 3, "month"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": "OK", "Data": data})
	}))
	defer srv.Close()
	t.Setenv("EULER_SVC_ORDER_URL", srv.URL)
	src := newHTTPOrderSource()
	o, err := src.Order(context.Background(), 100123, 78)
	if err != nil || o.Quantity != 2 || o.Duration != 3 || o.DurationUnit != pricing.DurationMonth {
		t.Fatalf("order = %+v, %v", o, err)
	}
	withTerm = false
	o, err = src.Order(context.Background(), 100123, 78)
	if err != nil || o.Quantity != 0 || o.Duration != 0 || o.DurationUnit != "" {
		t.Fatalf("absent term must decode as zero values: %+v, %v", o, err)
	}
}

// --- O12: provision records --------------------------------------------------

func TestProvisionRecordStatus(t *testing.T) {
	for _, c := range []struct {
		name       string
		phase      provision.Phase
		err        error
		wantStatus string
		wantErr    string
	}{
		{"ready", provision.PhaseReady, nil, "DONE", ""},
		{"booting", provision.PhaseProvisioning, nil, "DISPATCHED", ""},
		{"failed phase", provision.PhaseFailed, nil, "FAILED", `driver reported phase "Failed"`},
		{"driver error", "", errors.New("boom"), "FAILED", "boom"},
	} {
		st, le := provisionRecordStatus(provision.Status{Phase: c.phase}, c.err)
		if st != c.wantStatus || le != c.wantErr || strings.Contains(le, "<nil>") {
			t.Errorf("%s: got (%s, %q), want (%s, %q)", c.name, st, le, c.wantStatus, c.wantErr)
		}
	}
}

func TestTruncateUTF8(t *testing.T) {
	s := strings.Repeat("开", 200) // 600 bytes
	got := truncateUTF8(s, 512)
	if len(got) > 512 || !strings.HasPrefix(s, got) || len(got) != 510 {
		t.Fatalf("truncated to %d bytes", len(got))
	}
	if truncateUTF8("short", 512) != "short" {
		t.Fatal("short strings must pass through")
	}
}

func TestSQLLedgerProvisionRecordLongError(t *testing.T) {
	ledger := newSQLTestLedger(t)
	inst := newInstance("euecs-cn-north-1-05-00000094", 94)
	if err := ledger.Create(inst); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("驱动错误 ", 400)
	if err := ledger.RecordProvision(provisionRecord{
		ResourceID: inst.ResourceID, AccountID: 100123, OpType: "CREATE", IdempotKey: "94",
		Params: "{}", Status: "FAILED", LastError: long,
	}); err != nil {
		t.Fatalf("an over-long driver error failed the upsert: %v", err)
	}
	var stored string
	if err := ledger.db.QueryRow(`SELECT last_error FROM provision_task WHERE idempot_key = '94'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "" || !strings.HasPrefix(long, stored) {
		t.Fatalf("stored last_error = %q", stored)
	}
}

// --- O5 / O7 / O8 / O13 against MySQL ------------------------------------------

func TestSQLLedgerOrderIsUnique(t *testing.T) {
	ledger := newSQLTestLedger(t)
	if err := ledger.Create(newInstance("euecs-cn-north-1-01-00000095", 95)); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Create(newInstance("euecs-cn-north-1-02-00000095", 95)); err == nil {
		t.Fatal("uk_order: a second resource for one order was inserted")
	}
	// Rows without an order (seeds, imports) are not constrained.
	for _, id := range []string{"euvpc-cn-north-1-01-00000001", "euvpc-cn-north-1-01-00000002"} {
		if err := ledger.Create(newInstance(id, 0)); err != nil {
			t.Fatalf("order-less row %s: %v", id, err)
		}
	}
}

func TestSQLLedgerFailedTransitionLeavesCallerUntouched(t *testing.T) {
	ledger := newSQLTestLedger(t)
	inst := newInstance("euecs-cn-north-1-01-00000096", 96)
	if err := ledger.Create(inst); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Transition(inst, resource.StateRunning, "provisioned"); err != nil {
		t.Fatal(err)
	}
	stale, _ := ledger.Get(100123, inst.ResourceID)
	if err := ledger.Transition(inst, resource.StateStopped, "winner"); err != nil {
		t.Fatal(err)
	}
	before := *stale
	if err := ledger.Transition(stale, resource.StateStopped, "loser"); err == nil {
		t.Fatal("stale transition succeeded")
	}
	if stale.State != before.State || stale.Version != before.Version || !stale.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("a failed transition mutated the caller: %s v%d → %s v%d", before.State, before.Version, stale.State, stale.Version)
	}
}

func TestLedgerListOrderAgrees(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	run := func(l resourceLedger) []string {
		for i, spec := range []struct {
			id string
			at time.Time
		}{
			{"euecs-cn-north-1-01-0000000b", base},
			{"euecs-cn-north-1-01-0000000c", base.Add(time.Hour)},
			{"euecs-cn-north-1-01-0000000a", base},
		} {
			inst := newInstance(spec.id, int64(97+i))
			inst.CreatedAt = spec.at
			if err := l.Create(inst); err != nil {
				t.Fatal(err)
			}
		}
		list, err := l.List(100123)
		if err != nil {
			t.Fatal(err)
		}
		return ids(list)
	}
	sqlOrder := run(newSQLTestLedger(t))
	memOrder := run(newMemLedger())
	if strings.Join(sqlOrder, ",") != strings.Join(memOrder, ",") {
		t.Fatalf("list order differs: sql %v, memory %v", sqlOrder, memOrder)
	}
}

func TestSQLLedgerWritesAZ(t *testing.T) {
	ledger := newSQLTestLedger(t)
	inst := newInstance("euecs-cn-north-1-01-00000099", 99)
	inst.Zone = "cn-north-1-a"
	if err := ledger.Create(inst); err != nil {
		t.Fatal(err)
	}
	var az, zone string
	if err := ledger.db.QueryRow(`SELECT az_id, zone FROM resource_instance WHERE resource_id = ?`,
		inst.ResourceID).Scan(&az, &zone); err != nil {
		t.Fatal(err)
	}
	if az != "cn-north-1-a" || zone != "cn-north-1-a" {
		t.Fatalf("az_id = %q, zone = %q", az, zone)
	}
	var n int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM v_resource_az_failover_impact WHERE az_id = 'cn-north-1-a'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("AZ failover view sees %d resources, want 1", n)
	}
}
