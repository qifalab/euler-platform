/**
 * Resource attributes (svc-orchestrator): customer-facing parameters stored on
 * a resource — set at creation through the fulfil `params` ({string:string})
 * and read back as `attributes` on the resource list/detail records.
 *
 *   POST  /api/v1/orchestrator/fulfill                  { ..., params: {k: v} }
 *   PATCH /api/v1/orchestrator/resources/{id}/attributes {"attributes": {k: v}}
 *
 * Only some keys are mutable after creation (name, the euas policy keys,
 * eubackup retentionDays/schedule, eulog storageGb/retentionDays); the PATCH
 * answers 400 Resource.AttributeImmutable / Resource.InvalidAttribute, or 409
 * when the resource is not live, and returns the updated detail.
 */
import type { SDK } from "@eu/sdk";

export type ResourceAttributes = Record<string, string>;

/** Upper bound of the `name` attribute (every product). */
export const NAME_MAX = 64;

/**
 * The attributes of a resource row: the orchestrator's camelCase `attributes`
 * or a BFF row's PascalCase `Attributes`. Non-string scalars are stringified;
 * anything else (missing, malformed) reads as {}.
 */
export function attributesOf(row: unknown): ResourceAttributes {
  if (!row || typeof row !== "object") return {};
  const r = row as Record<string, unknown>;
  const raw = r.attributes ?? r.Attributes;
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return {};
  const out: ResourceAttributes = {};
  for (const [k, v] of Object.entries(raw as Record<string, unknown>)) {
    if (typeof v === "string") out[k] = v;
    else if (typeof v === "number" || typeof v === "boolean") out[k] = String(v);
  }
  return out;
}

/** A numeric attribute, or `fallback` when it is absent or not a number. */
export function numberAttr(attrs: ResourceAttributes, key: string, fallback?: number): number | undefined {
  const raw = attrs[key];
  if (raw === undefined || raw.trim() === "") return fallback;
  const n = Number(raw);
  return Number.isFinite(n) ? n : fallback;
}

/**
 * Build the {string:string} map fulfil `params` / PATCH `attributes` take:
 * strings are trimmed, numbers/booleans stringified, and empty / absent values
 * dropped (an unset optional field is simply not sent).
 */
export function toParams(values: Record<string, unknown>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(values)) {
    if (v === undefined || v === null) continue;
    if (typeof v === "number") {
      if (Number.isFinite(v)) out[k] = String(v);
    } else if (typeof v === "boolean") {
      out[k] = String(v);
    } else if (typeof v === "string") {
      const s = v.trim();
      if (s !== "") out[k] = s;
    }
  }
  return out;
}

/** `name` rule (1–64 characters); an empty name passes unless required. */
export function validateResourceName(name: string, opts: { required?: boolean } = {}): string | null {
  const v = (name ?? "").trim();
  if (!v) return opts.required ? "请输入名称" : null;
  if ([...v].length > NAME_MAX) return `名称长度为 1-${NAME_MAX} 个字符`;
  return null;
}

/** An integer within [min, max], or a user-facing message naming `label`. */
export function validateIntRange(value: unknown, min: number, max: number, label: string): string | null {
  const n = typeof value === "string" && value.trim() !== "" ? Number(value) : value;
  if (typeof n !== "number" || !Number.isFinite(n)) return `请填写${label}`;
  if (!Number.isInteger(n)) return `${label}须为整数`;
  if (n < min || n > max) return `${label}须在 ${min}-${max} 之间`;
  return null;
}

/** User-facing message for a failed attribute PATCH (400/409 per the contract). */
export function attributeErrorMessage(e: unknown): string {
  const err = (e ?? {}) as { message?: string; code?: string; status?: number };
  const msg = err.message && err.message !== "request failed" ? err.message : "";
  if (err.code === "Resource.AttributeImmutable") return `该参数创建后不可修改${msg ? `:${msg}` : ""}`;
  if (err.code === "Resource.InvalidAttribute") return `参数不合法${msg ? `:${msg}` : ""}`;
  if (err.status === 409) return `资源当前状态不允许修改,请待其运行后重试${msg ? `(${msg})` : ""}`;
  if (err.status === 404 || err.code === "Resource.NotFound") return "资源不存在或已释放";
  return msg || err.code || "保存失败";
}

/** PATCH the given attributes; resolves with the updated resource detail. */
export async function patchResourceAttributes<T = unknown>(
  sdk: Pick<SDK, "patch">,
  resourceId: string,
  attributes: Record<string, unknown>,
): Promise<T> {
  const res = await sdk.patch<T>(
    `/api/v1/orchestrator/resources/${encodeURIComponent(resourceId)}/attributes`,
    { attributes: toParams(attributes) },
  );
  return res.data;
}
