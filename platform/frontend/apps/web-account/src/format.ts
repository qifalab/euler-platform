/**
 * Timestamp display for the account center. svc-iam returns RFC3339 UTC
 * ("2026-10-01T12:00:00Z"); showing that raw (or slicing it) printed UTC
 * wall-clock with "T"/"Z" artefacts. Render the instant in the viewer's local
 * time instead, the same way the console (@eu/console-kit formatTime) does.
 */

/** Placeholder for an absent / unparseable / zero time. */
export const EMPTY_TIME = "—";

/** RFC3339 → viewer-local "2026/10/01 20:00:00"; "—" for empty, invalid or zero values. */
export function formatTime(value: string | null | undefined, opts: { timeZone?: string } = {}): string {
  if (!value) return EMPTY_TIME;
  const d = new Date(value);
  // Go zero times (0001-01-01) and pre-epoch values are placeholders.
  if (!Number.isFinite(d.getTime()) || d.getUTCFullYear() < 1970) return EMPTY_TIME;
  return d.toLocaleString("zh-CN", { hour12: false, timeZone: opts.timeZone });
}
