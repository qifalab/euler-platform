/**
 * @vitest-environment node
 *
 * 找回密码 calls: request shape, the always-200 send-code, and the
 * Auth.InvalidCode / 429 messages.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { forgotErrorMessage, isInvalidCode, resetPassword, sendResetCode } from "./forgot";

function respond(status: number, body?: unknown) {
  return vi.fn(async () => new Response(body === undefined ? "" : JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } }));
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("forgot-password API", () => {
  it("send-code posts the trimmed email and accepts an envelope or an empty 200", async () => {
    const f = respond(200, { Code: "OK", Data: null });
    vi.stubGlobal("fetch", f);
    await expect(sendResetCode("  a@b.co ")).resolves.toBeUndefined();
    const [path, init] = f.mock.calls[0] as unknown as [string, RequestInit];
    expect(path).toBe("/api/auth/forgot/send-code");
    expect(init.method).toBe("POST");
    expect(JSON.parse(String(init.body))).toEqual({ email: "a@b.co" });

    vi.stubGlobal("fetch", respond(200));
    await expect(sendResetCode("a@b.co")).resolves.toBeUndefined();
  });

  it("reset posts email/code/password and surfaces Auth.InvalidCode", async () => {
    const f = respond(400, { Code: "Auth.InvalidCode", Message: "invalid code" });
    vi.stubGlobal("fetch", f);
    const err = await resetPassword("a@b.co", " 123456 ", "pass-word-1").catch((e: unknown) => e);
    expect(JSON.parse(String((f.mock.calls[0] as unknown as [string, RequestInit])[1].body))).toEqual({
      email: "a@b.co", code: "123456", password: "pass-word-1",
    });
    expect(isInvalidCode(err)).toBe(true);
    expect(forgotErrorMessage(err)).toContain("验证码错误或已过期");
  });

  it("maps 429 and network failures; never reports the feature as unavailable", async () => {
    vi.stubGlobal("fetch", respond(429, { Code: "Auth.TooManyAttempts", Message: "slow down" }));
    const tooMany = await sendResetCode("a@b.co").catch((e: unknown) => e);
    expect(forgotErrorMessage(tooMany)).toContain("过于频繁");

    vi.stubGlobal("fetch", vi.fn(async () => { throw new TypeError("offline"); }));
    const offline = await sendResetCode("a@b.co").catch((e: unknown) => e);
    expect(forgotErrorMessage(offline)).toContain("网络异常");

    vi.stubGlobal("fetch", respond(404, { Code: "Common.NotFound", Message: "not found" }));
    const missing = await sendResetCode("a@b.co").catch((e: unknown) => e);
    expect(forgotErrorMessage(missing)).not.toContain("暂未开放");
  });
});
