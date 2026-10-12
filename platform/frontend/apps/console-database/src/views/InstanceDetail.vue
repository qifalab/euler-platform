<script setup lang="ts">
/**
 * console-database — RDS instance detail (02§7.3 detail-page pattern).
 * Resource info header (editable name, copyable id, status, expiry) + tab
 * group (监控/备份/参数/账号/操作日志). Reached via the list link
 * `#/eurds/instances/<instanceId>`.
 *
 * Header + info render the orchestrator's resource record (GET
 * /api/v1/orchestrator/resources/{id}) with the customer attributes set at
 * creation (name, engineVersion); the name is editable through PATCH
 * /api/v1/orchestrator/resources/{id}/attributes. The sub-tables
 * (备份/参数/账号/操作日志) and the monitor chart are owned by svc-rds /
 * svc-monitor / svc-audit (no phase-1 endpoint), so they show an honest empty
 * state rather than fabricated rows.
 */
import { ref, computed, onMounted } from "vue";
import { ElTabs, ElTabPane, ElButton, ElInput, ElTag, ElDescriptions, ElDescriptionsItem, ElEmpty, ElMessage } from "element-plus";
import { StatusBadge } from "@eu/ui";
import {
  formatTime,
  attributesOf,
  attributeErrorMessage,
  patchResourceAttributes,
  validateResourceName,
  NAME_MAX,
} from "@eu/console-kit";
import { sdk } from "../sdk";
import "@eu/tokens/style.css";

/** GET /api/v1/orchestrator/resources/{id}. */
interface ResourceItem {
  resourceId: string;
  productCode: string;
  region: string;
  chargeType: string;
  state: string;
  specCode: string;
  billingStart: string;
  expiredAt: string;
  createdAt: string;
  attributes?: Record<string, string>;
}

const instanceId = computed(() => {
  const m = (location.hash || "").match(/instances\/([^/?#]+)/);
  return m ? decodeURIComponent(m[1]) : "—";
});

const resource = ref<ResourceItem | null>(null);
const loading = ref(true);
const error = ref<string | null>(null);

async function load() {
  try {
    const res = await sdk.get<ResourceItem>(`/api/v1/orchestrator/resources/${encodeURIComponent(instanceId.value)}`);
    resource.value = res.data ?? null;
    error.value = resource.value ? null : "未找到该数据库实例";
  } catch (e) {
    const err = e as { status?: number; code?: string; message?: string };
    error.value = err.status === 404 || err.code === "Resource.NotFound" ? "未找到该数据库实例" : (err.message || "加载失败");
  } finally {
    loading.value = false;
  }
}
onMounted(load);

const attrs = computed(() => attributesOf(resource.value));
const displayName = computed(() => attrs.value.name || resource.value?.resourceId || instanceId.value);

const editingName = ref(false);
const nameDraft = ref("");
const savingName = ref(false);
function startEdit() {
  nameDraft.value = attrs.value.name ?? "";
  editingName.value = true;
}
/** Rename through the attributes PATCH (name is a mutable attribute). */
async function saveName() {
  if (!resource.value || savingName.value) return;
  const msg = validateResourceName(nameDraft.value, { required: true });
  if (msg) { ElMessage.warning(msg); return; }
  if (nameDraft.value.trim() === (attrs.value.name ?? "")) { editingName.value = false; return; }
  savingName.value = true;
  try {
    const updated = await patchResourceAttributes<ResourceItem>(sdk, resource.value.resourceId, { name: nameDraft.value });
    if (updated && typeof updated === "object" && "resourceId" in updated) resource.value = updated;
    else await load();
    editingName.value = false;
    ElMessage.success("名称已更新");
  } catch (e) {
    ElMessage.error(attributeErrorMessage(e));
  } finally {
    savingName.value = false;
  }
}
function cancelName() {
  editingName.value = false;
}
function copyId() {
  void navigator.clipboard?.writeText(instanceId.value);
}

// RFC3339 → viewer-local time; "—" for unset ("" / zero-year) values. The old
// textual strip showed UTC wall-clock (with a stray "Z") as if it were local.
const fmt = formatTime;
const chargeLabel = computed(() => resource.value?.chargeType === "PREPAID" ? "包年包月" : resource.value?.chargeType === "POSTPAID" ? "按量付费" : "—");

const activeTab = ref("monitor");
</script>

<template>
  <section class="rds-detail">
    <header class="rds-detail-header">
      <div class="rds-detail-title">
        <ElInput v-if="editingName" v-model="nameDraft" size="default" style="width: 220px" :maxlength="NAME_MAX" placeholder="1-64 个字符" @keyup.enter="saveName" />
        <h1 v-else>{{ displayName }}</h1>
        <ElButton v-if="editingName" type="primary" size="small" :loading="savingName" @click="saveName">保存</ElButton>
        <ElButton v-if="editingName" size="small" :disabled="savingName" @click="cancelName">取消</ElButton>
        <ElButton v-else size="small" text :disabled="!resource" @click="startEdit">编辑</ElButton>
        <span class="rds-detail-id" @click="copyId" title="点击复制实例 ID">
          {{ instanceId }} ⧉
        </span>
      </div>
      <div class="rds-detail-meta">
        <StatusBadge :status="resource?.state ?? ''" />
        <ElTag v-if="resource?.expiredAt" type="info" effect="plain">到期 {{ fmt(resource.expiredAt) }}</ElTag>
      </div>
    </header>

    <ElDescriptions v-if="resource" :column="3" border size="small" class="rds-info">
      <ElDescriptionsItem label="实例 ID">{{ resource.resourceId }}</ElDescriptionsItem>
      <ElDescriptionsItem label="实例名称">{{ attrs.name || "—" }}</ElDescriptionsItem>
      <ElDescriptionsItem label="数据库引擎">{{ attrs.engineVersion ? `MySQL ${attrs.engineVersion}` : "—" }}</ElDescriptionsItem>
      <ElDescriptionsItem label="规格">{{ resource.specCode }}</ElDescriptionsItem>
      <ElDescriptionsItem label="计费方式">{{ chargeLabel }}</ElDescriptionsItem>
      <ElDescriptionsItem label="地域">{{ resource.region }}</ElDescriptionsItem>
      <ElDescriptionsItem label="计费起始">{{ fmt(resource.billingStart) }}</ElDescriptionsItem>
      <ElDescriptionsItem label="创建时间">{{ fmt(resource.createdAt) }}</ElDescriptionsItem>
    </ElDescriptions>
    <p v-else-if="error" class="rds-error">{{ error }}(请确认 svc-orchestrator 在 :9203)</p>

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
  font-size: var(--eu-font-size-xs); color: var(--eu-text-secondary);
  cursor: pointer; user-select: none;
}
.rds-detail-id:hover { color: var(--eu-color-brand); }
.rds-detail-meta { display: flex; align-items: center; gap: 12px; }
.rds-info { margin-bottom: 20px; }
.rds-tabs { background: var(--eu-bg-container); border-radius: var(--eu-radius-md); padding: 0 12px; }
.rds-error { color: var(--eu-color-danger); padding: 16px 0; }
</style>
