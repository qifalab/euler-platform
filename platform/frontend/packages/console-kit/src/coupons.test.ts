/**
 * @vitest-environment node
 *
 * Coupon helpers: query building, kind-specific display text and the
 * client-side mirror of svc-order's stacking rules.
 */
import { describe, it, expect, vi } from "vitest";
import {
  couponQueryString,
  fetchCoupons,
  formatDiscountRate,
  couponValueText,
  couponScopeText,
  couponSourceLabel,
  couponExpiresSoon,
  toggleCoupon,
  couponRejectReason,
  pruneSelection,
  chargeTypeTakesCoupons,
  isCouponInvalidError,
  isCouponExpiredError,
  type Coupon,
} from "./coupons";

function coupon(id: string, kind: string, extra: Partial<Coupon> = {}): Coupon {
  return {
    couponId: id, kind, faceValue: "20", remainValue: "20", productCodes: [], skuCodes: [],
    source: "PROMO", expireAt: "2026-12-31T00:00:00Z", status: "USABLE", ...extra,
  };
}

describe("couponQueryString / fetchCoupons", () => {
  it("drops empty fields and upper-cases the charge type", () => {
    expect(couponQueryString({ status: "usable", productCode: "euecs", skuCode: "", chargeType: "prepaid", amount: "120.50" }))
      .toBe("status=usable&productCode=euecs&chargeType=PREPAID&amount=120.50");
    expect(couponQueryString({ status: "all", amount: "abc" })).toBe("status=all");
    expect(couponQueryString({ amount: -1 })).toBe("");
    expect(couponQueryString({ amount: 0 })).toBe("amount=0");
  });

  it("GETs /api/v1/coupons and unwraps the list", async () => {
    const get = vi.fn(async () => ({ data: { coupons: [coupon("c1", "VOUCHER")], total: 1 } }));
    const list = await fetchCoupons({ get: get as never }, { status: "usable", productCode: "euecs" });
    expect(get).toHaveBeenCalledWith("/api/v1/coupons?status=usable&productCode=euecs", undefined);
    expect(list.map((c) => c.couponId)).toEqual(["c1"]);
    const empty = vi.fn(async () => ({ data: { coupons: null, total: 0 } }));
    expect(await fetchCoupons({ get: empty as never }, {})).toEqual([]);
    expect(empty).toHaveBeenCalledWith("/api/v1/coupons", undefined);
  });
});

describe("display", () => {
  it("formats discount rates as 折 from the paid share", () => {
    expect(formatDiscountRate(8500)).toBe("8.5折");
    expect(formatDiscountRate(8000)).toBe("8折");
    expect(formatDiscountRate(8850)).toBe("8.85折");
    expect(formatDiscountRate(9500)).toBe("9.5折");
    expect(formatDiscountRate(undefined)).toBe("—");
  });

  it("renders each kind's value", () => {
    expect(couponValueText(coupon("v", "VOUCHER", { faceValue: "50", remainValue: "50" }))).toBe("¥50.00");
    expect(couponValueText(coupon("v", "VOUCHER", { faceValue: "50", remainValue: "12.5" }))).toBe("余额 ¥12.50 / 面值 ¥50.00");
    expect(couponValueText(coupon("f", "FULL_REDUCTION", { thresholdAmount: "300", faceValue: "30" }))).toBe("满300.00减30.00");
    expect(couponValueText(coupon("d", "DISCOUNT", { discountRateBp: 8500 }))).toBe("8.5折");
    expect(couponValueText(coupon("d", "DISCOUNT", { discountRateBp: 9000, capAmount: "100" }))).toBe("9折(最高抵扣 ¥100.00)");
  });

  it("describes scope, source and expiry", () => {
    expect(couponScopeText(coupon("a", "VOUCHER"))).toBe("全部产品");
    expect(couponScopeText(coupon("a", "VOUCHER", { productCodes: ["euecs"], skuCodes: ["ecs.c1"] }), (c) => c.toUpperCase()))
      .toBe("限 EUECS · 规格 ecs.c1");
    expect(couponSourceLabel("TRIAL")).toBe("免费试用");
    expect(couponSourceLabel("XYZ")).toBe("XYZ");
    const now = new Date("2026-12-28T00:00:00Z");
    expect(couponExpiresSoon(coupon("a", "VOUCHER"), now)).toBe(true);
    expect(couponExpiresSoon(coupon("a", "VOUCHER", { expireAt: "2027-06-01T00:00:00Z" }), now)).toBe(false);
  });

  it("only prepaid charge types take coupons", () => {
    expect(chargeTypeTakesCoupons("PREPAID")).toBe(true);
    expect(chargeTypeTakesCoupons("prepaid")).toBe(true);
    expect(chargeTypeTakesCoupons("POSTPAID")).toBe(false);
    expect(chargeTypeTakesCoupons(undefined)).toBe(false);
  });
});

describe("stacking", () => {
  const all = [
    coupon("v1", "VOUCHER"), coupon("v2", "VOUCHER"), coupon("v3", "VOUCHER"), coupon("v4", "VOUCHER"), coupon("v5", "VOUCHER"),
    coupon("d1", "DISCOUNT", { discountRateBp: 9000 }), coupon("d2", "DISCOUNT", { discountRateBp: 8000 }),
    coupon("f1", "FULL_REDUCTION"), coupon("f2", "FULL_REDUCTION"),
    coupon("na", "VOUCHER", { applicable: false }),
  ];

  it("toggles off a selected coupon", () => {
    expect(toggleCoupon(["v1", "d1"], all[0], all)).toEqual({ ids: ["d1"] });
  });

  it("keeps one DISCOUNT and one FULL_REDUCTION, swapping a second of the same kind", () => {
    let ids = toggleCoupon([], all[5], all).ids;
    ids = toggleCoupon(ids, all[7], all).ids;
    ids = toggleCoupon(ids, all[0], all).ids;
    expect(ids).toEqual(["d1", "f1", "v1"]);
    ids = toggleCoupon(ids, all[6], all).ids; // d2 replaces d1
    expect(ids).toEqual(["f1", "v1", "d2"]);
    ids = toggleCoupon(ids, all[8], all).ids; // f2 replaces f1
    expect(ids).toEqual(["v1", "d2", "f2"]);
  });

  it("caps an order at 5 coupons and refuses inapplicable ones", () => {
    const five = ["v1", "v2", "v3", "v4", "v5"];
    const res = toggleCoupon(five, all[5], all);
    expect(res.ids).toEqual(five);
    expect(res.rejected).toContain("最多使用 5 张");
    // A swap does not grow the selection, so it is allowed at the cap.
    expect(toggleCoupon(["v1", "v2", "v3", "v4", "d1"], all[6], all).ids).toEqual(["v1", "v2", "v3", "v4", "d2"]);
    expect(couponRejectReason([], all[9])).toContain("不满足");
    expect(couponRejectReason([], coupon("x", "VOUCHER", { status: "LOCKED" }))).toContain("已锁定");
    expect(couponRejectReason([], coupon("bad id!", "VOUCHER"))).toContain("无效");
  });

  it("prunes ids that are no longer usable", () => {
    expect(pruneSelection(["v1", "gone", "na", "d1"], all)).toEqual(["v1", "d1"]);
  });
});

describe("order errors", () => {
  it("recognises the coupon error codes", () => {
    expect(isCouponInvalidError({ status: 400, code: "Order.CouponInvalid" })).toBe(true);
    expect(isCouponInvalidError({ status: 400, code: "Common.InvalidParameter" })).toBe(false);
    expect(isCouponExpiredError({ status: 409, code: "Order.CouponExpired" })).toBe(true);
    expect(isCouponExpiredError(null)).toBe(false);
  });
});
