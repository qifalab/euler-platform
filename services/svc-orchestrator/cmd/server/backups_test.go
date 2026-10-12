package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type backupBody struct {
	BackupID   string `json:"backupId"`
	ResourceID string `json:"resourceId"`
	Status     string `json:"status"`
	CreatedAt  string `json:"createdAt"`
	ExpiresAt  string `json:"expiresAt"`
	SizeGB     *int   `json:"sizeGb"`
}

func backupPath(resID string) string { return "/api/v1/orchestrator/resources/" + resID + "/backups" }

func createBackupAs(t *testing.T, s *lifecycleStore, acct int64, resID string) *httptest.ResponseRecorder {
	t.Helper()
	return call(t, s.handleCreateBackup, "POST", backupPath(resID), acct, nil, "id", resID)
}

func listBackupsAs(t *testing.T, s *lifecycleStore, acct int64, resID string) []backupBody {
	t.Helper()
	rec := call(t, s.handleListBackups, "GET", backupPath(resID), acct, nil, "id", resID)
	if rec.Code != 200 {
		t.Fatalf("list backups: %d %s", rec.Code, rec.Body.String())
	}
	var out []backupBody
	if err := json.Unmarshal(decodeEnv(t, rec).Data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func deleteBackupAs(t *testing.T, s *lifecycleStore, acct int64, resID, backupID string) *httptest.ResponseRecorder {
	t.Helper()
	return call(t, s.handleDeleteBackup, "DELETE", backupPath(resID)+"/"+backupID, acct, nil, "id", resID, "backupId", backupID)
}

func decodeBackup(t *testing.T, rec *httptest.ResponseRecorder) backupBody {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("create backup: %d %s", rec.Code, rec.Body.String())
	}
	var b backupBody
	if err := json.Unmarshal(decodeEnv(t, rec).Data, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

// O3: snapshots of an eubackup resource — kept for its retentionDays, listed
// newest first, swept once lapsed, deletable, account-scoped.
func TestBackupsLifecycle(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
		s.clock = func() time.Time { return now }
		o := paidIn(401, "eubackup", "cn-north-1")
		orders.add(100123, o)
		resID := decodeFulfil(t, fulfilAs(t, s, 100123, fulfilBodyFor(o, map[string]any{"retentionDays": "7", "schedule": "02:00"}))).Data.ResourceID

		first := decodeBackup(t, createBackupAs(t, s, 100123, resID))
		if !strings.HasPrefix(first.BackupID, "bak-") || first.ResourceID != resID || first.Status != "COMPLETED" ||
			first.CreatedAt != "2026-10-08T09:00:00Z" || first.ExpiresAt != "2026-10-15T09:00:00Z" || first.SizeGB != nil {
			t.Fatalf("first backup = %+v", first)
		}
		now = now.Add(time.Hour)
		second := decodeBackup(t, createBackupAs(t, s, 100123, resID))
		now = now.Add(400 * time.Millisecond) // same second: the id breaks the tie
		third := decodeBackup(t, createBackupAs(t, s, 100123, resID))

		got := listBackupsAs(t, s, 100123, resID)
		if len(got) != 3 || got[0].BackupID != third.BackupID || got[1].BackupID != second.BackupID || got[2].BackupID != first.BackupID {
			t.Fatalf("list = %+v, want newest first", got)
		}
		// A shorter retention does not shorten the snapshots already taken.
		if rec := patchAs(t, s, 100123, resID, map[string]any{"attributes": map[string]any{"retentionDays": "1"}}); rec.Code != 200 {
			t.Fatalf("patch retention: %d %s", rec.Code, rec.Body.String())
		}
		fourth := decodeBackup(t, createBackupAs(t, s, 100123, resID))
		if fourth.ExpiresAt != "2026-10-09T10:00:00Z" {
			t.Fatalf("new snapshot under retention 1d expires %s", fourth.ExpiresAt)
		}

		// Other accounts see nothing.
		wantCode(t, "other account list", call(t, s.handleListBackups, "GET", backupPath(resID), 200456, nil, "id", resID), 404, "Resource.NotFound")
		wantCode(t, "other account delete", deleteBackupAs(t, s, 200456, resID, first.BackupID), 404, "Resource.NotFound")

		// Delete one; deleting it again is 404.
		if rec := deleteBackupAs(t, s, 100123, resID, second.BackupID); rec.Code != 200 {
			t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
		}
		wantCode(t, "delete twice", deleteBackupAs(t, s, 100123, resID, second.BackupID), 404, "Resource.BackupNotFound")

		// Two days on, the 1-day snapshot lapsed and is swept; the 7-day ones stay.
		now = now.Add(48 * time.Hour)
		got = listBackupsAs(t, s, 100123, resID)
		if len(got) != 2 || got[0].BackupID != third.BackupID || got[1].BackupID != first.BackupID {
			t.Fatalf("after 2 days = %+v", got)
		}
		// A week later everything lapsed — and was deleted, not just hidden.
		now = now.Add(7 * 24 * time.Hour)
		if got := listBackupsAs(t, s, 100123, resID); len(got) != 0 {
			t.Fatalf("after retention = %+v", got)
		}
		if sl, ok := s.ledger.(*sqlLedger); ok {
			var n int
			if err := sl.db.QueryRow(`SELECT COUNT(*) FROM resource_backup WHERE resource_id = ?`, resID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("%d lapsed snapshots left in resource_backup", n)
			}
		}
	})
}

func TestBackupsRequireRunningEUBackup(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		bk, ecs := paidIn(411, "eubackup", "cn-north-1"), paidIn(412, "euecs", "cn-north-1")
		orders.add(100123, bk)
		orders.add(100123, ecs)
		bkID := decodeFulfil(t, fulfilAs(t, s, 100123, fulfilBodyFor(bk, nil))).Data.ResourceID
		ecsID := decodeFulfil(t, fulfilAs(t, s, 100123, fulfilBodyFor(ecs, nil))).Data.ResourceID

		wantCode(t, "backup of a non-eubackup resource", createBackupAs(t, s, 100123, ecsID), 400, "Resource.BackupNotSupported")
		wantCode(t, "list of a non-eubackup resource", call(t, s.handleListBackups, "GET", backupPath(ecsID), 100123, nil, "id", ecsID), 400, "Resource.BackupNotSupported")
		wantCode(t, "missing resource", createBackupAs(t, s, 100123, "eubackup-cn-north-1-01-ffffffff"), 404, "Resource.NotFound")

		// Default retention (no retentionDays attribute) is 30 days.
		b := decodeBackup(t, createBackupAs(t, s, 100123, bkID))
		created, _ := time.Parse(time.RFC3339, b.CreatedAt)
		expires, _ := time.Parse(time.RFC3339, b.ExpiresAt)
		if expires.Sub(created) != 30*24*time.Hour {
			t.Fatalf("default retention = %s", expires.Sub(created))
		}
		// Only RUNNING takes new snapshots; existing ones stay listable.
		if rec := doActionAs(t, s, 100123, bkID, `{"action":"stop"}`); rec.Code != 200 {
			t.Fatalf("stop: %d %s", rec.Code, rec.Body.String())
		}
		wantCode(t, "backup while STOPPED", createBackupAs(t, s, 100123, bkID), 409, "Resource.InvalidState")
		if got := listBackupsAs(t, s, 100123, bkID); len(got) != 1 {
			t.Fatalf("list while STOPPED = %+v", got)
		}
	})
}

func doActionAs(t *testing.T, s *lifecycleStore, acct int64, resID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return call(t, s.handleResourceAction, "POST", "/api/v1/orchestrator/resources/"+resID+"/actions", acct, body, "id", resID)
}

func TestNewBackupIDSortsByTime(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	a, b := newBackupID(t0), newBackupID(t0.Add(time.Millisecond))
	if len(a) != len("bak-")+20 || !(a < b) {
		t.Fatalf("ids %s, %s: want fixed width and time order", a, b)
	}
	if first, second := newBackupID(t0), newBackupID(t0); first == second {
		t.Fatal("two ids in one millisecond collided")
	}
}
