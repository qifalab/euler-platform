// Package event defines the platform's Kafka event envelope and the canonical
// topic/aggregate constants. This is the Go-side mirror of the contract in
// 04-middleware-infrastructure.md §5.3/§5.4 (the 全平台唯一事实源) and
// 03-backend-services.md §7.
//
// Rules (C2/S8):
//   - topic names follow cloud.{domain}.{aggregate}.{event}; platform-level
//     channels use cloud.sys.{purpose}
//   - topic names NEVER contain environment identifiers; env isolation is by
//     Kafka cluster, never by topic-name prefix (avoids "test traffic writes
//     to prod topic")
//   - every event carries the统一信封 {event_id, event_type, occurred_at,
//     aggregate_id, payload}; payload holds only IDs and state, never full
//     data — consumers回查 as needed (03§7)
//   - consumers MUST be idempotent (03§8.3); production forces acks=all +
//     enable.idempotence=true (04§5.6)
package event

import (
	"encoding/json"
	"fmt"
	"time"
)

// Envelope is the统一信封 every cloud.* event carries. It is transport-agnostic:
// the payload field is a marshaled protobuf message body (or JSON for the
// platform's own Go services that have not yet adopted protobuf).
type Envelope struct {
	EventID     string          `json:"event_id"`      // unique, e.g. ev-YYYYMMDD-000123
	EventType   string          `json:"event_type"`    // e.g. "cloud.trade.order.paid"
	OccurredAt  time.Time      `json:"occurred_at"`   // UTC
	AggregateID string          `json:"aggregate_id"`  // account_id / resource_id / order_id
	Payload     json.RawMessage `json:"payload"`       // IDs + state only, never full data
}

// Marshal returns the envelope as JSON for the Kafka producer.
func (e Envelope) Marshal() ([]byte, error) {
	return json.Marshal(e)
}

// UnmarshalEnvelope decodes a Kafka message value into an Envelope.
func UnmarshalEnvelope(data []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return Envelope{}, fmt.Errorf("event: unmarshal envelope: %w", err)
	}
	return e, nil
}

// Topic is a strongly-typed Kafka topic name. Construct with the Topic* helpers
// or use the predeclared constants below.
type Topic string

// Topic returns the cloud.{domain}.{aggregate}.{event} topic name.
func TopicName(domain, aggregate, event string) Topic {
	return Topic(fmt.Sprintf("cloud.%s.%s.%s", domain, aggregate, event))
}

// SysTopic returns the cloud.sys.{purpose} platform-level topic name.
func SysTopic(purpose string) Topic {
	return Topic(fmt.Sprintf("cloud.sys.%s", purpose))
}

// String returns the topic as a string for the Kafka client.
func (t Topic) String() string { return string(t) }

// Canonical topic constants (04§5.4 is the authoritative source of truth).
// These are declared here so producers/consumers reference a single symbol
// rather than hand-typing the dotted string, eliminating spelling drift.
const (
	// 交易域.
	TradeOrderEvent        = Topic("cloud.trade.order.event")
	TradePaymentEvent      = Topic("cloud.trade.payment.event")

	// 计费域.
	BillingAccountEvent    = Topic("cloud.billing.account.event")
	BillingInvoiceEvent    = Topic("cloud.billing.invoice.event")

	// 资源域 (resource lifecycle bus — the hottest topic).
	ResourceLifecycleEvent = Topic("cloud.resource.lifecycle.event")
	ResourceQuotaEvent      = Topic("cloud.resource.quota.event")
	ResourceProvisionTask   = Topic("cloud.resource.provision.task")  // async callback/retry ONLY (S21)
	ResourceProvisionStatus = Topic("cloud.resource.provision.status") // async callback/retry ONLY (S21)

	// 计量域.
	MeteringUsageRaw        = Topic("cloud.metering.usage.raw")        // 64 partitions, largest traffic
	MeteringBillingEvent    = Topic("cloud.metering.billing.event")    // hourly aggregate → billing

	// 审计域.
	SysAuditAction          = Topic("cloud.sys.audit.action")          // partitioned by account_id
	SysAuditAccess          = Topic("cloud.sys.audit.access")
	SysAuditChange          = Topic("cloud.sys.audit.change")
	SysAuditAgent           = Topic("cloud.sys.audit.agent")

	// 告警域.
	SysAlertEvent           = Topic("cloud.sys.alert.event")

	// 通知域.
	NotifyMessage           = Topic("cloud.notify.message")

	// 账号域.
	UserEvent               = Topic("cloud.user.event")
	UserLoginEvent          = Topic("cloud.user.login.event")

	// 平台级.
	SysAuthzPolicyChanged   = Topic("cloud.sys.authz.policy.changed")
	SysThreatEvent          = Topic("cloud.sys.threat.event")
	SysWorkflowTask         = Topic("cloud.sys.workflow.task")
	SysLogBuffer            = Topic("cloud.sys.log.buffer")
	SysRUMEvent             = Topic("cloud.sys.rum.event")
	SysHostMetrics          = Topic("cloud.sys.host.metrics")
	SysAgentCommand         = Topic("cloud.sys.agent.command")
	SysNodeLifecycle        = Topic("cloud.sys.node.lifecycle")
	SysEtcdBackup           = Topic("cloud.sys.etcd.backup")
	SysCacheInvalidate      = Topic("cloud.sys.cache.invalidate")
	SysSearchSync           = Topic("cloud.sys.search.sync")

	// 营销域 (phase-2 only).
	MarketActivityEvent     = Topic("cloud.market.activity.event")
)

// PartitionKey returns the canonical Kafka partition key for a topic.
// The partition key is the "smallest unit needing order preservation"
// (04§5.3): same-resource usage and same-order state transitions must land on
// the same partition. The mapping below encodes the authoritative key per topic.
func PartitionKey(t Topic, aggregateID string) string {
	switch t {
	case TradePaymentEvent, ResourceLifecycleEvent, ResourceProvisionTask, ResourceProvisionStatus,
		MeteringUsageRaw:
		// resource_id / order_id / instance_id keyed — resource-scoped ordering.
		return aggregateID
	default:
		// account_id-keyed by convention for trade/billing/user/notify/audit.
		return aggregateID
	}
}
