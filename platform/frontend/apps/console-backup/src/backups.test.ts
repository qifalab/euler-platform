/**
 * @vitest-environment node
 *
 * EUBACKUP snapshot calls: paths, ordering and error copy.
 */
import { describe, it, expect, vi } from "vitest";
import { backupErrorMessage, createBackup, deleteBackup, listBackups, snapshotStatusLabel } from "./backups";

const snap = (backupId: string, createdAt: string) => ({ backupId, createdAt, status: "COMPLETED", expiresAt: "2026-11-01T00:00:00Z" });

describe("backup snapshot calls", () => {
  it("lists newest first, accepting a bare array or {backups}", async () => {
    const get = vi.fn()
      .mockResolvedValueOnce({ data: [snap("bak-1", "2026-10-01T00:00:00Z"), snap("bak-2", "2026-10-02T00:00:00Z")] })
      .mockResolvedValueOnce({ data: { backups: [snap("bak-3", "2026-10-03T00:00:00Z")] } })
      .mockResolvedValueOnce({ data: null });
    expect((await listBackups({ get }, "eubackup-1")).map((s) => s.backupId)).toEqual(["bak-2", "bak-1"]);
    expect(get).toHaveBeenCalledWith("/api/v1/orchestrator/resources/eubackup-1/backups");
    expect((await listBackups({ get }, "eubackup-1")).map((s) => s.backupId)).toEqual(["bak-3"]);
    expect(await listBackups({ get }, "eubackup-1")).toEqual([]);
  });

  it("creates and deletes on the resource's backups path", async () => {
    const post = vi.fn().mockResolvedValue({ data: snap("bak-9", "2026-10-09T00:00:00Z") });
    expect((await createBackup({ post }, "eubackup-1")).backupId).toBe("bak-9");
    expect(post).toHaveBeenCalledWith("/api/v1/orchestrator/resources/eubackup-1/backups");
    const del = vi.fn().mockResolvedValue({ data: {} });
    await deleteBackup({ del }, "eubackup-1", "bak-9");
    expect(del).toHaveBeenCalledWith("/api/v1/orchestrator/resources/eubackup-1/backups/bak-9");
  });

  it("labels statuses and errors", () => {
    expect(snapshotStatusLabel("COMPLETED")).toBe("已完成");
    expect(snapshotStatusLabel("weird")).toBe("weird");
    expect(backupErrorMessage(Object.assign(new Error("not live"), { status: 409 }), "备份")).toContain("状态不允许");
    expect(backupErrorMessage(Object.assign(new Error("boom"), { status: 500 }), "删除快照")).toBe("删除快照失败:boom");
  });
});
