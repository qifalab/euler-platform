/**
 * @vitest-environment node
 */
import { describe, it, expect } from "vitest";
import { formatTime, formatDate, EMPTY_TIME } from "./format";

describe("formatTime", () => {
  it("renders empty / missing values as the placeholder", () => {
    expect(formatTime("")).toBe(EMPTY_TIME);
    expect(formatTime(undefined)).toBe(EMPTY_TIME);
    expect(formatTime(null)).toBe(EMPTY_TIME);
  });

  it("treats unparseable input as absent", () => {
    expect(formatTime("not-a-time")).toBe(EMPTY_TIME);
  });

  it("hides Go zero times and pre-epoch placeholders", () => {
    expect(formatTime("0001-01-01T00:00:00Z")).toBe(EMPTY_TIME);
    expect(formatTime("1969-12-31T23:59:59Z")).toBe(EMPTY_TIME);
  });

  it("renders a real instant in local time without the RFC3339 'T'/'Z' artefacts", () => {
    const s = "2026-09-15T14:00:00Z";
    const out = formatTime(s);
    expect(out).toBe(new Date(s).toLocaleString("zh-CN", { hour12: false }));
    expect(out).not.toContain("T");
    expect(out).not.toContain("Z");
    expect(out).toContain("2026");
  });

  it("honours the offset instead of stripping it", () => {
    // Same instant, two spellings → same rendering.
    expect(formatTime("2026-09-15T22:00:00+08:00")).toBe(formatTime("2026-09-15T14:00:00Z"));
  });
});

describe("formatDate", () => {
  it("uses the viewer's time zone, not the UTC day of the string", () => {
    // 20:00 UTC on Oct 1 is already Oct 2 in Beijing (UTC+8).
    expect(formatDate("2026-10-01T20:00:00Z", { timeZone: "Asia/Shanghai" })).toBe("2026/10/02");
    expect(formatDate("2026-10-01T20:00:00Z", { timeZone: "UTC" })).toBe("2026/10/01");
  });

  it("defaults to the local time zone", () => {
    const s = "2026-10-01T20:00:00Z";
    expect(formatDate(s)).toBe(
      new Date(s).toLocaleDateString("zh-CN", { year: "numeric", month: "2-digit", day: "2-digit" }),
    );
  });

  it("renders empty, invalid and zero values as the placeholder", () => {
    expect(formatDate("")).toBe(EMPTY_TIME);
    expect(formatDate(undefined)).toBe(EMPTY_TIME);
    expect(formatDate("garbage")).toBe(EMPTY_TIME);
    expect(formatDate("0001-01-01T00:00:00Z")).toBe(EMPTY_TIME);
  });
});
