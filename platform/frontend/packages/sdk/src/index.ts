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
  /** Called with the structured error after a request fails. */
  onError?: (err: EuError) => void;
  /** Default timeout, ms. */
  timeout?: number;
}

const DEFAULT_TIMEOUT = 15_000;

function toError(payload: unknown, status: number, requestId?: string): EuError {
  // Go services write {Code, Message, RequestId} (capitalized); the lower-case
  // variants cover gateway-generated error bodies.
  const body = (payload ?? {}) as {
    code?: string; Code?: string;
    message?: string; Message?: string;
    detailUrl?: string; requestId?: string;
  };
  const message = body.message ?? body.Message;
  const err = new Error(message ?? "request failed") as EuError;
  err.code = body.code ?? body.Code ?? `HTTP_${status}`;
  err.message = message ?? "request failed";
  err.requestId = body.requestId ?? requestId;
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
    const timer = setTimeout(() => controller.abort(), timeout);
    // Chain a caller-supplied signal into the internal timeout controller so
    // both cancellation sources work (e.g. useResourceTable stale-abort).
    const external = fetchInit.signal;
    if (external) {
      if (external.aborted) controller.abort();
      else external.addEventListener("abort", () => controller.abort(), { once: true });
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
    } catch (e) {
      clearTimeout(timer);
      const err = toError({ message: (e as Error).message }, 0);
      options.onError?.(err);
      throw err;
    }
    clearTimeout(timer);

    const requestId = res.headers.get("X-Request-Id") ?? undefined;

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
      const fresh = await refreshToken();
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
        { code: body.Code, message: body.Message ?? "business error", requestId: body.RequestId },
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
  return Math.round(Number(yuan) * 100);
}
