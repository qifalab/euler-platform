/**
 * @vitest-environment node
 *
 * AccessKey view model: the 2-per-identity cap counts disabled keys, a null
 * lastUsed reads 从未使用, 32-char ids are masked, grace windows are shown.
 */
import { describe, expect, it } from "vitest";
import {
  akCapReached,
  akCreateBody,
  akErrorMessage,
  akListPath,
  lastUsedText,
  maskAccessKeyId,
  NEVER_USED,
  toAccessKeyView,
} from "./accessKeys";

const ID32 = "EUAK0123456789abcdefghijklmnopqr";

describe("toAccessKeyView", () => {
  it("tolerates lastUsed null and 32-char ids", () => {
    expect(ID32).toHaveLength(32);
    const v = toAccessKeyView({ akId: ID32, status: 1, createdAt: "2026-10-01T00:00:00Z", lastUsed: null, rotationGrace: false });
    expect(v.id).toBe(ID32);
    expect(v.maskedId).toBe("EUAK0123****mnopqr");
    expect(v.lastUsed).toBe(NEVER_USED);
    expect(v.status).toBe("active");
    expect(v.graceUntil).toBe("");
    expect(v.createdAt).not.toBe("—");
  });

  it("shows the grace deadline of a key being rotated out and the RAM owner", () => {
    const v = toAccessKeyView({ akId: ID32, status: 1, rotationGrace: true, graceUntil: "2026-10-04T00:00:00Z", lastUsed: "2026-10-02T08:00:00Z", ramUserId: 5 });
    expect(v.rotationGrace).toBe(true);
    expect(v.graceUntil).not.toBe("");
    expect(v.lastUsed).not.toBe(NEVER_USED);
    expect(v.ramUserId).toBe(5);
    expect(toAccessKeyView({ akId: "x", status: 2 }).status).toBe("inactive");
    // A graceUntil without rotationGrace (window closed) is not shown.
    expect(toAccessKeyView({ akId: "x", rotationGrace: false, graceUntil: "2026-10-04T00:00:00Z" }).graceUntil).toBe("");
  });

  it("masks only long ids and maps empty lastUsed", () => {
    expect(maskAccessKeyId("short")).toBe("short");
    expect(lastUsedText(undefined)).toBe(NEVER_USED);
    expect(lastUsedText("0001-01-01T00:00:00Z")).toBe(NEVER_USED);
  });
});

describe("cap and requests", () => {
  it("counts enabled and disabled keys toward the 2-key cap", () => {
    expect(akCapReached([])).toBe(false);
    expect(akCapReached([{ status: "active" }])).toBe(false);
    expect(akCapReached([{ status: "active" }, { status: "inactive" }])).toBe(true);
    expect(akCapReached([{ status: "inactive" }, { status: "inactive" }])).toBe(true);
  });

  it("targets the account root or a RAM user", () => {
    expect(akListPath()).toBe("/api/ak");
    expect(akListPath(12)).toBe("/api/ak?ramUserId=12");
    expect(akCreateBody()).toEqual({});
    expect(akCreateBody(12)).toEqual({ ramUserId: 12 });
  });

  it("maps the lifecycle error codes", () => {
    expect(akErrorMessage({ code: "Auth.AKLimitExceeded" })).toContain("含已禁用");
    expect(akErrorMessage({ code: "Auth.AKDisabled" })).toContain("不能轮换");
    expect(akErrorMessage({ code: "Auth.AKRotationInProgress" })).toContain("宽限期");
    expect(akErrorMessage({ code: "Auth.RAMUserDisabled" })).toContain("子用户已禁用");
    expect(akErrorMessage({ code: "X", message: "boom" })).toBe("boom");
  });
});
