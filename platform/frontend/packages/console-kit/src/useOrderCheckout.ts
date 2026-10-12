/**
 * useOrderCheckout — the create → pay → fulfil chain every buy wizard runs
 * (02§7.4), with a resume path that never creates a second order.
 *
 * Once svc-order has issued an order, a failure after that point must be
 * retried ON THAT ORDER: re-submitting the wizard used to create and pay a NEW
 * order, so a fulfil failure (or a lost pay response) charged the customer
 * twice. The composable keeps the issued order — and the fulfil body
 * snapshotted at submit time, since the orchestrator refuses a fulfil whose
 * region/spec differ from the order — as `pending`, and `resume()` continues
 * from the step that failed:
 *  - pay is idempotent server-side (a settled order answers OK again, never a
 *    second debit), so a failed/unknown pay is retried on the same order;
 *  - fulfil is idempotent per order (the ledger arbitrates), and answers 202
 *    while provisioning is still in progress — any 2xx is "accepted".
 * Outcomes that no retry can change (order cancelled / not payable, provisioning
 * failed and refunded, quota exceeded — refunded too —, order not found) drop
 * `pending` so a fresh order can be placed.
 *
 * The fulfil body may carry `params` — customer parameters the orchestrator
 * validates and stores as resource attributes (name, cidrBlock, …). They are
 * not part of the order, so when fulfil rejects them (400/409
 * Resource.InvalidAttribute) the order stays pending and a corrected
 * `params` map is retried on the SAME order: re-submitting the wizard
 * (`checkout`) or `retryWithParams` swaps in the new params only — region,
 * spec and zone remain the ones the order was placed with.
 *
 * Coupons (svc-order 券): `couponIds` (option) is read when a NEW order is
 * placed and handed to `createOrder` as `extras.couponIds`, which the wizard
 * spreads into the POST /api/v1/orders body. A 400 Order.CouponInvalid means
 * the order was NOT created — `couponError` carries the server's verdict and
 * the customer changes the selection and submits again. A 409
 * Order.CouponExpired on pay cannot be retried: the order is kept as
 * `couponBlocked` and `cancelBlockedOrder()` cancels it (releasing its
 * coupons) so a fresh order can be placed. The created order's amounts
 * (listAmount / couponDeduction / payableAmount) are exposed as `lastOrder`.
 */
import { ref } from "vue";
import { ElMessage } from "element-plus";
import type { SDK } from "@eu/sdk";
import { formatYuan, isCouponExpiredError, isCouponInvalidError } from "./coupons";

/** The order svc-order issued (POST /api/v1/orders). */
export interface CheckoutOrder {
  orderId: number;
  orderNo: string;
  /** Yuan decimals from the order JSON, when svc-order returned them. */
  listAmount?: string;
  /** What the order's coupons take off listAmount. */
  couponDeduction?: string;
  /** The cash due (= listAmount − couponDeduction). */
  payableAmount?: string;
  couponIds?: string[];
}

/** Extra POST /api/v1/orders fields for a new order (spread into the body). */
export interface OrderExtras {
  couponIds?: string[];
}

/** POST /api/v1/orchestrator/fulfill response (200 done, 202 provisioning). */
export interface FulfilResult {
  resourceId: string;
  /** RUNNING when provisioned, CREATING while still provisioning (HTTP 202). */
  state: string;
  orderState?: string;
  billingStart?: string;
  expiredAt?: string;
}

/** An issued order that has not finished pay + fulfil. */
export interface PendingCheckout extends CheckoutOrder {
  /** The step to resume from. */
  stage: "pay" | "fulfil";
  /** Fulfil body (minus orderId/orderNo) captured when the order was placed. */
  fulfilBody: Record<string, unknown>;
}

export interface OrderCheckoutOptions {
  sdk: Pick<SDK, "post">;
  /** Called once fulfil is accepted (200 or 202). */
  onFulfilled: (result: FulfilResult, order: CheckoutOrder) => void;
  /** Error sink; defaults to ElMessage.error. */
  notifyError?: (message: string) => void;
  /**
   * Coupons for the next NEW order, read at create time (return [] for a
   * postpaid order). Absent = no coupons, the pre-coupon behaviour.
   */
  couponIds?: () => readonly string[] | undefined;
}

interface ErrLike { message?: string; code?: string; status?: number }

function errText(e: unknown): string {
  const err = (e ?? {}) as ErrLike;
  const msg = err.message ?? "";
  if (!msg || msg === "request failed") return err.code ?? "请求失败";
  return msg;
}

/**
 * Pay failures no retry can fix: the order is gone (404), not awaiting payment
 * / cancelled / unverifiable (409), or was cancelled with its refund pending.
 * Everything else (402 insufficient balance, 5xx, network) keeps the order so
 * the same order is paid on resume.
 */
export function payIsFinal(e: unknown): boolean {
  const err = (e ?? {}) as ErrLike;
  return err.status === 404 || err.status === 409 || err.code === "Order.RefundPending";
}

/** Final copy for a fulfil refused for want of quota (409 Quota.Exceeded). */
export const QUOTA_EXCEEDED_MESSAGE = "配额不足，订单将自动退款";

/**
 * Fulfil refused the customer parameters (`params`): 400/409
 * Resource.InvalidAttribute (e.g. a CIDR block overlapping an existing VPC),
 * 409 Resource.BucketExists (a bucket name taken in the region) or a 400
 * Common.InvalidParameter. The order is paid and still provisionable —
 * corrected params are retried on it, never on a new order.
 */
export function fulfilParamsRejected(e: unknown): boolean {
  const err = (e ?? {}) as ErrLike;
  if (err.code === "Resource.InvalidAttribute") return err.status === 400 || err.status === 409;
  if (err.code === "Resource.BucketExists") return err.status === 409;
  return err.status === 400 && err.code === "Common.InvalidParameter";
}

/**
 * Fulfil failures no retry can fix: 404, and every 409 except the transient
 * state-transition races (a retry re-drives those) and a parameter rejection
 * (fixable on the same order). 409 Provision.Failed means provisioning failed
 * and the order was refunded; 409 Quota.Exceeded refunds it as well.
 */
export function fulfilIsFinal(e: unknown): boolean {
  const err = (e ?? {}) as ErrLike;
  if (fulfilParamsRejected(e)) return false;
  if (err.status === 404) return true;
  if (err.status !== 409) return false;
  return err.code !== "Order.StateTransitionFailed" && err.code !== "Resource.StateTransitionFailed";
}

/** Success copy for a fulfil result: 202 (CREATING) is accepted, not done. */
export function fulfilledMessage(noun: string, result: FulfilResult, order?: CheckoutOrder): string {
  const paid = orderAmountText(order);
  const tail = paid ? `,${paid}` : "";
  if (result.state === "CREATING") return `${noun}已受理,正在开通:${result.resourceId}${tail}`;
  return `${noun}已创建:${result.resourceId}(状态 ${result.state})${tail}`;
}

/**
 * "实付 ¥80.00(原价 ¥100.00,优惠券抵扣 ¥20.00)" — or "" when the order
 * carries no amounts. The deduction part appears only when coupons took
 * something off.
 */
export function orderAmountText(order: CheckoutOrder | null | undefined): string {
  if (!order || order.payableAmount === undefined || order.payableAmount === "") return "";
  const payable = formatYuan(order.payableAmount);
  const deduction = Number(order.couponDeduction);
  if (Number.isFinite(deduction) && deduction > 0) {
    return `实付 ${payable}(原价 ${formatYuan(order.listAmount)},优惠券抵扣 ${formatYuan(order.couponDeduction)})`;
  }
  return `实付 ${payable}`;
}

/** The order identity plus whichever amounts the order JSON carried. */
function orderOf(src: CheckoutOrder): CheckoutOrder {
  const out: CheckoutOrder = { orderId: src.orderId, orderNo: src.orderNo };
  if (typeof src.listAmount === "string") out.listAmount = src.listAmount;
  if (typeof src.couponDeduction === "string") out.couponDeduction = src.couponDeduction;
  if (typeof src.payableAmount === "string") out.payableAmount = src.payableAmount;
  if (Array.isArray(src.couponIds)) out.couponIds = [...src.couponIds];
  return out;
}

export function useOrderCheckout(opts: OrderCheckoutOptions) {
  const submitting = ref(false);
  const pending = ref<PendingCheckout | null>(null);
  const notify = opts.notifyError ?? ((m: string) => void ElMessage.error(m));
  /** The most recently created order (with its amounts); kept after fulfil. */
  const lastOrder = ref<CheckoutOrder | null>(null);
  /** The server's verdict on the coupon selection (400 Order.CouponInvalid). */
  const couponError = ref("");
  /** An order whose coupon expired before payment: cancel it, then re-order. */
  const couponBlocked = ref<CheckoutOrder | null>(null);
  /** Bumped whenever the account's coupons may have changed (re-fetch them). */
  const couponRevision = ref(0);

  async function advance(order: PendingCheckout): Promise<void> {
    let cur = order;
    if (cur.stage === "pay") {
      try {
        await opts.sdk.post(`/api/v1/orders/${cur.orderId}/pay`);
      } catch (e) {
        if (isCouponExpiredError(e)) {
          pending.value = null;
          couponBlocked.value = orderOf(cur);
          couponRevision.value++;
          notify(`订单 ${cur.orderNo} 使用的优惠券已过期,无法支付:${errText(e)}。请取消该订单(释放优惠券)后重新下单`);
        } else if (payIsFinal(e)) {
          pending.value = null;
          notify(`订单 ${cur.orderNo} 无法支付:${errText(e)},请重新下单`);
        } else {
          pending.value = cur;
          notify(`订单 ${cur.orderNo} 支付未完成:${errText(e)},可点击"继续支付"在同一订单上重试(不会重复扣费)`);
        }
        return;
      }
      cur = { ...cur, stage: "fulfil" };
    }
    try {
      const res = await opts.sdk.post<FulfilResult>("/api/v1/orchestrator/fulfill", {
        ...cur.fulfilBody,
        orderId: cur.orderId,
        orderNo: cur.orderNo,
      });
      pending.value = null;
      couponRevision.value++;
      opts.onFulfilled(res.data, orderOf(cur));
    } catch (e) {
      if (fulfilIsFinal(e)) {
        pending.value = null;
        const err = (e ?? {}) as ErrLike;
        if (err.code === "Quota.Exceeded") notify(`订单 ${cur.orderNo} 开通失败:${QUOTA_EXCEEDED_MESSAGE}`);
        else if (err.code === "Provision.Failed") notify(`订单 ${cur.orderNo} 开通失败,已进入退款:${errText(e)}`);
        else notify(`订单 ${cur.orderNo} 无法开通:${errText(e)}`);
      } else if (fulfilParamsRejected(e)) {
        pending.value = cur;
        notify(`订单已支付(${cur.orderNo}),但开通参数未通过校验:${errText(e)}。请修改后重新提交,将在同一订单上重试(不会重复扣费)`);
      } else {
        pending.value = cur;
        notify(`订单已支付(${cur.orderNo}),但开通失败:${errText(e)},可点击"重试开通"`);
      }
    }
  }

  /**
   * Place a new order and run it through pay + fulfil. `createOrder` quotes and
   * POSTs /api/v1/orders; `fulfilBody` is the fulfil request without
   * orderId/orderNo. With an order already pending this resumes it instead —
   * a second order is never created while one is outstanding.
   */
  async function checkout(
    createOrder: (extras: OrderExtras) => Promise<CheckoutOrder>,
    fulfilBody: Record<string, unknown>,
  ): Promise<void> {
    if (submitting.value) return;
    if (pending.value) {
      return fulfilBody.params !== undefined ? retryWithParams(fulfilBody.params as Record<string, string>) : resume();
    }
    submitting.value = true;
    try {
      const ids = [...(opts.couponIds?.() ?? [])].filter((id) => typeof id === "string" && id !== "");
      const extras: OrderExtras = ids.length ? { couponIds: ids } : {};
      couponError.value = "";
      let created: CheckoutOrder;
      try {
        created = await createOrder(extras);
      } catch (e) {
        if (isCouponInvalidError(e)) {
          // Nothing was created: the customer changes the selection and resubmits.
          couponError.value = errText(e);
          couponRevision.value++;
          notify(`优惠券不可用:${errText(e)}。订单未创建,请调整所选优惠券后重新下单`);
        } else {
          notify(errText(e) || "创建失败");
        }
        return;
      }
      lastOrder.value = orderOf(created);
      await advance({ ...orderOf(created), stage: "pay", fulfilBody: { ...fulfilBody } });
    } finally {
      submitting.value = false;
    }
  }

  /**
   * Cancel the order whose coupon expired before payment (POST
   * /api/v1/orders/{id}/cancel releases its coupons). Resolves true once it is
   * cancelled — the wizard can then place a fresh order.
   */
  async function cancelBlockedOrder(): Promise<boolean> {
    const order = couponBlocked.value;
    if (!order || submitting.value) return false;
    submitting.value = true;
    try {
      await opts.sdk.post(`/api/v1/orders/${order.orderId}/cancel`);
      couponBlocked.value = null;
      if (lastOrder.value?.orderId === order.orderId) lastOrder.value = null;
      couponRevision.value++;
      return true;
    } catch (e) {
      notify(`取消订单 ${order.orderNo} 失败:${errText(e)}`);
      return false;
    } finally {
      submitting.value = false;
    }
  }

  async function run(order: PendingCheckout): Promise<void> {
    submitting.value = true;
    try {
      await advance(order);
    } finally {
      submitting.value = false;
    }
  }

  /** Continue the pending order from the step that failed. */
  async function resume(): Promise<void> {
    const order = pending.value;
    if (!order || submitting.value) return;
    await run(order);
  }

  /**
   * Continue the pending order with corrected `params` (resource attributes):
   * only `params` is replaced — the order-bound fields of the snapshotted
   * fulfil body are kept.
   */
  async function retryWithParams(params: Record<string, string>): Promise<void> {
    const order = pending.value;
    if (!order || submitting.value) return;
    const next: PendingCheckout = { ...order, fulfilBody: { ...order.fulfilBody, params: { ...params } } };
    pending.value = next;
    await run(next);
  }

  return {
    submitting, pending, checkout, resume, retryWithParams,
    lastOrder, couponError, couponBlocked, couponRevision, cancelBlockedOrder,
  };
}
