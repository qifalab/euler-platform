package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/starcloud/sc-platform/audit"
)

// Concurrent appends must be safe and must mint distinct, gap-free event ids.
// The sequence read, the event construction and the append all touch the
// per-account chain, and chainFor writes a map — reading it outside the lock
// is a fatal concurrent-map-write, not a recoverable panic.
func TestConcurrentAppendIsSafeAndSequenced(t *testing.T) {
	s := newAuditStore()
	const writers = 8
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := `{"event_source":"scecs.api","event_name":"StopInstance","decision":"allow"}`
			req := httptest.NewRequest(http.MethodPost, "/internal/audit", strings.NewReader(body))
			req.Header.Set(accountHeader, "100123")
			rr := httptest.NewRecorder()
			s.handleAppend(rr, req)
			if rr.Code != http.StatusCreated {
				t.Errorf("append: %d %s", rr.Code, rr.Body.String())
			}
		}()
	}
	wg.Wait()

	s.mu.Lock()
	events := append([]audit.Event(nil), s.events[100123]...)
	s.mu.Unlock()
	if len(events) != writers {
		t.Fatalf("recorded %d events, want %d", len(events), writers)
	}
	seen := make(map[string]bool, len(events))
	for i, e := range events {
		if e.Sequence != int64(i+1) {
			t.Fatalf("event %d has sequence %d, want %d", i, e.Sequence, i+1)
		}
		if seen[e.EventID] {
			t.Fatalf("duplicate EventID %s", e.EventID)
		}
		seen[e.EventID] = true
	}
	if res := audit.Verify(events); !res.Intact {
		t.Fatalf("chain must stay intact under concurrency: %s", res.Reason)
	}
}

// The list and verify endpoints serve only the caller's own trail: the path
// names the trail, the gateway header names the caller, and they must agree.
func TestTrailReadsAreCallerScoped(t *testing.T) {
	s := newAuditStore()
	for _, path := range []string{"/internal/audit/100123/events", "/internal/audit/100123/verify"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.SetPathValue("account", "100123")
		req.Header.Set(accountHeader, "100999") // a different tenant
		rr := httptest.NewRecorder()
		if strings.HasSuffix(path, "/verify") {
			s.handleVerify(rr, req)
		} else {
			s.handleList(rr, req)
		}
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s for a foreign account: %d, want 403", path, rr.Code)
		}
	}
}
