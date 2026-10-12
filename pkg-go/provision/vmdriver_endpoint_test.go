package provision

import "testing"

// The endpoint of a running guest is stable: every Query used to mint a new
// GuestIP, so the console's access address changed on each poll. A new guest
// (after a halt/resume cycle) may get a new address.
func TestMemoryVMClientGuestIPStablePerGuest(t *testing.T) {
	d, _ := newVMTestDriver(0)
	spec := validSpec()
	if _, err := d.Apply(spec); err != nil {
		t.Fatal(err)
	}
	first, err := d.Query(spec.ResourceID)
	if err != nil || !first.Ready() || len(first.Endpoints) != 1 {
		t.Fatalf("first query: %+v, %v", first, err)
	}
	for i := 0; i < 5; i++ {
		st, err := d.Query(spec.ResourceID)
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Endpoints) != 1 || st.Endpoints[0] != first.Endpoints[0] {
			t.Fatalf("query %d: endpoint %v, want stable %v", i, st.Endpoints, first.Endpoints)
		}
	}
	// Re-applying the unchanged spec is a no-op and keeps the address too.
	if st, err := d.Apply(spec); err != nil || len(st.Endpoints) != 1 || st.Endpoints[0] != first.Endpoints[0] {
		t.Fatalf("re-apply: %+v, %v", st, err)
	}

	// Halt (arrears freeze) drops the guest; resume boots a new one with an
	// address of its own, which is then stable again.
	halted := spec
	halted.Suspend = true
	if _, err := d.Apply(halted); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.Query(spec.ResourceID); len(st.Endpoints) != 0 {
		t.Fatalf("halted guest still has endpoints %v", st.Endpoints)
	}
	if _, err := d.Apply(spec); err != nil {
		t.Fatal(err)
	}
	resumed, _ := d.Query(spec.ResourceID)
	again, _ := d.Query(spec.ResourceID)
	if len(resumed.Endpoints) != 1 || len(again.Endpoints) != 1 || resumed.Endpoints[0] != again.Endpoints[0] {
		t.Fatalf("resumed guest endpoints unstable: %v then %v", resumed.Endpoints, again.Endpoints)
	}
}
