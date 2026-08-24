<script setup lang="ts">
/**
 * 告警中心 — alert-center :9213 (phase-3 D-1 高级告警产品化, 09§4.0).
 * 四阶段收敛(去重/分组/抑制/静默) + 单租户 10 条/分钟限流在服务端执行;
 * 本视图消费 GET /alerts 已发送通知历史,并触发 ingest/flush 演练链路。
 */
import { ref, onMounted } from "vue";
import { ElButton, ElMessage } from "element-plus";
import { PageHeader, StatusBadge } from "@sc/ui";
import { createSDK } from "@sc/sdk";

const sdk = createSDK({ baseURL: "" });

interface NotificationDTO {
  alertId: string;
  tenantId: number;
  product: string;
  severity: "critical" | "warning" | "info" | string;
  channels: string[];
  emittedAt: string;
}

const alerts = ref<NotificationDTO[]>([]);
const loading = ref(true);
const error = ref<string | null>(null);
const working = ref(false);

async function load() {
  loading.value = true;
  error.value = null;
  try {
    const res = await sdk.get<{ alerts: NotificationDTO[] }>("/api/v1/alertcenter/alerts");
    alerts.value = res.data?.alerts ?? [];
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}

// 演练链路: 模拟平台告警源推送两条告警(同标签 → 去重合并),再触发收敛+限流+发送。
async function simulate() {
  working.value = true;
  try {
    const stamp = Date.now().toString(36);
    await sdk.post("/api/v1/alertcenter/ingest", {
      alertId: `sim-${stamp}-a`, product: "scecs", metric: "cpu_utilization",
      severity: "warning", labels: { instance: "i-demo-1", region: "cn-north-1" },
    });
    await sdk.post("/api/v1/alertcenter/ingest", {
      alertId: `sim-${stamp}-b`, product: "scecs", metric: "cpu_utilization",
      severity: "warning", labels: { instance: "i-demo-1", region: "cn-north-1" },
    });
    const res = await sdk.post<{ notifications: NotificationDTO[] }>("/api/v1/alertcenter/flush");
    const count = res.data?.notifications?.length ?? 0;
    ElMessage.success(`收敛完成,本轮发送 ${count} 条通知(重复告警已去重)`);
    await load();
  } catch (e) {
    ElMessage.error(`演练失败:${(e as Error).message}`);
  } finally {
    working.value = false;
  }
}

function severityBadge(s: string): string {
  if (s === "critical") return "Error";
  if (s === "warning") return "Warning";
  return "Info";
}

onMounted(() => void load());
</script>

<template>
  <section class="alert-center">
    <PageHeader title="告警中心">
      <template #actions>
        <ElButton :loading="working" @click="simulate">推送演示告警并收敛</ElButton>
        <ElButton type="primary" @click="load">刷新</ElButton>
      </template>
    </PageHeader>
    <p class="ac-hint">平台告警经 去重 → 分组 → 抑制 → 静默 四阶段收敛后按渠道发送;单租户限流 10 条/分钟,防止通知风暴。</p>
    <p v-if="error" class="ac-error">加载失败:{{ error }}(请确认 alert-center 在 :9213)</p>
    <p v-else-if="loading" class="ac-loading">加载中…</p>
    <div v-else-if="alerts.length" class="ac-table-card">
      <table class="ac-table">
        <thead><tr><th>告警 ID</th><th>产品</th><th>级别</th><th>渠道</th><th>发送时间</th></tr></thead>
        <tbody>
          <tr v-for="a in alerts" :key="a.alertId">
            <td class="ac-id">{{ a.alertId }}</td>
            <td>{{ a.product }}</td>
            <td><StatusBadge :status="severityBadge(a.severity)" /></td>
            <td>{{ a.channels.join("/") }}</td>
            <td>{{ a.emittedAt ? a.emittedAt.replace("T", " ").slice(0, 19) : "" }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-else class="ac-empty">暂无已发送通知——点击「推送演示告警并收敛」体验四阶段收敛链路。</p>
  </section>
</template>

<style scoped>
.alert-center { padding: 0; }
.ac-hint { color: var(--sc-text-secondary); font-size: 13px; margin: 0 0 12px; }
.ac-table-card {
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  box-shadow: var(--sc-shadow-sm);
  overflow: hidden;
}
.ac-table { width: 100%; border-collapse: collapse; background: transparent; }
.ac-table th, .ac-table td { padding: 12px 16px; text-align: left; border-bottom: 1px solid var(--sc-border); }
.ac-table th { background: var(--sc-glass-bg-soft); color: var(--sc-text-secondary); font-size: 12px; font-weight: 500; }
.ac-table td { font-size: 13px; color: var(--sc-text-primary); }
.ac-table tbody tr { transition: background var(--sc-transition); }
.ac-table tbody tr:hover { background: var(--sc-color-brand-soft); }
.ac-table tbody tr:last-child td { border-bottom: none; }
.ac-id { font-family: var(--sc-font-family-mono, monospace); }
.ac-error, .ac-loading, .ac-empty { color: var(--sc-text-secondary); padding: 24px; }
.ac-error { color: var(--sc-color-danger); }
</style>
