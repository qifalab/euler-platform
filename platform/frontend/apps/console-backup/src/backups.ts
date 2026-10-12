/**
 * EUBACKUP snapshots — svc-orchestrator's backup endpoints for a backup
 * policy resource:
 *   POST   /api/v1/orchestrator/resources/{id}/backups            — 立即备份
 *   GET    /api/v1/orchestrator/resources/{id}/backups            — snapshot list
 *   DELETE /api/v1/orchestrator/resources/{id}/backups/{backupId} — delete one
 * A snapshot's expiresAt is fixed when it is taken (createdAt + the policy's
 * retentionDays at that time); expired snapshots are cleaned up server-side.
 */
import type { SDK } from "@eu/sdk";

export interface BackupSnapshot {
  backupId: string;
  createdAt: string;
  status: string;
  expiresAt: string;
  sizeGb?: number;
}

const base = (resourceId: string) => `/api/v1/orchestrator/resources/${encodeURIComponent(resourceId)}/backups`;

/** Newest first (the backend orders the same way; this keeps it stable). */
export function sortSnapshots(list: BackupSnapshot[]): BackupSnapshot[] {
  return [...list].sort((a, b) => (Date.parse(b.createdAt) || 0) - (Date.parse(a.createdAt) || 0));
}

export async function listBackups(sdk: Pick<SDK, "get">, resourceId: string): Promise<BackupSnapshot[]> {
  const res = await sdk.get<BackupSnapshot[] | { backups?: BackupSnapshot[] }>(base(resourceId));
  const data = res.data;
  const list = Array.isArray(data) ? data : (data?.backups ?? []);
  return sortSnapshots(list);
}

export async function createBackup(sdk: Pick<SDK, "post">, resourceId: string): Promise<BackupSnapshot> {
  const res = await sdk.post<BackupSnapshot>(base(resourceId));
  return res.data;
}

export async function deleteBackup(sdk: Pick<SDK, "del">, resourceId: string, backupId: string): Promise<void> {
  await sdk.del(`${base(resourceId)}/${encodeURIComponent(backupId)}`);
}

/** Display label of a snapshot status. */
export function snapshotStatusLabel(status: string): string {
  switch (String(status).toUpperCase()) {
    case "COMPLETED": return "已完成";
    case "CREATING":
    case "RUNNING": return "备份中";
    case "FAILED": return "失败";
    case "EXPIRED": return "已过期";
    default: return status || "—";
  }
}

/** User-facing message for a failed backup call. */
export function backupErrorMessage(e: unknown, action: string): string {
  const err = (e ?? {}) as { message?: string; code?: string; status?: number };
  const msg = err.message && err.message !== "request failed" ? err.message : err.code ?? "";
  if (err.status === 409) return `${action}失败:策略当前状态不允许该操作${msg ? `(${msg})` : ""}`;
  if (err.status === 404) return `${action}失败:策略或快照不存在`;
  return `${action}失败:${msg || "请求失败"}`;
}
