/**
 * @vitest-environment node
 *
 * EULOG attribute bounds (storageGb, retentionDays).
 */
import { describe, it, expect } from "vitest";
import { logAttributes, retentionError, storageError } from "./logPolicy";

describe("eulog attribute rules", () => {
  it("storageGb: integer 10–100000", () => {
    expect(storageError(200)).toBeNull();
    expect(storageError(9)).toContain("10-100000");
    expect(storageError(100001)).toContain("10-100000");
    expect(storageError(20.5)).toContain("整数");
  });

  it("retentionDays: integer 1–3650", () => {
    expect(retentionError(30)).toBeNull();
    expect(retentionError(0)).toContain("1-3650");
    expect(retentionError(undefined)).toContain("请填写");
  });

  it("logAttributes picks the attribute keys only", () => {
    expect(logAttributes({ storageGb: 200, retentionDays: 30, crossAz: true } as never)).toEqual({ storageGb: 200, retentionDays: 30 });
  });
});
