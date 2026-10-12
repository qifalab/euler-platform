/**
 * @vitest-environment node
 *
 * Metrics queries always carry a resource and a window within the API's
 * limits; synthetic series and 400/404/503 answers map to clear states.
 */
import { describe, expect, it, vi } from "vitest";
import {
  chartGeometry,
  fetchMetrics,
  formatMetricValue,
  isSupportedMetric,
  isSyntheticSeries,
  MAX_METRIC_POINTS,
  metricsErrorState,
  metricsQueryString,
  metricsWindow,
  niceMax,
  RANGE_PRESETS,
  resourceOptions,
  summarizePoints,
} from "./metrics";

const NOW = new Date("2026-10-10T08:30:15.678Z");

describe("metricsWindow", () => {
  it("ends now (whole seconds) and keeps every preset within the point cap", () => {
    for (const p of RANGE_PRESETS) {
      const w = metricsWindow(p.key, NOW);
      expect(w.end).toBe("2026-10-10T08:30:15Z");
      const span = (new Date(w.end).getTime() - new Date(w.start).getTime()) / 1000;
      expect(span).toBe(p.seconds);
      expect(span / w.period + 1).toBeLessThanOrEqual(MAX_METRIC_POINTS);
      expect(w.period).toBeGreaterThanOrEqual(10);
      expect(span).toBeLessThanOrEqual(31 * 86_400);
    }
    expect(metricsWindow("1h", NOW)).toEqual({ start: "2026-10-10T07:30:15Z", end: "2026-10-10T08:30:15Z", period: 60 });
    expect(metricsWindow("7d", NOW).period).toBe(3600);
  });
});

describe("fetchMetrics", () => {
  it("sends metric, resourceId, start, end and period", async () => {
    const get = vi.fn(async () => ({
      data: { metric: "cpu_utilization", resourceId: "ecs-1", start: "", end: "", period: 60, source: "synthetic",
        points: [{ timestamp: "2026-10-10T08:00:00Z", value: 12.5 }, { timestamp: "bad", value: "x" }] },
    }));
    const w = metricsWindow("1h", NOW);
    const s = await fetchMetrics({ get: get as never }, { metric: "cpu_utilization", resourceId: "ecs-1", ...w });
    const url = String((get.mock.calls[0] as unknown[])[0]);
    expect(url.startsWith("/api/v1/monitor/metrics?")).toBe(true);
    const q = new URLSearchParams(url.split("?")[1]);
    expect(Object.fromEntries(q)).toEqual({
      metric: "cpu_utilization", resourceId: "ecs-1", start: w.start, end: w.end, period: "60",
    });
    expect(s.points).toEqual([{ timestamp: "2026-10-10T08:00:00Z", value: 12.5 }]);
    expect(isSyntheticSeries(s)).toBe(true);
    expect(isSyntheticSeries({ source: "prometheus" })).toBe(false);
  });

  it("never calls without a resource", async () => {
    const get = vi.fn();
    await expect(fetchMetrics({ get: get as never }, { metric: "cpu_utilization", resourceId: "", ...metricsWindow("1h", NOW) }))
      .rejects.toMatchObject({ status: 400 });
    expect(get).not.toHaveBeenCalled();
    expect(metricsQueryString({ metric: "m", resourceId: "a b", start: "s", end: "e", period: 60 })).toContain("resourceId=a+b");
  });
});

describe("metricsErrorState", () => {
  it("maps 400 / 404 / 503 to distinct states", () => {
    expect(metricsErrorState({ status: 400, code: "Common.InvalidParameter", message: "period must be…" }))
      .toMatchObject({ kind: "invalid", detail: "period must be…" });
    expect(metricsErrorState({ status: 404, code: "Monitor.ResourceNotFound" }).kind).toBe("notFound");
    expect(metricsErrorState({ status: 503, code: "Monitor.MetricsUnavailable", message: "监控数据源未配置" }))
      .toMatchObject({ kind: "unavailable", title: "监控数据暂不可用", detail: "监控数据源未配置" });
    expect(metricsErrorState({ status: 503, code: "Monitor.OwnerLookupFailed" }).title).toContain("归属");
    expect(metricsErrorState(new Error("offline")).kind).toBe("error");
  });
});

describe("display helpers", () => {
  it("summarises and formats", () => {
    const pts = [1, 5, 3].map((v, i) => ({ timestamp: `2026-10-10T08:0${i}:00Z`, value: v }));
    expect(summarizePoints(pts)).toEqual({ latest: 3, min: 1, max: 5, avg: 3 });
    expect(summarizePoints([])).toBeNull();
    expect(formatMetricValue(12.345, "cpu_utilization")).toBe("12.3%");
    expect(formatMetricValue(1234.4, "request_count")).toBe("1,234");
    expect(formatMetricValue(undefined, "request_count")).toBe("—");
    expect(niceMax(73)).toBe(100);
    expect(niceMax(180)).toBe(200);
    expect(niceMax(0)).toBe(1);
    expect(isSupportedMetric("memory_utilization")).toBe(true);
    expect(isSupportedMetric("disk_iops")).toBe(false);
  });

  it("lays out the chart on a 0–100 axis for percentages", () => {
    const pts = [{ timestamp: "2026-10-10T08:00:00Z", value: 0 }, { timestamp: "2026-10-10T09:00:00Z", value: 50 }];
    const box = { width: 200, height: 120, padLeft: 40, padRight: 10, padTop: 10, padBottom: 20 };
    const g = chartGeometry(pts, "cpu_utilization", box, () => "t");
    expect(g.line).toBe("40.0,100.0 190.0,55.0");
    expect(g.yTicks.at(-1)?.label).toBe("100.0%");
    expect(g.xTicks).toHaveLength(5);
    expect(g.area.startsWith("M40.0,100.0")).toBe(true);
    expect(chartGeometry([], "request_count", box, () => "t").line).toBe("");
  });

  it("offers live resources of the product, labelled by name and zone", () => {
    const rows = [
      { ResourceId: "ecs-1", ProductCode: "euecs", Zone: "cn-north-1a", State: "RUNNING", Attributes: { name: "web" } },
      { ResourceId: "ecs-2", ProductCode: "euecs", Region: "cn-north-1", State: "STOPPED" },
      { ResourceId: "ecs-3", ProductCode: "euecs", State: "RELEASED" },
      { ResourceId: "rds-1", ProductCode: "eurds", State: "RUNNING" },
    ];
    expect(resourceOptions(rows, "euecs")).toEqual([
      { value: "ecs-1", label: "web (ecs-1) · cn-north-1a" },
      { value: "ecs-2", label: "ecs-2 · cn-north-1" },
    ]);
    expect(resourceOptions(rows).map((o) => o.value)).toEqual(["ecs-1", "ecs-2", "rds-1"]);
  });
});
