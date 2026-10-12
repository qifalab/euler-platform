/**
 * Alert-rule form model — the client mirror of svc-monitor's rule handlers
 * (POST /api/v1/monitor/rules, PUT /api/v1/monitor/rules/{id}):
 *  - productCode / metric / threshold are required (resourceType too: it is
 *    part of the rule's natural key);
 *  - threshold is a plain decimal with at most 4 decimal places that fits
 *    DECIMAL(18,4) (≤ 14 integer digits);
 *  - comparisonOperator is 1(>) 2(≥) 3(<) 4(≤);
 *  - period / evalPeriods are positive integers (the update path refuses 0;
 *    the form never relies on create's "0 = default");
 *  - status is 1 (enabled) or 2 (disabled); updates carry the row version
 *    (optimistic lock → 409 Monitor.VersionConflict).
 * Create is an upsert on (productCode, resourceType, metric): creating a rule
 * that already exists re-parameters it.
 */

/** A rule as svc-monitor returns it. */
export interface MonitorRule {
  ruleId: number;
  productCode: string;
  resourceType: string;
  metric: string;
  threshold: string;
  comparisonOperator: number;
  period: number;
  evalPeriods: number;
  notificationChannels: string[];
  status: number;
  version: number;
}

export interface RuleFormValues {
  productCode: string;
  resourceType: string;
  metric: string;
  threshold: string;
  comparisonOperator: number;
  period: number;
  evalPeriods: number;
  notificationChannels: string[];
}

export const RULE_STATUS_ENABLED = 1;
export const RULE_STATUS_DISABLED = 2;

export const COMPARISON_OPERATORS: ReadonlyArray<{ value: number; label: string }> = [
  { value: 1, label: "大于 (>)" },
  { value: 2, label: "大于等于 (≥)" },
  { value: 3, label: "小于 (<)" },
  { value: 4, label: "小于等于 (≤)" },
];

/** Symbol of a comparison operator ("" when unknown). */
export function comparisonSymbol(op: number): string {
  return ({ 1: ">", 2: "≥", 3: "<", 4: "≤" } as Record<number, string>)[op] ?? "";
}

/** alert_rule.notification_channels_json values. */
export const NOTIFICATION_CHANNELS: ReadonlyArray<{ value: string; label: string }> = [
  { value: "IN_APP", label: "站内信" },
  { value: "EMAIL", label: "邮件" },
  { value: "SMS", label: "短信" },
  { value: "WEBHOOK", label: "Webhook" },
];

/** Column widths of alert_rule (VARCHAR). */
export const MAX_PRODUCT_CODE = 32;
export const MAX_RESOURCE_TYPE = 32;
export const MAX_METRIC = 64;
export const PERIOD_MAX = 86_400;
export const EVAL_PERIODS_MAX = 100;

export function emptyRuleForm(): RuleFormValues {
  return {
    productCode: "",
    resourceType: "",
    metric: "",
    threshold: "",
    comparisonOperator: 2,
    period: 60,
    evalPeriods: 1,
    notificationChannels: ["IN_APP"],
  };
}

const THRESHOLD_RE = /^([+-]?)([0-9]*)(?:\.([0-9]*))?$/;

/** svc-monitor normalizeThreshold's rules; a message or null. */
export function thresholdError(raw: string): string | null {
  const s = (raw ?? "").trim();
  if (!s) return "请输入阈值";
  const m = THRESHOLD_RE.exec(s);
  const frac = m?.[3] ?? "";
  if (!m || (m[2] === "" && frac === "")) return "阈值须为十进制数字,如 80 或 0.5";
  if (frac.length > 4) return "阈值最多保留 4 位小数";
  if (m[2].replace(/^0+/, "").length > 14) return "阈值超出范围";
  return null;
}

/** "80.0000" → "80", "0.5000" → "0.5" (the stored DECIMAL(18,4) for display). */
export function displayThreshold(raw: string): string {
  const s = String(raw ?? "").trim();
  if (!/^[+-]?[0-9]*\.[0-9]+$/.test(s)) return s;
  return s.replace(/0+$/, "").replace(/\.$/, "");
}

function positiveInt(n: unknown, max: number, label: string): string | null {
  if (typeof n !== "number" || !Number.isFinite(n)) return `请填写${label}`;
  if (!Number.isInteger(n) || n < 1) return `${label}须为正整数`;
  if (n > max) return `${label}不能超过 ${max}`;
  return null;
}

/** The first rule the form breaks, or null. `mode` "update" skips the key fields (immutable). */
export function validateRuleForm(v: RuleFormValues, mode: "create" | "update" = "create"): string | null {
  if (mode === "create") {
    const product = v.productCode.trim();
    if (!product) return "请选择产品";
    if (product.length > MAX_PRODUCT_CODE) return `产品代号不能超过 ${MAX_PRODUCT_CODE} 个字符`;
    const rtype = v.resourceType.trim();
    if (!rtype) return "请填写资源类型";
    if (rtype.length > MAX_RESOURCE_TYPE) return `资源类型不能超过 ${MAX_RESOURCE_TYPE} 个字符`;
    const metric = v.metric.trim();
    if (!metric) return "请选择或输入监控指标";
    if (metric.length > MAX_METRIC) return `指标名不能超过 ${MAX_METRIC} 个字符`;
  }
  const t = thresholdError(v.threshold);
  if (t) return t;
  if (!COMPARISON_OPERATORS.some((o) => o.value === v.comparisonOperator)) return "请选择比较方式";
  return (
    positiveInt(v.period, PERIOD_MAX, "统计周期") ??
    positiveInt(v.evalPeriods, EVAL_PERIODS_MAX, "连续周期数") ??
    (v.notificationChannels.length === 0 ? "请至少选择一个通知渠道" : null)
  );
}

/** POST /api/v1/monitor/rules body. */
export function buildCreateRuleBody(v: RuleFormValues) {
  return {
    productCode: v.productCode.trim(),
    resourceType: v.resourceType.trim(),
    metric: v.metric.trim(),
    threshold: v.threshold.trim(),
    comparisonOperator: v.comparisonOperator,
    period: v.period,
    evalPeriods: v.evalPeriods,
    notificationChannels: [...v.notificationChannels],
  };
}

/** PUT /api/v1/monitor/rules/{id} body (the key fields are immutable). */
export function buildUpdateRuleBody(v: RuleFormValues, version: number) {
  return {
    threshold: v.threshold.trim(),
    comparisonOperator: v.comparisonOperator,
    period: v.period,
    evalPeriods: v.evalPeriods,
    notificationChannels: [...v.notificationChannels],
    version,
  };
}

/** The form values of an existing rule. */
export function ruleToForm(r: MonitorRule): RuleFormValues {
  return {
    productCode: r.productCode ?? "",
    resourceType: r.resourceType ?? "",
    metric: r.metric ?? "",
    threshold: displayThreshold(r.threshold ?? ""),
    comparisonOperator: r.comparisonOperator || 2,
    period: r.period || 60,
    evalPeriods: r.evalPeriods || 1,
    notificationChannels: r.notificationChannels?.length ? [...r.notificationChannels] : ["IN_APP"],
  };
}

/** The existing rule a create would upsert (same natural key), if any. */
export function findSameKeyRule(rules: MonitorRule[], v: RuleFormValues): MonitorRule | undefined {
  const key = (r: Pick<MonitorRule, "productCode" | "resourceType" | "metric">) =>
    `${r.productCode.trim()}|${r.resourceType.trim()}|${r.metric.trim()}`;
  return rules.find((r) => key(r) === key(v));
}

/** User-facing message for a failed rule write. */
export function ruleErrorMessage(e: unknown): string {
  const err = (e ?? {}) as { message?: string; code?: string; status?: number };
  if (err.code === "Monitor.VersionConflict" || err.status === 409) return "规则已被他人修改,请刷新后重试";
  if (err.code === "Monitor.RuleNotFound" || err.status === 404) return "规则不存在或已删除";
  return err.message || err.code || "保存失败";
}
