<script setup lang="ts">
/**
 * VpcDetail — VPC resource detail page (02§7.3 detail-page pattern).
 * Resource info header (name/editable, id-copy, status) over an ElTabs group
 * (子网 / 路由表 / 网关 / 操作日志). Reached via the list's #/euvpc/vpcs/{vpcId} link.
 *
 * Header + descriptions render the orchestrator's resource record (GET
 * /api/v1/orchestrator/resources/{id}), including the customer attributes set
 * at creation (name, cidrBlock). The name is editable in place through PATCH
 * /api/v1/orchestrator/resources/{id}/attributes; the CIDR block is immutable.
 * The sub-tables
 * (subnets / route tables / gateways / operation logs) are owned by svc-vpc /
 * svc-audit (no phase-1 endpoint), so they show an honest empty state rather
 * than fake rows.
 */
import { computed, ref, onMounted } from "vue";
import { ElTabs, ElTabPane, ElDescriptions, ElDescriptionsItem, ElButton, ElEmpty, ElMessage } from "element-plus";
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

// Read the id from the hash (#/euvpc/vpcs/<id>); the base syncs the URL into
// the sub-app via Wujie, so location.hash is authoritative.
const vpcId = computed(() => {
  const m = (location.hash || "").match(/vpcs\/([^/?#]+)/);
  return m ? decodeURIComponent(m[1]) : "—";
});

const resource = ref<ResourceItem | null>(null);
const loading = ref(true);
const error = ref<string | null>(null);

async function load() {
  loading.value = true;
  error.value = null;
  try {
    // Through the auth-wired SDK (02§9.1): a bare fetch sent no token, and it
    // never checked res.ok — a 401/5xx error envelope read as "未找到该 VPC".
    const res = await sdk.get<ResourceItem>(`/api/v1/orchestrator/resources/${encodeURIComponent(vpcId.value)}`);
    resource.value = res.data ?? null;
    if (!resource.value) error.value = "未找到该 VPC";
  } catch (e) {
    const err = e as { status?: number; code?: string; message?: string };
    resource.value = null;
    error.value = err.status === 404 || err.code === "Resource.NotFound"
      ? "未找到该 VPC"
      : `加载失败:${err.message || "请求失败"}`;
  } finally {
    loading.value = false;
  }
}
onMounted(load);

const attrs = computed(() => attributesOf(resource.value));
/** Customer-given name, else the resource id. */
const displayName = computed(() => attrs.value.name || resource.value?.resourceId || vpcId.value);

const editing = ref(false);
const nameInput = ref("");
const savingName = ref(false);

function startEdit() {
  nameInput.value = attrs.value.name ?? "";
  editing.value = true;
}

/** Rename through the attributes PATCH (name is a mutable attribute). */
async function saveName() {
  if (!resource.value || savingName.value) return;
  const msg = validateResourceName(nameInput.value, { required: true });
  if (msg) { ElMessage.warning(msg); return; }
  if (nameInput.value.trim() === (attrs.value.name ?? "")) { editing.value = false; return; }
  savingName.value = true;
  try {
    const updated = await patchResourceAttributes<ResourceItem>(sdk, resource.value.resourceId, { name: nameInput.value });
    if (updated && typeof updated === "object" && "resourceId" in updated) resource.value = updated;
    else await load();
    editing.value = false;
    ElMessage.success("名称已更新");
  } catch (e) {
    ElMessage.error(attributeErrorMessage(e));
  } finally {
    savingName.value = false;
  }
}

function copyId() {
  void navigator.clipboard?.writeText(vpcId.value);
}

// RFC3339 → viewer-local time; "—" for unset ("" / zero-year) values.
const fmt = formatTime;
const chargeLabel = computed(() => resource.value?.chargeType === "PREPAID" ? "包年包月" : resource.value?.chargeType === "POSTPAID" ? "按量付费" : "—");

const activeTab = ref("subnet");
</script>

<template>
  <section class="vpc-detail">
    <!-- Resource info header (02§7.3): name (editable), id-copy, status. -->
    <header class="vpc-detail-header">
      <div class="vpc-detail-title">
        <span v-if="!editing" class="vpc-detail-name">{{ displayName }}</span>
        <input
          v-else
          v-model="nameInput"
          class="vpc-detail-name-input"
          :maxlength="NAME_MAX"
          placeholder="1-64 个字符"
          @keyup.enter="saveName"
        />
        <ElButton v-if="!editing" link size="small" :disabled="!resource" @click="startEdit">编辑</ElButton>
        <ElButton v-else link size="small" :loading="savingName" @click="saveName">保存</ElButton>
        <ElButton v-if="editing" link size="small" :disabled="savingName" @click="editing = false">取消</ElButton>
        <span class="vpc-detail-id" :title="vpcId">
          {{ vpcId }}
          <ElButton
            link
            size="small"
            title="复制 ID"
            @click="copyId"
          >复制</ElButton>
        </span>
        <StatusBadge :status="resource?.state ?? ''" />
      </div>
    </header>

    <div v-if="error" class="vpc-error">
      <span>{{ error }}(请确认 svc-orchestrator 在 :9203)</span>
      <ElButton size="small" :loading="loading" @click="load">重试</ElButton>
    </div>

    <ElDescriptions v-if="resource" :column="3" border class="vpc-detail-desc">
      <ElDescriptionsItem label="VPC ID">{{ resource.resourceId }}</ElDescriptionsItem>
      <ElDescriptionsItem label="名称">{{ attrs.name || "—" }}</ElDescriptionsItem>
      <ElDescriptionsItem label="IPv4 网段">{{ attrs.cidrBlock || "—" }}</ElDescriptionsItem>
      <ElDescriptionsItem label="地域">{{ resource.region }}</ElDescriptionsItem>
      <ElDescriptionsItem label="规格">{{ resource.specCode }}</ElDescriptionsItem>
      <ElDescriptionsItem label="计费方式">{{ chargeLabel }}</ElDescriptionsItem>
      <ElDescriptionsItem label="状态">{{ resource.state }}</ElDescriptionsItem>
      <ElDescriptionsItem label="创建时间">{{ fmt(resource.createdAt) }}</ElDescriptionsItem>
      <ElDescriptionsItem label="计费起始">{{ fmt(resource.billingStart) }}</ElDescriptionsItem>
      <ElDescriptionsItem label="到期时间">{{ fmt(resource.expiredAt) }}</ElDescriptionsItem>
    </ElDescriptions>

    <!-- Tab group (02§7.3): 子网 / 路由表 / 网关 / 操作日志. -->
    <ElTabs v-model="activeTab" class="vpc-detail-tabs">
      <ElTabPane label="子网" name="subnet">
        <ElEmpty description="子网列表由专有网络服务(svc-vpc)提供,该端点接入后在此展示" />
      </ElTabPane>

      <ElTabPane label="路由表" name="route">
        <ElEmpty description="路由表由专有网络服务(svc-vpc)提供,该端点接入后在此展示" />
      </ElTabPane>

      <ElTabPane label="网关" name="gateway">
        <ElEmpty description="网关由专有网络服务(svc-vpc)提供,该端点接入后在此展示" />
      </ElTabPane>

      <ElTabPane label="操作日志" name="logs">
        <ElEmpty description="操作日志由审计服务(svc-audit)提供,该端点接入后在此展示" />
      </ElTabPane>
    </ElTabs>
  </section>
</template>

<style scoped>
.vpc-detail { padding: 16px 24px; }
.vpc-detail-header { display: flex; align-items: flex-start; justify-content: space-between; margin-bottom: 16px; flex-wrap: wrap; gap: 8px; }
.vpc-detail-title { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.vpc-detail-name { font-size: 18px; font-weight: 600; color: var(--eu-text-primary); }
.vpc-detail-name-input { font-size: 16px; padding: 4px 8px; border: 1px solid var(--eu-border); border-radius: var(--eu-radius-sm); }
.vpc-detail-id { font-size: var(--eu-font-size-xs); color: var(--eu-text-secondary); display: flex; align-items: center; gap: 4px; }
.vpc-detail-desc { margin-bottom: 16px; }
.vpc-detail-tabs { background: var(--eu-bg-container); border-radius: var(--eu-radius-md); padding: 0 12px; }
.vpc-error { color: var(--eu-color-danger); padding: 16px 0; display: flex; align-items: center; gap: 12px; }
</style>
