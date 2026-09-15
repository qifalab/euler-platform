package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Concurrent replies must not lose each other. The store used to hand out the
// stored *Ticket, so handlers appended to a shared slice outside any lock:
// two concurrent replies could drop one, and a list read could see a torn
// ticket. The store now mutates under its own lock and returns copies.
func TestConcurrentRepliesDoNotLoseMessages(t *testing.T) {
	a := newApp(newMemoryStore())

	create := httptest.NewRequest(http.MethodPost, "/internal/tickets",
		strings.NewReader(`{"category":"billing","priority":"NORMAL","message":"hello"}`))
	create.Header.Set("X-Sc-Account-Id", "100123")
	createResp := httptest.NewRecorder()
	a.handleCreate(createResp, create)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", createResp.Code, createResp.Body.String())
	}
	var created Ticket
	if err := json.Unmarshal(createResp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created ticket: %v", err)
	}

	const replies = 8
	var wg sync.WaitGroup
	for i := 0; i < replies; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"message":"reply-%d"}`, i)
			req := httptest.NewRequest(http.MethodPost,
				"/internal/tickets/"+created.TicketID+"/reply", strings.NewReader(body))
			req.Header.Set("X-Sc-Account-Id", "100123")
			rr := httptest.NewRecorder()
			a.handleReply(rr, req)
			if rr.Code != http.StatusOK {
				t.Errorf("reply %d: %d %s", i, rr.Code, rr.Body.String())
			}
		}(i)
	}
	wg.Wait()

	stored, err := a.store.Get(100123, created.TicketID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := len(stored.Messages); got != replies+1 {
		t.Fatalf("ticket holds %d messages, want %d (replies were lost)", got, replies+1)
	}
}
