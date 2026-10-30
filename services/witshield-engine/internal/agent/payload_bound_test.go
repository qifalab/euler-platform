// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/qifalab/euler-platform/services/witshield-engine/internal/domain"
)

func TestOversizedEventPayloadIsBoundedAndNeverActionEligible(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"source": "procfs", "trust": "verified", "automaticActionEligible": true,
		"pid": 91, "uid": 0, "startTime": 4242, "executable": "/tmp/" + strings.Repeat("<", 1000), "reason": "transient path",
		"tags": []string{strings.Repeat("构", 600)},
	})
	event := fitSecurityEventForController(domain.SecurityEvent{ID: "evt_big", Type: "suspicious_privileged_process_started", OccurredAt: time.Now(), Payload: payload, SourceIP: strings.Repeat(":", 70)})
	if len(event.Payload) > 4*1024 || !json.Valid(event.Payload) || !utf8.Valid(event.Payload) || event.SourceIP != "" {
		t.Fatalf("event still outside the Controller contract: payload=%d sourceIp=%q", len(event.Payload), event.SourceIP)
	}
	var fields map[string]any
	if err := json.Unmarshal(event.Payload, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["automaticActionEligible"] != false || fields["payloadTruncated"] != true || fields["trust"] != "verified" || fields["pid"] != float64(91) || fields["startTime"] != float64(4242) {
		t.Fatalf("bounded payload lost provenance or kept action eligibility: %v", fields)
	}
	// A replay of the same event before the watcher commits must produce the
	// same bytes, so Controller deduplication by event ID stays consistent.
	again := fitSecurityEventForController(domain.SecurityEvent{ID: "evt_big", Type: "suspicious_privileged_process_started", OccurredAt: event.OccurredAt, Payload: payload})
	if !bytes.Equal(again.Payload, event.Payload) {
		t.Fatal("bounded payload is not deterministic")
	}
	small := json.RawMessage(`{"source":"journald","trust":"verified","automaticActionEligible":true}`)
	if kept := fitSecurityEventForController(domain.SecurityEvent{ID: "evt_small", Payload: small, SourceIP: "203.0.113.7"}); !bytes.Equal(kept.Payload, small) || kept.SourceIP != "203.0.113.7" {
		t.Fatalf("in-contract event was modified: %+v", kept)
	}
}

func TestTruncateUTF8KeepsRuneBoundary(t *testing.T) {
	if got := truncateUTF8("ab构建", 4); got != "ab" {
		t.Fatalf("got %q", got)
	}
	if got := truncateUTF8("abc", 5); got != "abc" {
		t.Fatalf("got %q", got)
	}
}
