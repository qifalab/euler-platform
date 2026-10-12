package audit

import (
	"encoding/json"
	"testing"
	"time"
)

// The chain hash must cover every field an investigator cites. UserAgent,
// Identity.Type and Identity.MFAPresent used to sit outside canonicalPayload,
// so they could be rewritten (hiding an MFA-less login or a tool fingerprint)
// without breaking the chain.
func TestTamperingIdentityFieldsBreaksTheChain(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	c := NewChain(100123)
	rec, err := c.Append(Event{
		EventID:     EventID(now, 1),
		EventTime:   now,
		EventSource: "euecs.api.euler.emoera.com",
		EventName:   "StopInstance",
		SourceIP:    "203.0.113.9",
		UserAgent:   "eu-cli/1.0",
		Identity: Identity{
			Type: "root", AccountID: 100123, Principal: "user/alice",
			AKID: "EU****3F", MFAPresent: true,
		},
		Decision:     DecisionAllow,
		ResponseCode: 200,
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if res := Verify([]Event{rec}); !res.Intact {
		t.Fatalf("fresh chain must verify: %s", res.Reason)
	}

	mutations := map[string]func(Event) Event{
		"mfa_present":   func(e Event) Event { e.Identity.MFAPresent = false; return e },
		"identity_type": func(e Event) Event { e.Identity.Type = "ram-user"; return e },
		"user_agent":    func(e Event) Event { e.UserAgent = "curl/8"; return e },
	}
	for name, mutate := range mutations {
		if res := Verify([]Event{mutate(rec)}); res.Intact {
			t.Fatalf("rewriting %s must break the chain", name)
		}
	}
}

func sampleEvent(acct int64, at time.Time, name string) Event {
	return Event{
		EventID: EventID(at, 1), EventTime: at, EventSource: "euecs.api.euler.emoera.com",
		EventName: name, SourceIP: "203.0.113.9", UserAgent: "eu-cli/1.0",
		Identity:      Identity{Type: "ram-user", AccountID: acct, Principal: "user/alice", AKID: "AKIDEXAMPLE3F", MFAPresent: true},
		Resources:     []string{"eurn:euecs:cn-north-1:100123:instance/i-1"},
		Decision:      DecisionAllow,
		RequestParams: map[string]string{"InstanceId": "i-1", "Password": "hunter2"},
		ResponseCode:  200, TraceID: "trace-1",
	}
}

// E17: the export rounded event_time to seconds while the hash covers
// UnixNano, so an exported trail could never be re-verified.
func TestExportedTrailRoundTripsAndVerifies(t *testing.T) {
	c := NewChain(100123)
	at := time.Date(2026, 9, 1, 12, 0, 0, 123456789, time.UTC)
	var trail []Event
	for i, name := range []string{"StopInstance", "StartInstance", "DeleteInstance"} {
		rec, err := c.Append(sampleEvent(100123, at.Add(time.Duration(i)*time.Millisecond), name))
		if err != nil {
			t.Fatal(err)
		}
		trail = append(trail, rec)
	}
	blob, err := json.Marshal(trail)
	if err != nil {
		t.Fatal(err)
	}
	var back []Event
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if res := Verify(back); !res.Intact {
		t.Fatalf("exported trail no longer verifies: %s", res.Reason)
	}
	if !back[0].EventTime.Equal(trail[0].EventTime) || back[0].Identity.AccountID != 100123 {
		t.Fatalf("round trip lost data: %+v", back[0])
	}
}

// E18: Verify demanded genesis/sequence 1, so a chain resumed from a
// checkpoint read as broken. VerifyFrom verifies from the trusted base.
func TestVerifyFromResumedChain(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	c := NewChain(100123)
	var before []Event
	for i := 0; i < 3; i++ {
		rec, err := c.Append(sampleEvent(100123, at.Add(time.Duration(i)*time.Second), "A"))
		if err != nil {
			t.Fatal(err)
		}
		before = append(before, rec)
	}
	head, seq := c.Head(), c.Sequence()

	resumed := ResumeChain(100123, head, seq)
	var after []Event
	for i := 0; i < 2; i++ {
		rec, err := resumed.Append(sampleEvent(100123, at.Add(time.Minute+time.Duration(i)*time.Second), "B"))
		if err != nil {
			t.Fatal(err)
		}
		after = append(after, rec)
	}
	if res := VerifyFrom(after, head, seq+1); !res.Intact {
		t.Fatalf("resumed segment must verify from its checkpoint: %s", res.Reason)
	}
	if res := Verify(append(append([]Event(nil), before...), after...)); !res.Intact {
		t.Fatalf("whole chain must verify from genesis: %s", res.Reason)
	}
	if res := VerifyFrom(after, GenesisHash, seq+1); res.Intact {
		t.Fatal("a segment must not verify against the wrong base")
	}
	if res := VerifyFrom(after[1:], head, seq+1); res.Intact {
		t.Fatal("a segment missing its first event must not verify")
	}
}

// E19: separator-joined fields let two different records share one payload
// (and hence one hash).
func TestCanonicalPayloadIsInjective(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	base := sampleEvent(100123, at, "StopInstance")
	base.RequestParams = nil
	pairs := map[string][2]func(Event) Event{
		"resources": {
			func(e Event) Event { e.Resources = []string{"a,b"}; return e },
			func(e Event) Event { e.Resources = []string{"a", "b"}; return e },
		},
		"params": {
			func(e Event) Event { e.RequestParams = map[string]string{"k": "v;x=y"}; return e },
			func(e Event) Event { e.RequestParams = map[string]string{"k": "v", "x": "y"}; return e },
		},
		"fields": {
			func(e Event) Event { e.EventSource, e.EventName = "a|b", "c"; return e },
			func(e Event) Event { e.EventSource, e.EventName = "a", "b|c"; return e },
		},
	}
	for name, p := range pairs {
		x, y := p[0](base), p[1](base)
		if x.canonicalPayload() == y.canonicalPayload() {
			t.Errorf("%s: two different records share a canonical payload", name)
		}
		hx, _ := NewChain(100123).Append(x)
		hy, _ := NewChain(100123).Append(y)
		if hx.ChainHash == hy.ChainHash {
			t.Errorf("%s: two different records share a chain hash", name)
		}
	}
}
