package main

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/qifalab/euler-platform/backup"
	"github.com/qifalab/euler-platform/resource"
)

// Backups of an EUBACKUP resource (O3):
//
//	POST   /api/v1/orchestrator/resources/{id}/backups             — take a snapshot
//	GET    /api/v1/orchestrator/resources/{id}/backups             — live snapshots, newest first
//	DELETE /api/v1/orchestrator/resources/{id}/backups/{backupId}  — delete one
//
// A snapshot's keep-by (expiresAt) is fixed when it is taken —
// backup.RetainUntil(createdAt, the resource's retentionDays) — so shortening
// the retention later does not destroy snapshots taken under the old promise.
// Listing sweeps the snapshots past their keep-by (pkg-go/backup's rule: past
// means now is after it), which is the retention gate: lapsed snapshots are
// never shown and never kept.
//
// The snapshot itself is the executor's job (rc-backup's reconcile loop,
// 06§4.1); this records it. Phase 1 records it COMPLETED at once, without a
// size (the executor reports none yet).

const backupStatusCompleted = "COMPLETED"

// backupTarget resolves {id} to the caller's EUBACKUP resource, answering the
// request itself (and returning ok=false) when it is not one.
func (s *lifecycleStore) backupTarget(w http.ResponseWriter, r *http.Request) (int64, *resource.Instance, bool) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return 0, nil, false
	}
	resID := r.PathValue("id")
	inst, err := s.ledger.Get(acct, resID)
	if err != nil {
		slog.Error("resource lookup failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.LookupFailed", 500, "资源查询失败")
		return 0, nil, false
	}
	if inst == nil {
		writeErr(w, "Resource.NotFound", 404, "资源不存在")
		return 0, nil, false
	}
	if inst.ProductCode != "eubackup" {
		writeErr(w, "Resource.BackupNotSupported", 400, "仅云备份(eubackup)资源支持备份快照")
		return 0, nil, false
	}
	return acct, inst, true
}

func (s *lifecycleStore) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	acct, inst, ok := s.backupTarget(w, r)
	if !ok {
		return
	}
	if inst.State != resource.StateRunning {
		writeErr(w, "Resource.InvalidState", 409, fmt.Sprintf("资源状态 %s 不允许备份,仅 RUNNING 可备份", inst.State))
		return
	}
	attrs, err := s.ledger.Attrs(acct, inst.ResourceID)
	if err != nil {
		slog.Error("resource attribute read failed", "resourceId", inst.ResourceID, "err", err)
		writeErr(w, "Resource.LookupFailed", 500, "资源查询失败")
		return
	}
	keep := defaultBackupKeepD
	if v, err := strconv.Atoi(attrs[attrRetentionDays]); err == nil && v > 0 {
		keep = v
	}
	now := s.clock()
	// DATETIME keeps whole seconds: store what the database will hand back, so
	// both ledgers answer the same createdAt/expiresAt.
	created := now.UTC().Truncate(time.Second)
	b := backupRecord{
		BackupID: newBackupID(now), ResourceID: inst.ResourceID, AccountID: acct,
		Status: backupStatusCompleted, CreatedAt: created, ExpiresAt: backup.RetainUntil(created, keep),
	}
	if err := s.ledger.CreateBackup(b); err != nil {
		slog.Error("backup record failed", "resourceId", inst.ResourceID, "err", err)
		writeErr(w, "Resource.BackupFailed", 500, "备份创建失败")
		return
	}
	writeJSON(w, "OK", backupView(b))
}

func (s *lifecycleStore) handleListBackups(w http.ResponseWriter, r *http.Request) {
	acct, inst, ok := s.backupTarget(w, r)
	if !ok {
		return
	}
	list, err := s.ledger.ListBackups(acct, inst.ResourceID, s.clock())
	if err != nil {
		slog.Error("backup list failed", "resourceId", inst.ResourceID, "err", err)
		writeErr(w, "Resource.BackupListFailed", 500, "备份列表读取失败")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, b := range list {
		out = append(out, backupView(b))
	}
	writeJSON(w, "OK", out)
}

func (s *lifecycleStore) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	acct, inst, ok := s.backupTarget(w, r)
	if !ok {
		return
	}
	backupID := r.PathValue("backupId")
	deleted, err := s.ledger.DeleteBackup(acct, inst.ResourceID, backupID)
	if err != nil {
		slog.Error("backup delete failed", "resourceId", inst.ResourceID, "backupId", backupID, "err", err)
		writeErr(w, "Resource.BackupDeleteFailed", 500, "备份删除失败")
		return
	}
	if !deleted {
		writeErr(w, "Resource.BackupNotFound", 404, "备份不存在")
		return
	}
	writeJSON(w, "OK", map[string]any{"backupId": backupID, "resourceId": inst.ResourceID, "deleted": true})
}

func backupView(b backupRecord) map[string]any {
	v := map[string]any{
		"backupId": b.BackupID, "resourceId": b.ResourceID, "status": b.Status,
		"createdAt": formatTime(b.CreatedAt), "expiresAt": formatTime(b.ExpiresAt),
	}
	if b.SizeGB > 0 {
		v["sizeGb"] = b.SizeGB
	}
	return v
}

// newBackupID is bak-{unix ms, 12 hex}{random, 8 hex}: unique, and in time
// order as a string, which breaks ties between snapshots taken in one second
// (created_at keeps whole seconds).
func newBackupID(now time.Time) string {
	var r [4]byte
	_, _ = rand.Read(r[:]) // crypto/rand.Read never fails (Go 1.24+)
	return fmt.Sprintf("bak-%012x%08x", now.UnixMilli(), binary.BigEndian.Uint32(r[:]))
}
