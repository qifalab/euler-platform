package eid

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSMTPPasswordBoundToDestination(t *testing.T) {
	f := fixture(t)
	t.Setenv("EULER_EID_SMTP_ADDRESS", "smtp.original.test:465")
	t.Setenv("EULER_EID_SMTP_FROM", "sender@example.test")
	t.Setenv("EULER_EID_SMTP_USERNAME", "deployment-user")
	t.Setenv("EULER_EID_SMTP_PASSWORD", "deployment-secret")
	owner := actor("owner", "read", "manage", "secrets")
	cfg := defaults()
	cfg.SMTPAddress = "smtp.other.test:465"
	cfg.SMTPPassword = ""
	status(t, call(t, f.m, owner, "PUT", "/settings", cfg), 400)
	existing, err := f.m.settings(context.Background(), owner)
	if err != nil || existing.SMTPAddress != "smtp.original.test:465" || existing.SMTPPassword != "deployment-secret" {
		t.Fatal("rejected destination change altered credentials")
	}
	cfg = defaults()
	cfg.SMTPUsername = "another-principal"
	cfg.SMTPPassword = ""
	status(t, call(t, f.m, owner, "PUT", "/settings", cfg), 400)
	cfg = defaults()
	cfg.SMTPPassword = ""
	cfg.ClubName = "New recruitment"
	status(t, call(t, f.m, owner, "PUT", "/settings", cfg), 200)
	existing, _ = f.m.settings(context.Background(), owner)
	if existing.SMTPPassword != "deployment-secret" {
		t.Fatal("same destination lost stored password")
	}
	cfg.SMTPAddress = "smtp.other.test:465"
	status(t, call(t, f.m, owner, "PUT", "/settings", struct {
		Settings
		ClearSMTPPassword bool `json:"clearSMTPPassword"`
	}{cfg, true}), 200)
	existing, _ = f.m.settings(context.Background(), owner)
	if existing.SMTPPassword != "" {
		t.Fatal("explicit clearing retained deployment password")
	}
}

func TestManualQualificationNeedsIndependentReviewer(t *testing.T) {
	f := fixture(t)
	alice := actor("alice", "read", "write")
	status(t, call(t, f.m, alice, "GET", "/profile", nil), 200)
	admin := actor("admin", "read", "admin")
	body := map[string]any{"bio": "updated", "identityLevel": 4, "identityTitle": "admin granted title"}
	status(t, call(t, f.m, admin, "PATCH", "/admin/members/alice", body), 403)
	p, err := f.m.member(context.Background(), alice, "alice")
	if err != nil || p.IdentityLevel != 0 || p.Bio != "" {
		t.Fatal("admin escalated qualification")
	}
	body["identityLevel"] = 0
	body["identityTitle"] = ""
	status(t, call(t, f.m, admin, "PATCH", "/admin/members/alice", body), 200)
	admin.Permissions = append(admin.Permissions, "review")
	body["identityLevel"] = 4
	body["identityTitle"] = "reviewed title"
	status(t, call(t, f.m, admin, "PATCH", "/admin/members/alice", body), 200)
}

func TestWebhookOutboxAtomicDurableAndAcknowledged(t *testing.T) {
	f := fixture(t)
	cfg := defaults()
	cfg.WecomURL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=test-only"
	saveCfg(t, f, cfg)
	alice := actor("alice", "read", "write")
	reviewer := actor("reviewer", "read", "review")
	response := "{}"
	var requests atomic.Int32 // the module's worker also calls the transport
	transport := testTransport(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		if r.Context().Err() != nil {
			t.Error("delivery inherited canceled browser context")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
	})
	f.m.client = &http.Client{Transport: transport}
	if _, err := f.rt.DB.Exec(`CREATE TRIGGER reject_eid_outbox BEFORE INSERT ON eid_notifications BEGIN SELECT RAISE(ABORT,'outbox unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	status(t, call(t, f.m, alice, "POST", "/verifications", verificationInput{RealName: "Alice", StudentID: "42", IdentityTitle: "Core", IdentityType: "core"}), 500)
	var n int
	f.rt.DB.QueryRow("SELECT count(*) FROM eid_verifications").Scan(&n)
	if n != 0 {
		t.Fatal("business survived missing outbox")
	}
	if _, err := f.rt.DB.Exec("DROP TRIGGER reject_eid_outbox"); err != nil {
		t.Fatal(err)
	}
	// Commit without dispatch once the worker has stopped, then reopen SQLite as
	// after a process restart: the new worker sends the row without a request.
	f.m.Close()
	if err := f.rt.Transaction(context.Background(), func(tx *sql.Tx) error {
		return f.m.event(context.Background(), tx, alice, "durable-target", "verification.submit", "", "pending")
	}); err != nil {
		t.Fatal(err)
	}
	reopenWithClient(t, f, &http.Client{Transport: transport})
	// An HTTP 200 empty object is not an acknowledgement.
	id := waitWebhookState(t, f, "durable-target", "failed")
	status(t, call(t, f.m, alice, "POST", "/notifications/"+id+"/retry", map[string]bool{"confirmed": true}), 403)
	status(t, call(t, f.m, reviewer, "POST", "/notifications/"+id+"/retry", map[string]bool{}), 400)
	foreign := reviewer
	foreign.ProjectID = "other"
	status(t, call(t, f.m, foreign, "POST", "/notifications/"+id+"/retry", map[string]bool{"confirmed": true}), 404)
	for _, invalid := range []string{"null", `{"code":0}`, `{"errcode":1}`} {
		response = invalid
		w := call(t, f.m, reviewer, "POST", "/notifications/"+id+"/retry", map[string]bool{"confirmed": true})
		status(t, w, 200)
		if result[Notification](t, w).State != "failed" {
			t.Fatal("unconfirmed provider response accepted", invalid)
		}
	}
	response = `{"errcode":0}`
	w := call(t, f.m, reviewer, "POST", "/notifications/"+id+"/retry", map[string]bool{"confirmed": true})
	status(t, w, 200)
	if result[Notification](t, w).State != "sent" {
		t.Fatal(w.Body.String())
	}
	f.m.flushWebhooks(context.Background(), context.Background(), alice)
	if n := requests.Load(); n != 5 {
		t.Fatal("already completed outbox was resent", n)
	}
	status(t, call(t, f.m, reviewer, "POST", "/notifications/"+id+"/retry", map[string]bool{"confirmed": true}), 409)
}
