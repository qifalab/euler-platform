/**
 * AccessKey view model — svc-iam /api/ak (07§2.2):
 *   GET  /api/ak[?ramUserId=n]  → {accessKeys:[{akId, status 1|2, createdAt,
 *        lastUsed (null until first use), rotationGrace, graceUntil?, ramUserId?}], total}
 *   POST /api/ak {ramUserId?}   → the new key, secretKey returned ONCE
 *   POST /api/ak/{id}/disable|enable|rotate, DELETE /api/ak/{id}
 * The cap is 2 keys per identity (the account root, or each RAM user)
 * counting enabled AND disabled keys: a third create — or a rotation, which
 * mints a key — is 409 Auth.AKLimitExceeded until one is deleted.
 */
import { formatTime } from "./format";

export const MAX_AKS_PER_IDENTITY = 2;

export interface AccessKeyDto {
  akId?: string;
  secretKey?: string;
  createdAt?: string;
  lastUsed?: string | null;
  /** 1 enabled, 2 disabled (a superseded key past its grace reports 2). */
  status?: number;
  rotationGrace?: boolean;
  /** RFC3339: when the outgoing key of a rotation stops working (≤72h). */
  graceUntil?: string;
  ramUserId?: number;
}
export interface AccessKeyListDto { accessKeys?: AccessKeyDto[] | null; total?: number }

export type AkStatus = "active" | "inactive";

export interface AccessKeyView {
  /** Full AccessKeyId (32 chars), kept in memory for the create dialog. */
  id: string;
  /** Masked form shown in the table, e.g. LTAI5tGm****S0fH. */
  maskedId: string;
  /** SecretKey — populated only at creation, blanked once the dialog closes. */
  secret: string;
  createdAt: string;
  /** Local time, or 从未使用 while lastUsed is null. */
  lastUsed: string;
  status: AkStatus;
  /** True while this AK is the outgoing side of a rotation (07§2.2 grace). */
  rotationGrace: boolean;
  /** Local time the grace window closes; "" when not in grace. */
  graceUntil: string;
  ramUserId?: number;
}

export const NEVER_USED = "从未使用";

export function maskAccessKeyId(id: string): string {
  if (id.length <= 14) return id;
  return `${id.slice(0, 8)}****${id.slice(-6)}`;
}

/** lastUsed is null until the key signs its first request. */
export function lastUsedText(v: string | null | undefined): string {
  const t = formatTime(v);
  return t === "—" ? NEVER_USED : t;
}

export function toAccessKeyView(k: AccessKeyDto): AccessKeyView {
  const id = k.akId ?? "";
  const grace = k.rotationGrace ?? false;
  const graceUntil = grace && k.graceUntil ? formatTime(k.graceUntil) : "";
  return {
    id,
    maskedId: maskAccessKeyId(id),
    secret: k.secretKey ?? "",
    createdAt: formatTime(k.createdAt),
    lastUsed: lastUsedText(k.lastUsed),
    status: k.status === 2 ? "inactive" : "active",
    rotationGrace: grace,
    graceUntil: graceUntil === "—" ? "" : graceUntil,
    ...(typeof k.ramUserId === "number" ? { ramUserId: k.ramUserId } : {}),
  };
}

/** Every listed key (enabled or disabled) takes one of the identity's 2 slots. */
export function akCapReached(keys: readonly { status: AkStatus }[]): boolean {
  return keys.length >= MAX_AKS_PER_IDENTITY;
}

/** GET path: the account root's keys, or one RAM user's. */
export function akListPath(ramUserId?: number | null): string {
  return ramUserId ? `/api/ak?ramUserId=${encodeURIComponent(String(ramUserId))}` : "/api/ak";
}

/** POST /api/ak body. */
export function akCreateBody(ramUserId?: number | null): Record<string, number> {
  return ramUserId ? { ramUserId } : {};
}

/** User-facing message for a failed AK call. */
export function akErrorMessage(e: unknown): string {
  const err = (e ?? {}) as { code?: string; message?: string };
  switch (err.code) {
    case "Auth.AKLimitExceeded":
      return `每个身份最多 ${MAX_AKS_PER_IDENTITY} 个 AccessKey(含已禁用),请先删除一个`;
    case "Auth.AKDisabled":
      return "已禁用(或宽限期已过)的 AccessKey 不能轮换,请先启用";
    case "Auth.AKRotationInProgress":
      return "该 AccessKey 已在轮换宽限期内,请完成迁移后删除旧 AK";
    case "Auth.AKNotFound":
      return "AccessKey 不存在或已删除";
    case "Auth.RAMUserDisabled":
      return "子用户已禁用,不能创建 AccessKey";
    case "Auth.RAMUserNotFound":
      return "子用户不存在";
    default:
      return err.message || "请求失败,请稍后重试";
  }
}
