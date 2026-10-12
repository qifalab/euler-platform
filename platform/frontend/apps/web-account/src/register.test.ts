/**
 * @vitest-environment node
 *
 * 注册验证码: send-code request shape, the 60s resend cooldown, and the
 * Auth.InvalidCode / 429 / Auth.EmailExists messages.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  CODE_COOLDOWN_SECONDS,
  isRateLimited,
  isRegisterCodeInvalid,
  REGISTER_CODE_PATTERN,
  registerErrorMessage,
  sendRegisterCode,
  useCodeCooldown,
} from "./register";

function respond(status: number, body?: unknown) {
  return vi.fn(async () => new Response(body === undefined ? "" : JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } }));
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("register send-code", () => {
  it("posts the trimmed email and accepts an envelope or an empty 200", async () => {
    const f = respond(200, { Code: "OK", Data: null });
    vi.stubGlobal("fetch", f);
    await expect(sendRegisterCode("  new@b.co ")).resolves.toBeUndefined();
    const [path, init] = f.mock.calls[0] as unknown as [string, RequestInit];
    expect(path).toBe("/api/auth/register/send-code");
    expect(init.method).toBe("POST");
    expect(JSON.parse(String(init.body))).toEqual({ email: "new@b.co" });

    vi.stubGlobal("fetch", respond(200));
    await expect(sendRegisterCode("new@b.co")).resolves.toBeUndefined();
  });

  it("maps 429 and an invalid email", async () => {
    vi.stubGlobal("fetch", respond(429, { Code: "Auth.TooManyAttempts", Message: "slow down" }));
    const tooMany = await sendRegisterCode("a@b.co").catch((e: unknown) => e);
    expect(isRateLimited(tooMany)).toBe(true);
    expect(registerErrorMessage(tooMany, "send")).toContain("发送过于频繁");

    vi.stubGlobal("fetch", respond(400, { Code: "Common.InvalidParameter", Message: "bad email" }));
    const bad = await sendRegisterCode("nope").catch((e: unknown) => e);
    expect(registerErrorMessage(bad, "send")).toBe("邮箱格式不正确");

    vi.stubGlobal("fetch", vi.fn(async () => { throw new TypeError("offline"); }));
    const offline = await sendRegisterCode("a@b.co").catch((e: unknown) => e);
    expect(registerErrorMessage(offline, "send")).toContain("网络异常");
  });
});

describe("registerErrorMessage", () => {
  it("covers the register outcomes", () => {
    const invalid = { status: 400, code: "Auth.InvalidCode", message: "invalid code" };
    expect(isRegisterCodeInvalid(invalid)).toBe(true);
    expect(registerErrorMessage(invalid)).toContain("验证码错误或已过期");
    expect(registerErrorMessage({ status: 429, code: "Auth.TooManyAttempts" })).toContain("重新获取验证码");
    expect(registerErrorMessage({ status: 409, code: "Auth.EmailExists" })).toContain("已注册");
    expect(registerErrorMessage({ status: 500, code: "HTTP_500", message: "服务暂时不可用" })).toBe("服务暂时不可用");
    expect(registerErrorMessage(null)).toBe("注册失败,请稍后重试");
  });

  it("accepts only 6-digit codes", () => {
    expect(REGISTER_CODE_PATTERN.test("123456")).toBe(true);
    expect(REGISTER_CODE_PATTERN.test("12345")).toBe(false);
    expect(REGISTER_CODE_PATTERN.test("12345a")).toBe(false);
  });
});

describe("useCodeCooldown", () => {
  it("counts down 60s, relabels and re-enables the button", () => {
    vi.useFakeTimers();
    const c = useCodeCooldown();
    expect(c.canSend.value).toBe(true);
    expect(c.label.value).toBe("获取验证码");
    c.start();
    expect(c.remaining.value).toBe(CODE_COOLDOWN_SECONDS);
    expect(c.canSend.value).toBe(false);
    expect(c.label.value).toBe("60s 后重新获取");
    vi.advanceTimersByTime(59_000);
    expect(c.remaining.value).toBe(1);
    vi.advanceTimersByTime(1_000);
    expect(c.canSend.value).toBe(true);
    expect(c.label.value).toBe("重新获取");
    // Restarting resets the countdown; stop() clears it.
    c.start();
    vi.advanceTimersByTime(10_000);
    c.start();
    expect(c.remaining.value).toBe(60);
    c.stop();
    expect(c.canSend.value).toBe(true);
  });
});
