/**
 * 卡券页:按状态分组与各券种文案。
 */
import { describe, it, expect } from "vitest";
import type { Coupon } from "@eu/console-kit";
import { groupCoupons, orderAmountsView, toCouponRow } from "./couponView";

function coupon(id: string, status: string, extra: Partial<Coupon> = {}): Coupon {
  return {
    couponId: id, kind: "VOUCHER", faceValue: "50", remainValue: "30", productCodes: [], skuCodes: [],
    source: "TRIAL", expireAt: "2026-12-31T00:00:00Z", status, ...extra,
  };
}

describe("groupCoupons", () => {
  it("splits by status and orders by expiry", () => {
    const g = groupCoupons([
      coupon("a", "USABLE", { expireAt: "2027-01-02T00:00:00Z" }),
      coupon("b", "USABLE", { expireAt: "2026-11-01T00:00:00Z" }),
      coupon("c", "LOCKED", { lockedOrderId: 42 }),
      coupon("d", "USED"),
      coupon("e", "EXPIRED"),
      coupon("f", "WEIRD"),
    ]);
    expect(g.USABLE.map((c) => c.couponId)).toEqual(["b", "a"]);
    expect(g.LOCKED.map((c) => c.couponId)).toEqual(["c"]);
    expect(g.USED.map((c) => c.couponId)).toEqual(["d"]);
    expect(g.EXPIRED.map((c) => c.couponId)).toEqual(["e"]);
  });
});

describe("toCouponRow", () => {
  const now = new Date("2026-12-28T00:00:00Z");
  it("renders kind-specific text, scope, source and the locking order", () => {
    const v = toCouponRow(coupon("v", "USABLE"), (x) => x, now);
    expect(v.kindLabel).toBe("代金券");
    expect(v.value).toBe("余额 ¥30.00 / 面值 ¥50.00");
    expect(v.scope).toBe("全部产品");
    expect(v.source).toBe("免费试用");
    expect(v.expiresSoon).toBe(true);
    expect(v.lockedOrderId).toBe("—");

    const f = toCouponRow(coupon("f", "LOCKED", { kind: "FULL_REDUCTION", thresholdAmount: "200", faceValue: "20", lockedOrderId: 9, productCodes: ["euecs"] }), (x) => (x === "euecs" ? "云服务器" : x), now);
    expect(f.kindLabel).toBe("满减券");
    expect(f.value).toBe("满200.00减20.00");
    expect(f.scope).toBe("限 云服务器");
    expect(f.lockedOrderId).toBe("9");
    expect(f.expiresSoon).toBe(false);

    const d = toCouponRow(coupon("d", "USED", { kind: "DISCOUNT", discountRateBp: 8500, capAmount: "50" }), undefined, now);
    expect(d.kindLabel).toBe("折扣券");
    expect(d.value).toBe("8.5折(最高抵扣 ¥50.00)");
  });
});

describe("orderAmountsView", () => {
  it("shows list / deduction / payable, falling back for pre-coupon orders", () => {
    expect(orderAmountsView({ listAmount: "100", couponDeduction: "20.5", payableAmount: "79.5", couponIds: ["a", "b"] }))
      .toEqual({ list: "100.00", deduction: "20.50", payable: "79.50", couponCount: 2 });
    expect(orderAmountsView({ listAmount: "100", couponDeduction: "0", payableAmount: "100", couponIds: [] }))
      .toEqual({ list: "100.00", deduction: "—", payable: "100.00", couponCount: 0 });
    expect(orderAmountsView({ payableAmount: "12.3" })).toEqual({ list: "12.30", deduction: "—", payable: "12.30", couponCount: 0 });
    expect(orderAmountsView({})).toEqual({ list: "—", deduction: "—", payable: "—", couponCount: 0 });
  });
});
