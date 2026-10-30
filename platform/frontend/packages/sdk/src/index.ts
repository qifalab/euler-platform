/**
 * @eu/sdk — unified request client (02-frontend-architecture.md §9.1, §7.5).
 *
 * The ONLY sanctioned way for any sub-app to talk to the backend. Lint forbids
 * bare axios/fetch. Interceptors handle: token injection, 401 single-flight
 * silent refresh, structured error-code mapping, requestId extraction, and
 * 429 exponential-backoff retry.
 *
 * Tokens are never stored here — the caller supplies a token getter (the base
 * wires it to the Pinia auth store; sub-apps read per-request via the bridge).
 */

/** Structured error model (03§9.3 unified OpenAPI error model). */
export interface EuError extends Error {
  code: string;
  message: string;
  requestId?: string;
  detailUrl?: string;
  /** HTTP status, for branching UI (403 → contact-admin, 402 → top-up). */
  status?: number;
}

export interface EuResponse<T> {
  data: T;
  requestId?: string;
}

export interface SdkOptions {
  /** Base gateway URL, e.g. https://api.euler.emoera.com. Empty = same-origin. */
  baseURL?: string;
  /** Returns the current access token (memory-only in base). */
  getToken?: () => string | undefined;
  /** Called on 401 to refresh; resolves with a fresh token, or rejects. */
  onUnauthorized?: () => Promise<string | undefined>;
  /** Called with the structured error after a request fails (not for one the caller aborted). */
  onError?: (err: EuError) => void;
  /** Default timeout, ms. */
  timeout?: number;
}

const DEFAULT_TIMEOUT = 15_000;

/**
 * Fallback message when the response carries no usable message — e.g. a
 * gateway's HTML 502/504 page or an empty body. Chinese like the rest of the
 * console UI, and it keeps the HTTP status so the cause is not lost.
 */
function fallbackMessage(status: number): string {
  return status > 0 ? `请求失败(HTTP ${status})` : "请求失败,请检查网络后重试";
}

function toError(payload: unknown, status: number, requestId?: string): EuError {
  // Go services write {Code, Message, RequestId} (capitalized); the lower-case
  // variants cover gateway-generated error bodies.
  const body = (payload ?? {}) as {
    code?: string; Code?: string;
    message?: string; Message?: string;
    detailUrl?: string; requestId?: string; RequestId?: string;
  };
  const raw = body.message ?? body.Message;
  const message = typeof raw === "string" && raw !== "" ? raw : fallbackMessage(status);
  const err = new Error(message) as EuError;
  err.code = body.code ?? body.Code ?? `HTTP_${status}`;
  err.message = message;
  // A service that ran without a trace id writes "RequestId": "": fall back to
  // the response header then.
  err.requestId = [body.requestId, body.RequestId, requestId].find((v) => typeof v === "string" && v !== "");
  err.detailUrl = body.detailUrl;
  err.status = status;
  return err;
}

export function createSDK(options: SdkOptions = {}) {
  const baseURL = options.baseURL ?? "";
  const getToken = options.getToken ?? (() => undefined);
  const timeout = options.timeout ?? DEFAULT_TIMEOUT;

  // Single-flight refresh: concurrent 401s wait on the same in-flight refresh.
  let refreshInFlight: Promise<string | undefined> | null = null;

  async function refreshToken(): Promise<string | undefined> {
    if (!options.onUnauthorized) return undefined;
    if (!refreshInFlight) {
      refreshInFlight = options.onUnauthorized().finally(() => {
        refreshInFlight = null;
      });
    }
    return refreshInFlight;
  }

  async function request<T>(
    path: string,
    init: RequestInit & { retries?: number; _skipToken?: boolean; idempotencyKey?: string } = {},
  ): Promise<EuResponse<T>> {
    const { retries = 1, _skipToken = false, idempotencyKey, ...fetchInit } = init;
    const url = baseURL + path;

    const controller = new AbortController();
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      controller.abort();
    }, timeout);
    // Chain a caller-supplied signal into the internal timeout controller so
    // both cancellation sources work (e.g. useResourceTable stale-abort). The
    // listener is removed once fetch settles: a long-lived caller signal would
    // otherwise keep one listener (and its controller) per request.
    const external = fetchInit.signal;
    const abortFromCaller = () => controller.abort();
    if (external) {
      if (external.aborted) controller.abort();
      else external.addEventListener("abort", abortFromCaller, { once: true });
    }

    const headers = new Headers(fetchInit.headers);
    headers.set("X-Requested-With", "XMLHttpRequest"); // CSRF double-submit (02§5.1)
    if (idempotencyKey) headers.set("Idempotency-Key", idempotencyKey);
    if (!_skipToken) {
      const token = getToken();
      if (token) headers.set("Authorization", `Bearer ${token}`);
    }

    let res: Response;
    try {
      res = await fetch(url, { ...fetchInit, headers, signal: controller.signal });
    } catch {
      // Transport failure (status 0). The browser's own text ("Failed to
      // fetch", "signal is aborted without reason") is not a message for the
      // console user, and a timeout must not read like a dropped connection.
      const cancelled = !timedOut && Boolean(external?.aborted);
      const message = timedOut ? "请求超时,请稍后重试" : cancelled ? "请求已取消" : fallbackMessage(0);
      const err = toError({ message }, 0);
      // A request the caller cancelled (a superseded list fetch) did not fail,
      // so it is not reported to onError.
      if (!cancelled) options.onError?.(err);
      throw err;
    } finally {
      clearTimeout(timer);
      external?.removeEventListener("abort", abortFromCaller);
    }

    // X-Request-Id comes from the APISIX request-id plugin; a service reached
    // without the gateway (dev proxy) sends only pkg-go's X-Euler-TraceId.
    const requestId = res.headers.get("X-Request-Id") ?? res.headers.get("X-Euler-TraceId") ?? undefined;

    // Auto-replay is only safe for idempotent requests: GET/HEAD/OPTIONS/PUT/
    // DELETE, or any method carrying an explicit idempotencyKey. A plain POST
    // may have been processed server-side, so it is never replayed blindly.
    const method = (fetchInit.method ?? "GET").toUpperCase();
    const replaySafe =
      ["GET", "HEAD", "OPTIONS", "PUT", "DELETE"].includes(method) || Boolean(idempotencyKey);

    // 401 → single-flight refresh → replay once. A 401 is rejected before the
    // handler runs, so the replay is safe even for POST. The replay carries the
    // fresh token directly in headers and skips getToken() (which still returns
    // the stale one — token storage is the caller's job, not the SDK's).
    if (res.status === 401 && retries > 0) {
      // onUnauthorized may reject (its contract allows it): that means "no
      // fresh token", and the caller gets the 401 below, not the refresh error.
      const fresh = await refreshToken().catch(() => undefined);
      if (fresh) {
        headers.set("Authorization", `Bearer ${fresh}`);
        return request<T>(path, { ...init, headers, retries: 0, _skipToken: true });
      }
    }

    // 429 → backoff retry once (02§7.5), honoring Retry-After when present.
    // Only idempotent/keyed requests are retried (see replaySafe above).
    if (res.status === 429 && retries > 0 && replaySafe) {
      const retryAfter = res.headers.get("Retry-After");
      const parsed = retryAfter ? Number(retryAfter) : NaN;
      const delayMs = Number.isFinite(parsed) && parsed >= 0 ? Math.min(parsed * 1000, 30_000) : 500;
      await new Promise((r) => setTimeout(r, delayMs));
      return request<T>(path, { ...init, retries: 0 });
    }

    const payload = await res.json().catch(() => ({}));

    if (!res.ok) {
      const err = toError(payload, res.status, requestId);
      options.onError?.(err);
      throw err;
    }

    // Unwrap the platform envelope { RequestId, Code, Message, Data } (03§9.3).
    // Callers receive Data directly; non-envelope bodies pass through unchanged.
    // HTTP 200 with Code != "OK" is a BUSINESS error — surface it as EuError
    // instead of silently handing callers an error envelope as data.
    const body = payload as { Code?: string; Message?: string; RequestId?: string; Data?: unknown };
    if (body && typeof body === "object" && typeof body.Code === "string" && body.Code !== "OK") {
      const err = toError(
        { code: body.Code, message: body.Message || `请求失败(${body.Code})`, requestId: body.RequestId },
        res.status,
        requestId,
      );
      options.onError?.(err);
      throw err;
    }
    const data = body && typeof body === "object" && "Data" in body ? body.Data : payload;
    return { data: data as T, requestId };
  }

  type CallInit = RequestInit & { idempotencyKey?: string };

  return {
    get: <T>(path: string, init?: CallInit) => request<T>(path, { ...init, method: "GET" }),
    post: <T>(path: string, body?: unknown, init?: CallInit) =>
      request<T>(path, {
        ...init,
        method: "POST",
        headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) },
        body: body !== undefined ? JSON.stringify(body) : undefined,
      }),
    put: <T>(path: string, body?: unknown, init?: CallInit) =>
      request<T>(path, {
        ...init,
        method: "PUT",
        headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) },
        body: body !== undefined ? JSON.stringify(body) : undefined,
      }),
    /**
     * PATCH — a partial update (e.g. /orchestrator/resources/{id}/attributes).
     * Not replay-safe: like POST it is never auto-retried on 429 unless the
     * caller supplies an idempotencyKey.
     */
    patch: <T>(path: string, body?: unknown, init?: CallInit) =>
      request<T>(path, {
        ...init,
        method: "PATCH",
        headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) },
        body: body !== undefined ? JSON.stringify(body) : undefined,
      }),
    del: <T>(path: string, init?: CallInit) => request<T>(path, { ...init, method: "DELETE" }),
  };
}

export type SDK = ReturnType<typeof createSDK>;

/**
 * Convert a yuan amount — the pricing engine's `payableAmount` decimal string,
 * or a number of yuan — into minor units (分).
 *
 * The platform's money wire contract is fixed and one-way: svc-order and
 * svc-payment read `amountMinor` in 分 (1 分 = 0.01 元) and convert to
 * micro-units internally (pricing.Amount, 1/1e6). Sending a yuan integer where
 * 分 is expected under-states an amount by 100×, which is silently accepted —
 * so every order-creation site MUST convert through this helper rather than
 * rounding the quote to a whole yuan.
 */
export function yuanToMinor(yuan: string | number): number {
  // Round half-up on the decimal digits. In binary floating point
  // 1.005 * 100 is 100.49999999999999, so Math.round(Number(yuan) * 100)
  // gave 100 分 for 1.005 元 (and 28 for 0.285).
  const m = /^(-?)(\d*)\.?(\d*)$/.exec(String(yuan).trim());
  if (!m || (m[2] === "" && m[3] === "")) return Math.round(Number(yuan) * 100);
  const [, sign, int, frac] = m;
  const minor = Number((int || "0") + frac.padEnd(2, "0").slice(0, 2)) + (frac.charAt(2) >= "5" ? 1 : 0);
  return sign && minor !== 0 ? -minor : minor;
}
