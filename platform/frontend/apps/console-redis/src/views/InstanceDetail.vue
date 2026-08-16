<script setup lang="ts">
/**
 * Instance detail (02§7.3): info header + tab group (监控/配置/网络/操作日志).
 *
 * Wired to the real resource lifecycle endpoint:
 *   GET /api/v1/orchestrator/resources/{id}  — svc-orchestrator (03§5.2)
 * The orchestrator is the sole owner of the resource lifecycle ledger, so this
 * is the authoritative source of state, spec, billing start, etc. SCREDIS
 * carries chargeType PREPAID or POSTPAID (managed DBs offer both).
 */
import { ref, computed, onMounted } from "vue";
import { useRoute, useRouter } from "vue-router";
import { ElTabs, ElTabPane, ElDescriptions, ElDescriptionsItem, ElButton, ElTag, ElEmpty, ElMessage } from "element-plus";
import { createSDK } from "@sc/sdk";

interface ResourceDetail {
  resourceId: string;
  productCode: string;
  region: string;
  chargeType: string; // PREPAID | POSTPAID
  state: string;      // RUNNING | CREATING | ...
  specCode: string;
  billingStart: string;
  createdAt: string;
  version: number;
}

const route = useRoute();
const router = useRouter();
const id = computed(() => String(route.params.id ?? ""));
const sdk = createSDK({ baseURL: "" });

const inst = ref<ResourceDetail | null>(null);
const loading = ref(true);
const error = ref<string | null>(null);
const activeTab = ref("monitor");

onMounted(async () => {
  if (!id.value) { error.value = "缺少实例 ID"; loading.value = false; return; }
  try {
    const res = await sdk.get<ResourceDetail>(`/api/v1/orchestrator/resources/${id.value}`);
    inst.value = res.data;
  } catch (e) {
    error.value = (e as Error).message ?? "加载失败";
    if (error.value.includes("404") || error.value.includes("不存在")) {
      ElMessage.error("实例不存在");
      router.push("/instances");
    }
  } finally {
    loading.value = false;
  }
});

function fmt(s: string): string {
  if (!s) return "—";
  return s.replace("T", " ").replace(/\+.*$/, "");
}
const chargeLabel = computed(() => inst.value?.chargeType === "PREPAID" ? "包年包月" : inst.value?.chargeType === "POSTPAID" ? "按量" : "—");
</script>

<template>
  <section class="detail">
    <header class="detail-head">
      <div>
        <h1 class="detail-title">{{ inst?.resourceId ?? id }}
          <ElTag v-if="inst" size="small" type="success">{{ inst.state }}</ElTag>
        </h1>
        <p class="detail-id">ID:{{ id }} · {{ inst?.region ?? "—" }}</p>
      </div>
      <div class="detail-actions">
        <ElButton size="small">重启</ElButton>
        <ElButton size="small">变配</ElButton>
        <ElButton size="small" type="primary">续费</ElButton>
        <ElButton size="small" type="danger">释放</ElButton>
      </div>
    </header>

    <p v-if="error" class="detail-error">加载失败:{{ error }}(请确认 svc-orchestrator 在 :9203)</p>

    <ElTabs v-else v-model="activeTab" class="detail-tabs">
      <ElTabPane label="监控" name="monitor">
        <ElEmpty description="监控指标(内存使用/连接数/命中率)由云监控(svc-monitor)提供,该端点接入后在此展示" />
      </ElTabPane>
      <ElTabPane label="配置信息" name="config">
        <ElDescriptions v-if="inst" :column="2" border>
          <ElDescriptionsItem label="实例规格">{{ inst.specCode }}</ElDescriptionsItem>
          <ElDescriptionsItem label="计费方式">{{ chargeLabel }}</ElDescriptionsItem>
          <ElDescriptionsItem label="产品">{{ inst.productCode }}</ElDescriptionsItem>
          <ElDescriptionsItem label="地域">{{ inst.region }}</ElDescriptionsItem>
          <ElDescriptionsItem label="计费起始">{{ fmt(inst.billingStart) }}</ElDescriptionsItem>
          <ElDescriptionsItem label="创建时间">{{ fmt(inst.createdAt) }}</ElDescriptionsItem>
          <ElDescriptionsItem label="资源版本">{{ inst.version }}</ElDescriptionsItem>
        </ElDescriptions>
        <p v-else class="detail-loading">加载中…</p>
      </ElTabPane>
      <ElTabPane label="网络" name="network">
        <ElEmpty description="网络配置(专有网络/内网地址/白名单)由 svc-vpc 提供,该端点接入后在此展示" />
      </ElTabPane>
      <ElTabPane label="操作日志" name="logs">
        <ElEmpty description="暂无操作日志(操作审计由 svc-audit 提供,该端点接入后在此展示)" />
      </ElTabPane>
    </ElTabs>

    <a class="detail-back" href="#" @click.prevent="router.push('/instances')">← 返回实例列表</a>
  </section>
</template>

<style scoped>
.detail { padding: 16px 24px; }
.detail-head { display: flex; align-items: flex-start; justify-content: space-between; margin-bottom: 16px; }
.detail-title { font-size: 18px; margin: 0; display: flex; align-items: center; gap: 8px; }
.detail-id { font-size: 12px; color: var(--sc-text-secondary); margin: 4px 0 0; }
.detail-actions { display: flex; gap: 8px; }
.detail-error { color: var(--sc-color-danger); padding: 24px 0; }
.detail-loading { color: var(--sc-text-secondary); padding: 24px 0; }
.detail-tabs { background: var(--sc-bg-container); border-radius: var(--sc-radius-md); padding: 0 16px 16px; }
.detail-back { display: inline-block; margin-top: 16px; color: var(--sc-color-brand); text-decoration: none; font-size: 13px; }
</style>
