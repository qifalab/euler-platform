package provision

import (
	"errors"
	"testing"
	"time"
)

var testNow = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

func newDriver() *MockDriver {
	return NewMockDriver(func() time.Time { return testNow })
}

func validSpec() Spec {
	return Spec{
		ResourceID:     "euecs-cn-north-1-01-a1b2c3d4",
		AccountID:      100123,
		ProjectID:      1,
		ProductCode:    "euecs",
		ResourceType:   "instance",
		Region:         "cn-north-1",
		Zone:           "cn-north-1-a",
		Edition:        "standard",
		Params:         map[string]string{"cpu": "2", "mem_gb": "4"},
		IdempotencyKey: "order-9001",
	}
}

// --- Spec validation ---

func TestSpecRequiresTenantAttribution(t *testing.T) {
	// A resource with no tenant cannot be isolated, billed or audited — all
	// three break at once.
	s := validSpec()
	s.AccountID = 0
	if err := s.Validate(); err == nil {
		t.Fatal("a spec without AccountID must be rejected")
	}
}

func TestSpecValidationCoversRequiredFields(t *testing.T) {
	for _, mutate := range []func(*Spec){
		func(s *Spec) { s.ResourceID = "" },
		func(s *Spec) { s.ProductCode = "" },
		func(s *Spec) { s.Region = "" },
		func(s *Spec) { s.IdempotencyKey = "" },
	} {
		s := validSpec()
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("incomplete spec accepted: %+v", s)
		}
	}
	if err := validSpec().Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
}

// TestMandatoryLabels covers the four labels every product CR must carry
// (06§4.2). Without them a cluster object cannot be traced to a tenant.
func TestMandatoryLabels(t *testing.T) {
	labels := validSpec().Labels()
	for _, key := range []string{LabelTenant, LabelProject, LabelProduct, LabelInstance} {
		if labels[key] == "" {
			t.Errorf("mandatory label %s missing", key)
		}
	}
	if labels[LabelTenant] != "100123" {
		t.Errorf("tenant label = %s, want 100123", labels[LabelTenant])
	}
	if labels[LabelInstance] != "euecs-cn-north-1-01-a1b2c3d4" {
		t.Errorf("instance label = %s", labels[LabelInstance])
	}
}

// --- Idempotency ---

// TestApplyIsIdempotent covers iron rule 3: the control plane retries, polls
// and redelivers, so a driver that cannot tolerate a repeat will corrupt state
// exactly when it is most likely to see one.
func TestApplyIsIdempotent(t *testing.T) {
	d := newDriver()
	spec := validSpec()

	first, err := d.Apply(spec)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		again, err := d.Apply(spec)
		if err != nil {
			t.Fatalf("repeat apply %d failed: %v", i, err)
		}
		if again.ResourceID != first.ResourceID {
			t.Fatal("repeat apply created a different resource")
		}
	}
}

// TestDeleteAlreadyDeletedSucceeds is what makes saga compensation safe: an
// error here would turn a successful cleanup into a false escalation.
func TestDeleteAlreadyDeletedSucceeds(t *testing.T) {
	d := newDriver()
	spec := validSpec()
	if _, err := d.Apply(spec); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(spec.ResourceID); err != nil {
		t.Fatal(err)
	}
	// Compensation retries.
	for i := 0; i < 3; i++ {
		if err := d.Delete(spec.ResourceID); err != nil {
			t.Fatalf("repeat delete %d must succeed, got %v", i, err)
		}
	}
	// Deleting something that never existed also succeeds.
	if err := d.Delete("euecs-cn-north-1-01-ffffffff"); err != nil {
		t.Fatalf("deleting a nonexistent resource must succeed, got %v", err)
	}
}

// --- Provisioning lifecycle ---

func TestProvisioningReachesReady(t *testing.T) {
	d := newDriver()
	d.ProvisioningDelay = 3
	spec := validSpec()

	st, _ := d.Apply(spec)
	if st.Phase != PhaseProvisioning {
		t.Fatalf("initial phase = %s, want Provisioning", st.Phase)
	}

	// Polling is the fallback for lost callbacks (03§5.3).
	for i := 0; i < 2; i++ {
		st, _ = d.Query(spec.ResourceID)
		if st.Ready() {
			t.Fatalf("became ready too early, after %d polls", i+1)
		}
	}
	st, _ = d.Query(spec.ResourceID)
	if !st.Ready() {
		t.Fatalf("phase = %s after enough polls, want Ready", st.Phase)
	}

	// Conditions must be structured and machine-readable (iron rule 2).
	cond, ok := st.ConditionByType("Provisioned")
	if !ok {
		t.Fatal("Provisioned condition missing")
	}
	if cond.Status != "True" || cond.Reason == "" {
		t.Fatalf("condition not machine-readable: %+v", cond)
	}
}

func TestQueryUnknownResource(t *testing.T) {
	d := newDriver()
	if _, err := d.Query("euecs-cn-north-1-01-ffffffff"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// --- Suspend semantics ---

// TestSuspendFreezesWithoutDeleting is the arrears lock (01§5.4 D8): a
// suspended resource must come back intact when the customer pays.
func TestSuspendFreezesWithoutDeleting(t *testing.T) {
	d := newDriver()
	spec := validSpec()
	if _, err := d.Apply(spec); err != nil {
		t.Fatal(err)
	}

	spec.Suspend = true
	st, err := d.Apply(spec)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != PhaseSuspended {
		t.Fatalf("phase = %s, want Suspended", st.Phase)
	}
	// The data is still there.
	if !d.Exists(spec.ResourceID) {
		t.Fatal("suspending must NOT delete the resource")
	}

	// Billing stops while suspended.
	usage, err := d.CollectUsage(spec.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 0 {
		t.Fatalf("a suspended resource must not accrue usage, got %+v", usage)
	}

	// Paying restores service.
	spec.Suspend = false
	st, _ = d.Apply(spec)
	if st.Phase != PhaseReady {
		t.Fatalf("phase after resume = %s, want Ready", st.Phase)
	}
	usage, _ = d.CollectUsage(spec.ResourceID)
	if len(usage) == 0 {
		t.Fatal("a resumed resource should accrue usage again")
	}
}

// --- Metering is first-class ---

// TestMeteringIsFirstClass covers iron rule 4: 不可计量的产品不允许上架.
func TestMeteringIsFirstClass(t *testing.T) {
	d := newDriver()
	d.ProvisioningDelay = 0
	spec := validSpec()
	if _, err := d.Apply(spec); err != nil {
		t.Fatal(err)
	}

	usage, err := d.CollectUsage(spec.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) == 0 {
		t.Fatal("a serving resource must report usage")
	}
	u := usage[0]
	if u.MeteringItem == "" || u.Quantity == "" || u.WindowSeconds == 0 {
		t.Fatalf("usage point incomplete: %+v", u)
	}
	// Quantity is a decimal string, never a float.
	if u.Quantity != "0.033333" {
		t.Fatalf("quantity = %s, want a decimal string", u.Quantity)
	}
}

func TestDeletedResourceReportsNoUsage(t *testing.T) {
	d := newDriver()
	d.ProvisioningDelay = 0
	spec := validSpec()
	_, _ = d.Apply(spec)
	_ = d.Delete(spec.ResourceID)

	usage, err := d.CollectUsage(spec.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 0 {
		t.Fatal("a deleted resource must not keep accruing charges")
	}
}

// --- Phase mapping ---

// TestPhaseMappingIsObservationNotAuthority covers adjudication S21: a
// controller reporting Ready proposes a state; the platform decides.
func TestPhaseMappingIsObservationNotAuthority(t *testing.T) {
	cases := []struct {
		phase   Phase
		current string
		want    string
	}{
		{PhaseReady, "CREATING", "RUNNING"},
		{PhaseFailed, "CREATING", "CREATE_FAILED"},
		{PhaseDeleting, "RUNNING", "RELEASING"},
		{PhaseModifying, "RUNNING", "UPGRADING"},
		// Ambiguous phases resolve using the platform's current state: only
		// the platform knows WHY it asked for a suspension.
		{PhaseSuspended, "LOCKED", "LOCKED"},
		{PhaseSuspended, "EXPIRED", "EXPIRED"},
		{PhaseSuspended, "RUNNING", "LOCKED"}, // first candidate as default
		{PhasePending, "INIT", "INIT"},
		{PhasePending, "RUNNING", "INIT"},
	}
	for _, c := range cases {
		got, ok := ProposedState(c.phase, c.current)
		if !ok {
			t.Errorf("phase %s has no mapping", c.phase)
			continue
		}
		if got != c.want {
			t.Errorf("phase %s from %s → %s, want %s", c.phase, c.current, got, c.want)
		}
	}
}

func TestUnknownPhaseHasNoMapping(t *testing.T) {
	if _, ok := ProposedState(Phase("Bogus"), "RUNNING"); ok {
		t.Fatal("an unknown phase must not propose a state")
	}
}

// --- Driver registry ---

func TestRegistryResolvesPerProduct(t *testing.T) {
	// Switching a product's backend is catalogue configuration, not a control
	// plane rewrite (06§6.2).
	r := NewRegistry()
	mock := newDriver()
	r.Register(mock)
	r.Register(VMDriver{})

	r.BindProduct("euecs", DriverMock)
	r.BindProduct("eurds", DriverVM)

	d, err := r.DriverFor("euecs")
	if err != nil || d.Type() != DriverMock {
		t.Fatalf("euecs → %v, %v", d, err)
	}
	d, err = r.DriverFor("eurds")
	if err != nil || d.Type() != DriverVM {
		t.Fatalf("eurds → %v, %v", d, err)
	}
	if _, err := r.DriverFor("euoss"); !errors.Is(err, ErrUnknownDriver) {
		t.Fatalf("unbound product should error, got %v", err)
	}
}

// TestVMDriverFailsLoudly guards against a misconfigured catalogue leaving an
// order stuck in CREATING until it times out fifteen minutes later.
func TestVMDriverFailsLoudly(t *testing.T) {
	var d Driver = VMDriver{}
	if _, err := d.Apply(validSpec()); !errors.Is(err, ErrDriverNotReady) {
		t.Fatalf("expected ErrDriverNotReady, got %v", err)
	}
	if err := d.Delete("x"); !errors.Is(err, ErrDriverNotReady) {
		t.Fatalf("expected ErrDriverNotReady, got %v", err)
	}
}

// --- Chaos hooks ---

func TestChaosHooksInjectFailures(t *testing.T) {
	// MockDriver is a first-class implementation precisely so failure
	// injection works without a cluster (06§6.2).
	d := newDriver()
	d.FailApply = func(id string) error { return errors.New("scheduler unavailable") }

	if _, err := d.Apply(validSpec()); err == nil {
		t.Fatal("chaos hook should have made Apply fail")
	}

	d.FailApply = nil
	if _, err := d.Apply(validSpec()); err != nil {
		t.Fatalf("apply should succeed once the hook is cleared: %v", err)
	}

	d.FailDelete = func(id string) error { return errors.New("controller unreachable") }
	if err := d.Delete(validSpec().ResourceID); err == nil {
		t.Fatal("chaos hook should have made Delete fail — this exercises the compensation-failure path")
	}
}

// --- CR naming conventions ---

func TestCRNamingConventions(t *testing.T) {
	if got := CRKind("euecs"); got != "EuecsInstance" {
		t.Errorf("CRKind = %s, want EuecsInstance", got)
	}
	if got := CRNamespace("euecs"); got != "plat-euecs" {
		t.Errorf("CRNamespace = %s, want plat-euecs", got)
	}
	if got := TenantNamespace(100123, 1); got != "t-100123-p-1" {
		t.Errorf("TenantNamespace = %s, want t-100123-p-1", got)
	}
	// The CR name is the platform resource id verbatim, so a cluster operator
	// and a support engineer are talking about the same string.
	rid := "euecs-cn-north-1-01-a1b2c3d4"
	if got := CRName(rid); got != rid {
		t.Errorf("CRName = %s, want the resource id unchanged", got)
	}
	if CRKind("") != "" {
		t.Error("empty product code should yield an empty kind")
	}
}

func TestFinalizerConstant(t *testing.T) {
	// "CR truly gone" is the release-complete signal; without the finalizer,
	// deletion races the final usage report.
	if Finalizer != "products.cloud.platform/cleanup" {
		t.Fatalf("finalizer = %s, does not match 06§4.2", Finalizer)
	}
}

// --- Preemptor (phase 2, M-4.2): spot reclaim two-step handshake ---

func TestReclaimNoticeWindowIsFiveMinutes(t *testing.T) {
	if ReclaimNoticeWindow != 5*time.Minute {
		t.Fatalf("reclaim notice window = %s, want 5m (09 §4.2)", ReclaimNoticeWindow)
	}
}

func TestMockDriverImplementsPreemptor(t *testing.T) {
	// The MockDriver must satisfy Preemptor so spot reclaim testing works
	// without a cluster (06§6.2 MockDriver验收门槛).
	var d Driver = newDriver()
	if _, ok := d.(Preemptor); !ok {
		t.Fatal("MockDriver must implement Preemptor for spot reclaim testing")
	}
}

func TestNotifyReclaimIsIdempotent(t *testing.T) {
	d := newDriver()
	if _, err := d.Apply(validSpec()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	first, err := d.NotifyReclaim(validSpec().ResourceID, testNow)
	if err != nil {
		t.Fatalf("notify 1: %v", err)
	}
	// A replay returns the original delivered instant, not a reset — the
	// customer keeps their notice time.
	second, err := d.NotifyReclaim(validSpec().ResourceID, testNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("notify 2: %v", err)
	}
	if !second.Equal(first) {
		t.Fatalf("idempotent replay changed delivered instant: %s → %s", first, second)
	}
}

func TestReclaimRefusedBeforeNoticeWindowElapses(t *testing.T) {
	d := newDriver()
	if _, err := d.Apply(validSpec()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	p := d
	if _, err := p.NotifyReclaim(validSpec().ResourceID, testNow); err != nil {
		t.Fatalf("notify: %v", err)
	}
	// Reclaiming 4 minutes later — before the 5m window — must be refused.
	if err := p.Reclaim(validSpec().ResourceID, testNow.Add(4*time.Minute)); !errors.Is(err, ErrNoticeNotDelivered) {
		t.Fatalf("reclaim before window: err = %v, want ErrNoticeNotDelivered", err)
	}
}

func TestReclaimRefusedWithoutPriorNotice(t *testing.T) {
	d := newDriver()
	if _, err := d.Apply(validSpec()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Reclaiming without ever delivering a notice is forbidden — the platform
	// cannot reclaim what it has not warned about.
	if err := d.Reclaim(validSpec().ResourceID, testNow.Add(time.Hour)); !errors.Is(err, ErrNoticeNotDelivered) {
		t.Fatalf("reclaim without notice: err = %v, want ErrNoticeNotDelivered", err)
	}
}

func TestReclaimSucceedsAfterNoticeWindow(t *testing.T) {
	d := newDriver()
	if _, err := d.Apply(validSpec()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	p := d
	if _, err := p.NotifyReclaim(validSpec().ResourceID, testNow); err != nil {
		t.Fatalf("notify: %v", err)
	}
	// Reclaimable at = notice + 5m.
	earliest, ok := p.ReclaimableAt(validSpec().ResourceID)
	if !ok {
		t.Fatal("ReclaimableAt should report a time after notice")
	}
	if !earliest.Equal(testNow.Add(5 * time.Minute)) {
		t.Fatalf("earliest reclaim = %s, want %s", earliest, testNow.Add(5*time.Minute))
	}
	// Reclaiming exactly at the window boundary succeeds.
	if err := p.Reclaim(validSpec().ResourceID, earliest); err != nil {
		t.Fatalf("reclaim at window: err = %v", err)
	}
}

func TestReclaimOnUnknownResourceErrors(t *testing.T) {
	d := newDriver()
	p := d
	if _, err := p.NotifyReclaim("does-not-exist", testNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("notify unknown: err = %v, want ErrNotFound", err)
	}
	if err := p.Reclaim("does-not-exist", testNow.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reclaim unknown: err = %v, want ErrNotFound", err)
	}
}

func TestReclaimableAtFalseBeforeNotice(t *testing.T) {
	d := newDriver()
	if _, err := d.Apply(validSpec()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, ok := d.ReclaimableAt(validSpec().ResourceID); ok {
		t.Fatal("ReclaimableAt should be false before any notice is delivered")
	}
}

// --- GLOBAL region scope (phase 4, M-12.1) ---

// globalSpec is a valid spec for a GLOBAL-scoped product (DNS zone class):
// sentinel region "global", no Zone.
func globalSpec() Spec {
	return Spec{
		ResourceID:     "eudns-global-01-a1b2c3d4",
		AccountID:      100123,
		ProjectID:      1,
		ProductCode:    "eudns",
		ResourceType:   "zone",
		Region:         GlobalRegion,
		RegionScope:    ScopeGlobal,
		Edition:        "standard",
		Params:         map[string]string{"zoneName": "example.com"},
		IdempotencyKey: "order-9101",
	}
}

// TestGlobalScopeValidate covers the GLOBAL placement contract: the sentinel
// region is mandatory and a Zone is a category error. Domain/DNS/CDN/WAF
// resources ride a single worldwide fleet (01§2.3/§2.7/§2.9) — a Zone on such
// a spec would silently split the fleet per-AZ.
func TestGlobalScopeValidate(t *testing.T) {
	if err := globalSpec().Validate(); err != nil {
		t.Fatalf("valid GLOBAL spec rejected: %v", err)
	}

	for name, mutate := range map[string]func(*Spec){
		"regional region on a GLOBAL product": func(s *Spec) { s.Region = "cn-north-1" },
		"Zone pinned onto a GLOBAL product":   func(s *Spec) { s.Zone = "cn-north-1-a" },
	} {
		s := globalSpec()
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: spec accepted, want rejection", name)
		}
	}
}

// TestGlobalSpecFlowsThroughMockDriver proves the GLOBAL contract is not
// decorative: a GLOBAL spec passes the same Apply/Query/CollectUsage evidence
// loop as a ZONAL one, so the MockDriver gate (06§6.3) can certify phase-4
// GLOBAL products unchanged.
func TestGlobalSpecFlowsThroughMockDriver(t *testing.T) {
	d := newDriver()
	spec := globalSpec()
	if _, err := d.Apply(spec); err != nil {
		t.Fatalf("apply GLOBAL spec: %v", err)
	}
	st, err := d.Query(spec.ResourceID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !st.Ready() {
		t.Fatalf("phase = %s, want Ready (ProvisioningDelay=0)", st.Phase)
	}
	if _, err := d.CollectUsage(spec.ResourceID); err != nil {
		t.Fatalf("collect usage: %v", err)
	}
}

// TestZonalScopeStillEnforced pins the existing ZONAL contract next to the new
// GLOBAL one, so a future refactor cannot loosen one while tightening the
// other.
func TestZonalScopeStillEnforced(t *testing.T) {
	s := validSpec()
	s.RegionScope = ScopeZonal
	s.Zone = ""
	if err := s.Validate(); err == nil {
		t.Fatal("ZONAL spec without Zone accepted")
	}
	s.Zone = "cn-north-1-a"
	if err := s.Validate(); err != nil {
		t.Fatalf("ZONAL spec with Zone rejected: %v", err)
	}
}
