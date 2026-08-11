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
		ResourceID:     "scecs-cn-north-1-01-a1b2c3d4",
		AccountID:      100123,
		ProjectID:      1,
		ProductCode:    "scecs",
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
	if labels[LabelInstance] != "scecs-cn-north-1-01-a1b2c3d4" {
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
	if err := d.Delete("scecs-cn-north-1-01-ffffffff"); err != nil {
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
	if _, err := d.Query("scecs-cn-north-1-01-ffffffff"); !errors.Is(err, ErrNotFound) {
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

	r.BindProduct("scecs", DriverMock)
	r.BindProduct("scrds", DriverVM)

	d, err := r.DriverFor("scecs")
	if err != nil || d.Type() != DriverMock {
		t.Fatalf("scecs → %v, %v", d, err)
	}
	d, err = r.DriverFor("scrds")
	if err != nil || d.Type() != DriverVM {
		t.Fatalf("scrds → %v, %v", d, err)
	}
	if _, err := r.DriverFor("scoss"); !errors.Is(err, ErrUnknownDriver) {
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
	if got := CRKind("scecs"); got != "ScecsInstance" {
		t.Errorf("CRKind = %s, want ScecsInstance", got)
	}
	if got := CRNamespace("scecs"); got != "plat-scecs" {
		t.Errorf("CRNamespace = %s, want plat-scecs", got)
	}
	if got := TenantNamespace(100123, 1); got != "t-100123-p-1" {
		t.Errorf("TenantNamespace = %s, want t-100123-p-1", got)
	}
	// The CR name is the platform resource id verbatim, so a cluster operator
	// and a support engineer are talking about the same string.
	rid := "scecs-cn-north-1-01-a1b2c3d4"
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
