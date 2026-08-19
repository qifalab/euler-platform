// Package chaos models the platform's chaos-engineering discipline — the drill
// plans, mandatory subjects, and the failover-result adjudication that phase 3
// (09-roadmap §5.2 M-9, 08§9.5) turns into a quarterly rhythm (09§5.3 C2:
// 混沌演练每季度 ≥1 次).
//
// The package carries the *plan and the verdict math*, not the injection tool:
// 08§9.5 picks K8s-native actions (kill pods, drain nodes) as the cheap
// default, with ChaosBlade/LitmusChaos as optional aids. Injection is the
// executor's job; here we enforce the discipline — staging first, bounded
// blast radius, a 10-minute abort for prod, and a >50% expected-vs-actual
// deviation that opens a remediation item.
package chaos

import (
	"errors"
	"fmt"
	"time"
)

// DrillKind is one of the six mandatory drill subjects (08§9.5 必练科目).
type DrillKind string

const (
	// DrillNacosSplitBrain: Nacos 集群脑裂/不可用时降级可用.
	DrillNacosSplitBrain DrillKind = "nacos-split-brain"
	// DrillRedisFailover: Redis Cluster 主从切换.
	DrillRedisFailover DrillKind = "redis-failover"
	// DrillMySQLFailover: MySQL 主库切换与分库分表恢复.
	DrillMySQLFailover DrillKind = "mysql-primary-failover"
	// DrillApisixEtcdSelfHeal: APISIX/etcd 故障网关自愈.
	DrillApisixEtcdSelfHeal DrillKind = "apisix-etcd-self-heal"
	// DrillKafkaBrokerLoss: Kafka broker 宕机计量数据不丢 (补偿重放).
	DrillKafkaBrokerLoss DrillKind = "kafka-broker-loss"
	// DrillSingleAZLoss: 单可用区整体不可用.
	DrillSingleAZLoss DrillKind = "single-az-loss"
)

// MandatoryDrills is the six 必练科目 (08§9.5), in table order.
var MandatoryDrills = []DrillKind{
	DrillNacosSplitBrain,
	DrillRedisFailover,
	DrillMySQLFailover,
	DrillApisixEtcdSelfHeal,
	DrillKafkaBrokerLoss,
	DrillSingleAZLoss,
}

// Valid reports whether k is a known drill kind. Unknown kinds are rejected —
// a free-form drill name is not the bounded six-subject discipline 08§9.5
// prescribes, and silently accepting it would widen the blast surface.
func (k DrillKind) Valid() bool {
	for _, m := range MandatoryDrills {
		if k == m {
			return true
		}
	}
	return false
}

// Stage is where a drill runs.
type Stage string

const (
	StageStaging Stage = "STAGING"
	StageProd    Stage = "PROD"
)

// ProdAbortDeadline is the prod discipline (08§9.5): a prod drill must have a
// termination path within 10 minutes. Staging drills do not carry this bound —
// staging is the rehearsal surface, and its whole point is room to fail.
const ProdAbortDeadline = 10 * time.Minute

// DrillPlan is a scheduled fault-injection drill.
type DrillPlan struct {
	// Kind is one of the six mandatory subjects.
	Kind DrillKind
	// Stage is where the drill runs (staging first is the discipline; prod is
	// the bounded-radius rehearsal).
	Stage Stage
	// BlastRadius names the bounded failure surface (e.g. "one kafka broker",
	// "the lighter MySQL AZ"). Prod requires a named, bounded radius.
	BlastRadius string
	// AbortDeadline is the guaranteed termination path. Required and bounded by
	// ProdAbortDeadline for prod drills.
	AbortDeadline time.Duration
}

// Validate enforces the drill discipline. A plan that fails Validate must be
// rejected at scheduling time, not discovered when the injected fault is
// already live with no abort path.
func (d DrillPlan) Validate() error {
	if !d.Kind.Valid() {
		return fmt.Errorf("chaos: unknown drill kind %q", d.Kind)
	}
	switch d.Stage {
	case StageStaging:
		// staging: no abort bound, no blast-radius requirement — room to fail.
	case StageProd:
		if d.AbortDeadline <= 0 {
			return errors.New("chaos: prod drill requires an abort deadline (08§9.5 10 分钟内终止手段)")
		}
		if d.AbortDeadline > ProdAbortDeadline {
			return fmt.Errorf("chaos: prod drill abort deadline %v exceeds %v (08§9.5)", d.AbortDeadline, ProdAbortDeadline)
		}
		if d.BlastRadius == "" {
			return errors.New("chaos: prod drill requires a named blast radius (08§9.5 限定爆炸半径)")
		}
	default:
		return fmt.Errorf("chaos: unknown stage %q", d.Stage)
	}
	return nil
}

// DrillResult records the expected-vs-actual recovery time for a drill.
type DrillResult struct {
	Kind     DrillKind
	Expected time.Duration
	Actual   time.Duration
}

// NeedsRemediation reports whether the deviation opens a remediation item:
// actual recovery exceeding 150% of expected (08§9.5: 偏差 > 50% 立项整改).
// Exactly 150% is not ">50%", so it does not trip — the threshold is strict.
func (r DrillResult) NeedsRemediation() bool {
	return r.Actual > r.Expected*3/2
}
