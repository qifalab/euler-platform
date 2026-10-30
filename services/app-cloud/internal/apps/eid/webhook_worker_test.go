package eid

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"
)

// reopenWithClient restarts the module on the same SQLite file. The bot client
// is set before Migrate, which starts the worker and its sweep.
func reopenWithClient(t *testing.T, f *testFixture, client *http.Client) {
	t.Helper()
	f.m.Close()
	f.rt.DB.Close()
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	f.rt.DB = db
	f.m = New(f.rt)
	f.m.client = client
	if err = f.m.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func commitWebhook(t *testing.T, f *testFixture, target string) {
	t.Helper()
	alice := actor("alice", "read", "write")
	if err := f.rt.Transaction(context.Background(), func(tx *sql.Tx) error {
		return f.m.event(context.Background(), tx, alice, target, "verification.submit", "", "pending")
	}); err != nil {
		t.Fatal(err)
	}
}

// A row committed just before a restart is sent by the next process on start,
// without waiting for another write in that installation.
func TestQueuedWebhookIsSentAfterRestart(t *testing.T) {
	f := fixture(t)
	cfg := defaults()
	cfg.WecomURL = testWecomURL
	saveCfg(t, f, cfg)
	f.m.Close() // the process stops right after committing the outbox row
	commitWebhook(t, f, "restart-target")
	bot := newBotEndpoint()
	close(bot.release)
	reopenWithClient(t, f, bot.client())
	waitWebhookState(t, f, "restart-target", "sent")
	if n := bot.calls.Load(); n != 1 {
		t.Fatalf("bot endpoint called %d times, want 1", n)
	}
	var actorID string
	if err := f.rt.DB.QueryRow("SELECT actor_id FROM audit WHERE action='eid.notification.sent' AND target_id='restart-target'").Scan(&actorID); err != nil || actorID != "system" {
		t.Fatalf("delivery audited for %+q (%v), want system", actorID, err)
	}
}

// Like other background work, the start sweep sends nothing for a project
// whose EID app is disabled or archived; its rows stay queued.
func TestStartSweepSkipsDisabledApplication(t *testing.T) {
	f := fixture(t)
	cfg := defaults()
	cfg.WecomURL = testWecomURL
	saveCfg(t, f, cfg)
	f.m.Close()
	commitWebhook(t, f, "disabled-target")
	f.m.takeWebhookRequests() // drop the requests of the writes above
	f.enabled = false
	f.m.sweepWebhooks(context.Background())
	if pending := f.m.takeWebhookRequests(); len(pending) != 0 {
		t.Fatalf("sweep requested delivery for a disabled application: %d", len(pending))
	}
	f.enabled = true
	f.m.sweepWebhooks(context.Background())
	pending := f.m.takeWebhookRequests()
	if len(pending) != 1 || pending[0].InstallationID != "eid-a" || pending[0].ActorID != "system" || pending[0].ApplicationID != "eid" {
		t.Fatalf("sweep requests: %+v", pending)
	}
}

// Close stops the worker: it waits a bounded time for the call in flight,
// records that call's result before returning, and claims nothing afterwards.
func TestCloseStopsWebhookWorker(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers bool
		grace   time.Duration
		want    string
	}{
		{"endpoint answers within the grace", true, webhookCloseGrace, "sent"},
		{"endpoint hangs past the grace", false, 300 * time.Millisecond, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture(t)
			bot := newBotEndpoint()
			f.m.client = bot.client()
			f.m.outbox.grace = tc.grace
			cfg := defaults()
			cfg.WecomURL = testWecomURL
			saveCfg(t, f, cfg)
			w := call(t, f.m, actor("alice", "read", "write"), "POST", "/verifications", verificationInput{RealName: "Alice", StudentID: "42", IdentityTitle: "Core", IdentityType: "core"})
			status(t, w, 201)
			first := result[Verification](t, w)
			eventually(t, "the bot endpoint was never called", func() bool { return bot.calls.Load() == 1 })
			if tc.answers {
				time.AfterFunc(50*time.Millisecond, func() { close(bot.release) })
			}
			start := time.Now()
			if err := f.m.Close(); err != nil {
				t.Fatal(err)
			}
			elapsed := time.Since(start)
			if elapsed > 3*time.Second || !tc.answers && elapsed < tc.grace {
				t.Fatalf("Close returned after %v with a %v grace", elapsed, tc.grace)
			}
			select {
			case <-f.m.outbox.done:
			default:
				t.Fatal("worker still running after Close")
			}
			var state string
			if err := f.rt.DB.QueryRow("SELECT state FROM eid_notifications WHERE target_id=?", first.ID).Scan(&state); err != nil || state != tc.want {
				t.Fatalf("in-flight notification is %+q (%v), want %+q", state, err, tc.want)
			}
			// No row is claimed after Close: a later write's row stays queued.
			w = call(t, f.m, actor("bob", "read", "write"), "POST", "/verifications", verificationInput{RealName: "Bob", StudentID: "43", IdentityTitle: "Core", IdentityType: "core"})
			status(t, w, 201)
			second := result[Verification](t, w)
			if err := f.rt.DB.QueryRow("SELECT state FROM eid_notifications WHERE target_id=?", second.ID).Scan(&state); err != nil || state != "queued" {
				t.Fatalf("notification after Close is %+q (%v), want queued", state, err)
			}
			if n := bot.calls.Load(); n != 1 {
				t.Fatalf("bot endpoint called %d times after Close, want 1", n)
			}
			if err := f.m.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
