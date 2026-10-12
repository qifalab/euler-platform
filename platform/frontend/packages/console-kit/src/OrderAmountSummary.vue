<script setup lang="ts">
/**
 * OrderAmountSummary — the amounts svc-order fixed for a created order
 * (listAmount / couponDeduction / payableAmount, yuan decimals). Renders
 * nothing until an order carrying them exists.
 */
import { computed } from "vue";
import type { CheckoutOrder } from "./useOrderCheckout";
import { formatYuan } from "./coupons";

const props = defineProps<{ order: CheckoutOrder | null | undefined }>();

const show = computed(() => !!props.order && typeof props.order.payableAmount === "string" && props.order.payableAmount !== "");
const deduction = computed(() => {
  const n = Number(props.order?.couponDeduction);
  return Number.isFinite(n) && n > 0 ? n : 0;
});
</script>

<template>
  <dl v-if="show && order" class="eu-order-amounts">
    <div class="eu-oa-title"><dt>订单 {{ order.orderNo }}</dt></div>
    <div><dt>订单原价</dt><dd>{{ formatYuan(order.listAmount) }}</dd></div>
    <div><dt>优惠券抵扣</dt><dd class="eu-oa-deduct">{{ deduction > 0 ? `-${formatYuan(order.couponDeduction)}` : "¥0.00" }}</dd></div>
    <div class="eu-oa-payable"><dt>实付金额</dt><dd>{{ formatYuan(order.payableAmount) }}</dd></div>
  </dl>
</template>

<style scoped>
.eu-order-amounts { margin: 16px 0 0; padding-top: 8px; border-top: 1px dashed var(--eu-border); }
.eu-order-amounts div { display: flex; justify-content: space-between; padding: 6px 0; font-size: 13px; }
.eu-order-amounts dt { color: var(--eu-text-secondary); }
.eu-order-amounts dd { margin: 0; color: var(--eu-text-primary); }
.eu-oa-title dt { font-size: 12px; }
.eu-oa-deduct { color: var(--eu-color-success); }
.eu-oa-payable dd { font-weight: 600; color: var(--eu-color-danger); }
</style>
