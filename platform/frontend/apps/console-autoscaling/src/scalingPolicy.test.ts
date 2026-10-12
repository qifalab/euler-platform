/**
 * @vitest-environment node
 */
import { describe, it, expect } from "vitest";
import { boundsFromAttributes, policyAttributes, validateScalingPolicy } from "./scalingPolicy";

const ok = { minReplicas: 1, maxReplicas: 10, desiredReplicas: 3, cooldownSeconds: 300 };

describe("validateScalingPolicy (mirrors pkg-go/autoscaling Validate)", () => {
  it("accepts min <= desired <= max, including the edges", () => {
    expect(validateScalingPolicy(ok)).toBeNull();
    expect(validateScalingPolicy({ ...ok, desiredReplicas: 1 })).toBeNull();
    expect(validateScalingPolicy({ ...ok, desiredReplicas: 10 })).toBeNull();
    expect(validateScalingPolicy({ ...ok, minReplicas: 0, desiredReplicas: 0 })).toBeNull();
  });

  it("rejects desired outside [min, max]", () => {
    expect(validateScalingPolicy({ ...ok, desiredReplicas: 0 })).toContain("期望副本数");
    expect(validateScalingPolicy({ ...ok, desiredReplicas: 11 })).toContain("期望副本数");
  });

  it("rejects max < min, negative min and negative cooldown", () => {
    expect(validateScalingPolicy({ ...ok, minReplicas: 5, maxReplicas: 4, desiredReplicas: 4 })).toContain("最大副本数");
    expect(validateScalingPolicy({ ...ok, minReplicas: -1 })).toContain("最小副本数");
    expect(validateScalingPolicy({ ...ok, cooldownSeconds: -1 })).toContain("冷却");
  });

  it("rejects a cleared number input", () => {
    expect(validateScalingPolicy({ ...ok, desiredReplicas: undefined as unknown as number })).not.toBeNull();
  });
});

describe("policy attributes (euas minSize/maxSize/desiredCapacity/cooldownSeconds)", () => {
  it("maps the wizard bounds to the attribute keys and back", () => {
    expect(policyAttributes(ok)).toEqual({ minSize: 1, maxSize: 10, desiredCapacity: 3, cooldownSeconds: 300 });
    expect(boundsFromAttributes({ minSize: "1", maxSize: 10, desiredCapacity: "3", cooldownSeconds: 300 })).toEqual(ok);
  });

  it("validates edited attributes with the same rules", () => {
    expect(validateScalingPolicy(boundsFromAttributes({ minSize: 2, maxSize: 4, desiredCapacity: 5, cooldownSeconds: 0 }))).toContain("期望副本数");
    expect(validateScalingPolicy(boundsFromAttributes({ minSize: 2, maxSize: 4, desiredCapacity: undefined, cooldownSeconds: 0 }))).not.toBeNull();
  });
});
