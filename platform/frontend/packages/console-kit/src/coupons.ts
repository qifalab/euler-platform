/**
 * Coupons (卡券) — svc-order's coupon ledger as the console reads it:
 *
 *   GET /api/v1/coupons?status=usable|used|expired|locked|all
 *       [&productCode=&skuCode=&chargeType=&amount=<yuan>]
 *       → {coupons:[...], total}; with an order context each coupon carries
 *         `applicable` (would it deduct from such an order)
 *   GET /api/v1/coupons/{id} → one coupon (404 Order.CouponNotFound)
 *
 * A checkout passes the chosen ids as `couponIds` on POST /api/v1/orders. The
 * server is authoritative (400 Order.CouponInvalid names the coupon and the
 * reason); the client mirrors the stacking rules only for a better picker:
 * at most MAX_COUPONS_PER_ORDER coupons, one DISCOUNT, one FULL_REDUCTION and
 * any number of vouchers. Postpaid orders have no upfront amount, so coupons
 * only ever apply to PREPAID orders.
 */
import type { SDK } from "@eu/sdk";

export type CouponKind = "VOUCHER" | "FULL_REDUCTION" | "DISCOUNT";
export type CouponStatus = "USABLE" | "USED" | "EXPIRED" | "LOCKED";
export type CouponStatusFilter = "usable" | "used" | "expired" | "locked" | "all";

/** One coupon as GET /api/v1/coupons returns it (amounts are yuan decimals). */
export interface Coupon {
  couponId: string;
  kind: CouponKind | string;
  faceValue: string;
  remainValue: string;
  /** Products / SKUs the coupon is limited to; empty = any. */
  productCodes: string[];
  skuCodes: string[];
  /** Issuing channel: TRIAL (免费试用), PROMO (活动), … */
  source: string;
  /** RFC3339 UTC. */
  expireAt: string;
  status: CouponStatus | string;
  /** FULL_REDUCTION: the order amount the reduction needs (满 X). */
  thresholdAmount?: string;
  /** DISCOUNT: the share PAID, in basis points (8500 = 85折 = 15% off). */
  discountRateBp?: number;
  /** DISCOUNT: the most the coupon takes off one order. */
  capAmount?: string;
  /** LOCKED: the order holding the coupon. */
  lockedOrderId?: number;
  /** Present only when the list was asked with an order context. */
  applicable?: boolean;
}

export interface CouponListData {
  coupons: Coupon[] | null;
  total: number;
}

/** The order a coupon list is asked about (each field optional). */
export interface CouponContext {
  productCode?: string;
  skuCode?: string;
  /** PREPAID / POSTPAID (any case). */
  chargeType?: string;
  /** The order amount in yuan (the quote's payableAmount). */
  amount?: string | number | null;
}

export interface CouponQuery extends CouponContext {
  status?: CouponStatusFilter;
}

/** svc-order caps couponIds per order. */
export const MAX_COUPONS_PER_ORDER = 5;
/** A coupon id as svc-order accepts it in couponIds. */
export const COUPON_ID_PATTERN = /^[A-Za-z0-9._-]{1,32}$/;

/** Whether a charge type takes coupons (only prepaid orders have an upfront amount). */
export function chargeTypeTakesCoupons(chargeType: string | undefined | null): boolean {
  return String(chargeType ?? "").toUpperCase() === "PREPAID";
}

function amountParam(v: string | number | null | undefined): string {
  if (v === null || v === undefined || v === "") return "";
  const n = Number(v);
  if (!Number.isFinite(n) || n < 0) return "";
  return typeof v === "string" ? v.trim() : String(v);
}

/** The query string of GET /api/v1/coupons (empty fields are left out). */
export function couponQueryString(q: CouponQuery): string {
  const params = new URLSearchParams();
  if (q.status) params.set("status", q.status);
  if (q.productCode) params.set("productCode", q.productCode);
  if (q.skuCode) params.set("skuCode", q.skuCode);
  if (q.chargeType) params.set("chargeType", q.chargeType.toUpperCase());
  const amount = amountParam(q.amount);
  if (amount) params.set("amount", amount);
  return params.toString();
}

/** GET /api/v1/coupons — the account's coupons for a status / order context. */
export async function fetchCoupons(
  sdk: Pick<SDK, "get">,
  q: CouponQuery,
  init?: { signal?: AbortSignal },
): Promise<Coupon[]> {
  const qs = couponQueryString(q);
  const res = await sdk.get<CouponListData>(`/api/v1/coupons${qs ? `?${qs}` : ""}`, init);
  return (res.data?.coupons ?? []).filter((c) => c && typeof c.couponId === "string");
}

// --- display ---------------------------------------------------------------

const KIND_LABELS: Record<string, string> = {
  VOUCHER: "代金券",
  FULL_REDUCTION: "满减券",
  DISCOUNT: "折扣券",
};
const STATUS_LABELS: Record<string, string> = {
  USABLE: "可用",
  LOCKED: "已锁定",
  USED: "已使用",
  EXPIRED: "已过期",
};
const SOURCE_LABELS: Record<string, string> = {
  TRIAL: "免费试用",
  PROMO: "活动赠送",
  PROMOTION: "活动赠送",
  COMPENSATION: "补偿发放",
  MANUAL: "人工发放",
  OPERATOR: "平台发放",
};

export function couponKindLabel(kind: string): string {
  return KIND_LABELS[kind] ?? kind;
}

export function couponStatusLabel(status: string): string {
  return STATUS_LABELS[status] ?? status;
}

export function couponSourceLabel(source: string | undefined): string {
  if (!source) return "—";
  return SOURCE_LABELS[source.toUpperCase()] ?? source;
}

/** A yuan decimal as "¥12.50"; "—" when absent / not a number. */
export function formatYuan(v: string | number | null | undefined): string {
  if (v === null || v === undefined || v === "") return "—";
  const n = Number(v);
  return Number.isFinite(n) ? `¥${n.toFixed(2)}` : "—";
}

/** discountRateBp (the share paid) as 折: 8500 → "8.5折", 8000 → "8折", 8850 → "8.85折". */
export function formatDiscountRate(bp: number | null | undefined): string {
  if (bp === null || bp === undefined || !Number.isFinite(bp) || bp <= 0) return "—";
  const zhe = (bp / 1000).toFixed(2).replace(/\.?0+$/, "");
  return `${zhe}折`;
}

/** The kind-specific value of a coupon: ¥remain / 满X减Y / X折. */
export function couponValueText(c: Coupon): string {
  switch (c.kind) {
    case "FULL_REDUCTION":
      return `满${formatYuan(c.thresholdAmount).replace("¥", "")}减${formatYuan(c.faceValue).replace("¥", "")}`;
    case "DISCOUNT": {
      const rate = formatDiscountRate(c.discountRateBp);
      return c.capAmount && Number(c.capAmount) > 0 ? `${rate}(最高抵扣 ${formatYuan(c.capAmount)})` : rate;
    }
    default: {
      const remain = formatYuan(c.remainValue);
      const face = formatYuan(c.faceValue);
      return remain === face || remain === "—" ? face : `余额 ${remain} / 面值 ${face}`;
    }
  }
}

/** Where a coupon applies: "全部产品" or the listed products / SKUs. */
export function couponScopeText(c: Coupon, productLabel: (code: string) => string = (x) => x): string {
  const products = (c.productCodes ?? []).filter(Boolean);
  const skus = (c.skuCodes ?? []).filter(Boolean);
  if (!products.length && !skus.length) return "全部产品";
  const parts: string[] = [];
  if (products.length) parts.push(products.map(productLabel).join("、"));
  if (skus.length) parts.push(`规格 ${skus.join("、")}`);
  return `限 ${parts.join(" · ")}`;
}

/** Whether a coupon expires within `days` of `now` (and has not yet). */
export function couponExpiresSoon(c: Coupon, now: Date = new Date(), days = 7): boolean {
  const t = new Date(c.expireAt).getTime();
  if (!Number.isFinite(t)) return false;
  const left = t - now.getTime();
  return left > 0 && left <= days * 86_400_000;
}

// --- selection (client-side mirror of the server's stacking rules) ----------

/**
 * Why `candidate` cannot join `selected`, or null when it can. A second
 * DISCOUNT / FULL_REDUCTION is not refused here — toggleCoupon swaps it in.
 */
export function couponRejectReason(selected: Coupon[], candidate: Coupon): string | null {
  if (!COUPON_ID_PATTERN.test(candidate.couponId)) return "优惠券编号无效";
  if (candidate.applicable === false) return "该优惠券不满足本单使用条件";
  if (candidate.status && candidate.status !== "USABLE") return `该优惠券${couponStatusLabel(String(candidate.status))}`;
  const others = selected.filter((c) => c.couponId !== candidate.couponId);
  const swapped = candidate.kind === "DISCOUNT" || candidate.kind === "FULL_REDUCTION"
    ? others.filter((c) => c.kind !== candidate.kind)
    : others;
  if (swapped.length >= MAX_COUPONS_PER_ORDER) return `每笔订单最多使用 ${MAX_COUPONS_PER_ORDER} 张优惠券`;
  return null;
}

/**
 * Toggle `candidate` in the selection. Selecting a DISCOUNT (or FULL_REDUCTION)
 * replaces an already-selected one of the same kind — an order takes one of
 * each. Returns the new id list, or the reason it was refused.
 */
export function toggleCoupon(
  selectedIds: readonly string[],
  candidate: Coupon,
  all: readonly Coupon[],
): { ids: string[]; rejected?: string } {
  if (selectedIds.includes(candidate.couponId)) {
    return { ids: selectedIds.filter((id) => id !== candidate.couponId) };
  }
  const byId = new Map(all.map((c) => [c.couponId, c] as const));
  const selected = selectedIds.map((id) => byId.get(id)).filter((c): c is Coupon => !!c);
  const reason = couponRejectReason(selected, candidate);
  if (reason) return { ids: [...selectedIds], rejected: reason };
  const exclusive = candidate.kind === "DISCOUNT" || candidate.kind === "FULL_REDUCTION";
  const kept = exclusive ? selected.filter((c) => c.kind !== candidate.kind) : selected;
  return { ids: [...kept.map((c) => c.couponId), candidate.couponId] };
}

/** The selection with ids no longer in `available` dropped (order kept). */
export function pruneSelection(selectedIds: readonly string[], available: readonly Coupon[]): string[] {
  const ok = new Set(available.filter((c) => c.applicable !== false && (!c.status || c.status === "USABLE")).map((c) => c.couponId));
  return selectedIds.filter((id) => ok.has(id));
}

// --- order-side errors -------------------------------------------------------

interface ErrLike { code?: string; status?: number }

/** 400 Order.CouponInvalid on order create: the order was NOT created. */
export function isCouponInvalidError(e: unknown): boolean {
  return ((e ?? {}) as ErrLike).code === "Order.CouponInvalid";
}

/** 409 Order.CouponExpired on pay: the order must be cancelled and placed again. */
export function isCouponExpiredError(e: unknown): boolean {
  return ((e ?? {}) as ErrLike).code === "Order.CouponExpired";
}
