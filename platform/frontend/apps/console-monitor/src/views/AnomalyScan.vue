<script setup lang="ts">
/**
 * 异常检测 — svc-metering :9206 GET /api/v1/metering/anomaly-scan (phase-3 D-2).
 * z-score 判定最新用量点相对基线是否 SPIKE/DROP;结论是告警信号,
 * 绝不作为计费依据(宁可重采不可漏采, 03§4.2.5)。
 */
import { ref } from "vue";
import { ElButton, ElMessage } from "element-plus";
import { PageHeader, StatusBadge } from "@eu/ui";
import { createSDK } from "@eu/sdk";

const sdk = createSDK({ baseURL: "" });

interface AnomalyResult {
  resourceId: string;
  metric: string;
  anomalous: boolean;
  kind: "SPIKE" | "DROP" | "NONE" | string;
  score?: number;
  baselineMean?: number;
  baselineStdDev?: number;
  reason?: string;
}

const resourceId = ref("i-demo-1");
const metric = ref("cpu_utilization");
const result = ref<AnomalyResult | null>(null);
const error = ref<string | null>(null);
const loading = ref(false);

async function scan() {
  if (!resourceId.value || !metric.value) { ElMessage.warning("请填写资源 ID 与指标名"); return; }
  loading.value = true;
  error.value = null;
  result.value = null;
  try {
    const q = `resourceId=${encodeURIComponent(resourceId.value)}&metric=${encodeURIComponent(metric.value)}`;
    const res = await sdk.get<AnomalyResult>(`/api/v1/metering/anomaly-scan?${q}`);
    result.value = res.data;
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}

function kindBadge(k: string): string {
  if (k === "SPIKE") return "Error";
  if (k === "DROP") return "Warning";
  return "Success";
}
</script>

<template>
  <section class="anomaly">
    <PageHeader title="异常检测" />
    <p class="an-hint">对资源最新用量点做 z-score 检测(基线不含被测点);异常仅作为告警信号,不修改任何计量记录。</p>
    <div class="an-form-card">
      <label class="an-field"><span>资源 ID</span>
        <input v-model="resourceId" placeholder="如 i-demo-1" @keyup.enter="scan" />
      </label>
      <label class="an-field"><span>指标</span>
        <input v-model="metric" placeholder="如 cpu_utilization" @keyup.enter="scan" />
      </label>
      <ElButton type="primary" :loading="loading" @click="scan">开始检测</ElButton>
    </div>
    <p v-if="error" class="an-error">检测失败:{{ error }}(请确认 svc-metering 在 :9206,且该资源已有 ≥2 个计量点)</p>
    <div v-else-if="result" class="an-result-card" :class="{ anomalous: result.anomalous }">
      <div class="an-verdict">
        <StatusBadge :status="kindBadge(result.kind)" />
        <span class="an-kind">{{ result.kind === "NONE" ? "无异常" : (result.kind === "SPIKE" ? "用量激增" : "用量骤降") }}</span>
      </div>
      <dl class="an-metrics">
        <div><dt>资源</dt><dd>{{ result.resourceId }}</dd></div>
        <div><dt>指标</dt><dd>{{ result.metric }}</dd></div>
        <div v-if="result.score !== undefined"><dt>z 分数</dt><dd>{{ result.score.toFixed(2) }}</dd></div>
        <div v-if="result.baselineMean !== undefined"><dt>基线均值</dt><dd>{{ result.baselineMean.toFixed(2) }}</dd></div>
        <div v-if="result.baselineStdDev !== undefined"><dt>基线标准差</dt><dd>{{ result.baselineStdDev.toFixed(2) }}</dd></div>
        <div v-if="result.reason"><dt>说明</dt><dd>{{ result.reason }}</dd></div>
      </dl>
    </div>
  </section>
</template>

<style scoped>
.anomaly { padding: 0; }
.an-hint { color: var(--eu-text-secondary); font-size: 13px; margin: 0 0 12px; }
.an-form-card {
  display: flex; gap: 16px; align-items: flex-end; flex-wrap: wrap;
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  padding: 16px 20px;
}
.an-field { display: flex; flex-direction: column; gap: 6px; font-size: 13px; color: var(--eu-text-secondary); }
.an-field input {
  padding: 9px 12px; min-width: 220px; background: var(--eu-bg-container); border: 1px solid var(--eu-border);
  border-radius: var(--eu-radius-md); font-size: 14px; color: var(--eu-text-primary); outline: none;
  transition: border-color var(--eu-transition), box-shadow var(--eu-transition);
}
.an-field input:focus { border-color: var(--eu-color-brand); box-shadow: 0 0 0 3px var(--eu-color-brand-soft); }
.an-result-card {
  margin-top: 16px; padding: 20px 24px;
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
}
.an-result-card.anomalous { border-color: var(--eu-color-danger); }
.an-verdict { display: flex; align-items: center; gap: 10px; margin-bottom: 14px; }
.an-kind { font-size: 15px; font-weight: 500; color: var(--eu-text-primary); }
.an-metrics { display: grid; grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)); gap: 12px; margin: 0; }
.an-metrics > div { display: flex; flex-direction: column; gap: 2px; }
.an-metrics dt { font-size: 12px; color: var(--eu-text-secondary); }
.an-metrics dd { margin: 0; font-size: 14px; color: var(--eu-text-primary); }
.an-error { color: var(--eu-color-danger); padding: 16px 0; font-size: 13px; }
</style>
