<script setup lang="ts">
/**
 * 稳定性平台 — svc-monitor :9202 /slo + /chaos (phase-3 M-9, 08§9.5/§10).
 * 错误预算、多窗口燃烧率告警(1h 快烧/3d 慢烧)、发布政策(NORMAL/SLOW_DOWN/FREEZE)、
 * SLA 门槛(连续两季度达标)与混沌演练六必练科目的整改判定全部来自后端计算。
 */
import { ref, onMounted } from "vue";
import { ElButton } from "element-plus";
import { PageHeader, StatusBadge } from "@eu/ui";
import { createSDK } from "@eu/sdk";

const sdk = createSDK({ baseURL: "" });

interface ObjectiveDTO {
  name: string;
  target: number;
  windowMs: number;
  errorBudgetMs: number;
  consumed1hMs: number;
  consumed3dMs: number;
  consumedTotalMs: number;
  remainingMs: number;
  burnRate1h: number;
  burnRate3d: number;
  alert1h: "PAGE" | "TICKET" | "NONE" | string;
  alert3d: "PAGE" | "TICKET" | "NONE" | string;
  budgetPolicy: "NORMAL" | "SLOW_DOWN" | "FREEZE" | string;
  quartersMet: number;
  slaEligible: boolean;
}

interface ChaosDTO {
  kind: string;
  stage: "STAGING" | "PROD" | string;
  blastRadius: string;
  abortDeadlineMs: number;
  expectedMs: number;
  actualMs: number;
  needsRemediation: boolean;
  drilledAt: string;
}

const objectives = ref<ObjectiveDTO[]>([]);
const drills = ref<ChaosDTO[]>([]);
const loading = ref(true);
const error = ref<string | null>(null);

async function load() {
  loading.value = true;
  error.value = null;
  try {
    const [sloRes, chaosRes] = await Promise.all([
      sdk.get<{ objectives: ObjectiveDTO[] }>("/api/v1/monitor/slo"),
      sdk.get<{ drills: ChaosDTO[] }>("/api/v1/monitor/chaos"),
    ]);
    objectives.value = sloRes.data?.objectives ?? [];
    drills.value = chaosRes.data?.drills ?? [];
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}

// ms → 人类可读时长 (d/h/m/s)。
function fmtMs(ms: number): string {
  if (ms <= 0) return "0";
  const s = Math.floor(ms / 1000);
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  if (m) return `${m}m ${sec}s`;
  return `${sec}s`;
}

function alertBadge(a: string): string {
  if (a === "PAGE") return "Error";
  if (a === "TICKET") return "Warning";
  return "Success";
}
function alertLabel(a: string): string {
  if (a === "PAGE") return "电话";
  if (a === "TICKET") return "工单";
  return "正常";
}
function policyBadge(p: string): string {
  if (p === "FREEZE") return "Error";
  if (p === "SLOW_DOWN") return "Warning";
  return "Success";
}
function policyLabel(p: string): string {
  if (p === "FREEZE") return "冻结发布";
  if (p === "SLOW_DOWN") return "降频发布";
  return "正常发布";
}
function pct(x: number): string { return `${(x * 100).toFixed(x * 100 >= 99.99 ? 4 : 2)}%`; }
function burn(x: number): string { return `${x.toFixed(2)}×`; }

// 演练科目中文对照 (08§9.5 六必练)。
const DRILL_LABELS: Record<string, string> = {
  "nacos-split-brain": "Nacos 脑裂降级",
  "redis-failover": "Redis 主从切换",
  "mysql-primary-failover": "MySQL 主库切换",
  "apisix-etcd-self-heal": "APISIX/etcd 自愈",
  "kafka-broker-loss": "Kafka broker 宕机",
  "single-az-loss": "单可用区整体不可用",
};
function drillLabel(k: string): string { return DRILL_LABELS[k] ?? k; }

onMounted(() => void load());
</script>

<template>
  <section class="stability">
    <PageHeader title="稳定性平台">
      <template #actions>
        <ElButton type="primary" @click="load">刷新</ElButton>
      </template>
    </PageHeader>

    <p v-if="error" class="st-error">加载失败:{{ error }}(请确认 svc-monitor 在 :9202)</p>
    <p v-else-if="loading" class="st-loading">加载中…</p>

    <template v-else>
      <h2 class="st-section">SLO 与错误预算</h2>
      <p class="st-hint">燃烧率 ≥14.4×(1h)电话告警,≥1×(3d)工单告警;预算耗尽冻结该域非可靠性发布,连续两季度达标方可承诺 SLA。</p>
      <div class="st-table-card">
        <table class="st-table">
          <thead><tr>
            <th>目标</th><th>SLO</th><th>错误预算</th><th>已消耗</th><th>剩余</th>
            <th>1h 燃烧</th><th>3d 燃烧</th><th>告警</th><th>发布政策</th><th>SLA 资格</th>
          </tr></thead>
          <tbody>
            <tr v-for="o in objectives" :key="o.name">
              <td class="st-name">{{ o.name }}</td>
              <td>{{ pct(o.target) }}</td>
              <td>{{ fmtMs(o.errorBudgetMs) }}</td>
              <td>{{ fmtMs(o.consumedTotalMs) }}</td>
              <td>{{ fmtMs(o.remainingMs) }}</td>
              <td :class="{ burn: o.burnRate1h >= 14.4 }">{{ burn(o.burnRate1h) }}</td>
              <td :class="{ burn: o.burnRate3d >= 1 }">{{ burn(o.burnRate3d) }}</td>
              <td>
                <StatusBadge :status="alertBadge(o.alert1h)" />{{ alertLabel(o.alert1h) }}
                <template v-if="o.alert3d !== 'NONE'">
                  <StatusBadge :status="alertBadge(o.alert3d)" />{{ alertLabel(o.alert3d) }}
                </template>
              </td>
              <td><StatusBadge :status="policyBadge(o.budgetPolicy)" />{{ policyLabel(o.budgetPolicy) }}</td>
              <td>
                <span class="st-sla" :class="{ ok: o.slaEligible }">{{ o.slaEligible ? `可承诺 (连续 ${o.quartersMet} 季度)` : "未达标" }}</span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <h2 class="st-section">混沌演练 · 六必练科目</h2>
      <p class="st-hint">每季度至少一次全科目演练;生产演练须具名爆炸半径且 10 分钟内可终止;实际恢复超预期 150% 立项整改。</p>
      <div class="st-table-card">
        <table class="st-table">
          <thead><tr>
            <th>科目</th><th>环境</th><th>爆炸半径</th><th>终止时限</th>
            <th>预期恢复</th><th>实际恢复</th><th>偏差</th><th>整改</th><th>最近演练</th>
          </tr></thead>
          <tbody>
            <tr v-for="d in drills" :key="d.kind" :class="{ remediate: d.needsRemediation }">
              <td>{{ drillLabel(d.kind) }}</td>
              <td>{{ d.stage === "PROD" ? "生产" : "预发" }}</td>
              <td>{{ d.blastRadius || "—" }}</td>
              <td>{{ d.abortDeadlineMs ? fmtMs(d.abortDeadlineMs) : "—" }}</td>
              <td>{{ fmtMs(d.expectedMs) }}</td>
              <td>{{ fmtMs(d.actualMs) }}</td>
              <td>{{ ((d.actualMs / d.expectedMs - 1) * 100).toFixed(0) }}%</td>
              <td>
                <StatusBadge :status="d.needsRemediation ? 'Error' : 'Success'" />
                {{ d.needsRemediation ? "需整改" : "通过" }}
              </td>
              <td>{{ d.drilledAt ? d.drilledAt.slice(0, 10) : "" }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>
  </section>
</template>

<style scoped>
.stability { padding: 0; }
.st-section { font-size: 15px; font-weight: 500; color: var(--eu-text-primary); margin: 20px 0 4px; }
.st-hint { color: var(--eu-text-secondary); font-size: 13px; margin: 0 0 12px; }
.st-table-card {
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  overflow-x: auto;
}
.st-table { width: 100%; border-collapse: collapse; background: transparent; white-space: nowrap; }
.st-table th, .st-table td { padding: 12px 14px; text-align: left; border-bottom: 1px solid var(--eu-border); }
.st-table th { background: var(--eu-glass-bg-soft); color: var(--eu-text-secondary); font-size: 12px; font-weight: 500; }
.st-table td { font-size: 13px; color: var(--eu-text-primary); }
.st-table tbody tr { transition: background var(--eu-transition); }
.st-table tbody tr:hover { background: var(--eu-color-brand-soft); }
.st-table tbody tr:last-child td { border-bottom: none; }
.st-table tbody tr.remediate td { background: var(--eu-color-danger-soft); }
.st-name { font-family: var(--eu-font-family-mono, monospace); font-size: 12px; }
td.burn { color: var(--eu-color-danger); font-weight: 600; }
.st-sla { font-size: 12px; color: var(--eu-text-secondary); }
.st-sla.ok { color: var(--eu-color-success, var(--eu-color-brand)); }
.st-error, .st-loading { color: var(--eu-text-secondary); padding: 24px; }
.st-error { color: var(--eu-color-danger); }
</style>
