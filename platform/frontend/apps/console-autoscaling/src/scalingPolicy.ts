/**
 * Client-side mirror of pkg-go/autoscaling ScalingPolicy.Validate: the
 * orchestrator rejects a policy outside these bounds, so the wizard checks
 * them before an order is created and paid.
 */
export interface ScalingBounds {
  minReplicas: number;
  maxReplicas: number;
  desiredReplicas: number;
  cooldownSeconds: number;
}

/** The first violated rule as a user-facing message, or null when valid. */
export function validateScalingPolicy(p: ScalingBounds): string | null {
  const nums = [p.minReplicas, p.maxReplicas, p.desiredReplicas, p.cooldownSeconds];
  if (nums.some((n) => typeof n !== "number" || !Number.isFinite(n))) return "请填写完整的副本数与冷却时间";
  if (p.minReplicas < 0) return "最小副本数不能小于 0";
  if (p.maxReplicas < p.minReplicas) return "最大副本数不能小于最小副本数";
  if (p.desiredReplicas < p.minReplicas || p.desiredReplicas > p.maxReplicas) {
    return `期望副本数必须在 [${p.minReplicas}, ${p.maxReplicas}] 之间`;
  }
  if (p.cooldownSeconds < 0) return "冷却时间不能为负";
  return null;
}

/**
 * The euas policy as resource attributes (svc-orchestrator fulfil `params` /
 * PATCH .../attributes): minSize, maxSize, desiredCapacity, cooldownSeconds.
 */
export const POLICY_ATTRIBUTE_KEYS = ["minSize", "maxSize", "desiredCapacity", "cooldownSeconds"] as const;

/** Wizard/form bounds → attribute values (numbers; the caller stringifies). */
export function policyAttributes(p: ScalingBounds): Record<(typeof POLICY_ATTRIBUTE_KEYS)[number], number> {
  return {
    minSize: p.minReplicas,
    maxSize: p.maxReplicas,
    desiredCapacity: p.desiredReplicas,
    cooldownSeconds: p.cooldownSeconds,
  };
}

/** Attribute values (as edited, keyed by attribute name) → bounds for validation. */
export function boundsFromAttributes(v: Record<string, number | string | undefined>): ScalingBounds {
  const num = (x: number | string | undefined) => (typeof x === "string" && x.trim() !== "" ? Number(x) : (x as number));
  return {
    minReplicas: num(v.minSize),
    maxReplicas: num(v.maxSize),
    desiredReplicas: num(v.desiredCapacity),
    cooldownSeconds: num(v.cooldownSeconds),
  };
}
