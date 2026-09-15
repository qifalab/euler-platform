<script setup lang="ts">
/** Renew management (02§7.4 renew). Wired to console-bff /console/resources
 *  filtered to PREPAID resources; autorenew flags from svc-order. */
import { ref, onMounted } from "vue";
import { ElSwitch, ElMessage } from "element-plus";
import { createSDK } from "@eu/sdk";
import { PageHeader } from "@eu/ui";
import { useProductLabels } from "@/useProductLabels";

const sdk = createSDK({ baseURL: "" });
const { label } = useProductLabels();

interface ResourceRow {
  ResourceId: string; ProductCode: string; Region: string; ChargeType: string;
  State: string; SpecCode: string; BillingStart: string; ExpiredAt: string; CreatedAt: string;
}
/** svc-order /api/v1/orders/autorenew 条目。 */
interface AutoRenewItem { resourceId: string; productCode: string; enabled: boolean; }
interface RenewView {
  id: string; productCode: string; spec: string; expired: string;
  autoRenew: boolean; state: string; toggling: boolean;
}

const resources = ref<RenewView[]>([]);
const loading = ref(true);
const error = ref<string | null>(null);

const stateLabels: Record<string, string> = {
  RUNNING: "运行中", STOPPED: "已停止", CREATING: "创建中", DELETED: "已删除",
};

/** 先拉自动续费配置,建 resourceId→enabled 映射;失败降级为空 Map(全部视为未开启)。 */
async function fetchAutoRenewMap(): Promise<Map<string, boolean>> {
  try {
    const res = await sdk.get<AutoRenewItem[]>("/api/v1/orders/autorenew");
    return new Map((res.data ?? []).map((a) => [a.resourceId, a.enabled] as const));
  } catch {
    return new Map();
  }
}

onMounted(async () => {
  try {
    const autoRenewMap = await fetchAutoRenewMap();
    const res = await sdk.get<ResourceRow[]>("/console/resources");
    resources.value = (res.data ?? [])
      .filter((r) => r.ChargeType === "PREPAID")
      .map((r) => ({
        id: r.ResourceId,
        productCode: r.ProductCode,
        spec: r.SpecCode,
        expired: r.ExpiredAt || "—",
        autoRenew: autoRenewMap.get(r.ResourceId) ?? false,
        state: stateLabels[r.State] ?? r.State,
        toggling: false,
      }));
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
});

/** ElSwitch change:乐观翻转已由 v-model 完成,此处 PUT 落库,失败回滚并提示。 */
async function onToggle(r: RenewView) {
  r.toggling = true;
  try {
    await sdk.put<AutoRenewItem>("/api/v1/orders/autorenew", {
      resourceId: r.id, productCode: r.productCode, enabled: r.autoRenew,
    });
    ElMessage.success(r.autoRenew ? "已开启自动续费" : "已关闭自动续费");
  } catch (e) {
    r.autoRenew = !r.autoRenew; // 回滚乐观翻转
    ElMessage.error(`自动续费设置失败:${(e as Error).message}`);
  } finally {
    r.toggling = false;
  }
}
</script>

<template>
  <div class="renew">
    <PageHeader title="续费管理" subtitle="到期资源续费与自动续费设置。" />
    <p v-if="error" class="renew-error">加载失败:{{ error }}(请确认 console-bff 在 :9200)</p>
    <p v-if="loading" class="renew-loading">加载中…</p>
    <div v-else-if="resources.length" class="renew-table-card">
      <table class="renew-table">
        <thead><tr><th>资源 ID</th><th>产品</th><th>规格</th><th>到期时间</th><th>自动续费</th></tr></thead>
        <tbody>
          <tr v-for="r in resources" :key="r.id">
            <td>{{ r.id }}</td><td>{{ label(r.productCode) }}</td><td>{{ r.spec }}</td>
            <td>{{ r.expired }}</td>
            <td>
              <ElSwitch v-model="r.autoRenew" :loading="r.toggling" @change="onToggle(r)" />
              <span class="renew-ar-text">{{ r.autoRenew ? "已开启" : "未开启" }}</span>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-else class="renew-empty">暂无可续费的预付费资源</p>
  </div>
</template>

<style scoped>
.renew { padding: var(--eu-spacing-6); max-width: 1200px; }
.renew-error, .renew-loading, .renew-empty { color: var(--eu-text-secondary); padding: var(--eu-spacing-6); }
.renew-error { color: var(--eu-color-danger); }
.renew-table-card {
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  overflow: hidden;
}
.renew-table { width: 100%; border-collapse: collapse; background: transparent; }
.renew-table th, .renew-table td { padding: var(--eu-spacing-3) var(--eu-spacing-4); text-align: left; border-bottom: 1px solid var(--eu-border); }
.renew-table th { background: var(--eu-glass-bg-soft); color: var(--eu-text-secondary); font-size: 12px; font-weight: 500; }
.renew-table td { font-size: 13px; color: var(--eu-text-primary); }
.renew-table tbody tr { transition: background var(--eu-transition); }
.renew-table tbody tr:hover { background: var(--eu-color-brand-soft); }
.renew-table tbody tr:last-child td { border-bottom: none; }
.renew-ar-text { margin-left: var(--eu-spacing-3); font-size: 12px; color: var(--eu-text-secondary); }
</style>
