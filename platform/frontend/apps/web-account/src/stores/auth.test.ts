/**
 * @vitest-environment jsdom
 *
 * web-account auth store: MFA login, session restore (refresh cookie / shell
 * token), 401 → refresh → replay, non-JSON gateway errors, and the router guard.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createPinia, setActivePinia } from "pinia";
import { useAccountAuth } from "./auth";
import { accountGuard, router } from "../router";

type FetchCall = [string, RequestInit | undefined];

function envelope(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}
const ok = (data: unknown, status = 200) => envelope(status, { Code: "OK", Data: data });
const fail = (status: number, code: string, message: string) => envelope(status, { Code: code, Message: message });

const user = { id: 100200, name: "alice@example.com", realName: "" };

/** Route fetch by "METHOD path" → queued responses. */
function routeFetch(routes: Record<string, Array<Response | (() => Response)>>) {
  const fn = vi.fn(async (input: string, init?: RequestInit) => {
    const key = `${(init?.method ?? "GET").toUpperCase()} ${input}`;
    const queue = routes[key];
    if (!queue || queue.length === 0) throw new Error(`unexpected fetch ${key}`);
    const next = queue.length > 1 ? queue.shift()! : queue[0];
    return typeof next === "function" ? next() : next.clone();
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

function bodyOf(call: FetchCall): unknown {
  return JSON.parse(String(call[1]?.body ?? "null"));
}
function authHeader(call: FetchCall): string | null {
  return new Headers(call[1]?.headers).get("Authorization");
}

const w = window as unknown as Record<string, unknown>;

beforeEach(() => {
  setActivePinia(createPinia());
});
afterEach(() => {
  vi.unstubAllGlobals();
  delete w.__POWERED_BY_WUJIE__;
  delete w.$wujie;
});

describe("login", () => {
  it("installs the session for a password-only account", async () => {
    routeFetch({ "POST /api/auth/login": [ok({ accessToken: "at-1", user })] });
    const auth = useAccountAuth();
    await expect(auth.login("alice@example.com", "pw-12345678")).resolves.toEqual({ mfaRequired: false, user });
    expect(auth.accessToken).toBe("at-1");
    expect(auth.user).toEqual(user);
    expect(auth.mfaPending).toBe(false);
  });

  it("does NOT treat an MFA challenge as a session", async () => {
    routeFetch({ "POST /api/auth/login": [ok({ mfaRequired: true, mfaToken: "mfa-1", user })] });
    const auth = useAccountAuth();
    const res = await auth.login("alice@example.com", "pw-12345678");
    expect(res.mfaRequired).toBe(true);
    expect(auth.isAuthenticated).toBe(false);
    expect(auth.mfaPending).toBe(true);
  });

  it("surfaces backend errors (wrong password, lockout)", async () => {
    routeFetch({ "POST /api/auth/login": [fail(429, "Auth.TooManyAttempts", "尝试次数过多,请 15 分钟后再试")] });
    const auth = useAccountAuth();
    await expect(auth.login("a@b.co", "x")).rejects.toMatchObject({
      code: "Auth.TooManyAttempts",
      status: 429,
      message: "尝试次数过多,请 15 分钟后再试",
    });
  });
});

describe("verifyMfa", () => {
  async function challenged(routes: Record<string, Array<Response | (() => Response)>>) {
    const fetchMock = routeFetch({ "POST /api/auth/login": [ok({ mfaRequired: true, mfaToken: "mfa-1", user })], ...routes });
    const auth = useAccountAuth();
    await auth.login("alice@example.com", "pw-12345678");
    return { auth, fetchMock };
  }

  it("posts {mfaToken, code} and installs the session", async () => {
    const { auth, fetchMock } = await challenged({ "POST /api/auth/mfa/verify": [ok({ accessToken: "at-2", user })] });
    await auth.verifyMfa(" 123456 ");
    const call = fetchMock.mock.calls.find((c) => c[0] === "/api/auth/mfa/verify") as FetchCall;
    expect(bodyOf(call)).toEqual({ mfaToken: "mfa-1", code: "123456" });
    expect(call[1]?.credentials).toBe("include");
    expect(auth.accessToken).toBe("at-2");
    expect(auth.mfaPending).toBe(false);
  });

  it.each([
    ["Auth.MFAInvalidCode", 401, "动态验证码错误"],
    ["Auth.MFACodeReused", 401, "验证码已被使用,请等待下一枚"],
    ["Auth.TooManyAttempts", 429, "动态验证码错误次数过多,请 15 分钟后再试"],
  ])("keeps the challenge on %s", async (code, status, message) => {
    const { auth } = await challenged({ "POST /api/auth/mfa/verify": [fail(status, code, message)] });
    await expect(auth.verifyMfa("000000")).rejects.toMatchObject({ code, message });
    expect(auth.mfaPending).toBe(true);
    expect(auth.isAuthenticated).toBe(false);
  });

  it("drops an expired challenge so the password step is redone", async () => {
    const { auth } = await challenged({
      "POST /api/auth/mfa/verify": [fail(401, "Auth.TokenExpired", "MFA 校验凭证无效,请重新登录")],
    });
    await expect(auth.verifyMfa("123456")).rejects.toMatchObject({ code: "Auth.TokenExpired" });
    expect(auth.mfaPending).toBe(false);
  });
});

describe("session restore", () => {
  it("re-mints the token from the refresh cookie once and loads the user", async () => {
    const fetchMock = routeFetch({
      "POST /api/auth/refresh": [ok({ accessToken: "at-r" })],
      "GET /api/auth/session": [ok({ user, realNameStatus: 0 })],
    });
    const auth = useAccountAuth();
    const [a, b] = await Promise.all([auth.ensureSession(), auth.ensureSession()]);
    expect(a && b).toBe(true);
    expect(auth.accessToken).toBe("at-r");
    expect(auth.user).toEqual(user);
    expect(fetchMock.mock.calls.filter((c) => c[0] === "/api/auth/refresh")).toHaveLength(1);
    const session = fetchMock.mock.calls.find((c) => c[0] === "/api/auth/session") as FetchCall;
    expect(authHeader(session)).toBe("Bearer at-r");
  });

  it("reports no session when the refresh cookie is gone", async () => {
    routeFetch({ "POST /api/auth/refresh": [fail(401, "Auth.NoSession", "无活跃会话")] });
    const auth = useAccountAuth();
    await expect(auth.ensureSession()).resolves.toBe(false);
    expect(auth.sessionChecked).toBe(true);
  });

  it("inside the console: seeds from the shell token and refreshes through the shell", async () => {
    const shellRefresh = vi.fn().mockResolvedValue("shell-fresh");
    w.__POWERED_BY_WUJIE__ = true;
    w.$wujie = {
      props: { getToken: () => "shell-token", refreshToken: shellRefresh },
      bus: { $on: vi.fn(), $off: vi.fn(), $emit: vi.fn() },
    };
    const fetchMock = routeFetch({
      "GET /api/auth/session": [ok({ user })],
      "GET /api/ak": [fail(401, "Auth.TokenExpired", "未认证"), ok({ accessKeys: [], total: 0 })],
    });
    const auth = useAccountAuth();
    await expect(auth.ensureSession()).resolves.toBe(true);
    expect(auth.accessToken).toBe("shell-token");

    await auth.request("/api/ak");
    expect(shellRefresh).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls.some((c) => c[0] === "/api/auth/refresh")).toBe(false);
  });
});

describe("request", () => {
  it("refreshes once on 401 and replays with the fresh token", async () => {
    const fetchMock = routeFetch({
      "GET /api/ram/users": [fail(401, "Auth.TokenExpired", "未认证"), ok({ users: [], total: 0 })],
      "POST /api/auth/refresh": [ok({ accessToken: "at-new" })],
    });
    const auth = useAccountAuth();
    auth.accessToken = "at-old";
    await expect(auth.request("/api/ram/users")).resolves.toEqual({ users: [], total: 0 });
    const calls = fetchMock.mock.calls.filter((c) => c[0] === "/api/ram/users") as FetchCall[];
    expect(calls.map(authHeader)).toEqual(["Bearer at-old", "Bearer at-new"]);
  });

  it("turns an HTML gateway error page into a readable error", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(new Response("<html><body>502 Bad Gateway</body></html>", { status: 502 })),
    );
    const auth = useAccountAuth();
    auth.accessToken = "at";
    const err = await auth.request("/api/ak").catch((e: Error & { code: string }) => e);
    expect(err).toBeInstanceOf(Error);
    expect((err as Error).message).toContain("HTTP 502");
    expect((err as Error).message).not.toContain("Unexpected token");
    expect((err as { code: string }).code).toBe("HTTP_502");
  });

  it("maps a network failure to a stable error", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("Failed to fetch")));
    const auth = useAccountAuth();
    await expect(auth.request("/api/ak")).rejects.toMatchObject({ code: "Common.NetworkError", status: 0 });
  });
});

describe("accountGuard", () => {
  const protectedRoute = { meta: {}, fullPath: "/profile" };

  it("restores the session before deciding (no bounce on reload)", async () => {
    routeFetch({
      "POST /api/auth/refresh": [ok({ accessToken: "at-r" })],
      "GET /api/auth/session": [ok({ user })],
    });
    await expect(accountGuard(protectedRoute)).resolves.toBe(true);
  });

  it("sends a visitor without a session to login with a redirect", async () => {
    routeFetch({ "POST /api/auth/refresh": [fail(401, "Auth.NoSession", "无活跃会话")] });
    await expect(accountGuard(protectedRoute)).resolves.toEqual({ name: "login", query: { redirect: "/profile" } });
  });

  it("sends a visitor without a session from /realname to login and back (redirect kept)", async () => {
    routeFetch({ "POST /api/auth/refresh": [fail(401, "Auth.NoSession", "无活跃会话")] });
    const to = router.resolve("/realname?redirect=https%3A%2F%2Fconsole.euler.emoera.com%2Feuecs");
    await expect(accountGuard(to)).resolves.toEqual({
      name: "login",
      query: { redirect: "/realname?redirect=https%3A%2F%2Fconsole.euler.emoera.com%2Feuecs" },
    });
  });

  it("restores at most once for public pages", async () => {
    const fetchMock = routeFetch({ "POST /api/auth/refresh": [fail(401, "Auth.NoSession", "无活跃会话")] });
    const pub = { meta: { public: true }, fullPath: "/register" };
    await expect(accountGuard(pub)).resolves.toBe(true);
    await expect(accountGuard(pub)).resolves.toBe(true);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});

describe("register", () => {
  it("sends the trimmed verification code with the sign-up", async () => {
    const fetchMock = routeFetch({
      "POST /api/auth/register": [ok({ id: 7, email: "new@example.com", realNameStatus: 0 }, 201)],
    });
    const auth = useAccountAuth();
    await expect(auth.register("new@example.com", "pass-word-1", " 123456 ")).resolves.toMatchObject({ id: 7 });
    expect(bodyOf(fetchMock.mock.calls[0] as unknown as FetchCall)).toEqual({
      email: "new@example.com", password: "pass-word-1", code: "123456",
    });
  });

  it("omits an empty code and surfaces Auth.InvalidCode", async () => {
    const fetchMock = routeFetch({
      "POST /api/auth/register": [fail(400, "Auth.InvalidCode", "invalid or expired code")],
    });
    const auth = useAccountAuth();
    const err = await auth.register("new@example.com", "pass-word-1", "").catch((e: unknown) => e);
    expect(bodyOf(fetchMock.mock.calls[0] as unknown as FetchCall)).toEqual({ email: "new@example.com", password: "pass-word-1" });
    expect(err).toMatchObject({ code: "Auth.InvalidCode", status: 400 });
  });
});
