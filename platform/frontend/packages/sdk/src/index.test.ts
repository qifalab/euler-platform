/**
 * Vitest tests for @eu/sdk createSDK client (co-located, auto-discovered).
 *
 * Covers the five load-bearing behaviors of the unified request client:
 *   1. Platform envelope unwrapping ({Code,Data} → Data passed through)
 *   2. 401 single-flight refresh: refresh called once, replayed with new token,
 *      Authorization header updated on the replayed request
 *   3. Non-OK status throws EuError with code/message/requestId/status
 *   4. X-Requested-With header always set (CSRF double-submit, 02§5.1)
 *   5. Token injection: getToken() → Authorization: Bearer
 *
 * No real network — global fetch is stubbed with vi.fn per test. Node 18+
 * provides native Headers / AbortController / Response used by the client.
 */
import { describe, it, expect, vi, beforeEach } from "vitest";
import { createSDK, yuanToMinor } from "./index";
import type { EuError } from "./index";

/** Build a JSON Response whose body will be res.json(). */
function jsonResponse(body: unknown, init: { status?: number; requestId?: string } = {}): Response {
  const status = init.status ?? 200;
  const headers = new Headers({ "Content-Type": "application/json" });
  if (init.requestId) headers.set("X-Request-Id", init.requestId);
  return new Response(JSON.stringify(body), { status, headers });
}

/** Extract JSON body captured on a fetch RequestInit so tests can assert it. */
async function capturedBody(init: RequestInit | undefined): Promise<unknown> {
  if (!init?.body) return undefined;
  const raw = init.body as string;
  try {
    return JSON.parse(raw);
  } catch {
    return raw;
  }
}

beforeEach(() => {
  vi.unstubAllGlobals();
});

describe("createSDK — envelope unwrapping", () => {
  it("returns body.Data when response is a {Code:'OK', Data:{x:1}} envelope", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(jsonResponse({ Code: "OK", Message: "", Data: { x: 1 } }));

    vi.stubGlobal("fetch", fetchMock);

    const sdk = createSDK();
    const res = await sdk.get<{ x: number }>("/anything");

    // The fix just added: callers receive Data directly, not the whole envelope.
    expect(res.data).toEqual({ x: 1 });
    expect(res.data.x).toBe(1);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("passes non-envelope JSON bodies through unchanged", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(jsonResponse({ name: "naked", count: 7 }));

    vi.stubGlobal("fetch", fetchMock);

    const sdk = createSDK();
    const res = await sdk.get<{ name: string; count: number }>("/list");

    // No `Data` key → payload returned as-is.
    expect(res.data).toEqual({ name: "naked", count: 7 });
    expect(res.data.name).toBe("naked");
  });

  it("exposes requestId from the X-Request-Id response header", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(
        jsonResponse({ Code: "OK", Data: { ok: true } }, { requestId: "req-xyz-123" }),
      );

    vi.stubGlobal("fetch", fetchMock);

    const sdk = createSDK();
    const res = await sdk.get<{ ok: boolean }>("/ping");

    expect(res.data).toEqual({ ok: true });
    expect(res.requestId).toBe("req-xyz-123");
  });
});

describe("createSDK — 401 silent refresh + replay", () => {
  it("on 401 calls onUnauthorized once, then replays with the fresh Bearer token", async () => {
    // Snapshot the Authorization header at call time: the SDK mutates a shared
    // Headers object on the 401-replay path, so reading calls[i][1].headers
    // after the fact would show the mutated value for both calls.
    const capturedAuth: string[] = [];
    const fetchMock = vi.fn().mockImplementation((_url: string, init: RequestInit) => {
      const h = new Headers(init.headers as HeadersInit);
      capturedAuth.push(h.get("Authorization") ?? "");
      if (capturedAuth.length === 1) {
        return Promise.resolve(
          jsonResponse({ Code: "UNAUTHORIZED" }, { status: 401, requestId: "r-401" }),
        );
      }
      return Promise.resolve(jsonResponse({ Code: "OK", Data: { id: 42 } }));
    });

    vi.stubGlobal("fetch", fetchMock);

    const onUnauthorized = vi.fn(async () => "FRESH-TOKEN-XYZ");

    // getToken starts with the stale token; SDK replays with the refreshed one.
    let currentToken: string | undefined = "STALE-TOKEN";
    const sdk = createSDK({
      getToken: () => currentToken,
      onUnauthorized,
    });

    const res = await sdk.get<{ id: number }>("/me");

    // Refresh fired exactly once (single-flight).
    expect(onUnauthorized).toHaveBeenCalledTimes(1);
    // fetch was called twice: original + replay.
    expect(fetchMock).toHaveBeenCalledTimes(2);

    // First request carried the stale token; replay carried the refreshed one.
    expect(capturedAuth).toEqual(["Bearer STALE-TOKEN", "Bearer FRESH-TOKEN-XYZ"]);

    // Envelope unwrapped on the replayed 200.
    expect(res.data).toEqual({ id: 42 });
  });

  it("does NOT refresh twice for a single request (single-flight guard on retries)", async () => {
    // 401 then another 401 then 200 — refresh must only happen once because the
    // replay uses retries:0, so the second 401 surfaces as an error rather than
    // looping. onUnauthorized is called at most once per original request.
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({ Code: "UNAUTHORIZED" }, { status: 401 }))
      .mockResolvedValueOnce(jsonResponse({ Code: "STILL_UNAUTHORIZED" }, { status: 401 }));

    vi.stubGlobal("fetch", fetchMock);

    const onUnauthorized = vi.fn(async () => "FRESH");

    const sdk = createSDK({ onUnauthorized });

    // Second 401 has retries:0 → no further refresh, surfaces as EuError.
    await expect(sdk.get("/guarded")).rejects.toThrow();

    expect(onUnauthorized).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("throws the 401 error when onUnauthorized resolves with no token (refresh failed)", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(jsonResponse({ code: "UNAUTHORIZED", message: "expired" }, { status: 401 }));

    vi.stubGlobal("fetch", fetchMock);

    const onUnauthorized = vi.fn(async () => undefined);

    const sdk = createSDK({ onUnauthorized });

    // No fresh token → 401 surfaces as a structured EuError (toError reads
    // lowercase code/message from the body, falling back to HTTP_<status>).
    await expect(sdk.get("/no-token")).rejects.toMatchObject({
      code: "UNAUTHORIZED",
      message: "expired",
      status: 401,
    });

    expect(onUnauthorized).toHaveBeenCalledTimes(1);
  });
});

describe("createSDK — structured error model (non-OK)", () => {
  it("throws an EuError carrying code, message, requestId, detailUrl, and status", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse(
        {
          // toError reads lowercase code/message/requestId/detailUrl from the
          // body (PascalCase Code/Data is reserved for the success envelope).
          code: "QUOTA_EXCEEDED",
          message: "disk quota exceeded",
          requestId: "req-from-body",
          detailUrl: "https://docs.euler.emoera.com/errors/quota",
        },
        { status: 402 },
      ),
    );

    vi.stubGlobal("fetch", fetchMock);

    const sdk = createSDK();
    let caught: unknown;
    try {
      await sdk.get("/bill");
    } catch (e) {
      caught = e;
    }

    const err = caught as EuError;
    expect(err).toBeInstanceOf(Error);
    expect(err.code).toBe("QUOTA_EXCEEDED");
    expect(err.message).toBe("disk quota exceeded");
    expect(err.requestId).toBe("req-from-body");
    expect(err.detailUrl).toBe("https://docs.euler.emoera.com/errors/quota");
    expect(err.status).toBe(402);
  });

  it("falls back to HTTP_<status> code and header requestId when body lacks them", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({ unexpected: "shape" }, { status: 500, requestId: "rid-from-header" }),
    );

    vi.stubGlobal("fetch", fetchMock);

    const sdk = createSDK();
    await expect(sdk.get("/boom")).rejects.toMatchObject({
      code: "HTTP_500",
      message: "请求失败(HTTP 500)",
      requestId: "rid-from-header",
      status: 500,
    });
  });

  it("names the HTTP status in Chinese for a non-JSON error body (gateway HTML page)", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response("<html><body>502 Bad Gateway</body></html>", {
        status: 502,
        headers: { "Content-Type": "text/html" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const err = await createSDK().get("/x").catch((e: unknown) => e as EuError);
    expect(err).toBeInstanceOf(Error);
    expect(err).toMatchObject({ code: "HTTP_502", message: "请求失败(HTTP 502)", status: 502 });
  });

  it("falls back to the business code when a Code != OK envelope has no Message", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ Code: "Order.NotPayable" }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(createSDK().get("/y")).rejects.toMatchObject({
      code: "Order.NotPayable",
      message: "请求失败(Order.NotPayable)",
      status: 200,
    });
  });

  it("invokes onError with the same structured error object", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({ code: "FORBIDDEN", message: "no access" }, { status: 403 }),
    );

    vi.stubGlobal("fetch", fetchMock);

    const onError = vi.fn();
    const sdk = createSDK({ onError });

    await expect(sdk.get("/secret")).rejects.toThrow("no access");
    expect(onError).toHaveBeenCalledTimes(1);

    const reported = onError.mock.calls[0][0] as EuError;
    expect(reported.code).toBe("FORBIDDEN");
    expect(reported.status).toBe(403);
  });
});

describe("createSDK — CSRF double-submit header (02§5.1)", () => {
  it("always sets X-Requested-With: XMLHttpRequest on GET", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({ Code: "OK", Data: {} }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const sdk = createSDK();
    await sdk.get("/csrf");

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const headers = new Headers(fetchMock.mock.calls[0][1]?.headers as HeadersInit);
    expect(headers.get("X-Requested-With")).toBe("XMLHttpRequest");
  });

  it("sets X-Requested-With on POST and preserves Content-Type + JSON body", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({ Code: "OK", Data: { created: true } }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const sdk = createSDK();
    await sdk.post("/items", { name: "widget", qty: 3 });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const init = fetchMock.mock.calls[0][1] as RequestInit;
    const headers = new Headers(init.headers as HeadersInit);

    expect(headers.get("X-Requested-With")).toBe("XMLHttpRequest");
    expect(headers.get("Content-Type")).toBe("application/json");

    const body = await capturedBody(init);
    expect(body).toEqual({ name: "widget", qty: 3 });
  });

  it("sets X-Requested-With even when the caller supplies custom headers", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({ Code: "OK", Data: {} }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const sdk = createSDK();
    await sdk.get("/custom", { headers: { "X-Trace-Id": "abc" } });

    const headers = new Headers(fetchMock.mock.calls[0][1]?.headers as HeadersInit);
    expect(headers.get("X-Requested-With")).toBe("XMLHttpRequest");
    expect(headers.get("X-Trace-Id")).toBe("abc");
  });
});

describe("createSDK — token injection into Authorization", () => {
  it("sends Authorization: Bearer <getToken()> when a token is present", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({ Code: "OK", Data: {} }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const sdk = createSDK({ getToken: () => "ACCESS-123" });
    await sdk.get("/whoami");

    const headers = new Headers(fetchMock.mock.calls[0][1]?.headers as HeadersInit);
    expect(headers.get("Authorization")).toBe("Bearer ACCESS-123");
  });

  it("omits Authorization entirely when getToken returns undefined", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({ Code: "OK", Data: {} }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const sdk = createSDK({ getToken: () => undefined });
    await sdk.get("/public");

    const headers = new Headers(fetchMock.mock.calls[0][1]?.headers as HeadersInit);
    expect(headers.get("Authorization")).toBeNull();
  });

  it("prepends baseURL to the request path", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({ Code: "OK", Data: {} }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const sdk = createSDK({ baseURL: "https://api.euler.emoera.com" });
    await sdk.get("/v1/clusters");

    const url = fetchMock.mock.calls[0][0] as string;
    expect(url).toBe("https://api.euler.emoera.com/v1/clusters");
  });
});

// PATCH carries a JSON body and is not replayed on 429 (not idempotent).
describe("createSDK patch", () => {
  it("sends PATCH with a JSON body and unwraps the envelope", async () => {
    const fetchMock = vi.fn(async () => jsonResponse({ Code: "OK", Data: { resourceId: "r1" } }));
    vi.stubGlobal("fetch", fetchMock);
    const sdk = createSDK();
    const res = await sdk.patch<{ resourceId: string }>("/api/v1/orchestrator/resources/r1/attributes", { attributes: { name: "a" } });
    expect(res.data).toEqual({ resourceId: "r1" });
    const init = (fetchMock.mock.calls[0] as unknown[])[1] as RequestInit;
    expect(init.method).toBe("PATCH");
    expect(new Headers(init.headers).get("Content-Type")).toBe("application/json");
    expect(await capturedBody(init)).toEqual({ attributes: { name: "a" } });
  });

  it("does not auto-retry a 429 PATCH", async () => {
    const fetchMock = vi.fn(async () => jsonResponse({ Code: "Common.TooManyRequests", Message: "slow down" }, { status: 429 }));
    vi.stubGlobal("fetch", fetchMock);
    const sdk = createSDK();
    await expect(sdk.patch("/x", {})).rejects.toMatchObject({ status: 429 });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});

// Transport failures: the console user gets a Chinese message, not the
// browser's own text, and a request the caller cancelled is not a failure.
describe("createSDK — transport failures (status 0)", () => {
  /** A fetch that never answers and rejects like the browser once aborted. */
  function hangingFetch() {
    return vi.fn(
      (_url: string, init: RequestInit) =>
        new Promise<Response>((_resolve, reject) => {
          init.signal?.addEventListener("abort", () =>
            reject(new DOMException("signal is aborted without reason", "AbortError")),
          );
        }),
    );
  }

  it("reports a network failure in Chinese, not as the browser's 'Failed to fetch'", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("Failed to fetch")));
    const onError = vi.fn();
    await expect(createSDK({ onError }).get("/x")).rejects.toMatchObject({
      code: "HTTP_0",
      status: 0,
      message: "请求失败,请检查网络后重试",
    });
    expect(onError).toHaveBeenCalledTimes(1);
  });

  it("reports a timeout as a timeout", async () => {
    vi.useFakeTimers();
    try {
      vi.stubGlobal("fetch", hangingFetch());
      const onError = vi.fn();
      const settled = expect(createSDK({ timeout: 1000, onError }).get("/slow")).rejects.toMatchObject({
        code: "HTTP_0",
        status: 0,
        message: "请求超时,请稍后重试",
      });
      await vi.advanceTimersByTimeAsync(1000);
      await settled;
      expect(onError).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("rejects a request the caller aborted without reporting it to onError", async () => {
    vi.stubGlobal("fetch", hangingFetch());
    const onError = vi.fn();
    const ctrl = new AbortController();
    const pending = createSDK({ onError }).get("/superseded", { signal: ctrl.signal });
    ctrl.abort();
    await expect(pending).rejects.toMatchObject({ status: 0, message: "请求已取消" });
    expect(onError).not.toHaveBeenCalled();
  });

  it("removes its abort listener from the caller's signal once the response is in", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ Code: "OK", Data: {} })));
    const ctrl = new AbortController();
    const add = vi.spyOn(ctrl.signal, "addEventListener");
    const remove = vi.spyOn(ctrl.signal, "removeEventListener");
    const sdk = createSDK();
    await sdk.get("/a", { signal: ctrl.signal });
    await sdk.get("/b", { signal: ctrl.signal });
    expect(add).toHaveBeenCalledTimes(2);
    expect(remove).toHaveBeenCalledTimes(2);
    expect(remove.mock.calls[0][1]).toBe(add.mock.calls[0][1]);
  });
});

describe("createSDK — 401 refresh edge cases", () => {
  it("surfaces the 401 (and reports it) when onUnauthorized rejects", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        jsonResponse({ Code: "Auth.TokenExpired", Message: "token expired" }, { status: 401, requestId: "r-401" }),
      ),
    );
    const onError = vi.fn();
    const sdk = createSDK({ onUnauthorized: () => Promise.reject(new TypeError("Failed to fetch")), onError });
    await expect(sdk.get("/me")).rejects.toMatchObject({ code: "Auth.TokenExpired", status: 401, requestId: "r-401" });
    expect(onError).toHaveBeenCalledTimes(1);
  });

  it("concurrent 401s share one refresh and both replay with the fresh token", async () => {
    const fetchMock = vi.fn(async (_url: string, init: RequestInit) =>
      new Headers(init.headers).get("Authorization") === "Bearer FRESH"
        ? jsonResponse({ Code: "OK", Data: { ok: true } })
        : jsonResponse({ Code: "Auth.TokenExpired" }, { status: 401 }),
    );
    vi.stubGlobal("fetch", fetchMock);
    // Resolves on a later macrotask, after both requests have seen their 401.
    const onUnauthorized = vi.fn(() => new Promise<string>((r) => setTimeout(() => r("FRESH"), 0)));
    const sdk = createSDK({ getToken: () => "STALE", onUnauthorized });

    const [a, b] = await Promise.all([sdk.get("/a"), sdk.get("/b")]);
    expect(a.data).toEqual({ ok: true });
    expect(b.data).toEqual({ ok: true });
    expect(onUnauthorized).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledTimes(4);
  });
});

// "requestId on all errors" (frontend notes, console-kit conventions): Go
// services write it as RequestId in the body and X-Euler-TraceId in a header.
describe("createSDK — requestId from platform services", () => {
  it("reads the Go envelope's RequestId on a non-2xx answer", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        jsonResponse({ RequestId: "rid-body", Code: "Order.NotPayable", Message: "订单不可支付" }, { status: 409 }),
      ),
    );
    await expect(createSDK().post("/api/v1/orders/1/pay")).rejects.toMatchObject({
      code: "Order.NotPayable",
      status: 409,
      requestId: "rid-body",
    });
  });

  it("falls back to pkg-go's X-Euler-TraceId header when no gateway sets X-Request-Id", async () => {
    const traced = (body: unknown, status: number) =>
      new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json", "X-Euler-TraceId": "trace-1" },
      });
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValueOnce(traced({ Code: "OK", Data: { x: 1 } }, 200))
        .mockResolvedValueOnce(traced({ RequestId: "", Code: "Common.InternalError", Message: "boom" }, 500)),
    );
    const sdk = createSDK();
    await expect(sdk.get("/ok")).resolves.toMatchObject({ requestId: "trace-1" });
    await expect(sdk.get("/fail")).rejects.toMatchObject({ requestId: "trace-1", status: 500 });
  });
});

// The money wire contract: svc-order/svc-payment read amountMinor in 分, so the
// yuan decimal a quote returns must be scaled by 100 — never rounded to yuan.
// (Every BuyWizard did the latter and under-billed by 100×.)
describe("yuanToMinor", () => {
  it("converts a yuan decimal to 分", () => {
    expect(yuanToMinor("180.50")).toBe(18050);
    expect(yuanToMinor("21.6")).toBe(2160);
    expect(yuanToMinor(0.01)).toBe(1);
  });

  it("rounds to the nearest 分 rather than truncating", () => {
    expect(yuanToMinor("0.005")).toBe(1); // half-up
    expect(yuanToMinor("1.004")).toBe(100);
    expect(yuanToMinor("1.995")).toBe(200);
  });

  it("is not a yuan passthrough (the 100× under-billing regression)", () => {
    expect(yuanToMinor("180.50")).not.toBe(181);
  });

  it("rounds half-up on the decimal digits, not on a binary float", () => {
    // 1.005 * 100 === 100.49999999999999 in IEEE-754 doubles.
    expect(yuanToMinor("1.005")).toBe(101);
    expect(yuanToMinor("0.285")).toBe(29);
    expect(yuanToMinor(2.675)).toBe(268);
    expect(yuanToMinor("0.00007")).toBe(0);
    expect(yuanToMinor("12")).toBe(1200);
  });
});
