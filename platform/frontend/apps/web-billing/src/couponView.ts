/** 卡券页的视图映射:svc-order GET /api/v1/coupons?status=all → 按状态分组的表格行。 */
import {
  couponKindLabel,
  couponScopeText,
  couponSourceLabel,
  couponValueText,
  couponExpiresSoon,
  formatTime,
  type Coupon,
} from "@eu/console-kit";

/** 页签顺序与 API 状态值。 */
export const COUPON_TABS = [
  { status: "USABLE", label: "可用" },
  { status: "LOCKED", label: "已锁定" },
  { status: "USED", label: "已使用" },
  { status: "EXPIRED", label: "已过期" },
] as const;
export type CouponTab = (typeof COUPON_TABS)[number]["status"];

export interface CouponRow {
  couponId: string;
  kind: string;
  kindLabel: string;
  value: string;
  scope: string;
  expireAt: string;
  expiresSoon: boolean;
  source: string;
  lockedOrderId: string;
}

export function toCouponRow(c: Coupon, productLabel: (code: string) => string = (x) => x, now: Date = new Date()): CouponRow {
  return {
    couponId: c.couponId,
    kind: String(c.kind),
    kindLabel: couponKindLabel(String(c.kind)),
    value: couponValueText(c),
    scope: couponScopeText(c, productLabel),
    expireAt: formatTime(c.expireAt),
    expiresSoon: c.status === "USABLE" && couponExpiresSoon(c, now),
    source: couponSourceLabel(c.source),
    lockedOrderId: c.lockedOrderId ? String(c.lockedOrderId) : "—",
  };
}

/** 按状态分组(未知状态忽略),组内按到期时间升序(svc-order 已按此排序,这里保底)。 */
export function groupCoupons(list: Coupon[]): Record<CouponTab, Coupon[]> {
  const out: Record<CouponTab, Coupon[]> = { USABLE: [], LOCKED: [], USED: [], EXPIRED: [] };
  for (const c of list) {
    const key = String(c.status).toUpperCase() as CouponTab;
    if (key in out) out[key].push(c);
  }
  const t = (c: Coupon) => {
    const n = new Date(c.expireAt).getTime();
    return Number.isFinite(n) ? n : Number.MAX_SAFE_INTEGER;
  };
  for (const k of Object.keys(out) as CouponTab[]) out[k].sort((a, b) => t(a) - t(b));
  return out;
}

/** 订单金额(svc-order 订单 JSON,元为单位的十进制字符串)。 */
export interface OrderAmountsDto {
  listAmount?: string;
  couponDeduction?: string;
  payableAmount?: string;
  couponIds?: string[] | null;
}

export interface OrderAmountsView {
  /** 订单原价(无 listAmount 的旧订单回退为实付)。 */
  list: string;
  /** 优惠券抵扣;未用券为 "—"。 */
  deduction: string;
  /** 实付(应付现金)。 */
  payable: string;
  couponCount: number;
}

function yuan(raw: string | undefined): string | null {
  if (raw === undefined || raw === null || raw === "") return null;
  const n = Number(raw);
  return Number.isFinite(n) ? n.toFixed(2) : null;
}

export function orderAmountsView(o: OrderAmountsDto): OrderAmountsView {
  const payable = yuan(o.payableAmount);
  const list = yuan(o.listAmount) ?? payable;
  const deduction = yuan(o.couponDeduction);
  return {
    list: list ?? "—",
    deduction: deduction !== null && Number(deduction) > 0 ? deduction : "—",
    payable: payable ?? "—",
    couponCount: Array.isArray(o.couponIds) ? o.couponIds.length : 0,
  };
}
