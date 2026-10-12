/**
 * EUBACKUP policy attributes — the client mirror of pkg-go/backup:
 *  - `schedule`: "daily HH:MM" (once a day, 24-hour clock, UTC+08:00 business
 *    clock) or "every Nh" (every N hours, 1 <= N <= 720); case-insensitive, a
 *    bare "HH:MM" is shorthand for daily. The canonical form is what is sent.
 *  - `retentionDays`: 1–3650 (svc-orchestrator's bound for eubackup).
 * Both are mutable after creation (PATCH .../attributes).
 */

export const SCHEDULE_MAX_HOURS = 720;
export const RETENTION_MIN = 1;
export const RETENTION_MAX = 3650;

export type BackupSchedule =
  | { kind: "every"; hours: number }
  | { kind: "daily"; hour: number; minute: number };

const pad = (n: number) => String(n).padStart(2, "0");

/** Parse a schedule string (pkg-go/backup.ParseSchedule grammar); null when invalid. */
export function parseSchedule(raw: string): BackupSchedule | null {
  const v = (raw ?? "").trim().toLowerCase();
  if (v.startsWith("every ")) {
    const m = /^([1-9][0-9]*)h$/.exec(v.slice(6).trim());
    if (!m) return null;
    const hours = Number(m[1]);
    return hours <= SCHEDULE_MAX_HOURS ? { kind: "every", hours } : null;
  }
  const clock = v.startsWith("daily ") ? v.slice(6).trim() : v;
  const m = /^([0-9]{2}):([0-9]{2})$/.exec(clock);
  if (!m) return null;
  const hour = Number(m[1]);
  const minute = Number(m[2]);
  if (hour > 23 || minute > 59) return null;
  return { kind: "daily", hour, minute };
}

/** Canonical text: "every 6h" / "daily 02:30". */
export function formatSchedule(s: BackupSchedule): string {
  return s.kind === "every" ? `every ${s.hours}h` : `daily ${pad(s.hour)}:${pad(s.minute)}`;
}

/** Interval form for the wizard's 备份间隔 (hours). */
export function everyHours(hours: number): string {
  return formatSchedule({ kind: "every", hours });
}

/** Human label: "每 6 小时" / "每天 02:30"; the raw text when unparsable, "—" when empty. */
export function describeSchedule(raw: string | undefined): string {
  if (!raw || !raw.trim()) return "—";
  const s = parseSchedule(raw);
  if (!s) return raw;
  return s.kind === "every" ? `每 ${s.hours} 小时` : `每天 ${pad(s.hour)}:${pad(s.minute)}`;
}

/** The schedule rule as a user-facing message, or null when valid. */
export function scheduleError(s: BackupSchedule): string | null {
  if (s.kind === "every") {
    if (!Number.isInteger(s.hours) || s.hours < 1 || s.hours > SCHEDULE_MAX_HOURS) {
      return `备份间隔须为 1-${SCHEDULE_MAX_HOURS} 小时的整数`;
    }
    return null;
  }
  if (!Number.isInteger(s.hour) || !Number.isInteger(s.minute) || s.hour < 0 || s.hour > 23 || s.minute < 0 || s.minute > 59) {
    return "请选择每天的备份时间";
  }
  return null;
}

/** Retention rule (1–3650 days), or null when valid. */
export function retentionError(days: unknown): string | null {
  if (typeof days !== "number" || !Number.isFinite(days)) return "请填写保留期";
  if (!Number.isInteger(days) || days < RETENTION_MIN || days > RETENTION_MAX) {
    return `保留期须为 ${RETENTION_MIN}-${RETENTION_MAX} 天的整数`;
  }
  return null;
}

/** "HH:MM" of a daily schedule (for a time picker). */
export function clockOf(s: BackupSchedule): string {
  return s.kind === "daily" ? `${pad(s.hour)}:${pad(s.minute)}` : "02:00";
}
