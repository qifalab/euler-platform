/**
 * @vitest-environment jsdom
 *
 * CouponPicker: lists the applicable coupons for the order context, v-models
 * the selection (with the stacking rules), shows the server verdict, and
 * renders nothing for a postpaid order.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";

const ui = vi.hoisted(() => ({ warning: vi.fn() }));
vi.mock("element-plus", async (importOriginal) => {
  const mod = await importOriginal<typeof import("element-plus")>();
  return { ...mod, ElMessage: { success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: ui.warning } };
});

import ElementPlus from "element-plus";
import CouponPicker from "./CouponPicker.vue";
import type { Coupon } from "./coupons";

function coupon(id: string, kind: string, extra: Partial<Coupon> = {}): Coupon {
  return {
    couponId: id, kind, faceValue: "20", remainValue: "20", productCodes: [], skuCodes: [],
    source: "PROMO", expireAt: "2027-12-31T00:00:00Z", status: "USABLE", applicable: true, ...extra,
  };
}

const list = [
  coupon("v1", "VOUCHER"),
  coupon("d1", "DISCOUNT", { discountRateBp: 9000 }),
  coupon("d2", "DISCOUNT", { discountRateBp: 8500 }),
  coupon("x1", "VOUCHER", { applicable: false }),
];

function mountPicker(props: Record<string, unknown> = {}) {
  const get = vi.fn(async () => ({ data: { coupons: list, total: list.length } }));
  const w = mount(CouponPicker, {
    props: { modelValue: [], sdk: { get: get as never }, productCode: "euecs", skuCode: "ecs.c1", chargeType: "PREPAID", amount: "100", ...props },
    global: { plugins: [ElementPlus] },
  });
  return { w, get };
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

async function settle() {
  vi.advanceTimersByTime(400);
  await flushPromises();
}

describe("CouponPicker", () => {
  it("lists applicable coupons and emits the selection, swapping a second discount", async () => {
    const { w, get } = mountPicker();
    await settle();
    expect(String((get.mock.calls[0] as unknown[])[0])).toBe(
      "/api/v1/coupons?status=usable&productCode=euecs&skuCode=ecs.c1&chargeType=PREPAID&amount=100",
    );
    const items = w.findAll(".eu-cp-item");
    expect(items).toHaveLength(3);
    expect(w.text()).toContain("另有 1 张优惠券不满足本单使用条件");
    expect(w.text()).toContain("9折");

    await items[0].trigger("click");
    await items[1].trigger("click");
    await items[2].trigger("click");
    const emitted = w.emitted("update:modelValue") ?? [];
    expect(emitted.at(-1)?.[0]).toEqual(["v1", "d2"]);
  });

  it("shows the server verdict and re-fetches on a new refresh key", async () => {
    const { w, get } = mountPicker({ error: "coupon v1 expired" });
    await settle();
    expect(w.find(".eu-cp-alert").text()).toContain("coupon v1 expired");
    await w.setProps({ refreshKey: 1 });
    await flushPromises();
    expect(get).toHaveBeenCalledTimes(2);
  });

  it("does not toggle while disabled", async () => {
    const { w } = mountPicker({ disabled: true, modelValue: [] });
    await settle();
    await w.findAll(".eu-cp-item")[0].trigger("click");
    expect(w.emitted("update:modelValue")).toBeUndefined();
  });

  it("renders nothing and fetches nothing for a postpaid order", async () => {
    const { w, get } = mountPicker({ chargeType: "POSTPAID" });
    await settle();
    expect(w.find(".eu-coupon-picker").exists()).toBe(false);
    expect(get).not.toHaveBeenCalled();
  });
});
