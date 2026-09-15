<script setup lang="ts">
/**
 * 地域与容灾 — svc-catalog :9207 GET /api/v1/catalog/region-topology (phase-3 M-8).
 * 两地三中心拓扑: 主地域(同城双活写)/异地只读灾备, 跨城复制通道 RPO 判定,
 * 灾备接管两步序列(管控面冷转热 + DNS 切换)与全局/地域服务分类。
 */
import { ref, onMounted } from "vue";
import { ElButton } from "element-plus";
import { PageHeader, StatusBadge } from "@eu/ui";
import { createSDK } from "@eu/sdk";

const sdk = createSDK({ baseURL: "" });

interface RegionView {
  name: string;
  role: "PRIMARY" | "STANDBY" | string;
  writable: boolean;
  distanceKm: number;
  zones: string[];
}
interface ChannelView {
  name: string;
  mode: string;
  rpoMs: number;
  lagMs: number;
  rpoMet: boolean;
}
interface FailoverStep { name: string; detail: string }
interface ServiceScope { service: string; scope: "GLOBAL" | "REGIONAL" | string }
interface StateClass { state: string; class: "SHARED" | "REPLICATED" | "REBUILT" | string }

interface TopologyDTO {
  plan: { primary: RegionView; standby: RegionView; maxRtoMs: number };
  channels: ChannelView[];
  failoverSteps: FailoverStep[];
  serviceScopes: ServiceScope[];
  stateClasses: StateClass[];
}

const topo = ref<TopologyDTO | null>(null);
const loading = ref(true);
const error = ref<string | null>(null);

async function load() {
  loading.value = true;
  error.value = null;
  try {
    const res = await sdk.get<TopologyDTO>("/api/v1/catalog/region-topology");
    topo.value = res.data;
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}

function fmtMs(ms: number): string {
  if (ms <= 0) return "0";
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}
function roleLabel(r: string): string { return r === "PRIMARY" ? "主地域" : "灾备地域"; }
function stateClassLabel(c: string): string {
  if (c === "SHARED") return "全局共享";
  if (c === "REPLICATED") return "跨城复制";
  return "异地重建";
}
function stateLabel(s: string): string {
  const map: Record<string, string> = { account: "账号体系", ledger: "核心账务", "object-storage": "对象存储", "kafka-topic": "Kafka topic" };
  return map[s] ?? s;
}

onMounted(() => void load());
</script>

<template>
  <section class="regions">
    <PageHeader title="地域与容灾">
      <template #actions>
        <ElButton type="primary" @click="load">刷新</ElButton>
      </template>
    </PageHeader>

    <p v-if="error" class="rg-error">加载失败:{{ error }}(请确认 svc-catalog 在 :9207)</p>
    <p v-else-if="loading" class="rg-loading">加载中…</p>

    <template v-else-if="topo">
      <div class="rg-plan">
        <div class="rg-region primary">
          <div class="rg-role"><StatusBadge status="Success" />{{ roleLabel(topo.plan.primary.role) }} · 可写</div>
          <div class="rg-name">{{ topo.plan.primary.name }}</div>
          <div class="rg-meta">可用区 {{ topo.plan.primary.zones.join(" / ") }}(同城双活)</div>
        </div>
        <div class="rg-link">
          <div class="rg-arrow">⇄</div>
          <div class="rg-rto">RTO ≤ {{ fmtMs(topo.plan.maxRtoMs) }}</div>
        </div>
        <div class="rg-region standby">
          <div class="rg-role"><StatusBadge status="Info" />{{ roleLabel(topo.plan.standby.role) }} · 只读</div>
          <div class="rg-name">{{ topo.plan.standby.name }}</div>
          <div class="rg-meta">距主城 {{ topo.plan.standby.distanceKm }} km · 可用区 {{ topo.plan.standby.zones.join(" / ") }}</div>
        </div>
      </div>

      <h2 class="rg-section">跨城复制通道</h2>
      <p class="rg-hint">RPO 是复制延迟的上限;账务 binlog 准实时(≈5s),对象存储异步(1h)。</p>
      <div class="rg-table-card">
        <table class="rg-table">
          <thead><tr><th>通道</th><th>模式</th><th>RPO 目标</th><th>当前延迟</th><th>判定</th></tr></thead>
          <tbody>
            <tr v-for="c in topo.channels" :key="c.name">
              <td class="rg-mono">{{ c.name }}</td>
              <td>{{ c.mode === "near-realtime" ? "准实时" : "异步" }}</td>
              <td>{{ fmtMs(c.rpoMs) }}</td>
              <td :class="{ lag: !c.rpoMet }">{{ fmtMs(c.lagMs) }}</td>
              <td><StatusBadge :status="c.rpoMet ? 'Success' : 'Error'" />{{ c.rpoMet ? "达标" : "超限" }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <h2 class="rg-section">灾备接管序列</h2>
      <div class="rg-steps">
        <div v-for="(s, i) in topo.failoverSteps" :key="s.name" class="rg-step">
          <span class="rg-step-no">{{ i + 1 }}</span>
          <div class="rg-step-body">
            <div class="rg-step-detail">{{ s.detail }}</div>
            <div class="rg-step-name">{{ s.name }}</div>
          </div>
        </div>
      </div>

      <div class="rg-grid">
        <div>
          <h2 class="rg-section">服务分类</h2>
          <p class="rg-hint">IAM 与计费为全局单例,其余服务按地域部署、数据跨城复制。</p>
          <div class="rg-table-card">
            <table class="rg-table">
              <thead><tr><th>服务</th><th>部署形态</th></tr></thead>
              <tbody>
                <tr v-for="s in topo.serviceScopes" :key="s.service">
                  <td class="rg-mono">{{ s.service }}</td>
                  <td>
                    <span class="rg-scope" :class="{ global: s.scope === 'GLOBAL' }">{{ s.scope === "GLOBAL" ? "全局单例" : "地域部署" }}</span>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
        <div>
          <h2 class="rg-section">状态分类</h2>
          <p class="rg-hint">账号全局共享;账务/对象存储跨城复制;Kafka 不做跨城镜像,异地重建。</p>
          <div class="rg-table-card">
            <table class="rg-table">
              <thead><tr><th>状态</th><th>跨城形态</th></tr></thead>
              <tbody>
                <tr v-for="st in topo.stateClasses" :key="st.state">
                  <td>{{ stateLabel(st.state) }}</td>
                  <td>{{ stateClassLabel(st.class) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>
    </template>
  </section>
</template>

<style scoped>
.regions { padding: 0; }
.rg-plan { display: grid; grid-template-columns: 1fr auto 1fr; gap: 16px; align-items: stretch; margin-bottom: 8px; }
.rg-region {
  padding: 20px 24px;
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
}
.rg-region.primary { border-color: var(--eu-color-brand); }
.rg-role { display: flex; align-items: center; gap: 8px; font-size: 13px; color: var(--eu-text-secondary); }
.rg-name { font-size: 22px; font-weight: 600; color: var(--eu-text-primary); margin: 6px 0 4px; font-family: var(--eu-font-family-mono, monospace); }
.rg-meta { font-size: 12px; color: var(--eu-text-secondary); }
.rg-link { display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 4px; }
.rg-arrow { font-size: 22px; color: var(--eu-color-brand); }
.rg-rto { font-size: 12px; color: var(--eu-text-secondary); white-space: nowrap; }
.rg-section { font-size: 15px; font-weight: 500; color: var(--eu-text-primary); margin: 20px 0 4px; }
.rg-hint { color: var(--eu-text-secondary); font-size: 13px; margin: 0 0 12px; }
.rg-table-card {
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  overflow: hidden;
}
.rg-table { width: 100%; border-collapse: collapse; background: transparent; }
.rg-table th, .rg-table td { padding: 12px 16px; text-align: left; border-bottom: 1px solid var(--eu-border); }
.rg-table th { background: var(--eu-glass-bg-soft); color: var(--eu-text-secondary); font-size: 12px; font-weight: 500; }
.rg-table td { font-size: 13px; color: var(--eu-text-primary); }
.rg-table tbody tr { transition: background var(--eu-transition); }
.rg-table tbody tr:hover { background: var(--eu-color-brand-soft); }
.rg-table tbody tr:last-child td { border-bottom: none; }
.rg-mono { font-family: var(--eu-font-family-mono, monospace); font-size: 12px; }
td.lag { color: var(--eu-color-danger); font-weight: 600; }
.rg-steps { display: flex; gap: 12px; }
.rg-step {
  flex: 1; display: flex; gap: 12px; align-items: flex-start; padding: 16px;
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
}
.rg-step-no {
  width: 24px; height: 24px; border-radius: 50%; flex-shrink: 0;
  display: flex; align-items: center; justify-content: center;
  background: var(--eu-color-brand); color: var(--eu-color-on-brand); font-size: 12px; font-weight: 600;
}
.rg-step-detail { font-size: 14px; color: var(--eu-text-primary); font-weight: 500; }
.rg-step-name { font-size: 12px; color: var(--eu-text-secondary); font-family: var(--eu-font-family-mono, monospace); }
.rg-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 24px; }
.rg-scope { font-size: 12px; padding: 2px 8px; border-radius: var(--eu-radius-sm); color: var(--eu-text-secondary); background: var(--eu-glass-bg-soft); }
.rg-scope.global { color: var(--eu-color-brand); background: var(--eu-color-brand-soft); }
.rg-error, .rg-loading { color: var(--eu-text-secondary); padding: 24px; }
.rg-error { color: var(--eu-color-danger); }
@media (max-width: 960px) {
  .rg-plan { grid-template-columns: 1fr; }
  .rg-grid { grid-template-columns: 1fr; }
}
</style>
