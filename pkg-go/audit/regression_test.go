package audit

import (
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
