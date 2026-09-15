// Package ipam implements the address-management engine behind EUVPC's subnet
// CIDR planning and per-subnet IP allocation (01-product-catalog.md §6 EUVPC
// "VPC/子网/路由表 CRUD、安全组、ECS 网卡接入、CIDR 规划"; 09-roadmap §3.2 D-02
// 二期"专有网络 VPC 进阶特性"; 06-kubernetes-productization.md §2.4.1 网络平面).
//
// # What phase 1 shipped vs what this package adds
//
// Phase 1 delivered EUVPC as VPC/subnet CRUD on the K8s network model
// (D-02: Calico + NetworkPolicy 承载隔离), with resource models already
// carrying vpc_id placeholder fields. The phase-2 VPC 进阶特性 turns that
// scaffolding into product semantics, and the first semantic that must be
// RIGHT — before SDN overlay, before advanced routing — is address math:
// two subnets handed the same CIDR cannot be diagnosed after the fact, they
// can only be prevented at allocation time. 重叠一旦发生,排查手段只剩"整段
// 重建",所以分配器是唯一允许写地址的入口.
//
// # Why the platform owns this and not Calico's IPAM
//
// Calico's IPAM manages the cluster's pod/service planes (06§2.4.1). A
// tenant's VPC subnet is a *product* object: its allocation rules are
// commercial semantics (RFC 1918 only, gateway reserved, minimum subnet
// size, release-and-reuse) that no CNI expresses. This package is the
// arithmetic core; the Calico/Cilium wiring translates its output into
// NetworkPolicy and route objects.
//
// # The three invariants (mirroring the iron rules in 06§4.1)
//
//  1. Overlap check precedes every write. Allocate and Reserve both pass
//     through the same Overlaps test — there is no code path that can
//     introduce a second owner of an address range.
//  2. Allocation is deterministic. First-fit from the VPC base means the
//     same allocation history always yields the same layout: replayable,
//     diffable, and testable without mocks.
//  3. Release is idempotent. Releasing an unknown subnet or address
//     succeeds — saga compensation retries releases, and a false error
//     here would escalate a clean cleanup into a ticket (03§8.4).
//
// # Reserved addresses per subnet
//
// Every subnet holds three addresses back from customers:
//
//	network  — the base address itself (x.x.x.0)
//	gateway  — base+1, the platform's virtual gateway for the subnet
//	broadcast — the last address (x.x.x.255)
//
// ECS NICs are allocated from base+2 upward. The gateway address is what a
// subnet's route table points at by default; handing it to a customer VM
// would break every other VM in the subnet.
//
// # Limits
//
// VPC CIDRs must lie inside RFC 1918 private space (10/8, 172.16/12,
// 192.168/16) with a prefix between /8 and /24 — 自建 IDC 内租户网络永远不
// 应宣告公网段. Subnet prefixes run from the VPC's own prefix down to /28
// (MinSubnetPrefix), the smallest sellable subnet: 16 addresses, 13
// usable — below that the reserved trio dominates and the product stops
// making sense.
package ipam

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"sync"
)

// MinSubnetPrefix is the finest (smallest) subnet the platform sells: /28 =
// 16 addresses, 13 allocatable after the network/gateway/broadcast trio.
const MinSubnetPrefix = 28

// Allocation errors. They are distinct so callers can distinguish "customer
// asked for something invalid" (4xx semantics) from "the VPC is full" (an
// operational condition worth an alert).
var (
	ErrInvalidCIDR   = errors.New("ipam: invalid CIDR")
	ErrNotPrivate    = errors.New("ipam: CIDR outside RFC 1918 private space")
	ErrNotInVPC      = errors.New("ipam: subnet not contained in the VPC")
	ErrOverlap       = errors.New("ipam: CIDR overlaps an existing subnet")
	ErrExhausted     = errors.New("ipam: no free CIDR of the requested size")
	ErrInvalidPrefix = errors.New("ipam: invalid subnet prefix length")
	ErrNotInSubnet   = errors.New("ipam: address not in the subnet")
	ErrIPReserved    = errors.New("ipam: address is reserved (network/gateway/broadcast)")
	ErrIPTaken       = errors.New("ipam: address already allocated")
	ErrIPExhausted   = errors.New("ipam: subnet has no free address")
	ErrOwnerRequired = errors.New("ipam: allocation requires an owner resource id")
)

// --- CIDR ----------------------------------------------------------------

// CIDR is an IPv4 network: a base address (always the network address) and a
// prefix length. The zero value is not a valid network; ParseCIDR is the
// constructor.
type CIDR struct {
	base netip.Addr
	bits int
}

// ParseCIDR parses "a.b.c.d/n". Two things netip.ParsePrefix accepts that a
// cloud product must not:
//
//   - IPv6 (the VPC product is IPv4-only in this phase);
//   - host bits set ("10.0.1.5/24"). Accepting it would silently hand the
//     customer 10.0.1.0/24 while they believe they got a range around .5 —
//     the mismatch surfaces exactly when they later reserve the real
//     10.0.1.0/24 and hit a mystery overlap. Reject loudly instead.
func ParseCIDR(s string) (CIDR, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() {
		return CIDR{}, fmt.Errorf("%w: %q", ErrInvalidCIDR, s)
	}
	if p.Addr() != p.Masked().Addr() {
		return CIDR{}, fmt.Errorf("%w: %q has host bits set", ErrInvalidCIDR, s)
	}
	return CIDR{base: p.Addr(), bits: p.Bits()}, nil
}

func newCIDR(base netip.Addr, bits int) CIDR { return CIDR{base: base, bits: bits} }

// String renders the canonical "a.b.c.d/n" form.
func (c CIDR) String() string { return c.base.String() + "/" + itoa(c.bits) }

func itoa(v int) string { return fmt.Sprint(v) }

// Bits returns the prefix length.
func (c CIDR) Bits() int { return c.bits }

// Network returns the base address.
func (c CIDR) Network() netip.Addr { return c.base }

// Broadcast returns the last address of the range.
func (c CIDR) Broadcast() netip.Addr {
	return fromU32(toU32(c.base) + uint32(c.Size()) - 1)
}

// Size returns the number of addresses in the range: 2^(32-bits).
func (c CIDR) Size() uint64 { return uint64(1) << (32 - c.bits) }

// Contains reports whether ip falls inside the range.
func (c CIDR) Contains(ip netip.Addr) bool {
	return ip.Is4() && ip.Compare(c.base) >= 0 && ip.Compare(c.Broadcast()) <= 0
}

// ContainsCIDR reports whether other lies entirely inside c. A CIDR contains
// itself.
func (c CIDR) ContainsCIDR(other CIDR) bool {
	return other.bits >= c.bits && c.Contains(other.base)
}

// Overlaps reports whether the two ranges share at least one address.
// Adjacent ranges (10.0.0.0/24 and 10.0.1.0/24) do NOT overlap — that is the
// property the allocator's first-fit stepper relies on.
func (c CIDR) Overlaps(other CIDR) bool {
	return c.Contains(other.base) || other.Contains(c.base)
}

// IsRFC1918 reports whether the range lies entirely inside private space
// (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16). A VPC must pass this: a
// tenant announcing public space from a self-built IDC is a routing accident
// waiting for the first external packet.
func (c CIDR) IsRFC1918() bool {
	for _, private := range []CIDR{
		mustParse("10.0.0.0/8"),
		mustParse("172.16.0.0/12"),
		mustParse("192.168.0.0/16"),
	} {
		if private.ContainsCIDR(c) {
			return true
		}
	}
	return false
}

func mustParse(s string) CIDR {
	c, err := ParseCIDR(s)
	if err != nil {
		panic("ipam: internal constant " + s + ": " + err.Error())
	}
	return c
}

// --- u32 helpers ----------------------------------------------------------

func toU32(a netip.Addr) uint32 {
	b := a.As4()
	return binary.BigEndian.Uint32(b[:])
}

func fromU32(v uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

// --- Subnet allocation ----------------------------------------------------

// SubnetAllocator hands out non-overlapping subnet CIDRs from one VPC block.
//
// It is the only component allowed to decide subnet addresses: controllers
// and the EUVPC API both ask it (Allocate for platform-chosen blocks,
// Reserve for customer-specified ones) so every write passes the same
// overlap check.
type SubnetAllocator struct {
	mu    sync.Mutex
	vpc   CIDR
	taken []CIDR // allocated + reserved, insertion order
}

// NewSubnetAllocator validates the VPC block and returns an allocator over
// it. The VPC must be RFC 1918 with a prefix between /8 and /24: smaller
// than /24 cannot host a meaningful subnet set, larger than /8 has no
// private space left to be.
func NewSubnetAllocator(vpc CIDR) (*SubnetAllocator, error) {
	if !vpc.IsRFC1918() {
		return nil, fmt.Errorf("%w: %s", ErrNotPrivate, vpc)
	}
	if vpc.bits < 8 || vpc.bits > 24 {
		return nil, fmt.Errorf("%w: VPC prefix must be /8../24, got /%d", ErrInvalidPrefix, vpc.bits)
	}
	return &SubnetAllocator{vpc: vpc}, nil
}

// VPC returns the block the allocator serves.
func (a *SubnetAllocator) VPC() CIDR { return a.vpc }

// checkPrefix validates a subnet prefix for this VPC: at least the VPC's own
// prefix (a subnet can be the whole VPC) and at most MinSubnetPrefix.
func (a *SubnetAllocator) checkPrefix(bits int) error {
	if bits < a.vpc.bits {
		return fmt.Errorf("%w: /%d is larger than the VPC /%d", ErrInvalidPrefix, bits, a.vpc.bits)
	}
	if bits > MinSubnetPrefix {
		return fmt.Errorf("%w: /%d exceeds the minimum subnet /%d", ErrInvalidPrefix, bits, MinSubnetPrefix)
	}
	return nil
}

func (a *SubnetAllocator) overlapsAny(c CIDR) bool {
	for _, t := range a.taken {
		if t.Overlaps(c) {
			return true
		}
	}
	return false
}

// Allocate returns the first free CIDR of the requested prefix length,
// scanning from the VPC base — deterministic first-fit. Candidate blocks are
// naturally aligned: stepping from an aligned base by the block size visits
// only aligned addresses, so no alignment repair pass is needed.
func (a *SubnetAllocator) Allocate(bits int) (CIDR, error) {
	if err := a.checkPrefix(bits); err != nil {
		return CIDR{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	step := uint64(1) << (32 - bits)
	vpcSize := a.vpc.Size()
	base := toU32(a.vpc.base)
	for off := uint64(0); off+step <= vpcSize; off += step {
		cand := newCIDR(fromU32(base+uint32(off)), bits)
		if !a.overlapsAny(cand) {
			a.taken = append(a.taken, cand)
			return cand, nil
		}
	}
	return CIDR{}, fmt.Errorf("%w: /%d in %s", ErrExhausted, bits, a.vpc)
}

// Reserve records a customer-specified subnet block. It must fit inside the
// VPC with a legal prefix and must not overlap anything already held.
// Platform-reserved ranges (e.g. an internal services block carved out
// before the product opens) use the same call — one writer, one rule.
func (a *SubnetAllocator) Reserve(c CIDR) error {
	if err := a.checkPrefix(c.bits); err != nil {
		return err
	}
	if !a.vpc.ContainsCIDR(c) {
		return fmt.Errorf("%w: %s not inside %s", ErrNotInVPC, c, a.vpc)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.overlapsAny(c) {
		return fmt.Errorf("%w: %s", ErrOverlap, c)
	}
	a.taken = append(a.taken, c)
	return nil
}

// Release returns a block to the pool. Releasing an unknown block succeeds —
// the idempotency saga compensation depends on (03§8.4).
func (a *SubnetAllocator) Release(c CIDR) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, t := range a.taken {
		if t == c {
			a.taken = append(a.taken[:i], a.taken[i+1:]...)
			return nil
		}
	}
	return nil
}

// Allocated returns a copy of every held block, sorted — a stable snapshot
// for rendering a VPC's subnet list or diffing layouts across replays.
func (a *SubnetAllocator) Allocated() []CIDR {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]CIDR, len(a.taken))
	copy(out, a.taken)
	sort.Slice(out, func(i, j int) bool {
		if out[i].base == out[j].base {
			return out[i].bits < out[j].bits
		}
		return toU32(out[i].base) < toU32(out[j].base)
	})
	return out
}

// --- IP allocation ---------------------------------------------------------

// IPAllocator assigns individual addresses inside one subnet to resources
// (ECS NICs, LB VIPs, ...). It never hands out the subnet's three reserved
// addresses; customer addresses start at base+2.
type IPAllocator struct {
	mu     sync.Mutex
	subnet CIDR
	owners map[netip.Addr]string // allocated address → owning resource id
}

// NewIPAllocator builds an allocator over one subnet. It is deliberately
// permissive about the prefix (capacity maths degrade gracefully); the
// SubnetAllocator is what enforces sellable sizes.
func NewIPAllocator(subnet CIDR) *IPAllocator {
	return &IPAllocator{subnet: subnet, owners: make(map[netip.Addr]string)}
}

// Subnet returns the block this allocator serves.
func (a *IPAllocator) Subnet() CIDR { return a.subnet }

// Gateway returns the subnet's virtual-gateway address: base+1. Route tables
// point here; no customer resource may hold it.
func (a *IPAllocator) Gateway() netip.Addr {
	return fromU32(toU32(a.subnet.base) + 1)
}

// Reserved lists the three addresses never handed to customers: network,
// gateway, broadcast.
func (a *IPAllocator) Reserved() []netip.Addr {
	base := toU32(a.subnet.base)
	return []netip.Addr{
		a.subnet.base,
		fromU32(base + 1),
		a.subnet.Broadcast(),
	}
}

// Capacity is the number of allocatable addresses: size minus the reserved
// trio, floored at zero for degenerate prefixes.
func (a *IPAllocator) Capacity() uint64 {
	if a.subnet.Size() <= 3 {
		return 0
	}
	return a.subnet.Size() - 3
}

func (a *IPAllocator) isReserved(ip netip.Addr) bool {
	for _, r := range a.Reserved() {
		if r == ip {
			return true
		}
	}
	return false
}

// Allocate assigns the lowest free customer address to the owner and returns
// it. First-fit, deterministic, and it reuses holes left by released
// addresses — a released IP is reclaimable immediately, which is what makes
// "rebuild this ECS" not exhaust a subnet.
func (a *IPAllocator) Allocate(owner string) (netip.Addr, error) {
	if owner == "" {
		return netip.Addr{}, ErrOwnerRequired
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	base := toU32(a.subnet.base)
	last := toU32(a.subnet.Broadcast())
	// Customer range: base+2 .. broadcast-1.
	for v := base + 2; v < last; v++ {
		ip := fromU32(v)
		if _, taken := a.owners[ip]; !taken {
			a.owners[ip] = owner
			return ip, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("%w: %s", ErrIPExhausted, a.subnet)
}

// AllocateSpecific assigns one chosen address. Use cases: a customer
// pinning a known address, or a standby NIC re-acquiring its old IP after
// failover. Reserved and taken addresses are refused with distinct errors.
func (a *IPAllocator) AllocateSpecific(ip netip.Addr, owner string) error {
	if owner == "" {
		return ErrOwnerRequired
	}
	if !a.subnet.Contains(ip) {
		return fmt.Errorf("%w: %s not inside %s", ErrNotInSubnet, ip, a.subnet)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.isReserved(ip) {
		return fmt.Errorf("%w: %s", ErrIPReserved, ip)
	}
	if _, taken := a.owners[ip]; taken {
		return fmt.Errorf("%w: %s", ErrIPTaken, ip)
	}
	a.owners[ip] = owner
	return nil
}

// Release frees an address. Idempotent: releasing an unknown or already-free
// address succeeds.
func (a *IPAllocator) Release(ip netip.Addr) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.owners, ip)
	return nil
}

// Owner reports which resource holds an address.
func (a *IPAllocator) Owner(ip netip.Addr) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	o, ok := a.owners[ip]
	return o, ok
}

// AllocatedCount reports how many addresses are currently held.
func (a *IPAllocator) AllocatedCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.owners)
}
