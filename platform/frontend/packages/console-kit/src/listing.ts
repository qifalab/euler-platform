/**
 * List-page helpers shared by the product sub-apps (02§7.2).
 */

/**
 * Resource lifecycle states that are still moving (pkg-go/resource lifecycle):
 * INIT → CREATING, UPGRADING, PREEMPTING, RELEASING. The console-bff passes the
 * orchestrator's state through verbatim, so list pages poll while any row is in
 * one of these — "Restoring" or "Creating"-only predicates missed most of them.
 */
export const TRANSITIONAL_STATES: readonly string[] = ["INIT", "CREATING", "UPGRADING", "PREEMPTING", "RELEASING"];

/** Whether a resource state (any casing, e.g. "Creating" / "CREATING") is transitional. */
export function isTransitionalState(state: unknown): boolean {
  return TRANSITIONAL_STATES.includes(String(state ?? "").trim().toUpperCase());
}

/**
 * Client-side pagination for endpoints that return the full list
 * (/console/resources, /api/v1/monitor/rules): slice the requested page and
 * keep `total` as the FULL count so the pager shows every page. Returning all
 * rows with total = length made every page render the whole list.
 */
export function paginate<T>(items: T[], params: { page?: unknown; pageSize?: unknown }): { items: T[]; total: number } {
  const size = Math.max(1, Math.floor(Number(params.pageSize) || 20));
  const total = items.length;
  const lastPage = Math.max(1, Math.ceil(total / size));
  // An out-of-range page (e.g. the list shrank under the user) clamps to the last.
  const page = Math.min(Math.max(1, Math.floor(Number(params.page) || 1)), lastPage);
  const start = (page - 1) * size;
  return { items: items.slice(start, start + size), total };
}

/**
 * The availability zone of a /console/resources row (`Zone`; `ZoneId` from
 * older rows), "" when the row carries none (a REGIONAL resource).
 */
export function resourceZone(row: unknown): string {
  if (!row || typeof row !== "object") return "";
  const r = row as Record<string, unknown>;
  const z = r.Zone || r.ZoneId || r.zone || r.zoneId;
  return typeof z === "string" ? z.trim() : "";
}

/** "name (id)" for a resource with a customer-given name, else the id alone. */
export function resourceDisplayName(id: string, name?: string | null): string {
  const n = (name ?? "").trim();
  return n && n !== id ? `${n} (${id})` : id;
}
