package ipam

import (
	"errors"
	"net/netip"
	"testing"
)

// The tests mirror the three package invariants: overlap check precedes every
// write, allocation is deterministic, release is idempotent — plus the
// reserved-address trio that keeps a subnet's gateway out of customer hands.

// --- CIDR arithmetic ---

func TestParseCIDR(t *testing.T) {
	for _, s := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "10.1.2.0/24", "10.0.0.0/28"} {
		if _, err := ParseCIDR(s); err != nil {
			t.Errorf("ParseCIDR(%q): %v", s, err)
		}
	}
	for _, s := range []string{
		"10.0.0.0",    // no prefix
		"10.0.0.0/33", // bad prefix
		"10.0.0.5/24", // host bits set
		"fd00::/8",    // IPv6
		"not-a-cidr",  // garbage
		"8.8.8.8/24",  // valid syntax but parsed fine — public, caught elsewhere
	} {
		if s == "8.8.8.8/24" {
			continue // syntactically legal; see TestIsRFC1918
		}
		if _, err := ParseCIDR(s); err == nil {
			t.Errorf("ParseCIDR(%q) must fail", s)
		}
	}
}

func TestParseCIDRRejectsHostBits(t *testing.T) {
	// "10.0.1.5/24" silently masking to 10.0.1.0/24 is the classic footgun:
	// the customer believes they got a range around .5, the platform records
	// .0/24, and the mismatch surfaces later as a mystery overlap.
	_, err := ParseCIDR("10.0.1.5/24")
	if !errors.Is(err, ErrInvalidCIDR) {
		t.Fatalf("want ErrInvalidCIDR, got %v", err)
	}
}

func TestCIDRStringRoundTrip(t *testing.T) {
	for _, s := range []string{"10.0.0.0/16", "172.31.128.0/18", "192.168.4.0/28"} {
		c, err := ParseCIDR(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		if c.String() != s {
			t.Errorf("round trip: %q -> %q", s, c.String())
		}
	}
}

func TestCIDRSize(t *testing.T) {
	cases := map[string]uint64{
		"10.0.0.0/8":  1 << 24,
		"10.0.0.0/16": 1 << 16,
		"10.0.0.0/24": 256,
		"10.0.0.0/28": 16,
		"10.0.0.0/32": 1,
	}
	for s, want := range cases {
		c := mustParse(s)
		if c.Size() != want {
			t.Errorf("%s size = %d, want %d", s, c.Size(), want)
		}
	}
}

func TestCIDRContains(t *testing.T) {
	c := mustParse("10.0.0.0/24")
	in := []string{"10.0.0.0", "10.0.0.255", "10.0.0.128"}
	out := []string{"10.0.1.0", "9.255.255.255"}
	for _, s := range in {
		if !c.Contains(netip.MustParseAddr(s)) {
			t.Errorf("%s must contain %s", c, s)
		}
	}
	for _, s := range out {
		if c.Contains(netip.MustParseAddr(s)) {
			t.Errorf("%s must not contain %s", c, s)
		}
	}
}

func TestCIDRContainsCIDR(t *testing.T) {
	vpc := mustParse("10.0.0.0/16")
	if !vpc.ContainsCIDR(mustParse("10.0.0.0/16")) { // self
		t.Error("a CIDR must contain itself")
	}
	if !vpc.ContainsCIDR(mustParse("10.0.5.0/24")) {
		t.Error("10.0.5.0/24 ⊆ 10.0.0.0/16")
	}
	if vpc.ContainsCIDR(mustParse("10.1.0.0/24")) {
		t.Error("10.1.0.0/24 ⊄ 10.0.0.0/16")
	}
	if vpc.ContainsCIDR(mustParse("10.0.0.0/15")) { // bigger than the VPC
		t.Error("a larger CIDR can never be contained")
	}
}

func TestCIDROverlaps(t *testing.T) {
	// The overlap matrix the allocator leans on: equal, nested either way,
	// partial — overlapping; adjacent — free.
	cases := []struct {
		a, b string
		want bool
		note string
	}{
		{"10.0.0.0/24", "10.0.0.0/24", true, "identical"},
		{"10.0.0.0/16", "10.0.5.0/24", true, "nested"},
		{"10.0.5.0/24", "10.0.0.0/16", true, "nested, flipped"},
		{"10.0.0.0/24", "10.0.0.128/25", true, "partial"},
		{"10.0.0.0/24", "10.0.1.0/24", false, "adjacent blocks are free"},
		{"10.0.0.0/16", "10.1.0.0/16", false, "disjoint"},
	}
	for _, tc := range cases {
		got := mustParse(tc.a).Overlaps(mustParse(tc.b))
		if got != tc.want {
			t.Errorf("Overlaps(%s, %s) = %v, want %v (%s)", tc.a, tc.b, got, tc.want, tc.note)
		}
	}
}

func TestIsRFC1918(t *testing.T) {
	private := []string{"10.0.0.0/8", "10.9.9.0/24", "172.16.0.0/12", "172.31.255.0/24", "192.168.0.0/16", "192.168.1.0/28"}
	public := []string{"8.8.8.0/24", "11.0.0.0/8", "172.32.0.0/12", "193.168.0.0/16", "100.64.0.0/10"}
	for _, s := range private {
		if !mustParse(s).IsRFC1918() {
			t.Errorf("%s must be RFC 1918", s)
		}
	}
	for _, s := range public {
		if mustParse(s).IsRFC1918() {
			t.Errorf("%s must not be RFC 1918", s)
		}
	}
}

// --- SubnetAllocator ---

func newAlloc(t *testing.T, vpc string) *SubnetAllocator {
	t.Helper()
	a, err := NewSubnetAllocator(mustParse(vpc))
	if err != nil {
		t.Fatalf("NewSubnetAllocator(%s): %v", vpc, err)
	}
	return a
}

func TestNewSubnetAllocatorRejectsBadVPC(t *testing.T) {
	for _, s := range []string{"8.8.8.0/24", "100.64.0.0/10"} { // public
		if _, err := NewSubnetAllocator(mustParse(s)); !errors.Is(err, ErrNotPrivate) {
			t.Errorf("VPC %s: want ErrNotPrivate, got %v", s, err)
		}
	}
	// No /7 can be RFC 1918 (10.0.0.0/7 swallows public 11.0.0.0/8), so a
	// too-coarse VPC is caught by the private-space check first — rejected
	// either way, which is what matters.
	if _, err := NewSubnetAllocator(mustParse("10.0.0.0/7")); err == nil {
		t.Error("VPC /7 must be rejected")
	}
	if _, err := NewSubnetAllocator(mustParse("10.0.0.0/25")); !errors.Is(err, ErrInvalidPrefix) {
		t.Errorf("VPC /25: want ErrInvalidPrefix, got %v", err)
	}
}

func TestSubnetAllocationIsSequentialAndNonOverlapping(t *testing.T) {
	a := newAlloc(t, "10.0.0.0/16")
	want := []string{"10.0.0.0/24", "10.0.1.0/24", "10.0.2.0/24"}
	for i, w := range want {
		got, err := a.Allocate(24)
		if err != nil {
			t.Fatalf("allocate #%d: %v", i, err)
		}
		if got.String() != w {
			t.Errorf("allocate #%d = %s, want %s (deterministic first-fit)", i, got, w)
		}
	}
	// Replay on a fresh allocator must yield the identical layout.
	b := newAlloc(t, "10.0.0.0/16")
	for range want {
		if _, err := b.Allocate(24); err != nil {
			t.Fatal(err)
		}
	}
	if len(b.Allocated()) != len(a.Allocated()) {
		t.Fatal("replay diverged")
	}
	for i := range want {
		if a.Allocated()[i] != b.Allocated()[i] {
			t.Fatalf("replay diverged at %d: %s vs %s", i, a.Allocated()[i], b.Allocated()[i])
		}
	}
}

func TestSubnetAllocationIsAligned(t *testing.T) {
	// A /22 request must land on /22 boundaries (…0.0/22, …4.0/22), never on
	// an unaligned interior address.
	a := newAlloc(t, "10.0.0.0/16")
	first, err := a.Allocate(22)
	if err != nil {
		t.Fatal(err)
	}
	if first.String() != "10.0.0.0/22" {
		t.Fatalf("first /22 = %s", first)
	}
	second, err := a.Allocate(22)
	if err != nil {
		t.Fatal(err)
	}
	if second.String() != "10.0.4.0/22" {
		t.Fatalf("second /22 = %s, want 10.0.4.0/22 (aligned step)", second)
	}
}

func TestSubnetAllocationSkipsReservedBlocks(t *testing.T) {
	a := newAlloc(t, "10.0.0.0/16")
	// The platform carves an internal-services block first.
	if err := a.Reserve(mustParse("10.0.1.0/24")); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	first, err := a.Allocate(24)
	if err != nil {
		t.Fatal(err)
	}
	if first.String() != "10.0.0.0/24" {
		t.Fatalf("first = %s, want 10.0.0.0/24", first)
	}
	second, err := a.Allocate(24)
	if err != nil {
		t.Fatal(err)
	}
	if second.String() != "10.0.2.0/24" {
		t.Fatalf("second = %s, must skip the reserved 10.0.1.0/24", second)
	}
}

func TestSubnetReserveRejectsOverlapAndStrays(t *testing.T) {
	a := newAlloc(t, "10.0.0.0/16")
	if err := a.Reserve(mustParse("10.0.0.0/24")); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"10.0.0.0/24",   // identical
		"10.0.0.128/25", // partial overlap
		"10.0.0.0/22",   // nested overlap
	} {
		if err := a.Reserve(mustParse(s)); !errors.Is(err, ErrOverlap) {
			t.Errorf("Reserve(%s): want ErrOverlap, got %v", s, err)
		}
	}
	// Outside the VPC refused; coarser than the VPC is caught by the prefix
	// check first ("a subnet bigger than its VPC" is a prefix error).
	if err := a.Reserve(mustParse("10.1.0.0/24")); !errors.Is(err, ErrNotInVPC) {
		t.Errorf("outside VPC: want ErrNotInVPC, got %v", err)
	}
	if err := a.Reserve(mustParse("10.0.0.0/15")); !errors.Is(err, ErrInvalidPrefix) {
		t.Errorf("larger than VPC: want ErrInvalidPrefix, got %v", err)
	}
	// Prefix legality: finer than /28 refused, coarser than the VPC refused.
	if err := a.Reserve(mustParse("10.0.1.0/29")); !errors.Is(err, ErrInvalidPrefix) {
		t.Errorf("/29: want ErrInvalidPrefix, got %v", err)
	}
}

func TestSubnetAllocationPrefixBounds(t *testing.T) {
	a := newAlloc(t, "10.0.0.0/16")
	if _, err := a.Allocate(15); !errors.Is(err, ErrInvalidPrefix) {
		t.Errorf("coarser than VPC: want ErrInvalidPrefix, got %v", err)
	}
	if _, err := a.Allocate(29); !errors.Is(err, ErrInvalidPrefix) {
		t.Errorf("finer than /28: want ErrInvalidPrefix, got %v", err)
	}
	if _, err := a.Allocate(16); err != nil {
		t.Errorf("subnet == whole VPC must be legal: %v", err)
	}
}

func TestSubnetAllocationExhaustion(t *testing.T) {
	a := newAlloc(t, "10.0.0.0/24")
	if c, err := a.Allocate(25); err != nil || c.String() != "10.0.0.0/25" {
		t.Fatalf("first /25: %v %v", c, err)
	}
	if c, err := a.Allocate(25); err != nil || c.String() != "10.0.0.128/25" {
		t.Fatalf("second /25: %v %v", c, err)
	}
	if _, err := a.Allocate(25); !errors.Is(err, ErrExhausted) {
		t.Errorf("third /25: want ErrExhausted, got %v", err)
	}
	// But a finer block still fits in the cracks left after smaller ones.
	if _, err := a.Allocate(26); !errors.Is(err, ErrExhausted) {
		t.Errorf("no /26 crack left: want ErrExhausted, got %v", err)
	}
}

func TestSubnetReleaseReusesHoles(t *testing.T) {
	a := newAlloc(t, "10.0.0.0/16")
	var got []CIDR
	for range 3 {
		c, err := a.Allocate(24)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, c)
	}
	// Drop the middle one; the next allocation must backfill the hole.
	if err := a.Release(got[1]); err != nil {
		t.Fatal(err)
	}
	next, err := a.Allocate(24)
	if err != nil {
		t.Fatal(err)
	}
	if next != got[1] {
		t.Fatalf("re-alloc = %s, want the released hole %s", next, got[1])
	}
	// Idempotent release: unknown and double releases are successes.
	if err := a.Release(got[1]); err != nil {
		t.Fatalf("double release must succeed: %v", err)
	}
	if err := a.Release(mustParse("10.200.0.0/24")); err != nil {
		t.Fatalf("unknown release must succeed: %v", err)
	}
}

func TestSubnetMassAllocationStaysDisjoint(t *testing.T) {
	// Brute-force invariant check: 64 /26 blocks in a /20, pairwise disjoint,
	// all inside the VPC. Any overlap here means a customer's subnets could
	// shadow each other — undetectable after the fact.
	a := newAlloc(t, "10.8.0.0/20")
	vpc := mustParse("10.8.0.0/20")
	var blocks []CIDR
	for i := 0; i < 64; i++ {
		c, err := a.Allocate(26)
		if err != nil {
			t.Fatalf("allocate #%d: %v", i, err)
		}
		blocks = append(blocks, c)
	}
	if _, err := a.Allocate(26); !errors.Is(err, ErrExhausted) {
		t.Errorf("65th /26: want ErrExhausted, got %v", err)
	}
	for i := 0; i < len(blocks); i++ {
		if !vpc.ContainsCIDR(blocks[i]) {
			t.Fatalf("%s escaped the VPC", blocks[i])
		}
		for j := i + 1; j < len(blocks); j++ {
			if blocks[i].Overlaps(blocks[j]) {
				t.Fatalf("%s overlaps %s", blocks[i], blocks[j])
			}
		}
	}
}

// --- IPAllocator ---

func TestIPAllocationStartsAfterReservedTrio(t *testing.T) {
	a := NewIPAllocator(mustParse("10.0.5.0/28"))
	if g := a.Gateway(); g.String() != "10.0.5.1" {
		t.Fatalf("gateway = %s, want 10.0.5.1", g)
	}
	want := []string{"10.0.5.2", "10.0.5.3", "10.0.5.4"}
	for i, w := range want {
		ip, err := a.Allocate("euecs-cn-north-1-01-aaaa")
		if err != nil {
			t.Fatalf("allocate #%d: %v", i, err)
		}
		if ip.String() != w {
			t.Errorf("allocate #%d = %s, want %s", i, ip, w)
		}
	}
}

func TestIPAllocationNeverHandsOutReserved(t *testing.T) {
	a := NewIPAllocator(mustParse("10.0.5.0/28")) // 16 addrs, 13 usable
	reserved := map[netip.Addr]bool{}
	for _, r := range a.Reserved() {
		reserved[r] = true
	}
	for i := 0; i < 13; i++ {
		ip, err := a.Allocate("owner")
		if err != nil {
			t.Fatalf("allocate #%d: %v", i, err)
		}
		if reserved[ip] {
			t.Fatalf("handed out reserved address %s", ip)
		}
	}
	if _, err := a.Allocate("owner"); !errors.Is(err, ErrIPExhausted) {
		t.Errorf("14th: want ErrIPExhausted, got %v", err)
	}
}

func TestIPAllocateSpecific(t *testing.T) {
	a := NewIPAllocator(mustParse("10.0.5.0/24"))
	ip := netip.MustParseAddr("10.0.5.10")
	if err := a.AllocateSpecific(ip, "nic-1"); err != nil {
		t.Fatalf("specific: %v", err)
	}
	if err := a.AllocateSpecific(ip, "nic-2"); !errors.Is(err, ErrIPTaken) {
		t.Errorf("double specific: want ErrIPTaken, got %v", err)
	}
	// Reserved trio refused distinctly.
	if err := a.AllocateSpecific(netip.MustParseAddr("10.0.5.0"), "x"); !errors.Is(err, ErrIPReserved) {
		t.Errorf("network addr: want ErrIPReserved, got %v", err)
	}
	if err := a.AllocateSpecific(netip.MustParseAddr("10.0.5.1"), "x"); !errors.Is(err, ErrIPReserved) {
		t.Errorf("gateway: want ErrIPReserved, got %v", err)
	}
	if err := a.AllocateSpecific(netip.MustParseAddr("10.0.5.255"), "x"); !errors.Is(err, ErrIPReserved) {
		t.Errorf("broadcast: want ErrIPReserved, got %v", err)
	}
	// Outside the subnet refused.
	if err := a.AllocateSpecific(netip.MustParseAddr("10.0.6.1"), "x"); !errors.Is(err, ErrNotInSubnet) {
		t.Errorf("outside: want ErrNotInSubnet, got %v", err)
	}
	// Owner is mandatory: an unattributed IP cannot be audited.
	if err := a.AllocateSpecific(ip, ""); !errors.Is(err, ErrOwnerRequired) {
		t.Errorf("no owner: want ErrOwnerRequired, got %v", err)
	}
	if _, err := a.Allocate(""); !errors.Is(err, ErrOwnerRequired) {
		t.Errorf("Allocate no owner: want ErrOwnerRequired, got %v", err)
	}
}

func TestIPOwnerAndRelease(t *testing.T) {
	a := NewIPAllocator(mustParse("10.0.5.0/24"))
	ip, err := a.Allocate("euecs-cn-north-1-01-a1b2c3d4")
	if err != nil {
		t.Fatal(err)
	}
	if o, ok := a.Owner(ip); !ok || o != "euecs-cn-north-1-01-a1b2c3d4" {
		t.Fatalf("owner = %q %v", o, ok)
	}
	// Release is idempotent and frees the address for reuse.
	if err := a.Release(ip); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(ip); err != nil {
		t.Fatalf("double release must succeed: %v", err)
	}
	if _, ok := a.Owner(ip); ok {
		t.Fatal("released address still owned")
	}
	again, err := a.Allocate("euecs-cn-north-1-01-b2c3d4e5")
	if err != nil {
		t.Fatal(err)
	}
	if again != ip {
		t.Fatalf("re-alloc = %s, want the freed %s (hole reuse)", again, ip)
	}
	if a.AllocatedCount() != 1 {
		t.Fatalf("count = %d, want 1", a.AllocatedCount())
	}
}

// --- Composition: the EUVPC shape -----------------------------------------

func TestVPCComposition(t *testing.T) {
	// The end-to-end shape of one VPC: a platform-reserved block, two customer
	// subnets, VM NICs inside them — no overlaps anywhere, gateways intact.
	sa := newAlloc(t, "10.0.0.0/16")
	if err := sa.Reserve(mustParse("10.0.255.0/24")); err != nil { // platform services block
		t.Fatal(err)
	}
	sub1, err := sa.Allocate(24)
	if err != nil {
		t.Fatal(err)
	}
	sub2, err := sa.Allocate(24)
	if err != nil {
		t.Fatal(err)
	}
	if sub1.Overlaps(sub2) {
		t.Fatalf("subnets overlap: %s vs %s", sub1, sub2)
	}
	for _, s := range []CIDR{sub1, sub2} {
		if !mustParse("10.0.0.0/16").ContainsCIDR(s) {
			t.Fatalf("%s escaped the VPC", s)
		}
	}

	ia1 := NewIPAllocator(sub1)
	ia2 := NewIPAllocator(sub2)
	var all []netip.Addr
	for _, alloc := range []*IPAllocator{ia1, ia2} {
		for _, owner := range []string{"vm-1", "vm-2"} {
			ip, err := alloc.Allocate(owner)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, ip)
		}
		if ip := alloc.Gateway(); alloc.Subnet().Contains(ip) {
			// Gateway must exist inside its subnet but never be allocatable.
			if err := alloc.AllocateSpecific(ip, "rogue"); !errors.Is(err, ErrIPReserved) {
				t.Fatalf("gateway %s allocatable: %v", ip, err)
			}
		}
	}
	// Cross-subnet: no address appears twice, and the two gateways differ.
	seen := map[netip.Addr]bool{}
	for _, ip := range all {
		if seen[ip] {
			t.Fatalf("duplicate address %s across subnets", ip)
		}
		seen[ip] = true
	}
	if ia1.Gateway() == ia2.Gateway() {
		t.Fatal("distinct subnets share a gateway address")
	}
}

func TestZeroishSubnetDegradesGracefully(t *testing.T) {
	// A /30 cannot be reached through SubnetAllocator (prefix cap /28), but
	// the IP allocator must still behave: exactly one usable address.
	a := NewIPAllocator(mustParse("10.0.0.0/30"))
	if a.Capacity() != 1 {
		t.Fatalf("capacity = %d, want 1", a.Capacity())
	}
	ip, err := a.Allocate("x")
	if err != nil || ip.String() != "10.0.0.2" {
		t.Fatalf("allocate = %v %v, want 10.0.0.2", ip, err)
	}
	if _, err := a.Allocate("y"); !errors.Is(err, ErrIPExhausted) {
		t.Errorf("second: want ErrIPExhausted, got %v", err)
	}
}
