<script setup lang="ts">
/** Orders list (02§1.2 billing center). Wired to svc-order GET /api/v1/orders. */
import { ref, onMounted } from "vue";
import { createSDK } from "@sc/sdk";
import { PageHeader } from "@sc/ui";
import { useProductLabels } from "@/useProductLabels";

const sdk = createSDK({ baseURL: "" });
// 产品名来自 svc-catalog /api/v1/catalog/products(模块级缓存,加载后响应式更新)。
const { label } = useProductLabels();

interface OrderRow {
  orderId: string; orderNo: string; type: string; state: string;
  productCode: string; payableAmount: string; createdAt: string; version: number;
}
interface OrderView {
  orderNo: string; productCode: string; type: string; amount: string;
  state: string; created: string;
}

const orders = ref<OrderView[]>([]);
const loading = ref(true);
const error = ref<string | null>(null);

/** Map raw svc-order type/state codes to display labels. */
const typeLabels: Record<string, string> = { NEW: "新购", RENEW: "续费", UPGRADE: "升配", DOWNGRADE: "降配", REFUND: "退款" };
const stateLabels: Record<string, string> = {
  PENDING_PAYMENT: "待支付", PAID: "已支付", FULFILLING: "履约中",
  COMPLETED: "已完成", CANCELLED: "已取消", REFUNDING: "退款中", REFUNDED: "已退款",
};
/** payableAmount arrives as a yuan-decimal string (pricing.Amount.String). */
function toYuan(raw: string): string {
  const n = Number(raw);
  return Number.isFinite(n) ? n.toFixed(2) : raw;
}

onMounted(async () => {
  try {
    const res = await sdk.get<OrderRow[]>("/api/v1/orders");
    orders.value = (res.data ?? []).map((o) => ({
      orderNo: o.orderNo,
      productCode: o.productCode,
      type: typeLabels[o.type] ?? o.type,
      amount: toYuan(o.payableAmount),
      state: stateLabels[o.state] ?? o.state,
      created: o.createdAt,
    }));
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
});
</script>

<template>
  <div class="orders">
    <PageHeader title="订单" subtitle="您的全部订单记录。" />
    <p v-if="error" class="orders-error">加载失败:{{ error }}(请确认 svc-order 在 :9204)</p>
    <p v-if="loading" class="orders-loading">加载中…</p>
    <div v-else-if="orders.length" class="orders-table-card">
      <table class="orders-table">
        <thead><tr><th>订单号</th><th>产品</th><th>类型</th><th>金额</th><th>状态</th><th>下单时间</th></tr></thead>
        <tbody>
          <tr v-for="o in orders" :key="o.orderNo">
            <td>{{ o.orderNo }}</td><td>{{ label(o.productCode) }}</td><td>{{ o.type }}</td>
            <td>¥{{ o.amount }}</td>
            <td :class="{ 'state-pending': o.state === '待支付' }">{{ o.state }}</td>
            <td>{{ o.created }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-else class="orders-empty">暂无订单</p>
  </div>
</template>

<style scoped>
.orders { padding: var(--sc-spacing-6); max-width: 1200px; }
.orders-error, .orders-loading, .orders-empty { color: var(--sc-text-secondary); padding: var(--sc-spacing-6); }
.orders-error { color: var(--sc-color-danger); }
.orders-table-card {
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  box-shadow: var(--sc-shadow-sm);
  overflow: hidden;
}
.orders-table { width: 100%; border-collapse: collapse; background: transparent; }
.orders-table th, .orders-table td { padding: var(--sc-spacing-3) var(--sc-spacing-4); text-align: left; border-bottom: 1px solid var(--sc-border); }
.orders-table th { background: var(--sc-glass-bg-soft); color: var(--sc-text-secondary); font-size: 12px; font-weight: 500; }
.orders-table td { font-size: 13px; color: var(--sc-text-primary); }
.orders-table tbody tr { transition: background var(--sc-transition); }
.orders-table tbody tr:hover { background: var(--sc-color-brand-soft); }
.orders-table tbody tr:last-child td { border-bottom: none; }
.state-pending { color: var(--sc-color-warning-text); font-weight: 500; }
</style>
