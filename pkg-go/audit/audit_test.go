package audit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var base = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

const acct = int64(100123)

func event(name string, seq int, decision Decision) Event {
	return Event{
		EventID:     EventID(base, int64(seq)),
		EventTime:   base.Add(time.Duration(seq) * time.Minute),
		EventSource: "scecs.api.starcloud.cn",
		EventName:   name,
		SourceIP:    "203.0.113.9",
		UserAgent:   "sc-sdk-go/1.0.0",
		Identity: Identity{
			Type:       "ram-user",
			AccountID:  acct,
			Principal:  "user/alice",
			AKID:       "SCAAAAAAAAAAAAAAAAAAAAAAAAAAAA3F",
			MFAPresent: true,
		},
		Resources:      []string{"sc:ecs:cn-north-1:100123:instance/scecs-cn-north-1-01-a1b2c3d4"},
		Decision:       decision,
		DecisionNumber: "A-p0-s1",
		RequestParams:  map[string]string{"InstanceId": "scecs-cn-north-1-01-a1b2c3d4"},
		ResponseCode:   200,
		TraceID:        "5b8e1234c2",
	}
}

func buildChain(t *testing.T, n int) (*Chain, []Event) {
	t.Helper()
	c := NewChain(acct)
	var out []Event
	for i := 1; i <= n; i++ {
		e, err := c.Append(event("StopInstance", i, DecisionAllow))
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		out = append(out, e)
	}
	return c, out
}

// --- Chain construction ---

func TestChainLinksEvents(t *testing.T) {
	c, events := buildChain(t, 5)

	if events[0].PrevHash != GenesisHash {
		t.Fatalf("first event PrevHash = %s, want genesis", events[0].PrevHash)
	}
	for i := 1; i < len(events); i++ {
		if events[i].PrevHash != events[i-1].ChainHash {
			t.Fatalf("event %d does not link to its predecessor", i)
		}
		if events[i].Sequence != int64(i+1) {
			t.Fatalf("event %d sequence = %d", i, events[i].Sequence)
		}
	}
	if c.Head() != events[len(events)-1].ChainHash {
		t.Fatal("chain head should be the last event's hash")
	}
	if c.Sequence() != 5 {
		t.Fatalf("sequence = %d, want 5", c.Sequence())
	}
}

func TestVerifyIntactChain(t *testing.T) {
	_, events := buildChain(t, 10)
	res := Verify(events)
	if !res.Intact {
		t.Fatalf("a freshly built chain must verify: %s", res.Reason)
	}
	if res.EventCount != 10 {
		t.Fatalf("event count = %d, want 10", res.EventCount)
	}
}

// --- The three attacks a hash chain catches ---

// TestDetectsEditedRecord: changing a field breaks the content hash. This is
// the attack that "no deletes" permissions cannot stop, because whoever holds
// the credentials can grant themselves the permission.
func TestDetectsEditedRecord(t *testing.T) {
	_, events := buildChain(t, 5)

	// Someone rewrites a denial into an approval.
	events[2].Decision = DecisionAllow
	events[2].EventName = "DeleteInstance"

	res := Verify(events)
	if res.Intact {
		t.Fatal("an edited record must break verification")
	}
	if res.BrokenAt != 3 {
		t.Fatalf("BrokenAt = %d, want 3", res.BrokenAt)
	}
	if !strings.Contains(res.Reason, "edited") {
		t.Fatalf("reason should identify an edit: %s", res.Reason)
	}
}

// TestDetectsRemovedRecord: deleting an entry leaves the next one pointing at
// a hash that is no longer present.
func TestDetectsRemovedRecord(t *testing.T) {
	_, events := buildChain(t, 5)

	// Remove the third event.
	tampered := append(append([]Event{}, events[:2]...), events[3:]...)

	res := Verify(tampered)
	if res.Intact {
		t.Fatal("a removed record must break verification")
	}
	if res.BrokenAt == 0 {
		t.Fatal("verification should report where the chain breaks")
	}
}

// TestDetectsReorderedRecords: sequence numbers must ascend, so shuffling is
// caught even if the hashes were somehow consistent.
func TestDetectsReorderedRecords(t *testing.T) {
	_, events := buildChain(t, 5)
	events[1], events[3] = events[3], events[1]

	res := Verify(events)
	if res.Intact {
		t.Fatal("reordered records must break verification")
	}
}

// TestTruncationIsDetected: an attacker who deletes the tail and recomputes
// nothing still leaves a chain shorter than the published head.
func TestTruncationIsDetected(t *testing.T) {
	c, events := buildChain(t, 10)
	publishedHead := c.Head()

	truncated := events[:6]
	res := Verify(truncated)
	// The remaining prefix is internally consistent...
	if !res.Intact {
		t.Fatalf("a clean prefix verifies on its own: %s", res.Reason)
	}
	// ...but it no longer reaches the published head, which is what a
	// checkpoint exists to reveal.
	if truncated[len(truncated)-1].ChainHash == publishedHead {
		t.Fatal("truncation should not reproduce the published head")
	}
}

func TestEmptyChainIsNotIntact(t *testing.T) {
	res := Verify(nil)
	if res.Intact {
		t.Fatal("an empty chain cannot be reported as verified")
	}
}

// --- Determinism ---

// TestHashIsDeterministicAcrossRuns guards against Go's randomized map
// iteration making the same event hash differently each time — which would
// make every chain report as broken and render verification worthless.
func TestHashIsDeterministicAcrossRuns(t *testing.T) {
	e := event("StopInstance", 1, DecisionAllow)
	e.RequestParams = map[string]string{
		"InstanceId": "i-1", "Force": "true", "Region": "cn-north-1",
		"Zone": "a", "Tag": "x", "Extra": "y",
	}
	e.Resources = []string{"arn:b", "arn:a", "arn:c"}

	c1 := NewChain(acct)
	first, err := c1.Append(e)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		c := NewChain(acct)
		again, err := c.Append(e)
		if err != nil {
			t.Fatal(err)
		}
		if again.ChainHash != first.ChainHash {
			t.Fatalf("hash varies across runs: %s vs %s", again.ChainHash, first.ChainHash)
		}
	}
}

// --- Redaction ---

// TestSecretsRedactedAtWriteTime: a credential that reaches the audit store
// has already leaked to everyone with read access to it.
func TestSecretsRedactedAtWriteTime(t *testing.T) {
	c := NewChain(acct)
	e := event("CreateAccessKey", 1, DecisionAllow)
	e.RequestParams = map[string]string{
		"UserName":  "alice",
		"Password":  "hunter2",
		"SecretKey": "very-secret-material",
		"Token":     "sts-token-abc",
	}

	recorded, err := c.Append(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"Password", "SecretKey", "Token"} {
		if recorded.RequestParams[key] != "[REDACTED]" {
			t.Errorf("%s was not redacted: %q", key, recorded.RequestParams[key])
		}
	}
	if recorded.RequestParams["UserName"] != "alice" {
		t.Error("non-sensitive parameters must survive")
	}

	// Serialized form must not contain the secret either.
	blob, err := json.Marshal(recorded)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"hunter2", "very-secret-material", "sts-token-abc"} {
		if strings.Contains(string(blob), secret) {
			t.Fatalf("secret %q leaked into the serialized event", secret)
		}
	}
}

func TestAccessKeyMasked(t *testing.T) {
	c := NewChain(acct)
	e := event("StopInstance", 1, DecisionAllow)
	e.Identity.AKID = "SCAAAAAAAAAAAAAAAAAAAAAAAAAAAA3F"

	recorded, _ := c.Append(e)
	if recorded.Identity.AKID != "SC****3F" {
		t.Fatalf("AK = %s, want SC****3F", recorded.Identity.AKID)
	}
	// Enough to correlate an incident to a key; not enough to use it.
	if strings.Contains(recorded.Identity.AKID, "AAAA") {
		t.Fatal("masked AK still exposes key material")
	}
}

func TestMaskAKEdgeCases(t *testing.T) {
	if MaskAK("") != "" {
		t.Error("empty AK should stay empty")
	}
	if got := MaskAK("ab"); got != "SC****" {
		t.Errorf("short AK = %s, want SC****", got)
	}
}

// --- Denied operations are audited ---

// TestDeniedOperationsAreRecorded: a rejected privileged operation is exactly
// the event an investigator wants.
func TestDeniedOperationsAreRecorded(t *testing.T) {
	c := NewChain(acct)
	e := event("DeleteInstance", 1, DecisionDeny)
	e.ResponseCode = 403
	e.DecisionNumber = "D-p2-s0"

	recorded, err := c.Append(e)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.Decision != DecisionDeny {
		t.Fatal("denials must be recorded, not dropped")
	}
	if recorded.DecisionNumber == "" {
		t.Fatal("a denial must carry its decision reference for investigation")
	}
	if res := Verify([]Event{recorded}); !res.Intact {
		t.Fatal("a denial event must be chained like any other")
	}
}

// --- Tenant attribution ---

func TestEventWithoutAccountRejected(t *testing.T) {
	// An event with no tenant cannot be filtered, retained per policy, or
	// answered for during an investigation.
	c := NewChain(acct)
	e := event("StopInstance", 1, DecisionAllow)
	e.Identity.AccountID = 0
	if _, err := c.Append(e); err == nil {
		t.Fatal("an event with no account attribution must be rejected")
	}
}

func TestCrossTenantAppendRejected(t *testing.T) {
	// One chain per tenant: mixing tenants would leak activity through
	// sequence numbers and let one tenant's volume affect another's chain.
	c := NewChain(acct)
	e := event("StopInstance", 1, DecisionAllow)
	e.Identity.AccountID = 999999
	if _, err := c.Append(e); err == nil {
		t.Fatal("appending another tenant's event must be rejected")
	}
}

// --- Chain resumption ---

func TestResumeChainContinuesLinks(t *testing.T) {
	// The writer restarts; the chain must continue from the persisted head
	// rather than starting a second genesis.
	c1, first := buildChain(t, 3)

	c2 := ResumeChain(acct, c1.Head(), c1.Sequence())
	e4, err := c2.Append(event("StartInstance", 4, DecisionAllow))
	if err != nil {
		t.Fatal(err)
	}
	if e4.PrevHash != first[2].ChainHash {
		t.Fatal("resumed chain must link to the previous head")
	}
	if e4.Sequence != 4 {
		t.Fatalf("sequence = %d, want 4", e4.Sequence)
	}

	if res := Verify(append(first, e4)); !res.Intact {
		t.Fatalf("resumed chain must verify: %s", res.Reason)
	}
}

func TestResumeFromEmptyHeadStartsAtGenesis(t *testing.T) {
	c := ResumeChain(acct, "", 0)
	e, err := c.Append(event("StopInstance", 1, DecisionAllow))
	if err != nil {
		t.Fatal(err)
	}
	if e.PrevHash != GenesisHash {
		t.Fatal("an empty head should mean genesis")
	}
}

// --- Retention ---

func TestRetentionFloorCannotBeShortened(t *testing.T) {
	// Paid tiers extend retention; nothing may take it below the compliance
	// floor (adjudication S24).
	shortened := RetentionDeadline(base, 30*24*time.Hour)
	if !shortened.Equal(base.Add(RetentionBaseline)) {
		t.Fatalf("a 30-day tier must be clamped to the 180-day floor, got %v", shortened.Sub(base))
	}

	paid := RetentionDeadline(base, 365*24*time.Hour)
	if !paid.Equal(base.Add(365 * 24 * time.Hour)) {
		t.Fatal("a 365-day tier should extend beyond the floor")
	}
}

// --- Serialization ---

func TestEventIDFormat(t *testing.T) {
	if got := EventID(base, 123); got != "ev-20260808-000123" {
		t.Fatalf("EventID = %s, want ev-20260808-000123", got)
	}
}

func TestMarshalMatchesSchema(t *testing.T) {
	c := NewChain(acct)
	recorded, _ := c.Append(event("StopInstance", 1, DecisionAllow))

	blob, err := json.Marshal(recorded)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatal(err)
	}
	// Fields the 07§6.1 schema requires.
	for _, key := range []string{
		"event_id", "event_time", "event_source", "event_name", "source_ip",
		"identity", "resource", "decision", "response_code", "trace_id", "chain_hash",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("serialized event missing %q", key)
		}
	}
	ident, ok := got["identity"].(map[string]any)
	if !ok {
		t.Fatal("identity should be an object")
	}
	if ident["account_id"] != "100123" {
		t.Errorf("account_id = %v, want the string 100123", ident["account_id"])
	}
}
