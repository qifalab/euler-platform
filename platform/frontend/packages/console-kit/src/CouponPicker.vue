<script setup lang="ts">
/**
 * CouponPicker — 优惠券 selection for a buy wizard's PREPAID order.
 *
 * Lists the account's usable coupons that apply to the order being placed
 * (GET /api/v1/coupons?status=usable&productCode&skuCode&chargeType&amount,
 * re-fetched debounced as the context changes) and v-models the chosen ids,
 * which the wizard hands to useOrderCheckout (`couponIds` option). The
 * stacking rules are mirrored for UX (≤5 coupons, one 折扣券, one 满减券,
 * vouchers stack); svc-order stays authoritative — its 400
 * Order.CouponInvalid verdict is passed back in as `error`. Renders nothing
 * for a postpaid order (no upfront amount, so no coupons).
 */
import { computed, watch } from "vue";
import { ElAlert, ElButton, ElCheckbox, ElMessage, ElTag } from "element-plus";
import type { SDK } from "@eu/sdk";
import { useCoupons } from "./useCoupons";
import {
  couponExpiresSoon,
  couponKindLabel,
  couponScopeText,
  couponValueText,
  MAX_COUPONS_PER_ORDER,
  type Coupon,
} from "./coupons";
import { formatDate } from "./format";

const props = defineProps<{
  /** Selected coupon ids (v-model). */
  modelValue: string[];
  sdk: Pick<SDK, "get">;
  productCode: string;
  skuCode?: string;
  /** PREPAID / POSTPAID (any case); anything but PREPAID hides the picker. */
  chargeType?: string;
  /** The order amount in yuan (the quote's payableAmount). */
  amount?: string | number | null;
  /** Lock the selection (e.g. while an order is pending: its coupons are bound). */
  disabled?: boolean;
  /** The server's verdict on the last order attempt (Order.CouponInvalid). */
  error?: string;
  /** Change it to force a re-fetch (useOrderCheckout's couponRevision). */
  refreshKey?: number;
  /** Product code → display name for the scope line. */
  productLabel?: (code: string) => string;
}>();
const emit = defineEmits<{ "update:modelValue": [string[]] }>();

const c = useCoupons({
  sdk: props.sdk,
  context: () => ({
    productCode: props.productCode,
    skuCode: props.skuCode,
    chargeType: props.chargeType,
    amount: props.amount,
  }),
});
const { usable, inapplicableCount, loading, error: loadError, enabled, selectedIds } = c;

function same(a: readonly string[], b: readonly string[] | undefined): boolean {
  return !!b && a.length === b.length && a.every((x, i) => x === b[i]);
}
watch(() => props.modelValue, (ids) => c.setSelection(ids ?? []), { immediate: true });
watch(c.couponIds, (ids) => {
  if (!same(ids, props.modelValue)) emit("update:modelValue", ids);
});
watch(() => props.refreshKey, () => void c.refresh());

function onToggle(coupon: Coupon) {
  if (props.disabled) return;
  const reason = c.toggle(coupon.couponId);
  if (reason) ElMessage.warning(reason);
}

const label = computed(() => props.productLabel ?? ((code: string) => code));
const kindType = (kind: string) => (kind === "DISCOUNT" ? "warning" : kind === "FULL_REDUCTION" ? "danger" : "success");
</script>

<template>
  <div v-if="enabled" class="eu-coupon-picker">
    <ElAlert v-if="error" type="error" :title="error" :closable="false" show-icon class="eu-cp-alert" />
    <p v-if="loading && !usable.length" class="eu-cp-muted">正在加载可用优惠券…</p>
    <p v-else-if="loadError" class="eu-cp-muted">
      优惠券加载失败:{{ loadError }}
      <ElButton link type="primary" size="small" @click="c.refresh()">重试</ElButton>
    </p>
    <p v-else-if="!usable.length" class="eu-cp-muted">暂无适用于本单的优惠券</p>
    <ul v-else class="eu-cp-list">
      <li
        v-for="cp in usable"
        :key="cp.couponId"
        class="eu-cp-item"
        :class="{ selected: selectedIds.includes(cp.couponId), disabled }"
        @click="onToggle(cp)"
      >
        <ElCheckbox :model-value="selectedIds.includes(cp.couponId)" :disabled="disabled" @click.stop @change="onToggle(cp)" />
        <div class="eu-cp-body">
          <div class="eu-cp-head">
            <ElTag size="small" :type="kindType(String(cp.kind))" effect="plain">{{ couponKindLabel(String(cp.kind)) }}</ElTag>
            <span class="eu-cp-value">{{ couponValueText(cp) }}</span>
          </div>
          <div class="eu-cp-meta">
            <span>{{ couponScopeText(cp, label) }}</span>
            <span :class="{ 'eu-cp-soon': couponExpiresSoon(cp) }">{{ formatDate(cp.expireAt) }} 到期</span>
          </div>
        </div>
      </li>
    </ul>
    <p class="eu-cp-hint">
      <template v-if="selectedIds.length">已选 {{ selectedIds.length }} 张,</template>抵扣金额以下单后的订单为准;每单最多 {{ MAX_COUPONS_PER_ORDER }} 张,折扣券、满减券各限 1 张,代金券可叠加。
      <template v-if="inapplicableCount > 0">另有 {{ inapplicableCount }} 张优惠券不满足本单使用条件。</template>
      <template v-if="disabled && selectedIds.length">订单已创建,优惠券已绑定该订单。</template>
    </p>
  </div>
</template>

<style scoped>
.eu-coupon-picker { width: 100%; }
.eu-cp-alert { margin-bottom: 8px; }
.eu-cp-muted { margin: 0; font-size: 13px; color: var(--eu-text-secondary); }
.eu-cp-list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 8px; }
.eu-cp-item {
  display: flex; align-items: flex-start; gap: 10px;
  padding: 8px 12px; cursor: pointer;
  border: 1px solid var(--eu-border); border-radius: var(--eu-radius-md);
  transition: border-color var(--eu-transition), background var(--eu-transition);
}
.eu-cp-item:hover { border-color: var(--eu-color-brand); }
.eu-cp-item.selected { border-color: var(--eu-color-brand); background: var(--eu-color-brand-soft); }
.eu-cp-item.disabled { cursor: not-allowed; opacity: 0.7; }
.eu-cp-body { flex: 1; min-width: 0; }
.eu-cp-head { display: flex; align-items: center; gap: 8px; }
.eu-cp-value { font-size: 14px; font-weight: 500; color: var(--eu-text-primary); }
.eu-cp-meta { display: flex; flex-wrap: wrap; gap: 4px 16px; margin-top: 4px; font-size: 12px; color: var(--eu-text-secondary); }
.eu-cp-soon { color: var(--eu-color-warning-text); }
.eu-cp-hint { margin: 8px 0 0; font-size: 12px; line-height: 1.6; color: var(--eu-text-disabled); }
</style>
