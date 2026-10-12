import { describe, expect, it } from "vitest";
import { formatDate } from "./format";

describe("formatDate", () => {
  it("uses the viewer's time zone, not the UTC day of the string", () => {
    // 20:00 UTC on Oct 1 is already Oct 2 in Beijing (UTC+8).
    expect(formatDate("2026-10-01T20:00:00Z", { locale: "en-CA", timeZone: "Asia/Shanghai" })).toBe("2026-10-02");
    expect(formatDate("2026-10-01T20:00:00Z", { locale: "en-CA", timeZone: "UTC" })).toBe("2026-10-01");
  });

  it("returns empty for missing values and the raw string for unparseable ones", () => {
    expect(formatDate("")).toBe("");
    expect(formatDate(undefined)).toBe("");
    expect(formatDate("not-a-date")).toBe("not-a-date");
  });
});
