<script setup lang="ts">
/**
 * Instance detail (02§7.3): info header + tab group (监控/配置/网络/操作日志).
 *
 * Wired to the real resource lifecycle endpoint:
 *   GET /api/v1/orchestrator/resources/{id}  — svc-orchestrator (03§5.2)
 * The orchestrator is the sole owner of the resource lifecycle ledger, so this
 * is the authoritative source of state, spec, billing start, etc. ECI carries
 * chargeType=POSTPAID (按秒) — the only form a container instance is sold in.
 */
import { ref, computed, onMounted } from "vue";
import { useRoute, useRouter } from "vue-router";
import { ElTabs, ElTabPane, ElDescriptions, ElDescriptionsItem, ElTag, ElEmpty, ElMessage } from "element-plus";
import { formatTime, InstanceActions } from "@eu/console-kit";
import { sdk } from "../sdk";

interface ResourceDetail {
  resourceId: string;
  productCode: string;
  region: string;
  chargeType: string; // POSTPAID (per-second)
  state: string;      // RUNNING | CREATING | ...
  specCode: string;
  billingStart: string;
  createdAt: string;
  version: number;
}

const route = useRoute();
const router = useRouter();
const id = computed(() => String(route.params.id ?? ""));

const inst = ref<ResourceDetail | null>(null);
const loading = ref(true);
const error = ref<string | null>(null);
const activeTab = ref("monitor");

/** (Re)load the resource record — on mount and after every lifecycle action. */
async function fetchDetail() {
  try {
    const res = await sdk.get<ResourceDetail>(`/api/v1/orchestrator/resources/${id.value}`);
    inst.value = res.data;
    error.value = null;
  } catch (e) {
    // Branch on the structured error the SDK attaches (status/code): matching
    // the message TEXT missed 404s whose wording differs from those literals.
    const err = e as { status?: number; code?: string; message?: string };
    error.value = err.message ?? "加载失败";
    if (err.status === 404 || err.code === "Resource.NotFound") {
      ElMessage.error("实例不存在");
      router.push("/instances");
    }
  }
}

onMounted(async () => {
  if (!id.value) { error.value = "缺少实例 ID"; loading.value = false; return; }
  await fetchDetail();
  loading.value = false;
});

// RFC3339 → viewer-local time; "—" for unset ("" / zero-year) values. The old
// textual strip showed UTC wall-clock (with a stray "Z") as if it were local.
const fmt = formatTime;
const chargeLabel = computed(() => inst.value?.chargeType === "POSTPAID" ? "按量(按秒)" : "—");
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
        <InstanceActions
          :sdk="sdk"
          :resource-id="id"
          :instance="inst"
          :actions="['stop', 'start', 'restart', 'release']"
          @changed="fetchDetail"
          @released="router.push('/instances')"
        />
      </div>
    </header>

    <p v-if="error" class="detail-error">加载失败:{{ error }}(请确认 svc-orchestrator 在 :9203)</p>

    <ElTabs v-else v-model="activeTab" class="detail-tabs">
      <ElTabPane label="监控" name="monitor">
        <ElEmpty description="监控指标由云监控(svc-monitor)提供,该端点接入后在此展示" />
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
        <ElEmpty description="网络配置(专有网络/内网 IP/安全组)由 svc-vpc 提供,该端点接入后在此展示" />
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
.detail-id { font-size: 12px; color: var(--eu-text-secondary); margin: 4px 0 0; }
.detail-actions { display: flex; gap: 8px; }
.detail-error { color: var(--eu-color-danger); padding: 24px 0; }
.detail-loading { color: var(--eu-text-secondary); padding: 24px 0; }
.detail-tabs { background: var(--eu-bg-container); border-radius: var(--eu-radius-md); padding: 0 16px 16px; }
.detail-back { display: inline-block; margin-top: 16px; color: var(--eu-color-brand); text-decoration: none; font-size: 13px; }
</style>
