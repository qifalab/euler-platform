// Package audit implements the tamper-evident operation audit trail
// (07-security.md §6, 03-backend-services.md §4.4.3).
//
// # Why a hash chain rather than "just don't allow deletes"
//
// Database permissions can be changed by whoever holds the credentials, and
// the people most motivated to alter an audit record are often the ones with
// the access to do it. A per-tenant hash chain makes tampering *detectable*
// rather than merely *forbidden*: every event embeds the hash of its
// predecessor, so removing, reordering or editing any record breaks every
// link after it, and the break cannot be repaired without recomputing the
// whole chain — which itself is visible if the chain head is published or
// checkpointed.
//
// This is what turns "we don't allow deletion" into something an auditor can
// verify instead of take on faith (07§6.2).
//
// # What is audited
//
// Every OpenAPI call (gateway side-stream) plus every console high-risk
// operation, including DENIED ones. A rejected privileged operation is
// precisely the event an investigator wants, so authorization failures are
// recorded rather than dropped.
//
// # Retention
//
// Adjudication S24: compliance baseline ≥180 days hot in ClickHouse plus MinIO
// cold backup; the sold product offers 365-day and 18-month tiers.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RetentionBaseline is the compliance floor for hot storage (adjudication S24,
// 等保三级). Cold backup to MinIO extends beyond it.
const RetentionBaseline = 180 * 24 * time.Hour

// Decision is the authorization outcome recorded on an event.
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionDeny  Decision = "deny"
)

// Identity describes who performed an operation (07§6.1).
type Identity struct {
	// Type is "root" for a master account, "ram-user" for a sub-identity, or
	// "role" for an assumed role.
	Type      string
	AccountID int64
	Principal string // e.g. "user/alice"
	// AKID is masked: only the prefix and last two characters are retained
	// (07§6.1 "EU****3F"). A full access key in a log is a credential leak
	// waiting for the first person who exports the audit table.
	AKID       string
	MFAPresent bool
}

// Event is one audit record. The field set matches the unified schema in
// 07§6.1.
type Event struct {
	EventID     string
	EventTime   time.Time
	EventSource string // e.g. euecs.api.euler.emoera.com
	EventName   string // e.g. StopInstance
	SourceIP    string
	UserAgent   string
	Identity    Identity
	Resources   []string // ARNs
	Decision    Decision
	// DecisionNumber references the policy evaluation, without leaking the
	// policy itself to the caller (07§3.4).
	DecisionNumber string
	RequestParams  map[string]string
	ResponseCode   int
	TraceID        string

	// PrevHash links to the preceding event in this tenant's chain.
	PrevHash string
	// ChainHash is this event's hash, computed over its content plus PrevHash.
	ChainHash string
	// Sequence is the tenant-scoped position, so a gap is detectable even if
	// an attacker recomputes hashes for a truncated chain.
	Sequence int64
}

// GenesisHash is the chain root for a tenant's first event.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Errors.
var (
	ErrChainBroken     = errors.New("audit: hash chain broken")
	ErrSequenceGap     = errors.New("audit: sequence gap in chain")
	ErrEmptyChain      = errors.New("audit: no events for tenant")
	ErrMissingIdentity = errors.New("audit: event has no account attribution")
)

// MaskAK reduces an access key to its auditable form: prefix plus the last two
// characters (07§6.1). Enough to correlate incidents to a key; not enough to
// use it.
func MaskAK(ak string) string {
	if ak == "" {
		return ""
	}
	if len(ak) < 4 {
		return "EU****"
	}
	return "EU****" + ak[len(ak)-2:]
}

// canonicalPayload renders the hashed content of an event deterministically.
//
// Map iteration order is randomized in Go, so request parameters are sorted
// before hashing. Without that, the same event would hash differently on each
// run and every chain would appear broken — the verification would be useless
// precisely because it reported constant failure.
//
// Every field is LENGTH-PREFIXED (<len>:<bytes>) and every list carries its
// element count, so the encoding is injective: with bare separators, a value
// containing the separator ("a,b" as one resource vs "a" and "b";
// {"k":"v;x=y"} vs {"k":"v","x":"y"}) produced the same payload — two
// different records with one hash, i.e. an edit the chain cannot see.
func (e Event) canonicalPayload() string {
	var b strings.Builder
	field := func(s string) {
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
	}
	field(e.EventID)
	field(strconv.FormatInt(e.EventTime.UTC().UnixNano(), 10))
	field(e.EventSource)
	field(e.EventName)
	field(e.SourceIP)
	field(strconv.FormatInt(e.Identity.AccountID, 10))
	field(e.Identity.Principal)
	// Identity.Type and MFAPresent are part of the hash: whether the caller was
	// a root account, a RAM user or an assumed role, and whether MFA was
	// present, are exactly the facts an investigator cites. Leaving them out
	// would let them be rewritten without breaking the chain.
	field(e.Identity.Type)
	field(strconv.FormatBool(e.Identity.MFAPresent))
	field(e.Identity.AKID)
	// UserAgent is hashed too — it is the tool fingerprint, and a forger's
	// first instinct is to rewrite it.
	field(e.UserAgent)
	field(string(e.Decision))
	field(e.DecisionNumber)

	res := append([]string(nil), e.Resources...)
	sort.Strings(res)
	field(strconv.Itoa(len(res)))
	for _, r := range res {
		field(r)
	}

	keys := make([]string, 0, len(e.RequestParams))
	for k := range e.RequestParams {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	field(strconv.Itoa(len(keys)))
	for _, k := range keys {
		field(k)
		field(e.RequestParams[k])
	}
	field(strconv.Itoa(e.ResponseCode))
	field(e.TraceID)
	field(strconv.FormatInt(e.Sequence, 10))
	return b.String()
}

// computeHash derives the chain hash from the event content and its
// predecessor.
func computeHash(e Event) string {
	h := sha256.Sum256([]byte(e.canonicalPayload() + "|" + e.PrevHash))
	return hex.EncodeToString(h[:])
}

// SensitiveParams are redacted before an event is recorded. Redaction happens
// at write time, not read time: a secret that reaches the audit store has
// already leaked to everyone with read access to it.
var SensitiveParams = []string{
	"Password", "SecretKey", "SK", "Token", "SecurityToken",
	"PrivateKey", "AccessKeySecret", "Credential",
}

// Redact replaces sensitive parameter values with a marker.
func Redact(params map[string]string) map[string]string {
	if params == nil {
		return nil
	}
	lookup := make(map[string]bool, len(SensitiveParams))
	for _, s := range SensitiveParams {
		lookup[strings.ToLower(s)] = true
	}
	out := make(map[string]string, len(params))
	for k, v := range params {
		if lookup[strings.ToLower(k)] {
			out[k] = "[REDACTED]"
		} else {
			out[k] = v
		}
	}
	return out
}

// Chain appends events to a tenant's audit trail, maintaining the hash links.
//
// One chain per tenant (07§6.2 tenant-dimension hash chain): a global chain
// would let one tenant's write volume delay another's, and would leak
// cross-tenant activity through sequence numbers.
type Chain struct {
	AccountID int64
	head      string
	sequence  int64
}

// NewChain starts a tenant's chain at genesis.
func NewChain(accountID int64) *Chain {
	return &Chain{AccountID: accountID, head: GenesisHash}
}

// ResumeChain continues an existing chain from its persisted head.
func ResumeChain(accountID int64, head string, sequence int64) *Chain {
	if head == "" {
		head = GenesisHash
	}
	return &Chain{AccountID: accountID, head: head, sequence: sequence}
}

// Head returns the current chain head, which is what a checkpoint publishes.
func (c *Chain) Head() string { return c.head }

// Sequence returns the number of events recorded.
func (c *Chain) Sequence() int64 { return c.sequence }

// Append links an event into the chain, returning the completed record.
//
// Sensitive parameters are redacted here rather than by the caller, so a
// forgetful call site cannot leak a credential into permanent storage.
func (c *Chain) Append(e Event) (Event, error) {
	if e.Identity.AccountID == 0 {
		// An event with no tenant cannot be filtered, retained per-policy, or
		// answered for during an investigation.
		return Event{}, ErrMissingIdentity
	}
	if e.Identity.AccountID != c.AccountID {
		return Event{}, fmt.Errorf("audit: event for account %d appended to chain for %d",
			e.Identity.AccountID, c.AccountID)
	}

	c.sequence++
	e.Sequence = c.sequence
	e.PrevHash = c.head
	e.RequestParams = Redact(e.RequestParams)
	e.Identity.AKID = MaskAK(e.Identity.AKID)
	e.ChainHash = computeHash(e)

	c.head = e.ChainHash
	return e, nil
}

// VerifyResult reports the outcome of a chain verification.
type VerifyResult struct {
	AccountID  int64
	EventCount int
	Intact     bool
	// BrokenAt is the sequence number where verification first failed, 0 if
	// intact.
	BrokenAt int64
	// Reason explains the break.
	Reason string
}

// Verify recomputes the chain and reports whether it is intact.
//
// This is the check an auditor runs. It catches three distinct attacks that a
// simple "no deletes" policy cannot:
//   - editing a record (its hash no longer matches its content)
//   - removing a record (the next record's PrevHash points at nothing present)
//   - reordering records (sequence numbers no longer ascend)
func Verify(events []Event) VerifyResult {
	return VerifyFrom(events, GenesisHash, 1)
}

// VerifyFrom verifies a chain segment that starts after a known, trusted
// point: prevHash is the head the first event must link to and startSeq its
// expected sequence. A chain resumed from a checkpoint (ResumeChain) does not
// start at genesis — its first event links to the checkpointed head — so
// verifying it from genesis reports every resumed chain as broken. Verify is
// VerifyFrom(events, GenesisHash, 1).
func VerifyFrom(events []Event, prevHash string, startSeq int64) VerifyResult {
	if len(events) == 0 {
		return VerifyResult{Intact: false, Reason: ErrEmptyChain.Error()}
	}
	if prevHash == "" {
		prevHash = GenesisHash
	}
	if startSeq < 1 {
		startSeq = 1
	}

	accountID := events[0].Identity.AccountID
	prev := prevHash
	expectedSeq := startSeq

	for _, e := range events {
		if e.Sequence != expectedSeq {
			return VerifyResult{
				AccountID: accountID, EventCount: len(events), Intact: false,
				BrokenAt: e.Sequence,
				Reason: fmt.Sprintf("%v: expected sequence %d, found %d",
					ErrSequenceGap, expectedSeq, e.Sequence),
			}
		}
		if e.PrevHash != prev {
			return VerifyResult{
				AccountID: accountID, EventCount: len(events), Intact: false,
				BrokenAt: e.Sequence,
				Reason:   fmt.Sprintf("%v: predecessor link mismatch at sequence %d", ErrChainBroken, e.Sequence),
			}
		}
		if got := computeHash(e); got != e.ChainHash {
			return VerifyResult{
				AccountID: accountID, EventCount: len(events), Intact: false,
				BrokenAt: e.Sequence,
				Reason:   fmt.Sprintf("%v: content hash mismatch at sequence %d (record was edited)", ErrChainBroken, e.Sequence),
			}
		}
		prev = e.ChainHash
		expectedSeq++
	}

	return VerifyResult{AccountID: accountID, EventCount: len(events), Intact: true}
}

// eventJSON is the wire form of an Event (07§6.1 schema).
type eventJSON struct {
	EventID        string            `json:"event_id"`
	EventTime      string            `json:"event_time"`
	EventSource    string            `json:"event_source"`
	EventName      string            `json:"event_name"`
	SourceIP       string            `json:"source_ip"`
	UserAgent      string            `json:"user_agent,omitempty"`
	Identity       eventIdentityJSON `json:"identity"`
	Resource       []string          `json:"resource"`
	Decision       string            `json:"decision"`
	DecisionNumber string            `json:"decision_number,omitempty"`
	RequestParams  map[string]string `json:"request_params,omitempty"`
	ResponseCode   int               `json:"response_code"`
	TraceID        string            `json:"trace_id"`
	ChainHash      string            `json:"chain_hash"`
	PrevHash       string            `json:"prev_hash"`
	Sequence       int64             `json:"sequence"`
}

// eventIdentityJSON is the wire form of an Identity. account_id is a string
// (64-bit ids do not survive JavaScript number parsing).
type eventIdentityJSON struct {
	Type       string `json:"type"`
	AccountID  string `json:"account_id"`
	Principal  string `json:"principal"`
	AKID       string `json:"ak_id"`
	MFAPresent bool   `json:"mfa_present"`
}

// MarshalJSON renders the event for the Kafka envelope
// (cloud.sys.audit.action, partitioned by account_id).
//
// event_time carries NANOSECOND precision (RFC3339Nano): the chain hash covers
// EventTime.UnixNano, so a second-precision export could never be re-verified
// — every exported record would read as edited.
func (e Event) MarshalJSON() ([]byte, error) {
	return json.Marshal(eventJSON{
		EventID:     e.EventID,
		EventTime:   e.EventTime.UTC().Format(time.RFC3339Nano),
		EventSource: e.EventSource,
		EventName:   e.EventName,
		SourceIP:    e.SourceIP,
		UserAgent:   e.UserAgent,
		Identity: eventIdentityJSON{
			Type:       e.Identity.Type,
			AccountID:  strconv.FormatInt(e.Identity.AccountID, 10),
			Principal:  e.Identity.Principal,
			AKID:       e.Identity.AKID,
			MFAPresent: e.Identity.MFAPresent,
		},
		Resource:       e.Resources,
		Decision:       string(e.Decision),
		DecisionNumber: e.DecisionNumber,
		RequestParams:  e.RequestParams,
		ResponseCode:   e.ResponseCode,
		TraceID:        e.TraceID,
		ChainHash:      e.ChainHash,
		PrevHash:       e.PrevHash,
		Sequence:       e.Sequence,
	})
}

// UnmarshalJSON reads the MarshalJSON form back, so an exported trail can be
// re-verified with Verify/VerifyFrom.
func (e *Event) UnmarshalJSON(data []byte) error {
	var w eventJSON
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	var t time.Time
	if w.EventTime != "" {
		var err error
		if t, err = time.Parse(time.RFC3339Nano, w.EventTime); err != nil {
			return fmt.Errorf("audit: event_time: %w", err)
		}
	}
	var acct int64
	if w.Identity.AccountID != "" {
		var err error
		if acct, err = strconv.ParseInt(w.Identity.AccountID, 10, 64); err != nil {
			return fmt.Errorf("audit: identity.account_id: %w", err)
		}
	}
	*e = Event{
		EventID:     w.EventID,
		EventTime:   t,
		EventSource: w.EventSource,
		EventName:   w.EventName,
		SourceIP:    w.SourceIP,
		UserAgent:   w.UserAgent,
		Identity: Identity{
			Type:       w.Identity.Type,
			AccountID:  acct,
			Principal:  w.Identity.Principal,
			AKID:       w.Identity.AKID,
			MFAPresent: w.Identity.MFAPresent,
		},
		Resources:      w.Resource,
		Decision:       Decision(w.Decision),
		DecisionNumber: w.DecisionNumber,
		RequestParams:  w.RequestParams,
		ResponseCode:   w.ResponseCode,
		TraceID:        w.TraceID,
		ChainHash:      w.ChainHash,
		PrevHash:       w.PrevHash,
		Sequence:       w.Sequence,
	}
	return nil
}

// EventID builds the platform event identifier: ev-YYYYMMDD-NNNNNN (07§6.1).
func EventID(t time.Time, seq int64) string {
	return fmt.Sprintf("ev-%s-%06d", t.UTC().Format("20060102"), seq)
}

// RetentionDeadline returns when an event may leave hot storage.
func RetentionDeadline(eventTime time.Time, tier time.Duration) time.Time {
	if tier < RetentionBaseline {
		// Paid tiers extend retention; nothing may shorten it below the
		// compliance floor.
		tier = RetentionBaseline
	}
	return eventTime.Add(tier)
}
