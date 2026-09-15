package notify

import (
	"context"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/storage/sqltest"
)

func newSQLTestStore(t *testing.T) *SQLStore {
	t.Helper()
	db := sqltest.Open(t, sqltest.Services("svc-quota"))
	return NewSQLStore(context.Background(), db)
}

var notifyClock = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

func testNotification(id string) Notification {
	return Notification{
		NotificationID: id, AccountID: 100123, Class: ClassReleaseWarning,
		TemplateID: "release-warning-v1",
		Params:     map[string]string{"resourceId": "euecs-cn-north-1-01-a1b2c3d4", "deadline": "2026-09-22"},
		Channels:   []Channel{ChannelInApp, ChannelEmail}, BizKey: "euecs-cn-north-1-01-a1b2c3d4",
		Status: StatusPending, CreatedAt: notifyClock,
	}
}

// TestSQLStorePersistBeforeSend runs the dispatcher's actual write pattern
// against the schema of record: the message is saved as PENDING *before* any
// channel is tried (a crash mid-delivery must still be on record as attempted),
// then saved again with the outcome and the per-channel evidence.
func TestSQLStorePersistBeforeSend(t *testing.T) {
	store := newSQLTestStore(t)

	n := testNotification("ntf-20260915-0001")
	if err := store.Save(n); err != nil {
		t.Fatal(err)
	}
	// Delivery outcomes arrive: one channel succeeded, one failed with the
	// provider-side message id that dispute resolution depends on.
	n.Status = StatusSent
	n.SentAt = notifyClock.Add(time.Minute)
	n.Attempts = 2
	n.Deliveries = []Delivery{
		{Channel: ChannelInApp, Success: true, SentAt: n.SentAt, Provider: "in-app", MessageID: "ia-77"},
		{Channel: ChannelEmail, Success: false, SentAt: n.SentAt, Provider: "mail-relay", Error: "smtp 550"},
	}
	if err := store.Save(n); err != nil {
		t.Fatal(err)
	}

	// The re-save updated the row; it did not append a second one.
	got, err := store.Get(n.NotificationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSent || got.Attempts != 2 || len(got.Deliveries) != 2 {
		t.Fatalf("after outcome save: status %s attempts %d deliveries %d", got.Status, got.Attempts, len(got.Deliveries))
	}
	if !got.Delivered() {
		t.Fatal("one successful channel must satisfy Delivered()")
	}
	if got.Params["resourceId"] != "euecs-cn-north-1-01-a1b2c3d4" {
		t.Fatalf("params round trip = %v", got.Params)
	}
	if len(got.Channels) != 2 || got.Channels[0] != ChannelInApp {
		t.Fatalf("channels round trip = %v", got.Channels)
	}
}

func TestSQLStoreQueries(t *testing.T) {
	store := newSQLTestStore(t)

	n := testNotification("ntf-20260915-0002")
	n.Deliveries = []Delivery{{Channel: ChannelInApp, Success: true, SentAt: notifyClock, MessageID: "ia-78"}}
	if err := store.Save(n); err != nil {
		t.Fatal(err)
	}

	// Throttle window: the notification just written counts.
	count, err := store.CountInWindow(100123, notifyClock.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("CountInWindow missed the just-written notification")
	}
	if other, err := store.CountInWindow(999, notifyClock.Add(-time.Hour)); err != nil || other != 0 {
		t.Fatalf("cross-account count = %d (err %v)", other, err)
	}

	// The support-agent query: "was this resource ever warned?"
	found, err := store.FindByBizKey(100123, ClassReleaseWarning, "euecs-cn-north-1-01-a1b2c3d4")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].NotificationID != n.NotificationID || len(found[0].Deliveries) != 1 {
		t.Fatalf("FindByBizKey = %+v", found)
	}

	// The console inbox, newest first.
	inbox, err := store.ListByAccount(100123)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) < 1 || inbox[0].NotificationID != n.NotificationID {
		t.Fatalf("inbox = %+v", inbox)
	}

	if _, err := store.Get("ntf-missing"); err != ErrNotFound {
		t.Fatalf("get missing: %v, want ErrNotFound", err)
	}
}

// TestSQLStoreSurvivesRestart is the trust-critical property: the evidence is
// in the database, not in whichever process happened to send the message.
func TestSQLStoreSurvivesRestart(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-quota"))
	ctx := context.Background()

	first := NewSQLStore(ctx, db)
	n := testNotification("ntf-20260915-0003")
	n.Deliveries = []Delivery{{Channel: ChannelSMS, Success: true, SentAt: notifyClock, Provider: "sms-gw", MessageID: "sms-9"}}
	if err := first.Save(n); err != nil {
		t.Fatal(err)
	}

	restarted := NewSQLStore(ctx, db)
	got, err := restarted.Get(n.NotificationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusPending || len(got.Deliveries) != 1 || got.Deliveries[0].MessageID != "sms-9" {
		t.Fatalf("restarted Get = %+v", got)
	}
}
