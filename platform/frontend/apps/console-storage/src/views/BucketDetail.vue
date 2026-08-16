<script setup lang="ts">
/**
 * console-storage — OSS Bucket detail page (SCOSS), the storage category sub-app (02§7.3).
 * Resource info header (editable name / id-copy / status / expiry) + tab group
 * (概览/对象管理/权限管理/操作日志). Route target reached via the list's bucket link.
 *
 * Header + overview tab render real backend fields from /console/resources (the
 * generic resource aggregator) filtered to this bucket's resourceId. The object
 * list, permissions, and operation-log sub-tables are owned by svc-oss/svc-audit
 * (no phase-1 endpoint), so they show an honest empty state rather than fake rows.
 */
import { ref, computed, onMounted } from "vue";
import { ElTabs, ElTabPane, ElDescriptions, ElDescriptionsItem, ElButton, ElEmpty, ElTag } from "element-plus";
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

const sdk = createSDK({ baseURL: "" });

const bucketId = computed(() => {
  const m = (location.hash || "").match(/buckets\/([^/?#]+)/);
  return m ? decodeURIComponent(m[1]) : "—";
});

const resource = ref<ResourceItem | null>(null);
const loading = ref(true);
const error = ref<string | null>(null);

onMounted(async () => {
  try {
    const res = await sdk.get<ResourceItem[]>("/console/resources");
    resource.value = (res.data ?? []).find((r) => r.ResourceId === bucketId.value) ?? null;
    if (!resource.value) error.value = "未找到该存储桶";
  } catch (e) {
    error.value = (e as Error).message ?? "加载失败";
  } finally {
    loading.value = false;
  }
});

const editingName = ref(false);
const nameDraft = ref("");
const copied = ref(false);
const activeTab = ref("overview");

function startEdit() {
  nameDraft.value = resource.value?.ResourceId ?? "";
  editingName.value = true;
}
function saveName() {
  // Name editing is display-only until a rename endpoint lands on svc-oss.
  editingName.value = false;
}
function cancelEdit() {
  editingName.value = false;
}
function copyId() {
  void navigator.clipboard.writeText(bucketId.value).then(() => {
    copied.value = true;
    setTimeout(() => (copied.value = false), 1500);
  });
}

function fmt(s: string): string {
  if (!s) return "—";
  return s.replace("T", " ").replace(/\+.*$/, "");
}
const chargeLabel = computed(() => resource.value?.ChargeType === "PREPAID" ? "包年包月" : resource.value?.ChargeType === "POSTPAID" ? "按量付费" : "—");
</script>

<template>
  <section class="bucket-detail">
    <!-- resource info header (02§7.3) -->
    <header class="bucket-header">
      <div class="bucket-title">
        <h1 v-if="!editingName" class="bucket-name">{{ resource?.ResourceId ?? bucketId }}</h1>
        <span v-if="!editingName" class="name-edit" @click="startEdit">编辑</span>
        <span v-else class="name-edit-group">
          <ElInput v-model="nameDraft" size="small" style="width: 220px" />
          <ElButton size="small" type="primary" @click="saveName">保存</ElButton>
          <ElButton size="small" @click="cancelEdit">取消</ElButton>
        </span>
      </div>
      <div class="bucket-id-row">
        <span class="bucket-id">{{ bucketId }}</span>
        <span class="id-copy" @click="copyId">{{ copied ? "已复制" : "复制ID" }}</span>
      </div>
      <div class="bucket-meta">
        <StatusBadge :status="resource?.State ?? ''" />
        <ElTag size="small" effect="plain">{{ resource?.SpecCode ?? "—" }}</ElTag>
        <ElTag size="small" effect="plain">{{ resource?.Region ?? "—" }}</ElTag>
      </div>
    </header>

    <p v-if="error" class="bucket-error">{{ error }}(请确认 console-bff 在 :9200)</p>

    <!-- tab group (02§7.3): 概览/对象管理/权限管理/操作日志 -->
    <ElTabs v-model="activeTab" class="bucket-tabs">
      <ElTabPane label="概览" name="overview">
        <ElDescriptions v-if="resource" :column="2" border>
          <ElDescriptionsItem label="存储桶名称">{{ resource.ResourceId }}</ElDescriptionsItem>
          <ElDescriptionsItem label="存储桶ID">{{ resource.ResourceId }}</ElDescriptionsItem>
          <ElDescriptionsItem label="地域">{{ resource.Region }}</ElDescriptionsItem>
          <ElDescriptionsItem label="存储类型">{{ resource.SpecCode }}</ElDescriptionsItem>
          <ElDescriptionsItem label="计费方式">{{ chargeLabel }}</ElDescriptionsItem>
          <ElDescriptionsItem label="状态">
            <StatusBadge :status="resource.State" />
          </ElDescriptionsItem>
          <ElDescriptionsItem label="计费起始">{{ fmt(resource.BillingStart) }}</ElDescriptionsItem>
          <ElDescriptionsItem label="到期时间">{{ resource.ExpiredAt || "—" }}</ElDescriptionsItem>
          <ElDescriptionsItem label="创建时间">{{ fmt(resource.CreatedAt) }}</ElDescriptionsItem>
        </ElDescriptions>
        <p v-else class="bucket-loading">加载中…</p>
      </ElTabPane>

      <ElTabPane label="对象管理" name="objects">
        <ElEmpty description="对象列表由对象存储(svc-oss)提供,该端点接入后在此展示" />
      </ElTabPane>

      <ElTabPane label="权限管理" name="permissions">
        <ElEmpty description="权限配置(ACL/CORS/防盗链/版本控制)由 svc-oss 提供,该端点接入后在此展示" />
      </ElTabPane>

      <ElTabPane label="操作日志" name="logs">
        <ElEmpty description="操作日志由审计服务(svc-audit)提供,该端点接入后在此展示" />
      </ElTabPane>
    </ElTabs>
  </section>
</template>

<style scoped>
.bucket-detail { padding: 16px 24px; }
.bucket-header { padding-bottom: 16px; margin-bottom: 16px; border-bottom: 1px solid var(--sc-border); }
.bucket-title { display: flex; align-items: center; gap: 12px; }
.bucket-name { font-size: 20px; margin: 0; }
.name-edit { color: var(--sc-color-brand); font-size: var(--sc-font-size-sm); cursor: pointer; }
.name-edit:hover { color: var(--sc-color-brand-hover); }
.name-edit-group { display: inline-flex; align-items: center; gap: 8px; }
.bucket-id-row { margin-top: 6px; display: flex; align-items: center; gap: 8px; }
.bucket-id { font-size: var(--sc-font-size-sm); color: var(--sc-text-secondary); font-family: ui-monospace, monospace; }
.id-copy { color: var(--sc-color-brand); font-size: var(--sc-font-size-sm); cursor: pointer; }
.id-copy:hover { color: var(--sc-color-brand-hover); }
.bucket-meta { margin-top: 10px; display: flex; align-items: center; gap: 10px; }
.bucket-tabs { margin-top: 8px; }
.bucket-error { color: var(--sc-color-danger); padding: 24px 0; }
.bucket-loading { color: var(--sc-text-secondary); padding: 24px 0; }
</style>
