<script setup lang="ts">
/** Resource packs — 资源包抵扣账本 (M-4.1, 09-roadmap §4.3).
 *  Wired to console-bff /console/reservepacks (fan-out svc-billing). */
import { ref, onMounted } from "vue";
import { createSDK } from "@sc/sdk";
import { PriceText, PageHeader } from "@sc/ui";

const sdk = createSDK({ baseURL: "" });
const packs = ref<Pack[]>([]);
const loading = ref(true);
const error = ref<string | null>(null);

interface Pack {
  packId: string; productCode: string; skuCode: string;
  faceValue: string; remaining: string; status: string;
  purchasedAt: string; expireAt: string;
}

async function load() {
  loading.value = true;
  error.value = null;
  try {
    const res = await sdk.get<{ packs: Pack[]; total: number }>("/console/reservepacks");
    packs.value = res.data.packs ?? [];
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}
onMounted(load);
</script>

<template>
  <div class="packs">
    <PageHeader title="资源包" subtitle="预付购买的固定额度,在计费瀑布中优先于代金券与余额抵扣。" />
    <p v-if="error" class="packs-error">加载失败:{{ error }}(请确认 console-bff 在 :9200)</p>
    <p v-if="loading" class="packs-loading">加载中…</p>
    <div v-else-if="packs.length" class="packs-table-card">
      <table class="packs-table">
        <thead><tr><th>资源包ID</th><th>产品</th><th>SKU</th><th>面值</th><th>剩余额度</th><th>状态</th><th>购买时间</th><th>到期</th></tr></thead>
        <tbody>
          <tr v-for="p in packs" :key="p.packId">
            <td>{{ p.packId }}</td>
            <td>{{ p.productCode }}</td>
            <td>{{ p.skuCode }}</td>
            <td><PriceText :amount-decimal="p.faceValue" /></td>
            <td><PriceText :amount-decimal="p.remaining" /></td>
            <td>{{ p.status }}</td>
            <td>{{ p.purchasedAt }}</td>
            <td>{{ p.expireAt || "永不过期" }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-else class="packs-empty">暂无资源包</p>
  </div>
</template>

<style scoped>
.packs { padding: var(--sc-spacing-6); max-width: 1200px; }
.packs-error, .packs-loading, .packs-empty { color: var(--sc-text-secondary); padding: var(--sc-spacing-6); }
.packs-error { color: var(--sc-color-danger); }
.packs-table-card {
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  box-shadow: var(--sc-shadow-sm);
  overflow: hidden;
}
.packs-table { width: 100%; border-collapse: collapse; background: transparent; }
.packs-table th, .packs-table td { padding: var(--sc-spacing-3) var(--sc-spacing-4); text-align: left; border-bottom: 1px solid var(--sc-border); }
.packs-table th { background: var(--sc-glass-bg-soft); color: var(--sc-text-secondary); font-size: 12px; font-weight: 500; }
.packs-table td { font-size: 13px; color: var(--sc-text-primary); }
.packs-table tbody tr:hover { background: var(--sc-color-brand-soft); }
.packs-table tbody tr:last-child td { border-bottom: none; }
</style>
