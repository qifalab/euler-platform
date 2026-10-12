package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qifalab/euler-platform/audit"
	"github.com/qifalab/euler-platform/storage"
)

// memCheckpointRepo is an in-memory checkpointRepo (the persistent mode's
// shape, without MySQL).
type memCheckpointRepo struct{ cps map[int64]checkpoint }

func (m *memCheckpointRepo) Upsert(_ context.Context, cp checkpoint) error {
	if cur, ok := m.cps[cp.AccountID]; !ok || cp.Sequence > cur.Sequence {
		m.cps[cp.AccountID] = cp
	}
	return nil
}

func (m *memCheckpointRepo) LoadAll(context.Context) ([]checkpoint, error) {
	out := make([]checkpoint, 0, len(m.cps))
	for _, cp := range m.cps {
		out = append(out, cp)
	}
	return out, nil
}

func appendEvent(t *testing.T, h http.Handler, acct string, hdr map[string]string) map[string]any {
	t.Helper()
	body := `{"event_source":"euecs.api","event_name":"StopInstance","decision":"allow"}`
	req := httptest.NewRequest(http.MethodPost, "/internal/audit", strings.NewReader(body))
	req.Header.Set(accountHeader, acct)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("append: %d %s", rr.Code, rr.Body.String())
	}
	var ev map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func verifyTrail(t *testing.T, s *auditStore, acct string) audit.VerifyResult {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/internal/audit/"+acct+"/verify", nil)
	req.SetPathValue("account", acct)
	req.Header.Set(accountHeader, acct)
	rr := httptest.NewRecorder()
	s.handleVerify(rr, req)
	var res audit.VerifyResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// E20: without EULER_DB_DSN, newCheckpointRepo returns (nil, nil) and the
// store called LoadAll on the nil repo — svc-audit panicked at startup in its
// default (in-memory) mode. Go through main()'s constructor.
func TestOpenAuditStoreWithoutDSN(t *testing.T) {
	t.Setenv(storage.DSNEnv, "")
	s, persistent, err := openAuditStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if persistent {
		t.Fatal("no DSN must mean the in-memory mode")
	}
	appendEvent(t, http.HandlerFunc(s.handleAppend), "100123", nil)
	if res := verifyTrail(t, s, "100123"); !res.Intact {
		t.Fatalf("fresh trail: %s", res.Reason)
	}
}

// E18: after a restart the store resumes each chain from its checkpoint, but
// /verify checked the hot trail from genesis — every resumed tenant read as
// broken.
func TestVerifyAfterResumeFromCheckpoint(t *testing.T) {
	ctx := context.Background()
	repo := &memCheckpointRepo{cps: map[int64]checkpoint{}}
	first, err := newAuditStoreWith(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		appendEvent(t, http.HandlerFunc(first.handleAppend), "100123", nil)
	}

	restarted, err := newAuditStoreWith(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	appendEvent(t, http.HandlerFunc(restarted.handleAppend), "100123", nil)
	appendEvent(t, http.HandlerFunc(restarted.handleAppend), "100123", nil)
	res := verifyTrail(t, restarted, "100123")
	if !res.Intact || res.EventCount != 2 {
		t.Fatalf("resumed trail must verify from its checkpoint: %+v", res)
	}

	// Tampering after the resume is still caught.
	restarted.mu.Lock()
	restarted.events[100123][1].EventName = "StartInstance"
	restarted.mu.Unlock()
	if res := verifyTrail(t, restarted, "100123"); res.Intact {
		t.Fatal("an edited event on a resumed chain must not verify")
	}
}

// E21: TraceID was read from an inbound X-Euler-TraceId header nothing sends;
// it must be the request id the middleware assigned.
func TestAuditEventCarriesRequestID(t *testing.T) {
	s := newAuditStore()
	h := withMiddleware(http.HandlerFunc(s.handleAppend))
	ev := appendEvent(t, h, "100123", map[string]string{"X-Request-Id": "req-7f3a"})
	if ev["trace_id"] != "req-7f3a" {
		t.Fatalf("trace_id = %v, want the request id req-7f3a", ev["trace_id"])
	}
	// No inbound id: the middleware mints one, and the event carries it.
	ev = appendEvent(t, h, "100123", nil)
	if id, _ := ev["trace_id"].(string); id == "" {
		t.Fatal("trace_id empty although the middleware assigned a request id")
	}
}
