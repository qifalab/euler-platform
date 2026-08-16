<script setup lang="ts">
/** Renew management (02§7.4 renew). Wired to console-bff /console/resources
 *  filtered to PREPAID resources (no dedicated renew backend yet). */
import { ref, onMounted } from "vue";
import { createSDK } from "@sc/sdk";
import { PageHeader } from "@sc/ui";

const sdk = createSDK({ baseURL: "" });

interface ResourceRow {
  ResourceId: string; ProductCode: string; Region: string; ChargeType: string;
  State: string; SpecCode: string; BillingStart: string; ExpiredAt: string; CreatedAt: string;
}
interface RenewView {
  id: string; product: string; spec: string; expired: string;
  autoRenew: boolean; state: string;
}

const resources = ref<RenewView[]>([]);
const loading = ref(true);
const error = ref<string | null>(null);

const productLabels: Record<string, string> = {
  scecs: "云服务器 ECS", scoss: "对象存储 OSS", scrds: "云数据库 RDS",
  scvpc: "私有网络 VPC", sceip: "弹性公网 IP", scmon: "云监控",
};
const stateLabels: Record<string, string> = {
  RUNNING: "运行中", STOPPED: "已停止", CREATING: "创建中", DELETED: "已删除",
};

onMounted(async () => {
  try {
    const res = await sdk.get<ResourceRow[]>("/console/resources");
    resources.value = (res.data ?? [])
      .filter((r) => r.ChargeType === "PREPAID")
      .map((r) => ({
        id: r.ResourceId,
        product: productLabels[r.ProductCode] ?? r.ProductCode,
        spec: r.SpecCode,
        expired: r.ExpiredAt || "—",
        autoRenew: false,
        state: stateLabels[r.State] ?? r.State,
      }));
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
});

function toggle(r: { autoRenew: boolean }) { r.autoRenew = !r.autoRenew; }
</script>

<template>
  <div class="renew">
    <PageHeader title="续费管理" subtitle="到期资源续费与自动续费设置。" />
    <p v-if="error" class="renew-error">加载失败:{{ error }}(请确认 console-bff 在 :9200)</p>
    <p v-if="loading" class="renew-loading">加载中…</p>
    <div v-else-if="resources.length" class="renew-table-card">
      <table class="renew-table">
        <thead><tr><th>资源 ID</th><th>产品</th><th>规格</th><th>到期时间</th><th>自动续费</th><th>操作</th></tr></thead>
        <tbody>
          <tr v-for="r in resources" :key="r.id">
            <td>{{ r.id }}</td><td>{{ r.product }}</td><td>{{ r.spec }}</td>
            <td>{{ r.expired }}</td>
            <td>{{ r.autoRenew ? "已开启" : "未开启" }}</td>
            <td><button class="renew-btn" @click="toggle(r)">{{ r.autoRenew ? "关闭自动续费" : "开启自动续费" }}</button></td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-else class="renew-empty">暂无可续费的预付费资源</p>
  </div>
</template>

<style scoped>
.renew { padding: var(--sc-spacing-6); max-width: 1200px; }
.renew-error, .renew-loading, .renew-empty { color: var(--sc-text-secondary); padding: var(--sc-spacing-6); }
.renew-error { color: var(--sc-color-danger); }
.renew-table-card {
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  box-shadow: var(--sc-shadow-sm);
  overflow: hidden;
}
.renew-table { width: 100%; border-collapse: collapse; background: transparent; }
.renew-table th, .renew-table td { padding: var(--sc-spacing-3) var(--sc-spacing-4); text-align: left; border-bottom: 1px solid var(--sc-border); }
.renew-table th { background: var(--sc-glass-bg-soft); color: var(--sc-text-secondary); font-size: 12px; font-weight: 500; }
.renew-table td { font-size: 13px; color: var(--sc-text-primary); }
.renew-table tbody tr { transition: background var(--sc-transition); }
.renew-table tbody tr:hover { background: var(--sc-color-brand-soft); }
.renew-table tbody tr:last-child td { border-bottom: none; }
.renew-btn {
  border: 1px solid var(--sc-color-brand); background: none; color: var(--sc-color-brand);
  border-radius: var(--sc-radius-sm); padding: 4px 12px; cursor: pointer; font-size: 12px;
  transition: background var(--sc-transition);
}
.renew-btn:hover { background: var(--sc-color-brand-soft); }
</style>
