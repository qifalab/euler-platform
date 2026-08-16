<script setup lang="ts">
/** Cost analysis — 成本分摊 (M-4.3, 09-roadmap §4.3).
 *  按产品/资源分摊 pretax 费用,与账单总额一致(无未解释差异).
 *  Wired to console-bff /console/cost-analysis (fan-out svc-billing). */
import { ref, computed, onMounted } from "vue";
import { createSDK } from "@sc/sdk";
import { PriceText, PageHeader } from "@sc/ui";

const sdk = createSDK({ baseURL: "" });
const byProduct = ref<Record<string, string>>({});
const byResource = ref<Record<string, string>>({});
const total = ref("0");
const period = ref(new Date().toISOString().slice(0, 7));
const loading = ref(true);
const error = ref<string | null>(null);

async function load() {
  loading.value = true;
  error.value = null;
  try {
    const res = await sdk.get<{
      byProduct: Record<string, string>;
      byResource: Record<string, string>;
      total: string;
    }>(`/console/cost-analysis?period=${period.value}`);
    byProduct.value = res.data.byProduct ?? {};
    byResource.value = res.data.byResource ?? {};
    total.value = res.data.total ?? "0";
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}
onMounted(load);

const productBars = computed(() => {
  const entries = Object.entries(byProduct.value);
  const max = Math.max(0.0001, ...entries.map(([, v]) => parseFloat(v) || 0));
  return entries.map(([k, v]) => ({ key: k, amount: v, pct: ((parseFloat(v) || 0) / max) * 100 }));
});
</script>

<template>
  <div class="cost">
    <PageHeader title="成本分析" subtitle="按产品与资源分摊的费用,与账单总额对账一致。" />
    <div class="cost-toolbar">
      <label>账期 <input v-model="period" type="month" @change="load" /></label>
      <span class="cost-total">合计 <PriceText :amount-decimal="total" /></span>
    </div>
    <p v-if="error" class="cost-error">{{ error }}</p>
    <p v-if="loading" class="cost-loading">加载中…</p>
    <template v-else>
      <section v-if="productBars.length" class="cost-section">
        <h3>按产品</h3>
        <div v-for="b in productBars" :key="b.key" class="bar-row">
          <span class="bar-label">{{ b.key }}</span>
          <div class="bar-track"><div class="bar-fill" :style="{ width: b.pct + '%' }" /></div>
          <PriceText :amount-decimal="b.amount" />
        </div>
      </section>
      <p v-else class="cost-empty">该账期暂无费用数据</p>
    </template>
  </div>
</template>

<style scoped>
.cost { padding: var(--sc-spacing-6); max-width: 1000px; }
.cost-toolbar { display: flex; align-items: center; gap: var(--sc-spacing-4); margin-bottom: var(--sc-spacing-4); }
.cost-toolbar input { padding: 4px 8px; border: 1px solid var(--sc-border); border-radius: var(--sc-radius-sm); background: var(--sc-glass-bg-soft); color: var(--sc-text-primary); }
.cost-total { margin-left: auto; font-weight: 500; color: var(--sc-text-primary); }
.cost-error, .cost-loading, .cost-empty { color: var(--sc-text-secondary); padding: var(--sc-spacing-6); }
.cost-error { color: var(--sc-color-danger); }
.cost-section h3 { font-size: 14px; color: var(--sc-text-secondary); margin: var(--sc-spacing-4) 0 var(--sc-spacing-3); }
.bar-row { display: grid; grid-template-columns: 120px 1fr auto; align-items: center; gap: var(--sc-spacing-3); padding: var(--sc-spacing-2) 0; }
.bar-label { font-size: 13px; color: var(--sc-text-primary); }
.bar-track { height: 10px; background: var(--sc-glass-bg-soft); border-radius: var(--sc-radius-sm); overflow: hidden; }
.bar-fill { height: 100%; background: var(--sc-color-brand); border-radius: var(--sc-radius-sm); transition: width 0.3s; }
</style>
