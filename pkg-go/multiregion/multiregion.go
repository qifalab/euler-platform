// Package multiregion implements the platform's cross-region (multi-region)
// model — the P3 "两地三中心" topology (00§4.2, §4.5; 09-roadmap §5.2 M-8) and
// the global-vs-regional service classification it imposes.
//
// This is the region-level layer ABOVE pkg-go/topology, which reasons about
// AZ-level spread and failover within ONE region (P2 同城双活). The two layers
// answer different questions:
//
//   - topology: "placed across the AZs of one region, does a replica set
//     survive losing an AZ?" (P2)
//   - multiregion: "given a second remote region, does the platform survive
//     losing the primary REGION, and within what RPO/RTO?" (P3 两地三中心)
//
// Phase 1/2 carried region_id as a reserved metadata field (00§4.1: every
// table/topic/label carries region_id, but it is not a shard key — the shard
// key is account_id). M-8 turns that reservation into an implemented contract:
// a region is now a first-class object with a role (PRIMARY/STANDBY), a
// distance, and the plan it belongs to carries replication channels with RPO
// targets and a failover sequence with an RTO bound.
//
// Region *names* are not re-defined here: identifier.IsValidRegion remains the
// single source for the cn-north-1 / cn-east-1 convention (00 附录A), exactly
// as topology delegates to identifier.AZName. This package builds on top of
// those names; it does not fork them.
package multiregion

import (
	"errors"
	"fmt"
	"time"

	"github.com/starcloud/sc-platform/identifier"
)

// ServiceScope classifies a control-plane service as either a global singleton
// or a regional replica (09§5.2 M-8: "IAM/计费全局单例 + 地域级数据复制方案上线").
type ServiceScope string

const (
	// ScopeGlobal: one instance serves every region. Only the account system
	// (IAM) and the billing spine are global (00§4.1: 账号为全局域; 09§5.3 C1:
	// "全局服务(IAM/计费)双活或主备自动切换").
	ScopeGlobal ServiceScope = "GLOBAL"
	// ScopeRegional: one instance per region, with cross-region replication.
	ScopeRegional ServiceScope = "REGIONAL"
)

// Valid reports whether s is a known scope. Unknown scopes are rejected rather
// than defaulted — a mistyped classification must fail loudly at load, not
// silently place a global service into a region it cannot leave.
func (s ServiceScope) Valid() bool {
	return s == ScopeGlobal || s == ScopeRegional
}

// RegionRole is a region's role in the 两地三中心 topology (00§4.5).
type RegionRole string

const (
	// RolePrimary: the in-city dual-active region — both AZs read/write (the
	// P2 shape), and the region that owns the authoritative data.
	RolePrimary RegionRole = "PRIMARY"
	// RoleStandby: the remote (>300km) disaster-recovery region — read-only
	// traffic, data replicas, a cold control plane; it takes no writes (00§4.5).
	RoleStandby RegionRole = "STANDBY"
)

// Valid reports whether r is a known role.
func (r RegionRole) Valid() bool { return r == RolePrimary || r == RoleStandby }

// Writable reports whether the region accepts writes. Only PRIMARY writes; the
// standby is read-only/DR. This encodes the P3 boundary "不承诺异地多活写"
// (00§4.5): a writable standby would be the unitization (单元化) architecture,
// which the spec explicitly defers to a separate project, not P3.
func (r RegionRole) Writable() bool { return r == RolePrimary }

// StateClass classifies a piece of platform state by how it exists across
// regions (00§4.1, §4.5).
type StateClass string

const (
	// StateShared: one global copy every region reads and writes — the account
	// system only (00§4.1: "账号体系为全局域" is the sole global-domain
	// statement).
	StateShared StateClass = "SHARED"
	// StateReplicated: one authoritative copy in the primary, asynchronously
	// copied to the standby (账务 binlog near-realtime; MinIO Replication).
	StateReplicated StateClass = "REPLICATED"
	// StateRebuilt: not copied at all — recreated per region by the cloud.*
	// conventions (Kafka topics: 00§4.5 "不做跨城镜像, 异地按 cloud.* 规范重建
	// topic").
	StateRebuilt StateClass = "REBUILT"
)

// Valid reports whether c is a known state class.
func (c StateClass) Valid() bool {
	return c == StateShared || c == StateReplicated || c == StateRebuilt
}

// Region is a physical region in the multi-region topology.
type Region struct {
	// Name is the canonical region name (cn-north-1 / cn-east-1, 00 附录A),
	// validated by identifier.IsValidRegion.
	Name string
	// Role is PRIMARY (in-city dual-active) or STANDBY (remote DR).
	Role RegionRole
	// DistanceKm is the distance from the primary city. It is meaningful only
	// for a STANDBY, where it must exceed the remote-site floor (300km).
	DistanceKm int
	// ZoneLetters are the AZ letters within the region (a, b, ...). The primary
	// carries its dual-AZ shape here; the standby carries its single AZ.
	ZoneLetters []string
}

// ReplicationChannel is a cross-region sync channel with its RPO target
// (00§4.5: 账务 binlog 准实时 + MinIO Replication 异步).
type ReplicationChannel struct {
	// Name is the channel's canonical name (ledger-binlog / object-storage).
	Name string
	// Mode is "near-realtime" (账务 binlog, canal/otter-class tools) or
	// "async" (MinIO Replication). It is descriptive; the RPO carries the
	// enforceable number.
	Mode string
	// RPO is the recovery point objective: the maximum acceptable data loss,
	// i.e. the ceiling on replication lag (now − last_replicated_at). Core
	// ledger is ≈0; object storage is looser.
	RPO time.Duration
}

// MeetsRPO reports whether an observed replication lag satisfies the channel's
// RPO target. lag is now − last_replicated_at; a negative lag is treated as
// satisfied (the replica is ahead of the observation clock).
func (c ReplicationChannel) MeetsRPO(lag time.Duration) bool {
	return lag <= c.RPO
}

// Plan is the 两地三中心 topology: one primary + one remote standby, plus the
// cross-region replication channels between them (00§4.2, §4.5).
type Plan struct {
	Primary  Region
	Standby  Region
	Channels []ReplicationChannel
}

// MaxRTO is the P3 disaster-recovery bound (00§4.2, §4.5: RTO ≤ 30min).
const MaxRTO = 30 * time.Minute

// StandbyDistanceFloor is the remote-site distance requirement (00§4.5: the
// standby city must be > 300km from the primary).
const StandbyDistanceFloor = 300

// CoreLedgerRPO is the RPO target for the core-ledger binlog channel (00§4.2,
// §4.5: 关键数据 RPO≈0). Near-realtime, not zero — "≈0" is a target, not an
// arithmetic identity, and a genuinely-zero RPO is unachievable by any
// asynchronous channel.
const CoreLedgerRPO = 5 * time.Second

// FailoverStep is one step of the 灾备接管 sequence (00§4.5).
type FailoverStep struct {
	// Name is the step's machine-readable name.
	Name string
	// Detail is the human-facing action.
	Detail string
}

// FailoverSteps returns the standby-takeover sequence: the cold control plane
// is brought hot, then DNS is cut over (00§4.5: "灾备接管: 管控面冷转热 + DNS
// 切换"). This is the canonical two-step sequence the cross-region drill
// runbook (tools/cross-region-failover-drill.md) executes.
func (p Plan) FailoverSteps() []FailoverStep {
	return []FailoverStep{
		{Name: "control-plane-cold-to-hot", Detail: "异地管控面冷转热"},
		{Name: "dns-cutover", Detail: "DNS 切换"},
	}
}

// MeetsRTO reports whether an observed failover duration satisfies the P3 RTO
// bound. observed is the wall-clock time from disaster declared to the standby
// serving traffic.
func MeetsRTO(observed time.Duration) bool { return observed <= MaxRTO }

// Validate checks the plan's invariants. A plan that fails Validate is a
// topology error that must be rejected at load/design time, not discovered
// when the primary region is actually gone.
func (p Plan) Validate() error {
	if err := p.validateRegion(p.Primary, RolePrimary); err != nil {
		return err
	}
	if err := p.validateRegion(p.Standby, RoleStandby); err != nil {
		return err
	}
	if p.Primary.Name == p.Standby.Name {
		return errors.New("multiregion: primary and standby must be different regions")
	}
	if p.Primary.DistanceKm != 0 {
		return fmt.Errorf("multiregion: primary distance must be 0, got %d", p.Primary.DistanceKm)
	}
	if p.Standby.DistanceKm <= StandbyDistanceFloor {
		return fmt.Errorf("multiregion: standby %q is %d km away, must exceed %d km (00§4.5 remote-site floor)",
			p.Standby.Name, p.Standby.DistanceKm, StandbyDistanceFloor)
	}
	for _, c := range p.Channels {
		if c.Name == "" {
			return errors.New("multiregion: replication channel name required")
		}
		if c.RPO < 0 {
			return fmt.Errorf("multiregion: replication channel %q has negative RPO", c.Name)
		}
	}
	return nil
}

func (p Plan) validateRegion(r Region, want RegionRole) error {
	if !identifier.IsValidRegion(r.Name) {
		return fmt.Errorf("multiregion: invalid region name %q (want cn-north-1 style)", r.Name)
	}
	if r.Role != want {
		return fmt.Errorf("multiregion: region %q has role %q, want %q", r.Name, r.Role, want)
	}
	return nil
}

// CoreLedgerChannel returns the standard core-ledger binlog replication channel
// (00§4.5: 账务 binlog 准实时同步, canal/otter 类工具可选), so callers reference
// one definition rather than hand-writing the RPO.
func CoreLedgerChannel() ReplicationChannel {
	return ReplicationChannel{Name: "ledger-binlog", Mode: "near-realtime", RPO: CoreLedgerRPO}
}

// ObjectStorageChannel returns the standard object-storage replication channel
// (00§4.5: MinIO Replication 异步复制).
func ObjectStorageChannel() ReplicationChannel {
	return ReplicationChannel{Name: "object-storage", Mode: "async", RPO: time.Hour}
}

// serviceScope is the authoritative global-vs-regional classification.
//
// GLOBAL is a small, closed set — the account system and the billing spine
// (00§4.1: 账号为全局域; 09§5.2 M-8: "IAM/计费全局单例"; 09§5.3 C1: "全局服务
// (IAM/计费)双活或主备自动切换"). Everything else is REGIONAL (one instance per
// region, data replicated). The full service catalogue is also listed so a
// mistyped service name fails loudly instead of silently defaulting to
// REGIONAL and mis-placing a global service.
var serviceScope = map[string]ServiceScope{
	// Global singletons.
	"svc-iam":    ScopeGlobal, // 账号/身份/策略 — 00§4.1 账号为全局域
	"svc-billing": ScopeGlobal, // 出账中枢 — 09§5.2 M-8 计费全局单例

	// Regional services.
	"svc-org":         ScopeRegional,
	"svc-catalog":     ScopeRegional,
	"svc-order":       ScopeRegional,
	"svc-payment":     ScopeRegional, // 资金侧; 账务 binlog 跨地域复制 (StateReplicated)
	"svc-metering":    ScopeRegional,
	"svc-orchestrator": ScopeRegional,
	"svc-quota":       ScopeRegional,
	"svc-workflow":    ScopeRegional,
	"svc-monitor":     ScopeRegional,
	"svc-notify":      ScopeRegional,
	"svc-audit":       ScopeRegional,
	"svc-ticket":      ScopeRegional,
	"svc-api-meta":    ScopeRegional,
	"console-bff":     ScopeRegional,
	"alert-engine":    ScopeRegional,
	"alert-center":    ScopeRegional,
	"rc-compute":      ScopeRegional,
	"rc-eci":          ScopeRegional,
	"rc-lb":           ScopeRegional,
	"rc-autoscaling":  ScopeRegional,
	"rc-backup":       ScopeRegional,
	"rc-redis":        ScopeRegional,
	"rc-kafka":        ScopeRegional,
	"rc-logservice":   ScopeRegional,
	"rc-storage":      ScopeRegional,
	"rc-network":      ScopeRegional,
	"rc-database":     ScopeRegional,
}

// Classify returns the service's scope. Unknown services are rejected so a
// typo cannot silently mis-place a service (the global set is the exception;
// defaulting an unknown name to REGIONAL would be the silent failure mode).
func Classify(service string) (ServiceScope, error) {
	scope, ok := serviceScope[service]
	if !ok {
		return "", fmt.Errorf("multiregion: unknown service %q", service)
	}
	return scope, nil
}

// IsGlobal reports whether the service is a global singleton.
func IsGlobal(service string) bool {
	scope, err := Classify(service)
	return err == nil && scope == ScopeGlobal
}

// stateClass is the authoritative cross-region state classification (00§4.1,
// §4.5). Unknown state names are rejected — the three classes are an explicit
// taxonomy, and "replicate it" is not a safe default for state whose class the
// platform has not declared.
var stateClass = map[string]StateClass{
	"account":         StateShared,     // 账号体系 — 00§4.1 唯一全局域
	"ledger":          StateReplicated, // 账务 binlog 准实时 — 00§4.5
	"ledger-binlog":   StateReplicated,
	"object-storage":  StateReplicated, // MinIO Replication 异步 — 00§4.5
	"kafka-topic":     StateRebuilt,    // 不做跨城镜像, 异地按 cloud.* 重建 — 00§4.5
}

// ClassifyState returns how a named piece of state exists across regions.
func ClassifyState(state string) (StateClass, error) {
	class, ok := stateClass[state]
	if !ok {
		return "", fmt.Errorf("multiregion: unknown state %q (want account/ledger/object-storage/kafka-topic)", state)
	}
	return class, nil
}
