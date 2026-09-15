<script setup lang="ts">
/** Bills list — wired to console-bff /console/bills (02§7.2, §1.2). */
import { ref, onMounted } from "vue";
import { createSDK } from "@eu/sdk";
import { PriceText, PageHeader } from "@eu/ui";

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
.bills { padding: var(--eu-spacing-6); max-width: 1200px; }
.bills-error, .bills-loading, .bills-empty { color: var(--eu-text-secondary); padding: var(--eu-spacing-6); }
.bills-error { color: var(--eu-color-danger); }
.bills-table-card {
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  overflow: hidden;
}
.bills-table { width: 100%; border-collapse: collapse; background: transparent; }
.bills-table th, .bills-table td { padding: var(--eu-spacing-3) var(--eu-spacing-4); text-align: left; border-bottom: 1px solid var(--eu-border); }
.bills-table th { background: var(--eu-glass-bg-soft); color: var(--eu-text-secondary); font-size: 12px; font-weight: 500; }
.bills-table td { font-size: 13px; color: var(--eu-text-primary); }
.bills-table tbody tr { transition: background var(--eu-transition); }
.bills-table tbody tr:hover { background: var(--eu-color-brand-soft); }
.bills-table tbody tr:last-child td { border-bottom: none; }
</style>
