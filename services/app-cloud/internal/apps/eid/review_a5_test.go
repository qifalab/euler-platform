package eid

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

// Automation rules grant packages for offer_confirmed membership and re-check
// eligibility only when an eid.status event arrives (deleteClub emits one).
// Deleting the member removes the same club records, so it must emit too.
func TestDeleteMemberEmitsClubDeletionEvents(t *testing.T) {
	f := fixture(t)
	alice := actor("alice", "read", "write")
	reviewer := actor("reviewer", "read", "review")
	operator := actor("operator", "read", "admin")
	cfg := defaults()
	cfg.RequireTrust = false
	cfg.SMTPAddress = "smtp.example.test:465"
	cfg.SMTPFrom = "recruit@example.test"
	saveCfg(t, f, cfg)
	f.m.mailer = func(context.Context, Settings, string, string, string) error { return nil }
	w := call(t, f.m, alice, "POST", "/club/applications", map[string]any{"realName": "Alice"})
	status(t, w, 201)
	a := result[ClubApplication](t, w)
	for _, kind := range []string{"interview", "offer"} {
		status(t, call(t, f.m, reviewer, "POST", "/review/club/"+a.ID+"/notifications", map[string]any{"kind": kind, "version": a.Version, "idempotencyKey": "key-" + kind}), 200)
		a, _ = f.m.club(context.Background(), alice, a.ID)
	}
	status(t, call(t, f.m, alice, "POST", "/club/applications/"+a.ID+"/confirm", map[string]any{"version": a.Version}), 200)
	a, _ = f.m.club(context.Background(), alice, a.ID)
	if a.Status != "offer_confirmed" {
		t.Fatal(a.Status)
	}
	status(t, call(t, f.m, operator, "DELETE", "/admin/members/alice", nil), 204)
	if _, err := f.m.club(context.Background(), alice, a.ID); err == nil {
		t.Fatal("club application survived member deletion")
	}
	rows, err := f.rt.DB.Query("SELECT body FROM automation_events WHERE source='eid.status'")
	if err != nil {
		t.Fatal(err)
	}
	var events []appkit.Event
	for rows.Next() {
		var body string
		var e appkit.Event
		if err = rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal([]byte(body), &e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	rows.Close()
	want := strconv.FormatInt(a.Version+1, 10)
	for _, e := range events {
		if e.ResourceID == a.ID && e.SubjectID == "alice" && e.Status == "deleted" && e.Version == want && e.FilterID == "club" {
			return
		}
	}
	t.Fatalf("member deletion removed an offer_confirmed application without an eid.status event; automation keeps its package: %+v", events)
}

// localSMTP serves one implicit-TLS SMTP session and answers the end of DATA
// with finalReply.
func localSMTP(t *testing.T, finalReply string) (string, func(string) *tls.Config) {
	t.Helper()
	certificateServer := httptest.NewTLSServer(http.NotFoundHandler())
	certificates := certificateServer.TLS.Certificates
	pool := x509.NewCertPool()
	pool.AddCert(certificateServer.Certificate())
	certificateServer.Close()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: certificates, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		p := textproto.NewConn(conn)
		p.PrintfLine("220 local SMTP ready")
		for {
			line, err := p.ReadLine()
			if err != nil {
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				p.PrintfLine("250 local")
			case strings.HasPrefix(line, "MAIL FROM"), strings.HasPrefix(line, "RCPT TO"):
				p.PrintfLine("250 OK")
			case line == "DATA":
				p.PrintfLine("354 continue")
				if _, err = p.ReadDotBytes(); err != nil {
					return
				}
				p.PrintfLine("%s", finalReply)
			case line == "QUIT":
				p.PrintfLine("221 goodbye")
				return
			default:
				p.PrintfLine("500 unexpected command")
				return
			}
		}
	}()
	return listener.Addr().String(), func(host string) *tls.Config {
		return &tls.Config{ServerName: host, RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
}

// A 4xx/5xx reply to the end of DATA is the server refusing the message. It
// must be recorded as failed; uncertain tells reviewers the mail may have
// arrived and to check the inbox before sending again.
func TestSMTPRejectionAfterDataIsFailedNotUncertain(t *testing.T) {
	f := fixture(t)
	t.Setenv("EULER_EID_ALLOW_LOCAL_SMTP", "true")
	for _, reply := range []string{"554 5.7.1 message rejected", "451 4.3.0 try again later"} {
		addr, tlsConfig := localSMTP(t, reply)
		f.m.smtpTLSConfig = tlsConfig
		cfg := defaults()
		cfg.SMTPAddress = addr
		cfg.SMTPFrom = "sender@example.test"
		cfg.SMTPImplicitTLS = true
		err := f.m.sendSMTP(context.Background(), cfg, "alice@example.test", "subject", "body")
		if err == nil {
			t.Fatalf("%q: refused message reported as delivered", reply)
		}
		var ambiguous *uncertainDelivery
		if errors.As(err, &ambiguous) {
			t.Fatalf("%q: explicit refusal recorded as uncertain delivery", reply)
		}
	}
}

// Through the HTTP flow the same refusal leaves the application unchanged and
// the notification failed, so a reviewer may simply send again.
func TestClubNotificationRefusedByServerIsFailed(t *testing.T) {
	f := fixture(t)
	t.Setenv("EULER_EID_ALLOW_LOCAL_SMTP", "true")
	addr, tlsConfig := localSMTP(t, "554 5.7.1 message rejected")
	f.m.smtpTLSConfig = tlsConfig
	alice := actor("alice", "read", "write")
	reviewer := actor("reviewer", "read", "review")
	cfg := defaults()
	cfg.RequireTrust = false
	cfg.SMTPAddress = addr
	cfg.SMTPFrom = "sender@example.test"
	cfg.SMTPImplicitTLS = true
	saveCfg(t, f, cfg)
	w := call(t, f.m, alice, "POST", "/club/applications", map[string]any{"realName": "Alice"})
	status(t, w, 201)
	a := result[ClubApplication](t, w)
	w = call(t, f.m, reviewer, "POST", "/review/club/"+a.ID+"/notifications", map[string]any{"kind": "interview", "version": a.Version, "idempotencyKey": "refused"})
	status(t, w, 200)
	if n := result[Notification](t, w); n.State != "failed" {
		t.Fatalf("refused mail recorded as %q: %s", n.State, w.Body.String())
	}
	if a, _ = f.m.club(context.Background(), alice, a.ID); a.Status != "pending" {
		t.Fatal("refused mail advanced the application", a.Status)
	}
}

// Covers the recruitment branches the original tests skip: a request key bound
// to another operation, a resend that keeps the stage, and re-application
// after a rejection.
func TestClubResendKeyReuseAndReapplication(t *testing.T) {
	f := fixture(t)
	alice := actor("alice", "read", "write")
	bob := actor("bob", "read", "write")
	reviewer := actor("reviewer", "read", "review")
	cfg := defaults()
	cfg.RequireTrust = false
	cfg.SMTPAddress = "smtp.example.test:465"
	cfg.SMTPFrom = "recruit@example.test"
	saveCfg(t, f, cfg)
	mails := 0
	f.m.mailer = func(context.Context, Settings, string, string, string) error { mails++; return nil }
	apply := func(s appkit.Scope) ClubApplication {
		w := call(t, f.m, s, "POST", "/club/applications", map[string]any{"realName": s.ActorName})
		status(t, w, 201)
		return result[ClubApplication](t, w)
	}
	send := func(a ClubApplication, kind, key string, resend bool) *httptest.ResponseRecorder {
		return call(t, f.m, reviewer, "POST", "/review/club/"+a.ID+"/notifications", map[string]any{"kind": kind, "resend": resend, "version": a.Version, "idempotencyKey": key})
	}
	a, b := apply(alice), apply(bob)
	status(t, send(a, "interview", "k1", false), 200)
	a, _ = f.m.club(context.Background(), alice, a.ID)
	status(t, send(a, "offer", "k1", false), 409)
	status(t, send(b, "interview", "k1", false), 409)
	if mails != 1 {
		t.Fatal("a reused request key sent mail", mails)
	}
	w := send(a, "interview", "k2", true)
	status(t, w, 200)
	if result[Notification](t, w).Kind != "resend_interview" {
		t.Fatal(w.Body.String())
	}
	resent, _ := f.m.club(context.Background(), alice, a.ID)
	if resent.Status != "interview_sent" || resent.Version != a.Version+1 || mails != 2 {
		t.Fatalf("resend changed the stage or skipped delivery: %+v mails=%d", resent, mails)
	}
	history, _ := f.m.events(context.Background(), alice, a.ID)
	if last := history[len(history)-1]; last.Action != "club.resend_interview" || last.From != "interview_sent" || last.To != "interview_sent" {
		t.Fatalf("resend history: %+v", last)
	}
	status(t, call(t, f.m, reviewer, "POST", "/review/club/"+a.ID+"/decision", map[string]any{"status": "rejected", "version": resent.Version}), 200)
	rejected, _ := f.m.club(context.Background(), alice, a.ID)
	status(t, call(t, f.m, reviewer, "POST", "/review/club/"+a.ID+"/decision", map[string]any{"status": "rejected", "version": rejected.Version}), 409)
	again := apply(alice)
	if again.ID == a.ID || again.Status != "pending" {
		t.Fatalf("re-application after rejection: %+v", again)
	}
}

// The offer re-checks the applicant's current Trust result: approval at
// application time is not enough once a reviewer has rejected it.
func TestOfferRequiresCurrentTrustQualification(t *testing.T) {
	f := fixture(t)
	alice := actor("alice", "read", "write")
	reviewer := actor("reviewer", "read", "review")
	scheme, sub := trustedActor(t, f, alice)
	cfg := defaults()
	cfg.TrustSchemeID = scheme.ID
	cfg.SMTPAddress = "smtp.example.test:465"
	cfg.SMTPFrom = "recruit@example.test"
	saveCfg(t, f, cfg)
	mails := 0
	f.m.mailer = func(context.Context, Settings, string, string, string) error { mails++; return nil }
	w := call(t, f.m, alice, "POST", "/club/applications", map[string]any{"realName": "Alice"})
	status(t, w, 201)
	a := result[ClubApplication](t, w)
	status(t, call(t, f.m, reviewer, "POST", "/review/club/"+a.ID+"/notifications", map[string]any{"kind": "interview", "version": a.Version, "idempotencyKey": "interview"}), 200)
	a, _ = f.m.club(context.Background(), alice, a.ID)
	trustReviewer := actor("trust-admin", "read", "write", "admin", "review")
	trustReviewer.ApplicationID, trustReviewer.InstallationID = "trust", "trust-a"
	status(t, call(t, f.trust, trustReviewer, "POST", "/review/submissions/"+sub.ID, map[string]any{"status": "rejected", "version": sub.Version, "reason": "资料不符"}), 200)
	status(t, call(t, f.m, reviewer, "POST", "/review/club/"+a.ID+"/notifications", map[string]any{"kind": "offer", "version": a.Version, "idempotencyKey": "offer"}), 409)
	if after, _ := f.m.club(context.Background(), alice, a.ID); after.Status != "interview_sent" || mails != 1 {
		t.Fatalf("offer sent without a current Trust qualification: %+v mails=%d", after, mails)
	}
}
