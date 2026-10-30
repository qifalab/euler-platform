// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The runner commits a watcher only after its events were queued. When
// queueing fails (for example "offline queue capacity reached") the snapshot
// from that poll must not be committed later by a poll that found no change.
func TestStalePendingBaselineIsNotCommittedByLaterQuietPoll(t *testing.T) {
	root := t.TempDir()
	passwd := filepath.Join(root, "etc", "passwd")
	if err := os.MkdirAll(filepath.Dir(passwd), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("root:x:0:0:root:/root:/bin/sh\n")
	stamp := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	write := func(content []byte) {
		t.Helper()
		if err := os.WriteFile(passwd, content, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(passwd, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	write(original)
	w := &baselineWatcher{hostRoot: root, statePath: filepath.Join(t.TempDir(), "baseline.json"), now: time.Now}
	if events, err := w.Poll(context.Background()); err != nil || len(events) != 0 {
		t.Fatalf("initial poll events=%d err=%v", len(events), err)
	}
	write([]byte("root:x:0:0:root:/root:/bin/sh\nbackdoor:x:0:0::/:/bin/sh\n"))
	if events, err := w.Poll(context.Background()); err != nil || len(events) != 1 {
		t.Fatalf("change poll events=%d err=%v", len(events), err)
	}
	// Queueing failed, so the runner did not call Commit. The change is reverted.
	write(original)
	if events, err := w.Poll(context.Background()); err != nil || len(events) != 0 {
		t.Fatalf("revert poll events=%d err=%v", len(events), err)
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	if events, err := w.Poll(context.Background()); err != nil || len(events) != 0 {
		t.Fatalf("stale snapshot was committed: unchanged host now reports %d phantom events (err=%v)", len(events), err)
	}
}

func TestStalePendingNetworkBaselineIsNotCommittedByLaterQuietPoll(t *testing.T) {
	root := t.TempDir()
	table := filepath.Join(root, "proc", "1", "net", "tcp")
	if err := os.MkdirAll(filepath.Dir(table), 0o700); err != nil {
		t.Fatal(err)
	}
	header := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	quiet := header + "   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0\n"
	opened := quiet + "   1: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2 1 0000000000000000 100 0 0 10 0\n"
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(table, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(quiet)
	w := &networkWatcher{hostRoot: root, statePath: filepath.Join(t.TempDir(), "network.json"), now: time.Now}
	if events, err := w.Poll(context.Background()); err != nil || len(events) != 0 {
		t.Fatalf("initial poll events=%d err=%v", len(events), err)
	}
	write(opened)
	if events, err := w.Poll(context.Background()); err != nil || len(events) != 1 {
		t.Fatalf("listener poll events=%d err=%v", len(events), err)
	}
	write(quiet)
	if events, err := w.Poll(context.Background()); err != nil || len(events) != 0 {
		t.Fatalf("closed poll events=%d err=%v", len(events), err)
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	if events, err := w.Poll(context.Background()); err != nil || len(events) != 0 {
		t.Fatalf("stale snapshot was committed: unchanged host now reports %d phantom events (err=%v)", len(events), err)
	}
}
