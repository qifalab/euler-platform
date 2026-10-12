/**
 * @vitest-environment node
 *
 * useCoupons: debounced re-fetch on order-context changes, stale responses
 * ignored, selection pruned / cleared, nothing fetched for postpaid.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { nextTick, reactive } from "vue";
import { useCoupons } from "./useCoupons";
import type { Coupon } from "./coupons";

function coupon(id: string, kind = "VOUCHER", extra: Partial<Coupon> = {}): Coupon {
  return {
    couponId: id, kind, faceValue: "10", remainValue: "10", productCodes: [], skuCodes: [],
    source: "PROMO", expireAt: "2026-12-31T00:00:00Z", status: "USABLE", ...extra,
  };
}

const flush = async () => {
  await nextTick();
  await Promise.resolve();
  await Promise.resolve();
};

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe("useCoupons", () => {
  it("fetches usable coupons for the order context after the debounce", async () => {
    const get = vi.fn(async () => ({ data: { coupons: [coupon("c1"), coupon("c2", "VOUCHER", { applicable: false })], total: 2 } }));
    const ctx = reactive({ productCode: "euecs", skuCode: "ecs.c1", chargeType: "PREPAID", amount: "100" as string | null });
    const c = useCoupons({ sdk: { get: get as never }, context: () => ({ ...ctx }) });
    expect(get).not.toHaveBeenCalled();
    vi.advanceTimersByTime(300);
    await flush();
    expect(get).toHaveBeenCalledTimes(1);
    expect(get.mock.calls[0]).toEqual(["/api/v1/coupons?status=usable&productCode=euecs&skuCode=ecs.c1&chargeType=PREPAID&amount=100", undefined]);
    expect(c.usable.value.map((x) => x.couponId)).toEqual(["c1"]);
    expect(c.inapplicableCount.value).toBe(1);

    // Several quick changes → one request with the final context.
    ctx.amount = "150";
    await nextTick();
    ctx.amount = "200";
    await nextTick();
    vi.advanceTimersByTime(300);
    await flush();
    expect(get).toHaveBeenCalledTimes(2);
    expect(String((get.mock.calls[1] as unknown[])[0])).toContain("amount=200");
  });

  it("drops selected coupons that stop applying and ignores stale responses", async () => {
    let resolveFirst: (v: unknown) => void = () => {};
    const responses = [
      new Promise((r) => { resolveFirst = r; }),
      Promise.resolve({ data: { coupons: [coupon("c1"), coupon("d1", "DISCOUNT", { discountRateBp: 9000 })], total: 2 } }),
      Promise.resolve({ data: { coupons: [coupon("c1", "VOUCHER", { applicable: false }), coupon("d1", "DISCOUNT", { discountRateBp: 9000 })], total: 2 } }),
    ];
    const get = vi.fn(() => responses.shift());
    const ctx = reactive({ productCode: "euecs", chargeType: "PREPAID", amount: "10" });
    const c = useCoupons({ sdk: { get: get as never }, context: () => ({ ...ctx }) });
    vi.advanceTimersByTime(300);
    await flush();
    ctx.amount = "20";
    await nextTick();
    vi.advanceTimersByTime(300);
    await flush();
    // The first (slow) response lands after the second: ignored.
    resolveFirst({ data: { coupons: [coupon("old")], total: 1 } });
    await flush();
    expect(c.coupons.value.map((x) => x.couponId)).toEqual(["c1", "d1"]);

    expect(c.toggle("c1")).toBeNull();
    expect(c.toggle("d1")).toBeNull();
    expect(c.couponIds.value).toEqual(["c1", "d1"]);
    expect(c.toggle("missing")).toContain("不存在");

    ctx.amount = "5";
    await nextTick();
    vi.advanceTimersByTime(300);
    await flush();
    expect(c.couponIds.value).toEqual(["d1"]);
  });

  it("clears and never fetches for a postpaid order; clears the selection on a failed load", async () => {
    const get = vi.fn(async () => ({ data: { coupons: [coupon("c1")], total: 1 } }));
    const ctx = reactive({ productCode: "euecs", chargeType: "PREPAID" });
    const c = useCoupons({ sdk: { get: get as never }, context: () => ({ ...ctx }) });
    vi.advanceTimersByTime(300);
    await flush();
    c.toggle("c1");
    expect(c.couponIds.value).toEqual(["c1"]);

    ctx.chargeType = "POSTPAID";
    await nextTick();
    expect(c.enabled.value).toBe(false);
    expect(c.couponIds.value).toEqual([]);
    vi.advanceTimersByTime(300);
    await flush();
    expect(get).toHaveBeenCalledTimes(1);
    expect(c.selectedIds.value).toEqual([]);

    get.mockImplementationOnce(async () => { throw Object.assign(new Error("boom"), { status: 500 }); });
    ctx.chargeType = "PREPAID";
    await nextTick();
    vi.advanceTimersByTime(300);
    await flush();
    expect(c.error.value).toBe("boom");
    expect(c.coupons.value).toEqual([]);
  });

  it("setSelection keeps only usable ids once the list is loaded", async () => {
    const get = vi.fn(async () => ({ data: { coupons: [coupon("c1"), coupon("c2")], total: 2 } }));
    const c = useCoupons({ sdk: { get: get as never }, context: () => ({ productCode: "euecs", chargeType: "PREPAID" }) });
    c.setSelection(["c9"]);
    expect(c.selectedIds.value).toEqual(["c9"]);
    vi.advanceTimersByTime(300);
    await flush();
    expect(c.selectedIds.value).toEqual([]);
    c.setSelection(["c2", "zz"]);
    expect(c.selectedIds.value).toEqual(["c2"]);
    c.clear();
    expect(c.couponIds.value).toEqual([]);
  });
});
