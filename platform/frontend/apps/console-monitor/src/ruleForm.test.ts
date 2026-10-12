/**
 * @vitest-environment node
 *
 * Alert-rule form: the svc-monitor field rules and the create/update bodies.
 */
import { describe, it, expect } from "vitest";
import {
  buildCreateRuleBody,
  buildUpdateRuleBody,
  displayThreshold,
  emptyRuleForm,
  findSameKeyRule,
  ruleErrorMessage,
  ruleToForm,
  thresholdError,
  validateRuleForm,
  type MonitorRule,
  type RuleFormValues,
} from "./ruleForm";

const valid = (): RuleFormValues => ({
  ...emptyRuleForm(),
  productCode: "euecs", resourceType: "instance", metric: "cpu_utilization", threshold: "80",
});

const rule: MonitorRule = {
  ruleId: 7, productCode: "euecs", resourceType: "instance", metric: "cpu_utilization",
  threshold: "80.5000", comparisonOperator: 1, period: 300, evalPeriods: 3,
  notificationChannels: ["SMS"], status: 1, version: 4,
};

describe("thresholdError (normalizeThreshold)", () => {
  it("accepts decimals with up to 4 places", () => {
    for (const s of ["80", "0.5", "-1.25", "+3", ".5", "5.", "12345678901234.1234"]) expect(thresholdError(s)).toBeNull();
  });
  it("rejects non-decimals, >4 places and out-of-range values", () => {
    expect(thresholdError("")).toContain("请输入");
    expect(thresholdError("abc")).toContain("十进制");
    expect(thresholdError(".")).toContain("十进制");
    expect(thresholdError("1e5")).toContain("十进制");
    expect(thresholdError("0.12345")).toContain("4 位");
    expect(thresholdError("123456789012345")).toContain("范围");
    expect(thresholdError("000000000000000001")).toBeNull();
  });
  it("displays the stored DECIMAL compactly", () => {
    expect(displayThreshold("80.0000")).toBe("80");
    expect(displayThreshold("0.5000")).toBe("0.5");
    expect(displayThreshold("80")).toBe("80");
  });
});

describe("validateRuleForm", () => {
  it("passes a complete form", () => {
    expect(validateRuleForm(valid())).toBeNull();
  });
  it("requires the key fields on create only", () => {
    expect(validateRuleForm({ ...valid(), productCode: "" })).toContain("产品");
    expect(validateRuleForm({ ...valid(), resourceType: " " })).toContain("资源类型");
    expect(validateRuleForm({ ...valid(), metric: "" })).toContain("指标");
    expect(validateRuleForm({ ...valid(), metric: "m".repeat(65) })).toContain("64");
    expect(validateRuleForm({ ...valid(), metric: "" }, "update")).toBeNull();
  });
  it("checks operator, periods and channels", () => {
    expect(validateRuleForm({ ...valid(), comparisonOperator: 0 })).toContain("比较方式");
    expect(validateRuleForm({ ...valid(), period: 0 })).toContain("统计周期");
    expect(validateRuleForm({ ...valid(), period: 1.5 })).toContain("正整数");
    expect(validateRuleForm({ ...valid(), evalPeriods: -1 })).toContain("连续周期数");
    expect(validateRuleForm({ ...valid(), notificationChannels: [] })).toContain("通知渠道");
    expect(validateRuleForm({ ...valid(), threshold: "1.23456" })).toContain("4 位");
  });
});

describe("rule bodies", () => {
  it("builds the POST body with trimmed key fields", () => {
    const body = buildCreateRuleBody({ ...valid(), metric: " cpu_utilization ", threshold: " 80 " });
    expect(body).toEqual({
      productCode: "euecs", resourceType: "instance", metric: "cpu_utilization", threshold: "80",
      comparisonOperator: 2, period: 60, evalPeriods: 1, notificationChannels: ["IN_APP"],
    });
  });
  it("builds the PUT body with the version and without key fields", () => {
    const body = buildUpdateRuleBody(ruleToForm(rule), rule.version);
    expect(body).toEqual({ threshold: "80.5", comparisonOperator: 1, period: 300, evalPeriods: 3, notificationChannels: ["SMS"], version: 4 });
    expect(body).not.toHaveProperty("metric");
  });
  it("finds the rule a create would upsert", () => {
    expect(findSameKeyRule([rule], valid())?.ruleId).toBe(7);
    expect(findSameKeyRule([rule], { ...valid(), metric: "memory_utilization" })).toBeUndefined();
  });
  it("maps write errors", () => {
    expect(ruleErrorMessage(Object.assign(new Error("x"), { status: 409, code: "Monitor.VersionConflict" }))).toContain("刷新");
    expect(ruleErrorMessage(Object.assign(new Error("threshold allows at most 4 decimal places"), { status: 400, code: "Common.InvalidParameter" })))
      .toBe("threshold allows at most 4 decimal places");
  });
});
