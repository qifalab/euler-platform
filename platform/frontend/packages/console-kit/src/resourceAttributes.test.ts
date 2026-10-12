/**
 * @vitest-environment node
 *
 * Resource attributes: reading `attributes` off list/detail rows, building the
 * {string:string} params map, the shared field rules and the PATCH call.
 */
import { describe, it, expect, vi } from "vitest";
import {
  attributeErrorMessage,
  attributesOf,
  numberAttr,
  patchResourceAttributes,
  toParams,
  validateIntRange,
  validateResourceName,
} from "./resourceAttributes";

const apiError = (status: number, code: string, message = "boom") => Object.assign(new Error(message), { status, code });

describe("attributesOf", () => {
  it("reads camelCase and PascalCase maps and stringifies scalars", () => {
    expect(attributesOf({ attributes: { name: "web", sizeGb: 40, on: true, bad: { x: 1 } } })).toEqual({ name: "web", sizeGb: "40", on: "true" });
    expect(attributesOf({ Attributes: { cidrBlock: "10.0.0.0/16" } })).toEqual({ cidrBlock: "10.0.0.0/16" });
  });

  it("is {} for rows without attributes", () => {
    expect(attributesOf(null)).toEqual({});
    expect(attributesOf({ resourceId: "r" })).toEqual({});
    expect(attributesOf({ attributes: ["x"] })).toEqual({});
  });

  it("numberAttr falls back on absent / non-numeric values", () => {
    expect(numberAttr({ a: "30" }, "a")).toBe(30);
    expect(numberAttr({ a: "x" }, "a", 7)).toBe(7);
    expect(numberAttr({}, "a", 7)).toBe(7);
    expect(numberAttr({ a: " " }, "a")).toBeUndefined();
  });
});

describe("toParams", () => {
  it("stringifies, trims and drops empty values", () => {
    expect(toParams({ name: "  db1 ", sizeGb: 40, empty: "", none: undefined, nil: null, nan: Number.NaN, ha: false })).toEqual({
      name: "db1", sizeGb: "40", ha: "false",
    });
  });
});

describe("field rules", () => {
  it("validateResourceName: 1–64 characters", () => {
    expect(validateResourceName("")).toBeNull();
    expect(validateResourceName("", { required: true })).toContain("请输入");
    expect(validateResourceName("数".repeat(64))).toBeNull();
    expect(validateResourceName("a".repeat(65))).toContain("1-64");
  });

  it("validateIntRange", () => {
    expect(validateIntRange(20, 20, 32768, "容量")).toBeNull();
    expect(validateIntRange("30", 1, 3650, "保留期")).toBeNull();
    expect(validateIntRange(0, 1, 3650, "保留期")).toContain("1-3650");
    expect(validateIntRange(1.5, 1, 10, "x")).toContain("整数");
    expect(validateIntRange(undefined, 1, 10, "x")).toContain("请填写");
    expect(validateIntRange("", 1, 10, "x")).toContain("请填写");
  });
});

describe("attributeErrorMessage", () => {
  it("maps the PATCH contract errors", () => {
    expect(attributeErrorMessage(apiError(400, "Resource.AttributeImmutable", "cidrBlock is immutable"))).toContain("不可修改");
    expect(attributeErrorMessage(apiError(400, "Resource.InvalidAttribute", "retentionDays out of range"))).toContain("retentionDays out of range");
    expect(attributeErrorMessage(apiError(409, "Resource.StateConflict", "not live"))).toContain("状态");
    expect(attributeErrorMessage(apiError(500, "Resource.Failed", "oops"))).toBe("oops");
  });
});

describe("patchResourceAttributes", () => {
  it("PATCHes {attributes} with string values and returns the detail", async () => {
    const patch = vi.fn().mockResolvedValue({ data: { resourceId: "eubackup-1", attributes: { retentionDays: "7" } } });
    const detail = await patchResourceAttributes({ patch }, "eubackup-1", { retentionDays: 7, schedule: "24" });
    expect(patch).toHaveBeenCalledWith("/api/v1/orchestrator/resources/eubackup-1/attributes", {
      attributes: { retentionDays: "7", schedule: "24" },
    });
    expect(detail).toMatchObject({ resourceId: "eubackup-1" });
  });
});
