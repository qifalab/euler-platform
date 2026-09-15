<script setup lang="ts">
/**
 * Overview — the shell's own home page (02§4.4), now wired to the real
 * console-bff aggregation endpoints. Shows balance, resource count, pending
 * orders, and a cross-product resource list.
 */
import { ref, onMounted } from "vue";
import { useRouter } from "vue-router";
import { useSDK, type OverviewData, type ResourceItem } from "../sdk";
import { PriceText } from "@eu/ui";

const router = useRouter();
const sdk = useSDK();
const overview = ref<OverviewData | null>(null);
const resources = ref<ResourceItem[]>([]);
const loading = ref(true);
const error = ref<string | null>(null);

onMounted(async () => {
  try {
    const [ov, res] = await Promise.all([
      sdk.get<OverviewData>("/console/overview"),
      sdk.get<ResourceItem[]>("/console/resources"),
    ]);
    overview.value = ov.data;
    resources.value = res.data;
  } catch (e) {
    error.value = (e as Error).message ?? "加载失败";
  } finally {
    loading.value = false;
  }
});

const PRODUCT_LABEL: Record<string, string> = {
  euecs: "云服务器", euoss: "对象存储", eurds: "云数据库", euvpc: "专有网络",
  eueip: "弹性公网 IP", eumon: "云监控", eubs: "块存储",
};
</script>

<template>
  <div class="overview">
    <h1>总览</h1>

    <div v-if="error" class="overview-error">无法加载账户概览:{{ error }}(请确认 console-bff 已在 :9200 启动)</div>

    <div v-if="loading" class="overview-loading">加载中…</div>

    <template v-else-if="overview">
      <div class="overview-cards">
        <div class="ov-card">
          <span class="ov-label">账户余额</span>
          <PriceText :amount-decimal="overview.Balance.Available" />
          <span class="ov-sub">冻结 ¥{{ overview.Balance.Frozen }}</span>
        </div>
        <div class="ov-card">
          <span class="ov-label">资源总数</span>
          <span class="ov-value">{{ overview.ResourceCount.Total }}</span>
          <span class="ov-sub">个</span>
        </div>
        <div class="ov-card">
          <span class="ov-label">待支付订单</span>
          <span class="ov-value" :class="{ 'ov-warn': overview.PendingOrders > 0 }">{{ overview.PendingOrders }}</span>
          <a v-if="overview.PendingOrders > 0" class="ov-link" href="#" @click.prevent="router.push('/billing')">去支付 →</a>
        </div>
      </div>

      <h2 class="overview-section">全部资源</h2>
      <table class="overview-table" v-if="resources.length">
        <thead>
          <tr><th>资源 ID</th><th>产品</th><th>地域</th><th>规格</th><th>计费</th><th>状态</th></tr>
        </thead>
        <tbody>
          <tr v-for="r in resources" :key="r.ResourceId">
            <td>{{ r.ResourceId }}</td>
            <td>{{ PRODUCT_LABEL[r.ProductCode] ?? r.ProductCode }}</td>
            <td>{{ r.Region }}</td>
            <td>{{ r.SpecCode }}</td>
            <td>{{ r.ChargeType }}</td>
            <td>{{ r.State }}</td>
          </tr>
        </tbody>
      </table>
      <p v-else class="overview-empty">暂无资源</p>
    </template>
  </div>
</template>

<style scoped>
/* Glass cards/tables sit on the body mesh; inner text stays plain for readability. */
.overview { padding: 24px; max-width: 1200px; }
.overview h1 { font-size: 20px; margin: 0 0 16px; }
.overview-loading, .overview-error, .overview-empty { color: var(--eu-text-secondary); padding: 24px; }
.overview-error { color: var(--eu-color-danger); }
.overview-cards { display: grid; grid-template-columns: repeat(auto-fill, minmax(260px, 1fr)); gap: 16px; margin-bottom: 32px; }
.ov-card {
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  padding: 20px; display: flex; flex-direction: column; gap: 4px;
  transition: var(--eu-transition);
}
.ov-card:hover { transform: translateY(-2px); box-shadow: var(--eu-shadow-md); }
.ov-label { font-size: 13px; color: var(--eu-text-secondary); }
.ov-value { font-size: 28px; font-weight: 600; color: var(--eu-text-primary); }
.ov-sub { font-size: 12px; color: var(--eu-text-disabled); }
.ov-warn { color: var(--eu-color-warning-text); }
.ov-link { font-size: 13px; color: var(--eu-color-brand); text-decoration: none; }
.overview-section { font-size: 16px; margin: 0 0 12px; }
.overview-table {
  width: 100%;
  border-collapse: separate; border-spacing: 0;
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  overflow: hidden;
}
.overview-table th, .overview-table td { padding: 12px 16px; text-align: left; font-size: 13px; border-bottom: 1px solid var(--eu-border); }
.overview-table th { background: var(--eu-glass-bg-soft); color: var(--eu-text-secondary); font-weight: 500; }
.overview-table td { color: var(--eu-text-primary); }
.overview-table tbody tr { transition: var(--eu-transition); }
.overview-table tbody tr:hover { background: var(--eu-color-brand-soft); }
</style>
