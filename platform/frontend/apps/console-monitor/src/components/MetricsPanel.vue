<script setup lang="ts">
/**
 * MetricsPanel — one resource's metric series (svc-monitor
 * GET /api/v1/monitor/metrics). Every query carries the selected resource and
 * an explicit window (start/end/period from the range presets, within the
 * API's 1440-point cap); nothing is fetched until a resource is chosen.
 *
 * Resources come from console-bff /console/resources?product=<productCode>
 * (the account's own resources, so the ownership check passes). A synthetic
 * series (no Prometheus behind svc-monitor, non-production only) is badged
 * 示例数据; 400 / 404 / 503 answers render as a clear empty/error state.
 */
import { computed, onMounted, onUnmounted, ref, watch } from "vue";
import { ElButton, ElEmpty, ElOption, ElRadioButton, ElRadioGroup, ElSelect, ElTag } from "element-plus";
import { sdk } from "../sdk";
import {
  chartGeometry,
  fetchMetrics,
  formatMetricValue,
  isSupportedMetric,
  isSyntheticSeries,
  metricLabel,
  metricsErrorState,
  metricsWindow,
  RANGE_PRESETS,
  resourceOptions,
  summarizePoints,
  SUPPORTED_METRICS,
  type ConsoleResourceRow,
  type MetricSeries,
  type MetricsErrorState,
  type RangeKey,
  type ResourceOption,
} from "../metrics";

const props = defineProps<{
  /** Product whose resources are offered (e.g. euecs); absent = all products. */
  productCode?: string;
  /** Initial metric (a rule's metric); falls back to CPU when unsupported. */
  metric?: string;
}>();

const resources = ref<ResourceOption[]>([]);
const resourcesLoading = ref(true);
const resourcesError = ref<string | null>(null);
const resourceId = ref("");
const metric = ref<string>(isSupportedMetric(props.metric) ? props.metric : "cpu_utilization");
const range = ref<RangeKey>("1h");
const unsupportedRuleMetric = computed(() => !!props.metric && !isSupportedMetric(props.metric));

const series = ref<MetricSeries | null>(null);
const loading = ref(false);
const failure = ref<MetricsErrorState | null>(null);

async function loadResources() {
  resourcesLoading.value = true;
  resourcesError.value = null;
  try {
    const qs = props.productCode ? `?product=${encodeURIComponent(props.productCode)}` : "";
    const res = await sdk.get<ConsoleResourceRow[]>(`/console/resources${qs}`);
    resources.value = resourceOptions(res.data ?? [], props.productCode);
    if (!resources.value.some((r) => r.value === resourceId.value)) resourceId.value = resources.value[0]?.value ?? "";
  } catch (e) {
    resources.value = [];
    resourceId.value = "";
    resourcesError.value = (e as Error).message || "加载资源列表失败";
  } finally {
    resourcesLoading.value = false;
  }
}

let seq = 0;
let ctrl: AbortController | null = null;
async function loadSeries() {
  const my = ++seq;
  ctrl?.abort();
  if (!resourceId.value) {
    series.value = null;
    failure.value = null;
    loading.value = false;
    return;
  }
  ctrl = new AbortController();
  loading.value = true;
  failure.value = null;
  try {
    const s = await fetchMetrics(
      sdk,
      { metric: metric.value, resourceId: resourceId.value, ...metricsWindow(range.value) },
      { signal: ctrl.signal },
    );
    if (my !== seq) return;
    series.value = s;
  } catch (e) {
    if (my !== seq) return;
    series.value = null;
    failure.value = metricsErrorState(e);
  } finally {
    if (my === seq) loading.value = false;
  }
}

// Follow the caller's metric (e.g. the rule form's selection) when it is one we chart.
watch(() => props.metric, (m) => {
  if (isSupportedMetric(m)) metric.value = m;
});
watch([resourceId, metric, range], () => void loadSeries());
onMounted(async () => {
  await loadResources();
  await loadSeries();
});
onUnmounted(() => {
  seq++;
  ctrl?.abort();
});

const synthetic = computed(() => isSyntheticSeries(series.value));
const summary = computed(() => summarizePoints(series.value?.points ?? []));

const BOX = { width: 760, height: 240, padLeft: 56, padRight: 16, padTop: 12, padBottom: 28 };
const tickFormat = computed(() => {
  const long = range.value === "7d";
  return (d: Date) =>
    d.toLocaleString("zh-CN", long
      ? { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hour12: false }
      : { hour: "2-digit", minute: "2-digit", hour12: false });
});
const geometry = computed(() =>
  series.value?.points.length ? chartGeometry(series.value.points, metric.value, BOX, tickFormat.value) : null,
);
</script>

<template>
  <div class="mp">
    <div class="mp-toolbar">
      <ElSelect
        v-model="resourceId" class="mp-resource" filterable :loading="resourcesLoading"
        :placeholder="resources.length ? '选择资源' : '暂无可选资源'" :disabled="!resources.length"
      >
        <ElOption v-for="r in resources" :key="r.value" :value="r.value" :label="r.label" />
      </ElSelect>
      <ElSelect v-model="metric" class="mp-metric">
        <ElOption v-for="m in SUPPORTED_METRICS" :key="m.value" :value="m.value" :label="m.label" />
      </ElSelect>
      <ElRadioGroup v-model="range" size="default">
        <ElRadioButton v-for="p in RANGE_PRESETS" :key="p.key" :value="p.key">{{ p.label }}</ElRadioButton>
      </ElRadioGroup>
      <ElButton :loading="loading" :disabled="!resourceId" @click="loadSeries">刷新</ElButton>
      <ElTag v-if="synthetic" type="warning" effect="light" class="mp-synthetic">示例数据</ElTag>
    </div>
    <p v-if="unsupportedRuleMetric" class="mp-note">规则指标「{{ props.metric }}」暂无监控曲线,可查看 CPU、内存或请求数。</p>
    <p v-if="synthetic" class="mp-note">监控数据源尚未接入,以下为演示用的示例数据,不代表资源的真实负载。</p>

    <p v-if="resourcesLoading" class="mp-muted">加载资源列表…</p>
    <ElEmpty v-else-if="resourcesError" :description="`资源列表加载失败:${resourcesError}`">
      <ElButton size="small" @click="loadResources().then(loadSeries)">重试</ElButton>
    </ElEmpty>
    <ElEmpty v-else-if="!resources.length" description="当前账号下暂无可监控的资源,创建资源后可在此查看监控曲线" />
    <ElEmpty v-else-if="failure" :description="failure.detail ? `${failure.title}:${failure.detail}` : failure.title">
      <ElButton size="small" @click="loadSeries">重试</ElButton>
    </ElEmpty>
    <p v-else-if="loading && !series" class="mp-muted">加载监控数据…</p>
    <ElEmpty v-else-if="series && !series.points.length" description="所选时间范围内暂无数据" />
    <template v-else-if="series && geometry">
      <dl v-if="summary" class="mp-stats">
        <div><dt>最新</dt><dd>{{ formatMetricValue(summary.latest, metric) }}</dd></div>
        <div><dt>平均</dt><dd>{{ formatMetricValue(summary.avg, metric) }}</dd></div>
        <div><dt>最大</dt><dd>{{ formatMetricValue(summary.max, metric) }}</dd></div>
        <div><dt>最小</dt><dd>{{ formatMetricValue(summary.min, metric) }}</dd></div>
      </dl>
      <svg class="mp-chart" :viewBox="`0 0 ${BOX.width} ${BOX.height}`" role="img" :aria-label="`${metricLabel(metric)}曲线`">
        <g class="mp-grid">
          <line v-for="t in geometry.yTicks" :key="`y${t.y}`" :x1="BOX.padLeft" :x2="BOX.width - BOX.padRight" :y1="t.y" :y2="t.y" />
        </g>
        <g class="mp-axis">
          <text v-for="t in geometry.yTicks" :key="`yl${t.y}`" :x="BOX.padLeft - 6" :y="t.y + 4" text-anchor="end">{{ t.label }}</text>
          <text
            v-for="(t, i) in geometry.xTicks" :key="`xl${i}`" :x="t.x" :y="BOX.height - 8"
            :text-anchor="i === 0 ? 'start' : i === geometry.xTicks.length - 1 ? 'end' : 'middle'"
          >{{ t.label }}</text>
        </g>
        <path class="mp-area" :d="geometry.area" />
        <polyline class="mp-line" :points="geometry.line" />
      </svg>
      <p class="mp-muted mp-caption">{{ metricLabel(metric) }} · 粒度 {{ series.period }} 秒 · 共 {{ series.points.length }} 个数据点</p>
    </template>
  </div>
</template>

<style scoped>
.mp { display: flex; flex-direction: column; gap: 12px; }
.mp-toolbar { display: flex; flex-wrap: wrap; align-items: center; gap: 12px; }
.mp-resource { width: 320px; }
.mp-metric { width: 160px; }
.mp-synthetic { margin-left: 4px; }
.mp-note { margin: 0; font-size: 12px; color: var(--eu-color-warning-text); }
.mp-muted { margin: 0; font-size: 13px; color: var(--eu-text-secondary); padding: 16px 0; }
.mp-caption { padding: 0; font-size: 12px; }
.mp-stats { display: grid; grid-template-columns: repeat(4, minmax(100px, 1fr)); gap: 12px; margin: 0; }
.mp-stats > div {
  display: flex; flex-direction: column; gap: 2px; padding: 10px 12px;
  border: 1px solid var(--eu-border); border-radius: var(--eu-radius-md);
}
.mp-stats dt { font-size: 12px; color: var(--eu-text-secondary); }
.mp-stats dd { margin: 0; font-size: 18px; color: var(--eu-text-primary); font-weight: 500; }
.mp-chart { width: 100%; height: auto; display: block; }
.mp-grid line { stroke: var(--eu-border); stroke-width: 1; }
.mp-axis text { font-size: 11px; fill: var(--eu-text-secondary); }
.mp-line { fill: none; stroke: var(--eu-color-brand); stroke-width: 2; stroke-linejoin: round; }
.mp-area { fill: var(--eu-color-brand-soft); stroke: none; }
</style>
