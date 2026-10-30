package trust

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A committed lifecycle notice must still be sent when the browser request that
// produced it is cancelled right after the commit (tab closed, app switched,
// client timeout). Otherwise it stays "pending" with no automatic delivery.
func TestCommittedNoticeIsDeliveredAfterCallerCancels(t *testing.T) {
	f := newFixture(t)
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0}`))
	}))
	defer server.Close()
	t.Setenv("EULER_NOTIFY_WECOM_URL", server.URL)
	user := person("alice", "read", "write")
	if e := f.rt.Transaction(context.Background(), func(tx *sql.Tx) error {
		return f.m.enqueue(context.Background(), tx, user, "tsu_cancelled", "成员实名", "submit", "pending")
	}); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.m.deliverFor(ctx, user, "tsu_cancelled")
	var status string
	var attempts int
	if e := f.rt.DB.QueryRow("SELECT status,attempts FROM trust_notifications WHERE submission_id=?", "tsu_cancelled").Scan(&status, &attempts); e != nil {
		t.Fatal(e)
	}
	if status != "sent" || attempts != 1 || received.Load() != 1 {
		t.Fatalf("status=%s attempts=%d received=%d", status, attempts, received.Load())
	}
}
