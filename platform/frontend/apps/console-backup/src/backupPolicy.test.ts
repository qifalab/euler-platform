/**
 * @vitest-environment node
 *
 * EUBACKUP policy attributes: the pkg-go/backup schedule grammar and the
 * retention bound.
 */
import { describe, it, expect } from "vitest";
import { describeSchedule, everyHours, formatSchedule, parseSchedule, retentionError, scheduleError } from "./backupPolicy";

describe("parseSchedule", () => {
  it("reads both forms, case-insensitively, with the HH:MM shorthand", () => {
    expect(parseSchedule("every 6h")).toEqual({ kind: "every", hours: 6 });
    expect(parseSchedule("  EVERY 720H ")).toEqual({ kind: "every", hours: 720 });
    expect(parseSchedule("daily 02:30")).toEqual({ kind: "daily", hour: 2, minute: 30 });
    expect(parseSchedule("23:59")).toEqual({ kind: "daily", hour: 23, minute: 59 });
  });

  it("rejects what ParseSchedule rejects", () => {
    for (const s of ["", "every 0h", "every 721h", "every 06h", "every h", "every 6", "daily 24:00", "daily 2:30", "12:60", "weekly 02:00", "*/5 * * * *"]) {
      expect(parseSchedule(s)).toBeNull();
    }
  });

  it("formats the canonical text the orchestrator stores", () => {
    expect(formatSchedule({ kind: "daily", hour: 2, minute: 5 })).toBe("daily 02:05");
    expect(everyHours(24)).toBe("every 24h");
    expect(formatSchedule(parseSchedule("Daily 07:00")!)).toBe("daily 07:00");
  });

  it("describes schedules for display", () => {
    expect(describeSchedule("every 24h")).toBe("每 24 小时");
    expect(describeSchedule("daily 02:30")).toBe("每天 02:30");
    expect(describeSchedule(undefined)).toBe("—");
    expect(describeSchedule("legacy")).toBe("legacy");
  });
});

describe("rules", () => {
  it("scheduleError", () => {
    expect(scheduleError({ kind: "every", hours: 1 })).toBeNull();
    expect(scheduleError({ kind: "every", hours: 721 })).toContain("720");
    expect(scheduleError({ kind: "every", hours: 1.5 })).toContain("整数");
    expect(scheduleError({ kind: "daily", hour: 24, minute: 0 })).toContain("时间");
  });

  it("retentionError: 1–3650 days", () => {
    expect(retentionError(1)).toBeNull();
    expect(retentionError(3650)).toBeNull();
    expect(retentionError(0)).toContain("1-3650");
    expect(retentionError(3651)).toContain("1-3650");
    expect(retentionError(undefined)).toContain("请填写");
  });
});
