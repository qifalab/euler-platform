/**
 * @vitest-environment node
 *
 * useOrderCheckout: an issued order is resumed — never re-created — after a
 * pay or fulfil failure, so a retry can never charge twice.
 */
import { describe, it, expect, vi } from "vitest";

vi.mock("element-plus", () => ({ ElMessage: { error: vi.fn() } }));

import {
  useOrderCheckout, fulfilledMessage, orderAmountText, payIsFinal, fulfilIsFinal, fulfilParamsRejected,
  QUOTA_EXCEEDED_MESSAGE, type FulfilResult,
} from "./useOrderCheckout";

function apiError(status: number, code: string, message = "boom") {
  return Object.assign(new Error(message), { status, code });
}

const ok = (data: unknown = {}) => Promise.resolve({ data });
const accepted: FulfilResult = { resourceId: "euredis-cn-north-1-1-0000002a", state: "CREATING" };

function setup(post: ReturnType<typeof vi.fn>) {
  const onFulfilled = vi.fn();
  const notifyError = vi.fn();
  const c = useOrderCheckout({ sdk: { post: post as never }, onFulfilled, notifyError });
  const createOrder = vi.fn(async () => ({ orderId: 42, orderNo: "SO42" }));
  return { c, onFulfilled, notifyError, createOrder };
}

describe("useOrderCheckout", () => {
  it("pays then fulfils with the snapshotted body and the issued order", async () => {
    const post = vi.fn((path: string, _body?: unknown) => (path.endsWith("/fulfill") ? ok(accepted) : ok()));
    const { c, onFulfilled, createOrder } = setup(post);
    await c.checkout(createOrder, { productCode: "euredis", region: "cn-north-1" });
    expect(post.mock.calls.map((x) => x[0])).toEqual(["/api/v1/orders/42/pay", "/api/v1/orchestrator/fulfill"]);
    expect(post.mock.calls[1][1]).toEqual({ productCode: "euredis", region: "cn-north-1", orderId: 42, orderNo: "SO42" });
    expect(onFulfilled).toHaveBeenCalledWith(accepted, { orderId: 42, orderNo: "SO42" });
    expect(c.pending.value).toBeNull();
    expect(c.submitting.value).toBe(false);
  });

  it("keeps a paid order after a fulfil failure and retries fulfil only", async () => {
    let fulfilCalls = 0;
    const post = vi.fn((path: string, _body?: unknown) => {
      if (path.endsWith("/fulfill")) {
        fulfilCalls++;
        return fulfilCalls === 1 ? Promise.reject(apiError(500, "Provision.Failed")) : ok(accepted);
      }
      return ok();
    });
    const { c, onFulfilled, notifyError, createOrder } = setup(post);
    await c.checkout(createOrder, { region: "cn-north-1" });
    expect(c.pending.value).toMatchObject({ orderId: 42, stage: "fulfil" });
    expect(notifyError).toHaveBeenCalledTimes(1);

    // Re-submitting while an order is pending resumes it — no second order.
    await c.checkout(createOrder, { region: "cn-east-1" });
    expect(createOrder).toHaveBeenCalledTimes(1);
    const paths = post.mock.calls.map((x) => x[0]);
    expect(paths.filter((p) => p.endsWith("/pay"))).toHaveLength(1);
    // The retry carries the body captured with the order, not the edited form.
    expect(post.mock.calls[2][1]).toMatchObject({ region: "cn-north-1", orderId: 42 });
    expect(onFulfilled).toHaveBeenCalledTimes(1);
    expect(c.pending.value).toBeNull();
  });

  it("retries pay on the SAME order when pay fails transiently", async () => {
    let payCalls = 0;
    const post = vi.fn((path: string) => {
      if (path.endsWith("/pay")) {
        payCalls++;
        return payCalls === 1 ? Promise.reject(apiError(503, "Order.PaymentUnavailable")) : ok();
      }
      return ok(accepted);
    });
    const { c, onFulfilled, createOrder } = setup(post);
    await c.checkout(createOrder, {});
    expect(c.pending.value).toMatchObject({ orderId: 42, stage: "pay" });
    await c.resume();
    expect(createOrder).toHaveBeenCalledTimes(1);
    expect(post.mock.calls.map((x) => x[0])).toEqual([
      "/api/v1/orders/42/pay", "/api/v1/orders/42/pay", "/api/v1/orchestrator/fulfill",
    ]);
    expect(onFulfilled).toHaveBeenCalledTimes(1);
    expect(c.pending.value).toBeNull();
  });

  it("drops the order when provisioning failed and was refunded (409)", async () => {
    const post = vi.fn((path: string) =>
      path.endsWith("/fulfill") ? Promise.reject(apiError(409, "Provision.Failed", "开通失败,已进入退款")) : ok(),
    );
    const { c, onFulfilled, notifyError, createOrder } = setup(post);
    await c.checkout(createOrder, {});
    expect(c.pending.value).toBeNull();
    expect(onFulfilled).not.toHaveBeenCalled();
    expect(String(notifyError.mock.calls[0][0])).toContain("退款");
  });

  it("ends with the quota message when fulfil answers 409 Quota.Exceeded", async () => {
    const post = vi.fn((path: string) =>
      path.endsWith("/fulfill") ? Promise.reject(apiError(409, "Quota.Exceeded", "quota exceeded")) : ok(),
    );
    const { c, onFulfilled, notifyError, createOrder } = setup(post);
    await c.checkout(createOrder, { productCode: "euvpc" });
    expect(c.pending.value).toBeNull();
    expect(onFulfilled).not.toHaveBeenCalled();
    expect(notifyError).toHaveBeenCalledTimes(1);
    expect(String(notifyError.mock.calls[0][0])).toContain(QUOTA_EXCEEDED_MESSAGE);
    expect(String(notifyError.mock.calls[0][0])).toContain("SO42");
  });

  it("keeps the paid order when fulfil rejects the params, and retries corrected params on it", async () => {
    let fulfilCalls = 0;
    const post = vi.fn((path: string, _body?: unknown) => {
      if (path.endsWith("/fulfill")) {
        fulfilCalls++;
        return fulfilCalls === 1 ? Promise.reject(apiError(409, "Resource.InvalidAttribute", "cidrBlock overlaps")) : ok(accepted);
      }
      return ok();
    });
    const { c, onFulfilled, notifyError, createOrder } = setup(post);
    await c.checkout(createOrder, { productCode: "euvpc", region: "cn-north-1", params: { name: "a", cidrBlock: "10.0.0.0/16" } });
    expect(c.pending.value).toMatchObject({ orderId: 42, stage: "fulfil" });
    expect(String(notifyError.mock.calls[0][0])).toContain("cidrBlock overlaps");

    // Re-submit with a corrected CIDR and an edited region: only params change.
    await c.checkout(createOrder, { productCode: "euvpc", region: "cn-east-1", params: { name: "a", cidrBlock: "10.1.0.0/16" } });
    expect(createOrder).toHaveBeenCalledTimes(1);
    expect(post.mock.calls.filter((x) => String(x[0]).endsWith("/pay"))).toHaveLength(1);
    expect(post.mock.calls[2][1]).toEqual({
      productCode: "euvpc", region: "cn-north-1", params: { name: "a", cidrBlock: "10.1.0.0/16" }, orderId: 42, orderNo: "SO42",
    });
    expect(onFulfilled).toHaveBeenCalledTimes(1);
    expect(c.pending.value).toBeNull();
  });

  it("retryWithParams swaps params on the pending order only", async () => {
    let fulfilCalls = 0;
    const post = vi.fn((path: string, _body?: unknown) => {
      if (path.endsWith("/fulfill")) {
        fulfilCalls++;
        return fulfilCalls === 1 ? Promise.reject(apiError(400, "Resource.InvalidAttribute", "bad name")) : ok(accepted);
      }
      return ok();
    });
    const { c, createOrder } = setup(post);
    await c.retryWithParams({ name: "x" }); // nothing pending: no-op
    expect(post).not.toHaveBeenCalled();
    await c.checkout(createOrder, { region: "cn-north-1", params: { name: "" } });
    await c.retryWithParams({ name: "ok" });
    expect(post.mock.calls[2][1]).toMatchObject({ region: "cn-north-1", params: { name: "ok" }, orderId: 42 });
    expect(c.pending.value).toBeNull();
  });

  it("drops an order that can no longer be paid (409 / refund pending)", async () => {
    const post = vi.fn(() => Promise.reject(apiError(409, "Order.StateTransitionFailed")));
    const { c, createOrder } = setup(post);
    await c.checkout(createOrder, {});
    expect(c.pending.value).toBeNull();
  });

  it("reports an order-creation failure without leaving anything pending", async () => {
    const post = vi.fn(() => ok());
    const { c, notifyError } = setup(post);
    await c.checkout(async () => { throw apiError(400, "Catalog.ChargeTypeMismatch", "mismatch"); }, {});
    expect(post).not.toHaveBeenCalled();
    expect(c.pending.value).toBeNull();
    expect(notifyError).toHaveBeenCalledWith("mismatch");
  });
});

describe("error classification", () => {
  it("payIsFinal", () => {
    expect(payIsFinal(apiError(409, "Order.PaymentConflict"))).toBe(true);
    expect(payIsFinal(apiError(503, "Order.RefundPending"))).toBe(true);
    expect(payIsFinal(apiError(402, "Order.InsufficientBalance"))).toBe(false);
    expect(payIsFinal(apiError(0, "HTTP_0"))).toBe(false);
  });

  it("fulfilIsFinal", () => {
    expect(fulfilIsFinal(apiError(409, "Provision.Failed"))).toBe(true);
    expect(fulfilIsFinal(apiError(409, "Order.NotPaid"))).toBe(true);
    expect(fulfilIsFinal(apiError(409, "Resource.StateTransitionFailed"))).toBe(false);
    expect(fulfilIsFinal(apiError(500, "Provision.Failed"))).toBe(false);
    expect(fulfilIsFinal(apiError(503, "Order.LookupFailed"))).toBe(false);
    expect(fulfilIsFinal(apiError(409, "Quota.Exceeded"))).toBe(true);
    expect(fulfilIsFinal(apiError(409, "Resource.InvalidAttribute"))).toBe(false);
    expect(fulfilIsFinal(apiError(400, "Resource.InvalidAttribute"))).toBe(false);
  });

  it("fulfilParamsRejected", () => {
    expect(fulfilParamsRejected(apiError(400, "Resource.InvalidAttribute"))).toBe(true);
    expect(fulfilParamsRejected(apiError(409, "Resource.InvalidAttribute"))).toBe(true);
    expect(fulfilParamsRejected(apiError(400, "Common.InvalidParameter"))).toBe(true);
    expect(fulfilParamsRejected(apiError(409, "Resource.BucketExists"))).toBe(true);
    expect(fulfilParamsRejected(apiError(409, "Quota.Exceeded"))).toBe(false);
    expect(fulfilParamsRejected(apiError(500, "Resource.InvalidAttribute"))).toBe(false);
  });

  it("fulfilledMessage distinguishes accepted (202) from done", () => {
    expect(fulfilledMessage("实例", accepted)).toContain("正在开通");
    expect(fulfilledMessage("实例", { resourceId: "r1", state: "RUNNING" })).toContain("已创建");
    expect(fulfilledMessage("实例", accepted, { orderId: 1, orderNo: "SO1", payableAmount: "80", listAmount: "100", couponDeduction: "20" }))
      .toContain("实付 ¥80.00(原价 ¥100.00,优惠券抵扣 ¥20.00)");
  });

  it("orderAmountText", () => {
    expect(orderAmountText(null)).toBe("");
    expect(orderAmountText({ orderId: 1, orderNo: "SO1" })).toBe("");
    expect(orderAmountText({ orderId: 1, orderNo: "SO1", listAmount: "100", couponDeduction: "0", payableAmount: "100" })).toBe("实付 ¥100.00");
  });
});

describe("useOrderCheckout — coupons", () => {
  const amounts = { listAmount: "100.00", couponDeduction: "20.00", payableAmount: "80.00", couponIds: ["c1"] };

  it("hands the selected couponIds to createOrder and keeps the order amounts", async () => {
    const post = vi.fn((path: string, _body?: unknown) => (path.endsWith("/fulfill") ? ok(accepted) : ok()));
    const onFulfilled = vi.fn();
    const ids = ["c1"];
    const c = useOrderCheckout({ sdk: { post: post as never }, onFulfilled, notifyError: vi.fn(), couponIds: () => ids });
    const createOrder = vi.fn(async (_extras: unknown) => ({ orderId: 7, orderNo: "SO7", state: "PENDING_PAYMENT", ...amounts }));
    await c.checkout(createOrder, { region: "r" });
    expect(createOrder).toHaveBeenCalledWith({ couponIds: ["c1"] });
    expect(c.lastOrder.value).toEqual({ orderId: 7, orderNo: "SO7", ...amounts });
    expect(onFulfilled).toHaveBeenCalledWith(accepted, { orderId: 7, orderNo: "SO7", ...amounts });
    // The fulfil body never carries order amounts.
    expect(post.mock.calls[1][1]).toEqual({ region: "r", orderId: 7, orderNo: "SO7" });
  });

  it("sends no couponIds when none are selected (backward compatible)", async () => {
    const post = vi.fn((path: string, _body?: unknown) => (path.endsWith("/fulfill") ? ok(accepted) : ok()));
    const c = useOrderCheckout({ sdk: { post: post as never }, onFulfilled: vi.fn(), notifyError: vi.fn(), couponIds: () => [] });
    const createOrder = vi.fn(async (_extras: unknown) => ({ orderId: 8, orderNo: "SO8" }));
    await c.checkout(createOrder, {});
    expect(createOrder).toHaveBeenCalledWith({});
  });

  it("400 Order.CouponInvalid: no order, the verdict is kept and a resubmit creates the order", async () => {
    const post = vi.fn((path: string, _body?: unknown) => (path.endsWith("/fulfill") ? ok(accepted) : ok()));
    const notifyError = vi.fn();
    let ids = ["bad"];
    const c = useOrderCheckout({ sdk: { post: post as never }, onFulfilled: vi.fn(), notifyError, couponIds: () => ids });
    const createOrder = vi.fn(async (extras: { couponIds?: string[] }) => {
      if (extras.couponIds?.includes("bad")) throw apiError(400, "Order.CouponInvalid", "coupon bad expired");
      return { orderId: 9, orderNo: "SO9" };
    });
    const rev = c.couponRevision.value;
    await c.checkout(createOrder, {});
    expect(post).not.toHaveBeenCalled();
    expect(c.pending.value).toBeNull();
    expect(c.couponError.value).toBe("coupon bad expired");
    expect(c.couponRevision.value).toBe(rev + 1);
    expect(notifyError.mock.calls[0][0]).toContain("订单未创建");

    ids = ["good"];
    await c.checkout(createOrder, {});
    expect(createOrder).toHaveBeenCalledTimes(2);
    expect(c.couponError.value).toBe("");
    expect(post.mock.calls.map((x) => x[0])).toEqual(["/api/v1/orders/9/pay", "/api/v1/orchestrator/fulfill"]);
  });

  it("409 Order.CouponExpired at pay: the order is blocked until cancelled", async () => {
    const post = vi.fn((path: string, _body?: unknown) => {
      if (path.endsWith("/pay")) return Promise.reject(apiError(409, "Order.CouponExpired", "coupon c1 expired before payment"));
      return ok();
    });
    const notifyError = vi.fn();
    const c = useOrderCheckout({ sdk: { post: post as never }, onFulfilled: vi.fn(), notifyError, couponIds: () => ["c1"] });
    await c.checkout(async () => ({ orderId: 11, orderNo: "SO11", ...amounts }), {});
    expect(c.pending.value).toBeNull();
    expect(c.couponBlocked.value).toMatchObject({ orderId: 11, orderNo: "SO11" });
    expect(notifyError.mock.calls[0][0]).toContain("取消该订单");

    const rev = c.couponRevision.value;
    await expect(c.cancelBlockedOrder()).resolves.toBe(true);
    expect(post.mock.calls.at(-1)?.[0]).toBe("/api/v1/orders/11/cancel");
    expect(c.couponBlocked.value).toBeNull();
    expect(c.lastOrder.value).toBeNull();
    expect(c.couponRevision.value).toBe(rev + 1);
    await expect(c.cancelBlockedOrder()).resolves.toBe(false);
  });

  it("a failed cancel keeps the blocked order", async () => {
    const post = vi.fn((path: string, _body?: unknown) => {
      if (path.endsWith("/pay")) return Promise.reject(apiError(409, "Order.CouponExpired"));
      if (path.endsWith("/cancel")) return Promise.reject(apiError(503, "Order.LookupFailed", "down"));
      return ok();
    });
    const notifyError = vi.fn();
    const c = useOrderCheckout({ sdk: { post: post as never }, onFulfilled: vi.fn(), notifyError });
    await c.checkout(async () => ({ orderId: 12, orderNo: "SO12" }), {});
    await expect(c.cancelBlockedOrder()).resolves.toBe(false);
    expect(c.couponBlocked.value).toMatchObject({ orderId: 12 });
    expect(notifyError.mock.calls.at(-1)?.[0]).toContain("取消订单 SO12 失败");
    expect(c.submitting.value).toBe(false);
  });
});
