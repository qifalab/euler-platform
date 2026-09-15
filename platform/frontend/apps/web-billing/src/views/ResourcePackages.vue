<script setup lang="ts">
/** Resource packs — 资源包抵扣账本 (M-4.1, 09-roadmap §4.3).
 *  Wired to console-bff /console/reservepacks (fan-out svc-billing). */
import { ref, onMounted } from "vue";
import { createSDK } from "@eu/sdk";
import { PriceText, PageHeader } from "@eu/ui";

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
.packs { padding: var(--eu-spacing-6); max-width: 1200px; }
.packs-error, .packs-loading, .packs-empty { color: var(--eu-text-secondary); padding: var(--eu-spacing-6); }
.packs-error { color: var(--eu-color-danger); }
.packs-table-card {
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  overflow: hidden;
}
.packs-table { width: 100%; border-collapse: collapse; background: transparent; }
.packs-table th, .packs-table td { padding: var(--eu-spacing-3) var(--eu-spacing-4); text-align: left; border-bottom: 1px solid var(--eu-border); }
.packs-table th { background: var(--eu-glass-bg-soft); color: var(--eu-text-secondary); font-size: 12px; font-weight: 500; }
.packs-table td { font-size: 13px; color: var(--eu-text-primary); }
.packs-table tbody tr:hover { background: var(--eu-color-brand-soft); }
.packs-table tbody tr:last-child td { border-bottom: none; }
</style>
