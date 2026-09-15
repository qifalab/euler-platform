<script setup lang="ts">
/**
 * VpcDetail — VPC resource detail page (02§7.3 detail-page pattern).
 * Resource info header (name/editable, id-copy, status) over an ElTabs group
 * (子网 / 路由表 / 网关 / 操作日志). Reached via the list's #/euvpc/vpcs/{vpcId} link.
 *
 * Header + descriptions render real backend fields from /console/resources (the
 * generic resource aggregator) filtered to this VPC's resourceId. The sub-tables
 * (subnets / route tables / gateways / operation logs) are owned by svc-vpc /
 * svc-audit (no phase-1 endpoint), so they show an honest empty state rather
 * than fake rows.
 */
import { computed, ref, onMounted } from "vue";
import { ElTabs, ElTabPane, ElDescriptions, ElDescriptionsItem, ElButton, ElTag, ElEmpty } from "element-plus";
import { StatusBadge } from "@eu/ui";

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

// Read the id from the hash (#/euvpc/vpcs/<id>); the base syncs the URL into
// the sub-app via Wujie, so location.hash is authoritative.
const vpcId = computed(() => {
  const m = (location.hash || "").match(/vpcs\/([^/?#]+)/);
  return m ? decodeURIComponent(m[1]) : "—";
});

const resource = ref<ResourceItem | null>(null);
const loading = ref(true);
const error = ref<string | null>(null);

onMounted(async () => {
  try {
    const res = await fetch("/console/resources");
    const body = await res.json();
    const list = (body?.Data ?? []) as ResourceListEnvelope[];
    resource.value = (list.find((r) => r.ResourceId === vpcId.value) as ResourceItem | undefined) ?? null;
    if (!resource.value) error.value = "未找到该 VPC";
  } catch (e) {
    error.value = (e as Error).message ?? "加载失败";
  } finally {
    loading.value = false;
  }
});

const editing = ref(false);
const nameInput = ref("");

function saveName() {
  // Name editing is display-only until a rename endpoint lands on svc-vpc.
  editing.value = false;
}

function copyId() {
  void navigator.clipboard?.writeText(vpcId.value);
}

function fmt(s: string): string {
  if (!s) return "—";
  return s.replace("T", " ").replace(/\+.*$/, "");
}
const chargeLabel = computed(() => resource.value?.ChargeType === "PREPAID" ? "包年包月" : resource.value?.ChargeType === "POSTPAID" ? "按量付费" : "—");
void chargeLabel;
void ElTag;

const activeTab = ref("subnet");
</script>

<template>
  <section class="vpc-detail">
    <!-- Resource info header (02§7.3): name (editable), id-copy, status. -->
    <header class="vpc-detail-header">
      <div class="vpc-detail-title">
        <span v-if="!editing" class="vpc-detail-name">{{ resource?.ResourceId ?? vpcId }}</span>
        <input
          v-else
          v-model="nameInput"
          class="vpc-detail-name-input"
          @keyup.enter="saveName"
        />
        <ElButton v-if="!editing" link size="small" @click="editing = true; nameInput = resource?.ResourceId ?? ''">编辑</ElButton>
        <ElButton v-else link size="small" @click="saveName">保存</ElButton>
        <ElButton v-if="editing" link size="small" @click="editing = false">取消</ElButton>
        <span class="vpc-detail-id" :title="vpcId">
          {{ vpcId }}
          <ElButton
            link
            size="small"
            title="复制 ID"
            @click="copyId"
          >复制</ElButton>
        </span>
        <StatusBadge :status="resource?.State ?? ''" />
      </div>
    </header>

    <p v-if="error" class="vpc-error">{{ error }}(请确认 console-bff 在 :9200)</p>

    <ElDescriptions v-if="resource" :column="3" border class="vpc-detail-desc">
      <ElDescriptionsItem label="VPC ID">{{ resource.ResourceId }}</ElDescriptionsItem>
      <ElDescriptionsItem label="地域">{{ resource.Region }}</ElDescriptionsItem>
      <ElDescriptionsItem label="规格">{{ resource.SpecCode }}</ElDescriptionsItem>
      <ElDescriptionsItem label="计费方式">{{ chargeLabel }}</ElDescriptionsItem>
      <ElDescriptionsItem label="状态">{{ resource.State }}</ElDescriptionsItem>
      <ElDescriptionsItem label="创建时间">{{ fmt(resource.CreatedAt) }}</ElDescriptionsItem>
      <ElDescriptionsItem label="计费起始">{{ fmt(resource.BillingStart) }}</ElDescriptionsItem>
      <ElDescriptionsItem label="到期时间">{{ resource.ExpiredAt || "—" }}</ElDescriptionsItem>
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
.vpc-error { color: var(--eu-color-danger); padding: 16px 0; }
</style>
