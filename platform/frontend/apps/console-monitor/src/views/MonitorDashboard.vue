<script setup lang="ts">
/**
 * console-monitor — monitoring dashboard (EUMON), the monitor category sub-app
 * detail page (02§7.3). Rule info header + tabbed dashboard. Reached via the
 * rule list's name link (#/eumon/dashboard/:id).
 *
 * The header + config tab render the real rule from GET /api/v1/monitor/rules
 * (svc-monitor), filtered to this rule's id. The metric chart, sample stat
 * cards, and operation-log rows are owned by svc-monitor/svc-audit and are
 * shown as an honest empty state rather than fabricated values.
 */
import { ref, computed, onMounted } from "vue";
import {
  ElCard,
  ElTabs,
  ElTabPane,
  ElInput,
  ElButton,
  ElDescriptions,
  ElDescriptionsItem,
  ElEmpty,
} from "element-plus";
import { StatusBadge } from "@eu/ui";
import { createSDK } from "@eu/sdk";
import "@eu/tokens/style.css";

interface MonitorRule {
  ruleId: string;
  productCode: string;
  resourceType: string;
  metric: string;
  threshold: string | number;
  period: number;
  evalPeriods: number;
  notificationChannels: string[];
  status: number; // 1 enabled, 0 disabled
  createdAt: string;
}

const ruleId = computed(() => {
  const m = (location.hash || "").match(/dashboard\/([^/?#]+)/);
  return m ? decodeURIComponent(m[1]) : "—";
});

const sdk = createSDK({ baseURL: "" });
const rule = ref<MonitorRule | null>(null);
const loading = ref(true);
const error = ref<string | null>(null);
const activeTab = ref("monitor");

onMounted(async () => {
  try {
    const res = await sdk.get<MonitorRule[]>("/api/v1/monitor/rules");
    rule.value = (res.data ?? []).find((r) => String(r.ruleId) === ruleId.value) ?? null;
    if (!rule.value) error.value = "未找到该告警规则";
  } catch (e) {
    error.value = (e as Error).message ?? "加载失败";
  } finally {
    loading.value = false;
  }
});

const statusLabel = computed(() => (rule.value?.status === 1 ? "启用" : "禁用"));
const statusValue = computed(() => (rule.value?.status === 1 ? "Active" : "Inactive"));
const durationLabel = computed(() => rule.value ? `${rule.value.period}s × ${rule.value.evalPeriods}` : "—");
const contactLabel = computed(() => rule.value?.notificationChannels?.join("/") ?? "—");
const scopeLabel = computed(() => rule.value ? `${rule.value.productCode}/${rule.value.resourceType}` : "—");

const copied = ref(false);
function copyId() {
  void navigator.clipboard?.writeText(ruleId.value).then(() => {
    copied.value = true;
    setTimeout(() => (copied.value = false), 1500);
  });
}
</script>

<template>
  <section class="mon-detail">
    <!-- Resource info header (02§7.3): rule name, id-copy, status -->
    <header class="mon-header">
      <div class="mon-header-left">
        <ElInput :model-value="rule?.metric ? `${rule.productCode} · ${rule.metric}` : ruleId" class="mon-name-input" size="large" readonly />
        <div class="mon-id-row">
          <span class="mon-id">规则ID: {{ ruleId }}</span>
          <ElButton size="small" link @click="copyId">{{ copied ? "已复制" : "复制" }}</ElButton>
        </div>
      </div>
      <div class="mon-header-right">
        <div class="mon-meta">
          <span class="mon-meta-label">状态</span>
          <StatusBadge :status="statusValue" :label="statusLabel" />
        </div>
        <div class="mon-meta">
          <span class="mon-meta-label">持续条件</span>
          <span class="mon-meta-value">{{ durationLabel }}</span>
        </div>
      </div>
    </header>

    <p v-if="error" class="mon-error">{{ error }}(请确认 svc-monitor 在 :9202)</p>

    <ElTabs v-model="activeTab" class="mon-tabs">
      <ElTabPane label="监控" name="monitor">
        <ElEmpty description="监控指标图表由云监控(svc-monitor)提供,该端点接入后在此展示" />
      </ElTabPane>

      <ElTabPane label="配置信息" name="config">
        <ElDescriptions v-if="rule" :column="2" border>
          <ElDescriptionsItem label="指标">{{ rule.metric }}</ElDescriptionsItem>
          <ElDescriptionsItem label="阈值">{{ rule.threshold }}</ElDescriptionsItem>
          <ElDescriptionsItem label="持续">{{ durationLabel }}</ElDescriptionsItem>
          <ElDescriptionsItem label="通知联系人">{{ contactLabel }}</ElDescriptionsItem>
          <ElDescriptionsItem label="资源范围" :span="2">{{ scopeLabel }}</ElDescriptionsItem>
          <ElDescriptionsItem label="创建时间" :span="2">{{ rule.createdAt || "—" }}</ElDescriptionsItem>
        </ElDescriptions>
        <p v-else class="mon-loading">加载中…</p>
      </ElTabPane>

      <ElTabPane label="网络" name="network">
        <ElEmpty description="暂无网络数据" />
      </ElTabPane>

      <ElTabPane label="操作日志" name="logs">
        <ElEmpty description="操作日志由审计服务(svc-audit)提供,该端点接入后在此展示" />
      </ElTabPane>
    </ElTabs>
  </section>
</template>

<style scoped>
.mon-detail { padding: 16px 24px; }
.mon-header {
  display: flex; align-items: flex-start; justify-content: space-between;
  gap: 16px; padding-bottom: 16px; border-bottom: 1px solid var(--eu-border);
}
.mon-header-left { display: flex; flex-direction: column; gap: 8px; }
.mon-name-input { width: 320px; }
.mon-id-row { display: flex; align-items: center; gap: 8px; }
.mon-id { font-size: var(--eu-font-size-sm); color: var(--eu-text-secondary); }
.mon-header-right { display: flex; gap: 24px; padding-top: 4px; }
.mon-meta { display: flex; flex-direction: column; gap: 4px; }
.mon-meta-label { font-size: var(--eu-font-size-sm); color: var(--eu-text-secondary); }
.mon-meta-value { font-size: var(--eu-font-size-md); color: var(--eu-text-primary); }
.mon-tabs { margin-top: 16px; }
.mon-error { color: var(--eu-color-danger); padding: 16px 0; }
.mon-loading { color: var(--eu-text-secondary); padding: 16px 0; }
</style>
