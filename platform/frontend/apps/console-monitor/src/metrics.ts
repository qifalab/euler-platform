/**
 * Resource metrics — svc-monitor GET /api/v1/monitor/metrics (03§4.4.1):
 *
 *   ?metric=cpu_utilization|memory_utilization|request_count
 *   &resourceId=<id>            (required; must be the caller's resource)
 *   &start=&end=                (RFC3339; range ≤ 31 days)
 *   &period=<10..86400 s>       (step; ≤ 1440 points per query)
 *   → {metric, resourceId, start, end, period, source, points:[{timestamp, value}]}
 *
 * Errors: 400 Common.InvalidParameter, 404 Monitor.ResourceNotFound (not the
 * caller's resource), 503 Monitor.MetricsUnavailable / Monitor.OwnerLookupFailed.
 * `source: "synthetic"` marks a stand-in series (no Prometheus configured,
 * non-production only) — the UI must label it as sample data.
 *
 * Every query carries an explicit resource and window: the range presets pick
 * a period that keeps the series within the 1440-point cap.
 */
import type { SDK } from "@eu/sdk";

export const METRICS_PATH = "/api/v1/monitor/metrics";
export const MAX_METRIC_POINTS = 1440;
export const MIN_METRIC_PERIOD = 10;
export const MAX_METRIC_PERIOD = 86_400;

export const SUPPORTED_METRICS = [
  { value: "cpu_utilization", label: "CPU 使用率", unit: "%" },
  { value: "memory_utilization", label: "内存使用率", unit: "%" },
  { value: "request_count", label: "请求数", unit: "次" },
] as const;
export type MetricName = (typeof SUPPORTED_METRICS)[number]["value"];

export function isSupportedMetric(m: string | null | undefined): m is MetricName {
  return SUPPORTED_METRICS.some((x) => x.value === m);
}

export function metricLabel(m: string): string {
  return SUPPORTED_METRICS.find((x) => x.value === m)?.label ?? m;
}

export function isPercentMetric(m: string): boolean {
  return m === "cpu_utilization" || m === "memory_utilization";
}

/** Time-range presets; each period keeps the window within MAX_METRIC_POINTS. */
export const RANGE_PRESETS = [
  { key: "1h", label: "1 小时", seconds: 3_600, period: 60 },
  { key: "6h", label: "6 小时", seconds: 21_600, period: 300 },
  { key: "24h", label: "24 小时", seconds: 86_400, period: 600 },
  { key: "7d", label: "7 天", seconds: 604_800, period: 3_600 },
] as const;
export type RangeKey = (typeof RANGE_PRESETS)[number]["key"];

export interface MetricsWindow {
  start: string;
  end: string;
  period: number;
}

/** The [start, end] window and step of a preset ending at `now` (whole seconds, UTC). */
export function metricsWindow(key: RangeKey, now: Date = new Date()): MetricsWindow {
  const preset = RANGE_PRESETS.find((p) => p.key === key) ?? RANGE_PRESETS[0];
  const endMs = Math.floor(now.getTime() / 1000) * 1000;
  const startMs = endMs - preset.seconds * 1000;
  let period: number = preset.period;
  // points = range / period + 1 must stay within the cap.
  if (preset.seconds / period + 1 > MAX_METRIC_POINTS) period = Math.ceil(preset.seconds / (MAX_METRIC_POINTS - 1));
  period = Math.min(MAX_METRIC_PERIOD, Math.max(MIN_METRIC_PERIOD, period));
  const iso = (ms: number) => new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");
  return { start: iso(startMs), end: iso(endMs), period };
}

export interface MetricsQuery extends MetricsWindow {
  metric: string;
  resourceId: string;
}

export function metricsQueryString(q: MetricsQuery): string {
  const p = new URLSearchParams({
    metric: q.metric,
    resourceId: q.resourceId,
    start: q.start,
    end: q.end,
    period: String(q.period),
  });
  return p.toString();
}

export interface MetricPoint {
  timestamp: string;
  value: number;
}

export interface MetricSeries {
  metric: string;
  resourceId: string;
  start: string;
  end: string;
  period: number;
  source: "prometheus" | "synthetic" | string;
  points: MetricPoint[];
}

/** GET the series; refuses to call without a resource (the API requires one). */
export async function fetchMetrics(
  sdk: Pick<SDK, "get">,
  q: MetricsQuery,
  init?: { signal?: AbortSignal },
): Promise<MetricSeries> {
  if (!q.resourceId) throw Object.assign(new Error("请选择资源"), { code: "Common.InvalidParameter", status: 400 });
  const res = await sdk.get<MetricSeries>(`${METRICS_PATH}?${metricsQueryString(q)}`, init);
  const d = res.data ?? ({} as MetricSeries);
  const points = (Array.isArray(d.points) ? d.points : [])
    .filter((p) => p && typeof p.timestamp === "string" && Number.isFinite(Number(p.value)))
    .map((p) => ({ timestamp: p.timestamp, value: Number(p.value) }));
  return { ...d, points };
}

export function isSyntheticSeries(s: Pick<MetricSeries, "source"> | null | undefined): boolean {
  return s?.source === "synthetic";
}

export type MetricsErrorKind = "invalid" | "notFound" | "unavailable" | "error";

export interface MetricsErrorState {
  kind: MetricsErrorKind;
  title: string;
  detail: string;
}

/** The empty/error state for a failed metrics query. */
export function metricsErrorState(e: unknown): MetricsErrorState {
  const err = (e ?? {}) as { code?: string; status?: number; message?: string };
  const detail = err.message ?? "";
  if (err.status === 400 || err.code === "Common.InvalidParameter") {
    return { kind: "invalid", title: "查询参数无效", detail };
  }
  if (err.status === 404 || err.code === "Monitor.ResourceNotFound") {
    return { kind: "notFound", title: "资源不存在或不属于当前账号", detail: "请重新选择资源" };
  }
  if (err.code === "Monitor.OwnerLookupFailed") {
    return { kind: "unavailable", title: "资源归属暂无法校验", detail: "请稍后重试" };
  }
  if (err.status === 503 || err.code === "Monitor.MetricsUnavailable") {
    return { kind: "unavailable", title: "监控数据暂不可用", detail: detail || "监控数据源未配置或暂时故障,请稍后重试" };
  }
  return { kind: "error", title: "加载监控数据失败", detail };
}

export interface MetricSummary {
  latest: number;
  min: number;
  max: number;
  avg: number;
}

export function summarizePoints(points: readonly MetricPoint[]): MetricSummary | null {
  if (!points.length) return null;
  let min = Infinity;
  let max = -Infinity;
  let sum = 0;
  for (const p of points) {
    min = Math.min(min, p.value);
    max = Math.max(max, p.value);
    sum += p.value;
  }
  return { latest: points[points.length - 1].value, min, max, avg: sum / points.length };
}

export function formatMetricValue(v: number | null | undefined, metric: string): string {
  if (v === null || v === undefined || !Number.isFinite(v)) return "—";
  if (isPercentMetric(metric)) return `${v.toFixed(1)}%`;
  return Math.round(v).toLocaleString("zh-CN");
}

/** A rounded axis maximum ≥ v: 1, 2, 2.5, 5 × 10^n. */
export function niceMax(v: number): number {
  if (!Number.isFinite(v) || v <= 0) return 1;
  const exp = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (v <= m * exp) return m * exp;
  return 10 * exp;
}

export interface ChartGeometry {
  /** SVG polyline `points` attribute. */
  line: string;
  /** Closed area path under the line. */
  area: string;
  yTicks: { y: number; label: string }[];
  xTicks: { x: number; label: string }[];
}

/**
 * Lay a series out in a width×height box with `pad` margins (left pad holds
 * the y labels). Percent metrics use a 0–100 axis unless values exceed it.
 */
export function chartGeometry(
  points: readonly MetricPoint[],
  metric: string,
  box: { width: number; height: number; padLeft: number; padRight: number; padTop: number; padBottom: number },
  formatTick: (d: Date) => string,
): ChartGeometry {
  const { width, height, padLeft, padRight, padTop, padBottom } = box;
  const plotW = width - padLeft - padRight;
  const plotH = height - padTop - padBottom;
  const maxV = points.reduce((m, p) => Math.max(m, p.value), 0);
  const top = isPercentMetric(metric) && maxV <= 100 ? 100 : niceMax(maxV);
  const times = points.map((p) => new Date(p.timestamp).getTime());
  const t0 = times.length ? Math.min(...times) : 0;
  const t1 = times.length ? Math.max(...times) : 1;
  const span = t1 - t0 || 1;
  const xOf = (t: number) => padLeft + ((t - t0) / span) * plotW;
  const yOf = (v: number) => padTop + plotH - (Math.max(0, v) / top) * plotH;
  const coords = points.map((p, i) => `${xOf(times[i]).toFixed(1)},${yOf(p.value).toFixed(1)}`);
  const line = coords.join(" ");
  const baseY = (padTop + plotH).toFixed(1);
  const area = coords.length
    ? `M${xOf(t0).toFixed(1)},${baseY} L${coords.join(" L")} L${xOf(t1).toFixed(1)},${baseY} Z`
    : "";
  const yTicks = [0, 0.25, 0.5, 0.75, 1].map((f) => ({
    y: padTop + plotH - f * plotH,
    label: formatMetricValue(top * f, metric),
  }));
  const xTicks = times.length
    ? [0, 0.25, 0.5, 0.75, 1].map((f) => ({ x: padLeft + f * plotW, label: formatTick(new Date(t0 + f * span)) }))
    : [];
  return { line, area, yTicks, xTicks };
}

/** A /console/resources row (PascalCase BFF shape). */
export interface ConsoleResourceRow {
  ResourceId: string;
  ProductCode: string;
  Region?: string;
  Zone?: string;
  State?: string;
  Attributes?: Record<string, unknown> | null;
}

export interface ResourceOption {
  value: string;
  label: string;
}

const GONE_STATES = new Set(["RELEASED", "RELEASING", "CREATE_FAILED", "DELETED"]);

/** Selectable resources of a product: live ones, labelled "name (id) · zone". */
export function resourceOptions(rows: readonly ConsoleResourceRow[], productCode?: string): ResourceOption[] {
  return rows
    .filter((r) => r && typeof r.ResourceId === "string" && r.ResourceId !== "")
    .filter((r) => !productCode || r.ProductCode === productCode)
    .filter((r) => !GONE_STATES.has(String(r.State ?? "").toUpperCase()))
    .map((r) => {
      const rawName = r.Attributes && typeof r.Attributes === "object" ? r.Attributes.name : undefined;
      const name = typeof rawName === "string" ? rawName.trim() : "";
      const head = name && name !== r.ResourceId ? `${name} (${r.ResourceId})` : r.ResourceId;
      const where = r.Zone || r.Region || "";
      return { value: r.ResourceId, label: where ? `${head} · ${where}` : head };
    });
}
