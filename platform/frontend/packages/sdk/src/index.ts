/**
 * @sc/sdk — unified request client (02-frontend-architecture.md §9.1, §7.5).
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
export interface ScError extends Error {
  code: string;
  message: string;
  requestId?: string;
  detailUrl?: string;
  /** HTTP status, for branching UI (403 → contact-admin, 402 → top-up). */
  status?: number;
}

export interface ScResponse<T> {
  data: T;
  requestId?: string;
}

export interface SdkOptions {
  /** Base gateway URL, e.g. https://api.starcloud.cn. Empty = same-origin. */
  baseURL?: string;
  /** Returns the current access token (memory-only in base). */
  getToken?: () => string | undefined;
  /** Called on 401 to refresh; resolves with a fresh token, or rejects. */
  onUnauthorized?: () => Promise<string | undefined>;
  /** Called with the structured error after a request fails. */
  onError?: (err: ScError) => void;
  /** Default timeout, ms. */
  timeout?: number;
}

const DEFAULT_TIMEOUT = 15_000;

function toError(payload: unknown, status: number, requestId?: string): ScError {
  const body = (payload ?? {}) as { code?: string; message?: string; detailUrl?: string; requestId?: string };
  const err = new Error(body.message ?? "request failed") as ScError;
  err.code = body.code ?? `HTTP_${status}`;
  err.message = body.message ?? "request failed";
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
    init: RequestInit & { retries?: number; _skipToken?: boolean } = {},
  ): Promise<ScResponse<T>> {
    const { retries = 1, _skipToken = false, ...fetchInit } = init;
    const url = baseURL + path;

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeout);

    const headers = new Headers(fetchInit.headers);
    headers.set("X-Requested-With", "XMLHttpRequest"); // CSRF double-submit (02§5.1)
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

    // 401 → single-flight refresh → replay once. The replay carries the fresh
    // token directly in headers and skips getToken() (which still returns the
    // stale one — token storage is the caller's job, not the SDK's).
    if (res.status === 401 && retries > 0) {
      const fresh = await refreshToken();
      if (fresh) {
        headers.set("Authorization", `Bearer ${fresh}`);
        return request<T>(path, { ...init, headers, retries: 0, _skipToken: true });
      }
    }

    // 429 → exponential backoff retry once (02§7.5).
    if (res.status === 429 && retries > 0) {
      await new Promise((r) => setTimeout(r, 500));
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
    const body = payload as { Code?: string; Data?: unknown };
    const data = body && typeof body === "object" && "Data" in body ? body.Data : payload;
    return { data: data as T, requestId };
  }

  return {
    get: <T>(path: string, init?: RequestInit) => request<T>(path, { ...init, method: "GET" }),
    post: <T>(path: string, body?: unknown, init?: RequestInit) =>
      request<T>(path, {
        ...init,
        method: "POST",
        headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) },
        body: body !== undefined ? JSON.stringify(body) : undefined,
      }),
    put: <T>(path: string, body?: unknown, init?: RequestInit) =>
      request<T>(path, {
        ...init,
        method: "PUT",
        headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) },
        body: body !== undefined ? JSON.stringify(body) : undefined,
      }),
    del: <T>(path: string, init?: RequestInit) => request<T>(path, { ...init, method: "DELETE" }),
  };
}

export type SDK = ReturnType<typeof createSDK>;
