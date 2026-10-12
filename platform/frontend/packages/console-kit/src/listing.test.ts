/**
 * @vitest-environment node
 */
import { describe, it, expect } from "vitest";
import { paginate, isTransitionalState, resourceZone, resourceDisplayName } from "./listing";

const rows = Array.from({ length: 45 }, (_, i) => i + 1);

describe("paginate", () => {
  it("slices the requested page and keeps the full total", () => {
    expect(paginate(rows, { page: 1, pageSize: 20 })).toEqual({ items: rows.slice(0, 20), total: 45 });
    expect(paginate(rows, { page: 2, pageSize: 20 })).toEqual({ items: rows.slice(20, 40), total: 45 });
    expect(paginate(rows, { page: 3, pageSize: 20 })).toEqual({ items: rows.slice(40), total: 45 });
  });

  it("clamps an out-of-range page to the last page", () => {
    expect(paginate(rows, { page: 9, pageSize: 20 }).items).toEqual(rows.slice(40));
    expect(paginate(rows, { page: 0, pageSize: 20 }).items).toEqual(rows.slice(0, 20));
  });

  it("defaults missing params and handles an empty list", () => {
    expect(paginate(rows, {}).items).toHaveLength(20);
    expect(paginate([], { page: 3, pageSize: 20 })).toEqual({ items: [], total: 0 });
  });
});

describe("isTransitionalState", () => {
  it("recognises every moving lifecycle state in any casing", () => {
    for (const s of ["INIT", "Creating", "CREATING", "Upgrading", "releasing", "Preempting"]) {
      expect(isTransitionalState(s)).toBe(true);
    }
  });

  it("rejects settled and unknown states", () => {
    for (const s of ["RUNNING", "Stopped", "Released", "Create_failed", "Restoring", "", undefined]) {
      expect(isTransitionalState(s)).toBe(false);
    }
  });
});

describe("resourceZone / resourceDisplayName", () => {
  it("reads the BFF Zone (or a legacy ZoneId), empty for regional rows", () => {
    expect(resourceZone({ Zone: "cn-north-1a", Region: "cn-north-1" })).toBe("cn-north-1a");
    expect(resourceZone({ ZoneId: "cn-north-1b" })).toBe("cn-north-1b");
    expect(resourceZone({ Zone: "", ZoneId: "cn-north-1c" })).toBe("cn-north-1c");
    expect(resourceZone({ Region: "cn-north-1" })).toBe("");
    expect(resourceZone(null)).toBe("");
  });

  it("shows the name with the id, or the id alone", () => {
    expect(resourceDisplayName("ecs-1", "web")).toBe("web (ecs-1)");
    expect(resourceDisplayName("ecs-1", "ecs-1")).toBe("ecs-1");
    expect(resourceDisplayName("ecs-1", "  ")).toBe("ecs-1");
    expect(resourceDisplayName("ecs-1")).toBe("ecs-1");
  });
});
