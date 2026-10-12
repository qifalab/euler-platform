import { describe, expect, it } from "vitest";
import { EMPTY_TIME, formatTime } from "./format";

describe("formatTime", () => {
  it("renders the instant in the viewer's time zone without RFC3339 artefacts", () => {
    // 20:00 UTC on Oct 1 is 04:00 on Oct 2 in Beijing.
    expect(formatTime("2026-10-01T20:00:00Z", { timeZone: "Asia/Shanghai" })).toBe("2026/10/2 04:00:00");
    const local = formatTime("2026-10-01T20:00:00Z");
    expect(local).toBe(new Date("2026-10-01T20:00:00Z").toLocaleString("zh-CN", { hour12: false }));
    expect(local).not.toMatch(/[TZ]/);
  });

  it("shows the placeholder for empty, invalid and zero values", () => {
    expect(formatTime("")).toBe(EMPTY_TIME);
    expect(formatTime(undefined)).toBe(EMPTY_TIME);
    expect(formatTime("garbage")).toBe(EMPTY_TIME);
    expect(formatTime("0001-01-01T00:00:00Z")).toBe(EMPTY_TIME);
  });
});
