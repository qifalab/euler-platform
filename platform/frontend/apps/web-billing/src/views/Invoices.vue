<script setup lang="ts">
/** Invoices — 发票与红冲 (M-4.3, 09-roadmap §4.3 B6).
 *  红冲 = 开一张全额负数发票冲抵原票,原票置 VOIDED 终态保留.
 *  Wired to console-bff /console/invoices (fan-out svc-billing). */
import { ref, onMounted } from "vue";
import { createSDK } from "@sc/sdk";
import { PriceText, PageHeader } from "@sc/ui";

const sdk = createSDK({ baseURL: "" });
const invoices = ref<Invoice[]>([]);
const loading = ref(true);
const error = ref<string | null>(null);

interface LineItem { description: string; amount: string; }
interface Invoice {
  invoiceId: string; billPeriod: string; title: string; taxNo: string;
  amount: string; status: string; issuedAt: string; voidedAt: string;
  voidedById: string; reversesId: string; items: LineItem[];
}

async function load() {
  loading.value = true;
  error.value = null;
  try {
    const res = await sdk.get<{ invoices: Invoice[]; total: number }>("/console/invoices");
    invoices.value = res.data.invoices ?? [];
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}

async function issue(inv: Invoice) {
  try {
    await sdk.post(`/console/invoices/issue`, { invoiceId: inv.invoiceId });
    await load();
  } catch (e) {
    error.value = (e as Error).message;
  }
}

async function voidInvoice(inv: Invoice) {
  if (!confirm(`确认红冲发票 ${inv.invoiceId}?将开具一张全额负数发票冲抵原票,原票作废保留。`)) return;
  const reversalId = `rev-${Date.now()}`;
  try {
    await sdk.post(`/console/invoices/void`, { originalId: inv.invoiceId, reversalId });
    await load();
  } catch (e) {
    error.value = (e as Error).message;
  }
}
onMounted(load);
</script>

<template>
  <div class="invoices">
    <PageHeader title="发票" subtitle="税务文档,已开具不可删,红冲以负数发票冲抵。" />
    <p v-if="error" class="invoices-error">{{ error }}</p>
    <p v-if="loading" class="invoices-loading">加载中…</p>
    <div v-else-if="invoices.length" class="invoices-table-card">
      <table class="invoices-table">
        <thead><tr><th>发票ID</th><th>账期</th><th>抬头</th><th>金额</th><th>状态</th><th>开具时间</th><th>红冲</th><th>操作</th></tr></thead>
        <tbody>
          <tr v-for="inv in invoices" :key="inv.invoiceId">
            <td>{{ inv.invoiceId }}</td>
            <td>{{ inv.billPeriod }}</td>
            <td>{{ inv.title }}</td>
            <td><PriceText :amount-decimal="inv.amount" /></td>
            <td>{{ inv.status }}</td>
            <td>{{ inv.issuedAt || "—" }}</td>
            <td>{{ inv.reversesId ? `冲抵 ${inv.reversesId}` : (inv.voidedById ? `已被 ${inv.voidedById} 红冲` : "—") }}</td>
            <td>
              <button v-if="inv.status === 'DRAFT'" class="btn" @click="issue(inv)">开具</button>
              <button v-if="inv.status === 'ISSUED'" class="btn btn-danger" @click="voidInvoice(inv)">红冲</button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-else class="invoices-empty">暂无发票</p>
  </div>
</template>

<style scoped>
.invoices { padding: var(--sc-spacing-6); max-width: 1200px; }
.invoices-error, .invoices-loading, .invoices-empty { color: var(--sc-text-secondary); padding: var(--sc-spacing-6); }
.invoices-error { color: var(--sc-color-danger); }
.invoices-table-card {
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  box-shadow: var(--sc-shadow-sm);
  overflow: hidden;
}
.invoices-table { width: 100%; border-collapse: collapse; background: transparent; }
.invoices-table th, .invoices-table td { padding: var(--sc-spacing-3) var(--sc-spacing-4); text-align: left; border-bottom: 1px solid var(--sc-border); }
.invoices-table th { background: var(--sc-glass-bg-soft); color: var(--sc-text-secondary); font-size: 12px; font-weight: 500; }
.invoices-table td { font-size: 13px; color: var(--sc-text-primary); }
.invoices-table tbody tr:hover { background: var(--sc-color-brand-soft); }
.invoices-table tbody tr:last-child td { border-bottom: none; }
.btn { padding: 4px 12px; font-size: 12px; border: 1px solid var(--sc-border); border-radius: var(--sc-radius-sm); background: var(--sc-glass-bg-soft); color: var(--sc-text-primary); cursor: pointer; }
.btn:hover { background: var(--sc-color-brand-soft); }
.btn-danger { color: var(--sc-color-danger); border-color: var(--sc-color-danger); }
</style>
