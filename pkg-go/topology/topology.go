// Package topology implements the platform's Region/AZ model and the
// cross-availability-zone spread + failover reasoning (00§4.1, §4.4 P2;
// 09-roadmap §4.3 M-6).
//
// Phase 1 modelled region/az as reserved fields (P3 "Day1 预留、实现按需"):
// resource_instance.zone existed but carried no placement meaning, and every
// product's RegionScope was the literal string "REGIONAL" with nothing to
// distinguish it. M-6 turns that into the implemented P2 topology: two AZs in
// one city (同城双 AZ), MySQL MGR single-primary across zones, Kafka/Redis
// replicas spread so a client tolerates losing one AZ (00§4.4).
//
// The package reasons about *survival*, not about Kubernetes scheduling — the
// Helm chart and the topology validator carry the scheduling intent. Here we
// answer the questions the control plane needs at create-time and failover-time:
//
//   - Is this product ZONAL (pinned to one AZ, e.g. a VM) or REGIONAL (spread
//     across AZs, e.g. an object-storage bucket)?
//   - Given a replica set placed across N zones, does it survive the loss of
//     any single AZ? (the P2 promise: a client tolerates one AZ gone)
//   - What is the fault-domain spread we should hand to the scheduler, and is
//     a desired distribution actually satisfiable by the available zones?
//
// Region/AZ *names* are not re-defined here: identifier.AZName/IsValidRegion
// remain the single source for the cn-north-1 / cn-north-1-a convention
// (00 附录A). This package builds on top of those names; it does not fork them.
package topology

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/qifalab/euler-platform/identifier"
)

// RegionScope is the per-product placement scope (00§4.4 point 1; 09§4.2).
//
// A REGIONAL resource is spread across AZs of its region and has no single
// AZ of residence — object storage, VPC, monitoring. A ZONAL resource is pinned
// to one AZ: a VM, a managed-MySQL primary. ZONAL placement must therefore be
// chosen up-front (you cannot relocate a running VM between AZs without a
// stop/migrate), and a ZONAL resource that survives an AZ loss does so via
// cross-AZ *replicas*, not by moving.
type RegionScope string

const (
	// ScopeRegional: spread across AZs, no single AZ of residence.
	ScopeRegional RegionScope = "REGIONAL"
	// ScopeZonal: pinned to one AZ at create time.
	ScopeZonal RegionScope = "ZONAL"
)

// Valid reports whether s is a known scope value. Unknown scopes are rejected
// rather than defaulted: a product whose scope is "" or "regional" (wrong case)
// is a catalogue seed error, and silently treating it as REGIONAL would hide it
// until an AZ failure exposes the mis-placement.
func (s RegionScope) Valid() bool {
	return s == ScopeRegional || s == ScopeZonal
}

// Zonal reports whether a resource of this scope is pinned to one AZ.
func (s RegionScope) Zonal() bool { return s == ScopeZonal }

// ProductPlacement is the catalogue's declared placement for a product:
// its scope and, for ZONAL products, whether cross-AZ replicas exist.
type ProductPlacement struct {
	ProductCode string
	Scope       RegionScope
	// CrossAZReplicas is meaningful only for ZONAL scopes: whether the product
	// provisions replicas across AZs (so a ZONAL resource still survives an AZ
	// loss). A single-AZ ZONAL product (e.g. a non-HA VM) has CrossAZReplicas
	// false and is the thing the failover check must flag as at-risk.
	CrossAZReplicas bool
}

// AZ is an availability zone — a region + a zone letter (00§4.1).
//
// The name is built by identifier.AZName so the convention cannot drift; this
// struct only carries the structured form a placement decision needs.
type AZ struct {
	Region string
	Letter string
}

// Name returns the AZ's canonical name (cn-north-1-a), delegating to
// identifier.AZName.
func (a AZ) Name() string { return identifier.AZName(a.Region, a.Letter) }

// ParseAZName splits cn-north-1-a back into (region, letter). It validates the
// region via identifier.IsValidRegion and requires a single trailing zone
// letter; an AZ name that does not match the convention is rejected so a
// mis-typed topology config fails loudly at load, not silently at failover.
func ParseAZName(name string) (AZ, error) {
	idx := strings.LastIndex(name, "-")
	if idx <= 0 {
		return AZ{}, fmt.Errorf("topology: invalid az name %q (want {region}-{letter})", name)
	}
	region := name[:idx]
	letter := name[idx+1:]
	if !identifier.IsValidRegion(region) {
		return AZ{}, fmt.Errorf("topology: invalid region in az name %q", name)
	}
	if !validZoneLetter(letter) {
		return AZ{}, fmt.Errorf("topology: invalid zone letter %q in az name %q (want a single letter a-z)", letter, name)
	}
	return AZ{Region: region, Letter: letter}, nil
}

// validZoneLetter accepts a single lowercase letter a-z. Zone identifiers are
// letters, not numbers, by convention (cn-north-1-a not cn-north-1-1), because
// a digit would collide visually with the region's trailing index.
func validZoneLetter(s string) bool {
	if len(s) != 1 {
		return false
	}
	c := s[0]
	return c >= 'a' && c <= 'z'
}

// Topology is the set of zones a region offers. P2 has two; P3 will add a
// remote region. The count is a parameter, not a constant, so the failover
// math generalises — but SurvivesAZLoss hard-codes the P2 contract (survive
// exactly one AZ gone), because that is the promise 00§4.4 makes and a number
// that tolerates two-AZ loss would be a stronger claim than the topology gives.
type Topology struct {
	Region string
	Zones  []AZ
}

// NewTopology builds a topology for a region from its zone letters, validating
// the region and deduping/sorting the zones so two callers that pass zones in
// different orders compare equal.
func NewTopology(region string, letters []string) (Topology, error) {
	if !identifier.IsValidRegion(region) {
		return Topology{}, fmt.Errorf("topology: invalid region %q", region)
	}
	seen := make(map[string]bool, len(letters))
	zones := make([]AZ, 0, len(letters))
	for _, l := range letters {
		if !validZoneLetter(l) {
			return Topology{}, fmt.Errorf("topology: invalid zone letter %q", l)
		}
		az := AZ{Region: region, Letter: l}
		if seen[az.Letter] {
			continue // dedupe; same letter twice is a config mistake we tolerate
		}
		seen[az.Letter] = true
		zones = append(zones, az)
	}
	sort.Slice(zones, func(i, j int) bool { return zones[i].Letter < zones[j].Letter })
	if len(zones) == 0 {
		return Topology{}, errors.New("topology: at least one zone required")
	}
	return Topology{Region: region, Zones: zones}, nil
}

// IsDualAZ reports whether the topology has exactly two zones — the P2 shape.
// A single zone is the P1 shape (still valid during migration); three is P3
// regional and is flagged so the failover contract is not silently widened.
func (t Topology) IsDualAZ() bool { return len(t.Zones) == 2 }

// ZoneNames returns the canonical AZ names, sorted.
func (t Topology) ZoneNames() []string {
	out := make([]string, len(t.Zones))
	for i, z := range t.Zones {
		out[i] = z.Name()
	}
	return out
}

// Placement is a decided placement of replicas across zones. It is the output
// of Distribute and the input to SurvivesAZLoss.
type Placement struct {
	// ByZone maps AZ name → replica count placed there. Zones with zero are
	// included so a sparse distribution is visible, not implied.
	ByZone map[string]int
	// Total is the sum of ByZone values, cached so survival checks and callers
	// do not re-sum each time.
	Total int
}

// SurvivesAZLoss answers the P2 contract for a NON-quorum replica set: does
// this placement keep at least one live replica after the loss of any single
// AZ (00§4.4 point 3)?
//
// Non-quorum semantics: stateless replicas, Kafka partitions with
// unclean-leader disabled off the table, Redis read replicas — anything where
// "service survives" means "≥1 replica remains", not "a majority remains".
// Quorum systems (MySQL MGR and anything Raft/Paxos-shaped) need strictly more
// than half the members alive and must use SurvivesAZLossMGR instead; the two
// contracts are different and conflating them either over-rejects harmless
// placements (a 2-of-2 spread is fine for a stateless pair) or under-protects
// quorum ones.
//
// The one-replica case never survives (losing that AZ loses everything); a
// placement concentrated entirely in one AZ never survives. Both are the
// failure modes the topology validator must catch before a deploy.
func (p Placement) SurvivesAZLoss() bool {
	if p.Total < 2 || len(p.ByZone) == 0 {
		return false
	}
	zonesWithReplicas := 0
	for _, here := range p.ByZone {
		if here > 0 {
			zonesWithReplicas++
		}
		if p.Total-here < 1 {
			// Losing this zone loses every replica.
			return false
		}
	}
	// All replicas in one zone (possibly with empty zones listed) never survives.
	return zonesWithReplicas >= 2
}

// SurvivesAZLossMGR answers the MySQL MGR single-primary contract specifically
// (04§6.9, 00§4.4 point 2). MGR is single-primary with group replication
// majority: a write commit needs ACK from a majority of group members. Losing
// one AZ must leave a majority of members reachable, i.e. the surviving
// replicas must be > floor(total/2).
//
// NOTE — dual-AZ is mathematically insufficient for MGR: with only two zones,
// one zone always holds at least half the members, so losing it can never
// leave a strict majority; this function correctly returns false for every
// two-zone placement. That is not a bug to relax — surviving either AZ's loss
// with quorum intact requires a third fault domain (a witness/arbiter AZ or a
// P3 remote region) holding at least one member.
func (p Placement) SurvivesAZLossMGR() bool {
	if p.Total < 3 || len(p.ByZone) == 0 {
		return false
	}
	// majority threshold: strictly more than half of Total.
	threshold := p.Total / 2 // floor(total/2); majority is > threshold
	for _, here := range p.ByZone {
		// losing this zone leaves Total-here; that must still be a majority.
		remaining := p.Total - here
		if remaining <= threshold {
			return false
		}
	}
	return true
}

// Distribute spreads `replicas` across the topology's zones as evenly as
// possible, preferring the earlier zones for the remainder (so a deterministic
// input yields a deterministic placement — no Math.random in placement, which
// would make a dry-run non-reproducible).
//
// The result is the *desired* distribution; whether it survives AZ loss is a
// separate question the caller must ask (SurvivesAZLoss). Distribute does not
// refuse to produce an at-risk placement — it returns the even spread, and the
// survival check is the gate. This separation matters: a 2-replica set across 2
// zones is the even spread (1,1), and it correctly does NOT survive AZ loss;
// refusing to distribute would hide that the user asked for an unsatisfiable
// HA contract, which is worse than distributing and flagging it.
func (t Topology) Distribute(replicas int) (Placement, error) {
	if replicas <= 0 {
		return Placement{}, errors.New("topology: replicas must be > 0")
	}
	if len(t.Zones) == 0 {
		return Placement{}, errors.New("topology: no zones to distribute across")
	}
	byZone := make(map[string]int, len(t.Zones))
	base := replicas / len(t.Zones)
	rem := replicas % len(t.Zones)
	for i, z := range t.Zones {
		n := base
		if i < rem {
			n++ // spread the remainder across the first `rem` zones
		}
		byZone[z.Name()] = n
	}
	return Placement{ByZone: byZone, Total: replicas}, nil
}

// CanSatisfy reports whether the topology can place `replicas` such that the
// P2 AZ-loss contract holds. It is the preflight a create-flow calls before
// provisioning: a ZONAL cross-AZ-replica product whose required replica count
// cannot satisfy survival must be rejected at order time, not discovered at
// the moment an AZ actually fails (which is the worst time to learn a
// placement is not fault-tolerant).
//
// With mgr=true the quorum rule applies, and on a dual-AZ topology this is
// ALWAYS false regardless of replica count (see SurvivesAZLossMGR): MGR needs
// a third arbitration point before its AZ-loss contract can be satisfied.
func (t Topology) CanSatisfy(replicas int, mgr bool) bool {
	p, err := t.Distribute(replicas)
	if err != nil {
		return false
	}
	if mgr {
		return p.SurvivesAZLossMGR()
	}
	return p.SurvivesAZLoss()
}

// FaultDomainSpread returns the Kubernetes topologySpreadConstraints intent
// (maxSkew:1, whenUnsatisfiable:DoNotSchedule) the Helm chart should render for
// a stateful workload, keyed on zone. This is the *intent* the chart carries;
// the topology validator checks that it is present. The function lives here so
// the "spread across zones, skew 1, hard" policy is defined once and named,
// rather than re-stated (and re-drifted) in every chart.
//
// The returned map is the structured form; the chart serialises it to YAML.
func FaultDomainSpread(selectorLabels map[string]string) map[string]any {
	return map[string]any{
		"maxSkew":           1,
		"topologyKey":       "topology.kubernetes.io/zone",
		"whenUnsatisfiable": "DoNotSchedule",
		"labelSelector": map[string]any{
			"matchLabels": selectorLabels,
		},
	}
}

// PlacementReport summarises a placement for the failover runbook: the
// distribution, whether it survives a generic AZ loss, and the zones that are
// the single point of failure (the at-risk zones a drill should target).
type PlacementReport struct {
	Replicas     int
	Distribution map[string]int
	Survives     bool
	SurvivesMGR  bool
	AtRiskZones  []string // zones whose loss breaks the contract
}

// Report builds the human/machine-readable survival report for a placement.
func (p Placement) Report(mgr bool) PlacementReport {
	r := PlacementReport{
		Distribution: p.ByZone,
		Survives:     p.SurvivesAZLoss(),
		SurvivesMGR:  p.SurvivesAZLossMGR(),
	}
	threshold := p.Total / 2
	for zone, here := range p.ByZone {
		remaining := p.Total - here
		broken := remaining < 1 // generic (non-quorum) contract: ≥1 must remain
		if mgr {
			broken = remaining <= threshold
		}
		if broken {
			r.AtRiskZones = append(r.AtRiskZones, zone)
		}
	}
	sort.Strings(r.AtRiskZones)
	r.Replicas = p.Total
	return r
}
