/**
 * EULOG quota / retention attributes — the client mirror of svc-orchestrator's
 * eulog rules: storageGb > 0 (this console sells 10–100000 GB, the wizard's
 * range) and retentionDays 1–3650. Both are mutable after creation through
 * PATCH /api/v1/orchestrator/resources/{id}/attributes.
 */
import { validateIntRange } from "@eu/console-kit";

export const STORAGE_MIN_GB = 10;
export const STORAGE_MAX_GB = 100_000;
export const RETENTION_MIN = 1;
export const RETENTION_MAX = 3650;

export function storageError(gb: unknown): string | null {
  return validateIntRange(gb, STORAGE_MIN_GB, STORAGE_MAX_GB, "存储容量(GB)");
}

export function retentionError(days: unknown): string | null {
  return validateIntRange(days, RETENTION_MIN, RETENTION_MAX, "保留期(天)");
}

/** Fulfil `params` values for a new log service. */
export function logAttributes(v: { storageGb: number; retentionDays: number }): { storageGb: number; retentionDays: number } {
  return { storageGb: v.storageGb, retentionDays: v.retentionDays };
}
