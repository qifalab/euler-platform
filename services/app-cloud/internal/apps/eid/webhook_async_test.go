package eid

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

const testWecomURL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=test-only"

// botEndpoint stands in for the WeCom robot API. Until release is closed every
// call hangs like an unreachable endpoint; cancelling the call still ends it.
type botEndpoint struct {
	calls   atomic.Int32
	release chan struct{}
}

func newBotEndpoint() *botEndpoint { return &botEndpoint{release: make(chan struct{})} }

func (b *botEndpoint) client() *http.Client {
	return &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		b.calls.Add(1)
		select {
		case <-b.release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"errcode":0}`)), Request: r}, nil
	})}
}

func eventually(t *testing.T, failure string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !done(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(failure)
		}
	}
}

// waitWebhookState waits until the bot notification for target reaches want
// and returns its ID.
func waitWebhookState(t *testing.T, f *testFixture, target, want string) string {
	t.Helper()
	var id, state string
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		err := f.rt.DB.QueryRow("SELECT id,state FROM eid_notifications WHERE target_id=? AND kind LIKE 'webhook_%'", target).Scan(&id, &state)
		if err == nil && state == want {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("bot notification for %+q is %+q (%v), want %+q", target, state, err, want)
		}
	}
}

// A write must not wait for bot delivery. With an unreachable endpoint every
// EID write used to take 10-25 s before its response, past the console's 20 s
// timeout, so committed writes were reported as failed and retries got 409.
// The delivery still completes after the browser has gone away.
func TestWriteDoesNotWaitForBotEndpoint(t *testing.T) {
	f := fixture(t)
	bot := newBotEndpoint()
	f.m.client = bot.client()
	cfg := defaults()
	cfg.WecomURL = testWecomURL
	saveCfg(t, f, cfg)
	alice := actor("alice", "read", "write")
	body, err := json.Marshal(verificationInput{RealName: "Alice", StudentID: "42", IdentityTitle: "Core", IdentityType: "core"})
	if err != nil {
		t.Fatal(err)
	}
	browser, leave := context.WithCancel(context.Background())
	defer leave()
	written := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest("POST", "/verifications", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(appkit.WithScope(browser, alice))
		w := httptest.NewRecorder()
		f.m.Handler().ServeHTTP(w, r)
		written <- w
	}()
	var w *httptest.ResponseRecorder
	select {
	case w = <-written:
	case <-time.After(2 * time.Second):
		close(bot.release) // let the request's own delivery finish so it returns
		<-written
		t.Fatal("the write response waited for the bot endpoint")
	}
	status(t, w, 201)
	v := result[Verification](t, w)
	leave() // the browser goes away once it has the response
	// The row committed with the write is in flight and is recorded as sent
	// once the endpoint answers.
	eventually(t, "the bot endpoint was never called", func() bool { return bot.calls.Load() == 1 })
	var state string
	if err := f.rt.DB.QueryRow("SELECT state FROM eid_notifications WHERE target_id=?", v.ID).Scan(&state); err != nil || state != "sending" {
		t.Fatalf("in-flight bot notification is %+q (%v), want sending", state, err)
	}
	close(bot.release)
	waitWebhookState(t, f, v.ID, "sent")
	if n := bot.calls.Load(); n != 1 {
		t.Fatalf("bot endpoint called %d times, want 1", n)
	}
}
