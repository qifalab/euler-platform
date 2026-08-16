<script setup lang="ts">
/**
 * console-database — RDS instance detail (02§7.3 detail-page pattern).
 * Resource info header (editable name, copyable id, status, expiry) + tab
 * group (监控/备份/参数/账号/操作日志). Reached via the list link
 * `#/scrds/instances/<instanceId>`.
 *
 * Header + info render real backend fields from /console/resources (the generic
 * resource aggregator) filtered to this instance's resourceId. The sub-tables
 * (备份/参数/账号/操作日志) and the monitor chart are owned by svc-rds /
 * svc-monitor / svc-audit (no phase-1 endpoint), so they show an honest empty
 * state rather than fabricated rows.
 */
import { ref, computed, onMounted } from "vue";
import { ElTabs, ElTabPane, ElButton, ElInput, ElTag, ElDescriptions, ElDescriptionsItem, ElEmpty } from "element-plus";
import { StatusBadge } from "@sc/ui";
import { createSDK } from "@sc/sdk";
import "@sc/tokens/style.css";

interface ResourceItem {
  ResourceId: string;
  ProductCode: string;
  Region: string;
  ChargeType: string;
  State: string;
  SpecCode: string;
  BillingStart: string;
  ExpiredAt: string;
  CreatedAt: string;
}

interface ResourceListEnvelope {
  ResourceId: string;
  ProductCode: string;
  Region: string;
  ChargeType: string;
  State: string;
  SpecCode: string;
  BillingStart: string;
  ExpiredAt: string;
  CreatedAt: string;
}

const instanceId = computed(() => {
  const m = (location.hash || "").match(/instances\/([^/?#]+)/);
  return m ? decodeURIComponent(m[1]) : "—";
});

const sdk = createSDK({ baseURL: "" });
const resource = ref<ResourceItem | null>(null);
const loading = ref(true);
const error = ref<string | null>(null);

onMounted(async () => {
  try {
    const res = await sdk.get<ResourceListEnvelope[]>("/console/resources");
    resource.value = (res.data ?? []).find((r) => r.ResourceId === instanceId.value) as ResourceItem | undefined ?? null;
    if (!resource.value) error.value = "未找到该数据库实例";
  } catch (e) {
    error.value = (e as Error).message ?? "加载失败";
  } finally {
    loading.value = false;
  }
});

const editingName = ref(false);
const nameDraft = ref("");
function saveName() {
  // Name editing is display-only until a rename endpoint lands on svc-rds.
  editingName.value = false;
}
function cancelName() {
  editingName.value = false;
}
function copyId() {
  void navigator.clipboard?.writeText(instanceId.value);
}

function fmt(s: string): string {
  if (!s) return "—";
  return s.replace("T", " ").replace(/\+.*$/, "");
}
const chargeLabel = computed(() => resource.value?.ChargeType === "PREPAID" ? "包年包月" : resource.value?.ChargeType === "POSTPAID" ? "按量付费" : "—");

const activeTab = ref("monitor");
void ElInput; void ElButton; void ElTag;
</script>

<template>
  <section class="rds-detail">
    <header class="rds-detail-header">
      <div class="rds-detail-title">
        <ElInput v-if="editingName" v-model="nameDraft" size="default" style="width: 220px" />
        <h1 v-else>{{ resource?.ResourceId ?? instanceId }}</h1>
        <ElButton v-if="editingName" type="primary" size="small" @click="saveName">保存</ElButton>
        <ElButton v-if="editingName" size="small" @click="cancelName">取消</ElButton>
        <ElButton v-else size="small" text @click="editingName = true; nameDraft = resource?.ResourceId ?? ''">编辑</ElButton>
        <span class="rds-detail-id" @click="copyId" title="点击复制实例 ID">
          {{ instanceId }} ⧉
        </span>
      </div>
      <div class="rds-detail-meta">
        <StatusBadge :status="resource?.State ?? ''" />
        <ElTag v-if="resource?.ExpiredAt" type="info" effect="plain">到期 {{ resource.ExpiredAt }}</ElTag>
      </div>
    </header>

    <ElDescriptions v-if="resource" :column="3" border size="small" class="rds-info">
      <ElDescriptionsItem label="实例 ID">{{ resource.ResourceId }}</ElDescriptionsItem>
      <ElDescriptionsItem label="规格">{{ resource.SpecCode }}</ElDescriptionsItem>
      <ElDescriptionsItem label="计费方式">{{ chargeLabel }}</ElDescriptionsItem>
      <ElDescriptionsItem label="地域">{{ resource.Region }}</ElDescriptionsItem>
      <ElDescriptionsItem label="计费起始">{{ fmt(resource.BillingStart) }}</ElDescriptionsItem>
      <ElDescriptionsItem label="创建时间">{{ fmt(resource.CreatedAt) }}</ElDescriptionsItem>
    </ElDescriptions>
    <p v-else-if="error" class="rds-error">{{ error }}(请确认 console-bff 在 :9200)</p>

    <ElTabs v-model="activeTab" class="rds-tabs">
      <ElTabPane label="监控" name="monitor">
        <ElEmpty description="监控指标由云监控(svc-monitor)提供,该端点接入后在此展示" />
      </ElTabPane>

      <ElTabPane label="备份" name="backup">
        <ElEmpty description="备份记录由云数据库(svc-rds)提供,该端点接入后在此展示" />
      </ElTabPane>

      <ElTabPane label="参数" name="param">
        <ElEmpty description="参数配置由云数据库(svc-rds)提供,该端点接入后在此展示" />
      </ElTabPane>

      <ElTabPane label="账号" name="account">
        <ElEmpty description="数据库账号由云数据库(svc-rds)提供,该端点接入后在此展示" />
      </ElTabPane>

      <ElTabPane label="操作日志" name="log">
        <ElEmpty description="操作日志由审计服务(svc-audit)提供,该端点接入后在此展示" />
      </ElTabPane>
    </ElTabs>
  </section>
</template>

<style scoped>
.rds-detail { padding: 16px 24px; }
.rds-detail-header {
  display: flex; align-items: flex-start; justify-content: space-between;
  margin-bottom: 16px; flex-wrap: wrap; gap: 8px;
}
.rds-detail-title { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.rds-detail-title h1 { font-size: 18px; margin: 0; }
.rds-detail-id {
  font-size: var(--sc-font-size-xs); color: var(--sc-text-secondary);
  cursor: pointer; user-select: none;
}
.rds-detail-id:hover { color: var(--sc-color-brand); }
.rds-detail-meta { display: flex; align-items: center; gap: 12px; }
.rds-info { margin-bottom: 20px; }
.rds-tabs { background: var(--sc-bg-container); border-radius: var(--sc-radius-md); padding: 0 12px; }
.rds-error { color: var(--sc-color-danger); padding: 16px 0; }
</style>
