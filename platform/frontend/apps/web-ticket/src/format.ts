/**
 * Render an RFC3339 timestamp as a calendar date in the viewer's locale and
 * time zone. Slicing the string (`created_at.slice(0, 10)`) shows the UTC day,
 * which is the wrong date for anyone east/west of UTC near midnight
 * (2026-10-01T20:00:00Z is already 10-02 in Beijing).
 */
export function formatDate(
  iso: string | null | undefined,
  opts: { locale?: string; timeZone?: string } = {},
): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleDateString(opts.locale, {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    timeZone: opts.timeZone,
  });
}
