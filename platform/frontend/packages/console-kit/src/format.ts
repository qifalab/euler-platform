/**
 * Console display formatting shared by every product sub-app (02§7.3).
 *
 * Backends emit RFC3339 timestamps ("2026-09-15T14:00:00Z" or with an offset)
 * and "" for an unset time (svc-orchestrator renders zero times as ""; older
 * rows may still carry "0001-01-01T00:00:00Z"). Stripping the offset textually
 * showed UTC wall-clock with a stray "Z" as if it were local time, and printed
 * year-1 placeholders; this parses the instant and renders it in the viewer's
 * local time instead.
 */

/** Placeholder for an absent / unparseable / zero time. */
export const EMPTY_TIME = "—";

/**
 * Format an RFC3339 (or any Date-parseable) timestamp in the viewer's local
 * time, 24-hour clock (zh-CN). Returns "—" for empty, invalid, or pre-1970
 * (zero-value) input.
 */
export function formatTime(value: string | number | Date | null | undefined): string {
  const d = toInstant(value);
  return d ? d.toLocaleString("zh-CN", { hour12: false }) : EMPTY_TIME;
}

/**
 * The calendar date of a timestamp in the viewer's local time zone (zh-CN,
 * e.g. 2026/10/02). Slicing the RFC3339 string (`value.slice(0, 10)`) showed
 * the UTC day, which is the wrong date near midnight for anyone not on UTC.
 * "—" for empty / invalid / zero input, like formatTime. `timeZone` is for
 * tests; callers omit it.
 */
export function formatDate(
  value: string | number | Date | null | undefined,
  opts: { timeZone?: string } = {},
): string {
  const d = toInstant(value);
  if (!d) return EMPTY_TIME;
  return d.toLocaleDateString("zh-CN", { year: "numeric", month: "2-digit", day: "2-digit", timeZone: opts.timeZone });
}

/** Parse a timestamp; null for empty, invalid, or zero-value (pre-1970) input. */
function toInstant(value: string | number | Date | null | undefined): Date | null {
  if (value === null || value === undefined || value === "") return null;
  const d = value instanceof Date ? value : new Date(value);
  if (!Number.isFinite(d.getTime())) return null;
  // Zero values (Go's time.Time{} → 0001-01-01) and anything before the epoch
  // are placeholders, never a real lifecycle timestamp.
  if (d.getUTCFullYear() < 1970) return null;
  return d;
}
