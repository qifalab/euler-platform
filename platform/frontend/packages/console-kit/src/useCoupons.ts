/**
 * useCoupons — the usable coupons for the order a buy wizard is about to
 * place, plus the customer's selection (see coupons.ts for the contract).
 *
 * The list is re-fetched (debounced) whenever the order context — product,
 * SKU, charge type, amount — changes, since `applicable` depends on all of
 * them; a stale response never overwrites a newer one. Selected coupons that
 * stop being applicable are dropped. Postpaid orders take no coupons: the list
 * and the selection are cleared and nothing is fetched.
 */
import { computed, getCurrentScope, onScopeDispose, ref, watch } from "vue";
import type { SDK } from "@eu/sdk";
import {
  chargeTypeTakesCoupons,
  couponQueryString,
  fetchCoupons,
  pruneSelection,
  toggleCoupon,
  type Coupon,
  type CouponContext,
} from "./coupons";

export interface UseCouponsOptions {
  sdk: Pick<SDK, "get">;
  /** Reactive getter of the order context (read inside a watcher). */
  context: () => CouponContext;
  /** Debounce of a context change, ms (default 300). */
  debounceMs?: number;
}

function errMessage(e: unknown): string {
  const msg = (e as { message?: string } | null)?.message;
  return msg && msg !== "request failed" ? msg : "加载优惠券失败";
}

export function useCoupons(opts: UseCouponsOptions) {
  const debounceMs = opts.debounceMs ?? 300;
  const coupons = ref<Coupon[]>([]);
  const loading = ref(false);
  const error = ref<string | null>(null);
  const selectedIds = ref<string[]>([]);

  const enabled = computed(() => chargeTypeTakesCoupons(opts.context().chargeType));
  /** Coupons that would deduct from this order (applicable unknown = shown). */
  const usable = computed(() => coupons.value.filter((c) => c.applicable !== false));
  /** Usable-status coupons the order context rules out (scope, threshold, …). */
  const inapplicableCount = computed(() => coupons.value.length - usable.value.length);
  const selected = computed(() => {
    const byId = new Map(coupons.value.map((c) => [c.couponId, c] as const));
    return selectedIds.value.map((id) => byId.get(id)).filter((c): c is Coupon => !!c);
  });
  /** What an order create sends: nothing for a postpaid order. */
  const couponIds = computed(() => (enabled.value ? [...selectedIds.value] : []));

  let seq = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;
  function clearTimer() {
    if (timer) {
      clearTimeout(timer);
      timer = null;
    }
  }

  async function refresh(): Promise<void> {
    clearTimer();
    const ctx = opts.context();
    const my = ++seq;
    if (!chargeTypeTakesCoupons(ctx.chargeType) || !ctx.productCode) {
      coupons.value = [];
      selectedIds.value = [];
      loading.value = false;
      error.value = null;
      return;
    }
    loading.value = true;
    error.value = null;
    try {
      const list = await fetchCoupons(opts.sdk, { status: "usable", ...ctx });
      if (my !== seq) return;
      coupons.value = list;
      selectedIds.value = pruneSelection(selectedIds.value, list);
    } catch (e) {
      if (my !== seq) return;
      coupons.value = [];
      // Unverifiable coupons are not sent: the order would only 400.
      selectedIds.value = [];
      error.value = errMessage(e);
    } finally {
      if (my === seq) loading.value = false;
    }
  }

  function schedule() {
    clearTimer();
    timer = setTimeout(() => void refresh(), debounceMs);
  }

  // The context key: re-fetch only when something the server filters on changes.
  watch(() => couponQueryString({ status: "usable", ...opts.context() }), schedule, { immediate: true });

  if (getCurrentScope()) onScopeDispose(() => {
    clearTimer();
    seq++;
  });

  /** Select / deselect a coupon; returns why it was refused, or null. */
  function toggle(couponId: string): string | null {
    const candidate = coupons.value.find((c) => c.couponId === couponId);
    if (!candidate) return "优惠券不存在或已失效";
    const res = toggleCoupon(selectedIds.value, candidate, coupons.value);
    selectedIds.value = res.ids;
    return res.rejected ?? null;
  }

  /** Replace the selection (e.g. from a v-model), keeping only usable ids. */
  function setSelection(ids: readonly string[]) {
    const next = coupons.value.length ? pruneSelection(ids, coupons.value) : [...ids];
    if (next.length !== selectedIds.value.length || next.some((id, i) => id !== selectedIds.value[i])) {
      selectedIds.value = next;
    }
  }

  function clear() {
    selectedIds.value = [];
  }

  return {
    coupons, usable, inapplicableCount, loading, error, enabled,
    selectedIds, selected, couponIds,
    refresh, toggle, setSelection, clear,
  };
}
