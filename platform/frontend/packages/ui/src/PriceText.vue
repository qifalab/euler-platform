<script setup lang="ts">
/**
 * PriceText — money formatter (02§10.3).
 * Storage GiB/TiB, bandwidth Mbit/s, money with thousands separator + 2 decimals.
 * Money is fixed-point in the backend (pricing.Amount); here we only render.
 */
import { computed } from "vue";

const props = withDefaults(defineProps<{
  /** Minor units (fen / cents) — fixed-point, no float drift. */
  amountMinor?: number;
  /** Or a decimal amount (when already a string from the backend). */
  amountDecimal?: string;
  currency?: string;
}>(), { currency: "CNY" });

const symbol = computed(() => (props.currency === "CNY" ? "¥" : "$"));
const formatted = computed(() => {
  if (props.amountMinor !== undefined) {
    const yuan = props.amountMinor / 100;
    return yuan.toLocaleString("zh-CN", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  }
  if (props.amountDecimal !== undefined) {
    const n = Number(props.amountDecimal);
    return n.toLocaleString("zh-CN", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  }
  return "0.00";
});
</script>

<template>
  <span class="sc-price-text">
    <span class="sc-price-symbol">{{ symbol }}</span>
    <span class="sc-price-amount">{{ formatted }}</span>
  </span>
</template>

<style>
.sc-price-text { color: var(--sc-text-primary); font-variant-numeric: tabular-nums; }
.sc-price-symbol { font-size: var(--sc-font-size-sm); margin-right: 2px; }
.sc-price-amount { font-size: var(--sc-font-size-lg); font-weight: 600; }
</style>
