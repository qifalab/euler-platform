<script setup lang="ts">
/** Bills list — wired to console-bff /console/bills (02§7.2, §1.2). */
import { ref, onMounted } from "vue";
import { createSDK } from "@sc/sdk";
import { PriceText, PageHeader } from "@sc/ui";

const sdk = createSDK({ baseURL: "" });
const bills = ref<BillItem[]>([]);
const loading = ref(true);
const error = ref<string | null>(null);

interface BillItem {
  BillPeriod: string; TotalAmount: string; PaidAmount: string;
  ChargeCount: number; IncompleteCharges: number; UnreconciledCount: number; Final: boolean;
}

onMounted(async () => {
  try {
    const res = await sdk.get<BillItem[]>("/console/bills");
    bills.value = res.data;
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
});
</script>

<template>
  <div class="bills">
    <PageHeader title="账单" subtitle="按账期汇总的费用账单,数据来自费用服务。" />
    <p v-if="error" class="bills-error">加载失败:{{ error }}(请确认 console-bff 在 :9200)</p>
    <p v-if="loading" class="bills-loading">加载中…</p>
    <div v-else-if="bills.length" class="bills-table-card">
      <table class="bills-table">
        <thead><tr><th>账期</th><th>费用总额</th><th>已支付</th><th>扣费笔数</th><th>未对账</th><th>状态</th></tr></thead>
        <tbody>
          <tr v-for="b in bills" :key="b.BillPeriod">
            <td>{{ b.BillPeriod }}</td>
            <td><PriceText :amount-decimal="b.TotalAmount" /></td>
            <td><PriceText :amount-decimal="b.PaidAmount" /></td>
            <td>{{ b.ChargeCount }}</td>
            <td>{{ b.UnreconciledCount }}</td>
            <td>{{ b.Final ? "已结清" : "未结清" }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-else class="bills-empty">暂无账单</p>
  </div>
</template>

<style scoped>
.bills { padding: var(--sc-spacing-6); max-width: 1200px; }
.bills-error, .bills-loading, .bills-empty { color: var(--sc-text-secondary); padding: var(--sc-spacing-6); }
.bills-error { color: var(--sc-color-danger); }
.bills-table-card {
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  box-shadow: var(--sc-shadow-sm);
  overflow: hidden;
}
.bills-table { width: 100%; border-collapse: collapse; background: transparent; }
.bills-table th, .bills-table td { padding: var(--sc-spacing-3) var(--sc-spacing-4); text-align: left; border-bottom: 1px solid var(--sc-border); }
.bills-table th { background: var(--sc-glass-bg-soft); color: var(--sc-text-secondary); font-size: 12px; font-weight: 500; }
.bills-table td { font-size: 13px; color: var(--sc-text-primary); }
.bills-table tbody tr { transition: background var(--sc-transition); }
.bills-table tbody tr:hover { background: var(--sc-color-brand-soft); }
.bills-table tbody tr:last-child td { border-bottom: none; }
</style>
