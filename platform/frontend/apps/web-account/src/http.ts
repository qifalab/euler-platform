/**
 * Envelope helpers for svc-iam web-auth responses ({RequestId, Code, Message,
 * Data}, 03§9.3).
 *
 * A gateway (APISIX / nginx) answers 502/504 — and some misroutes 200 — with an
 * HTML page, so `res.json()` must never be trusted blindly: that surfaced as
 * "Unexpected token '<'" in the UI. Non-JSON bodies become a stable, readable
 * gateway/network error instead.
 */

export interface ApiError extends Error {
  /** Backend business code (e.g. Auth.InvalidCredentials) or HTTP_<status>. */
  code: string;
  /** HTTP status; 0 when the request never got a response. */
  status: number;
}

export function apiError(message: string, code: string, status: number): ApiError {
  return Object.assign(new Error(message), { code, status });
}

/** The request never reached a server (offline, DNS, CORS, aborted). */
export function networkError(): ApiError {
  return apiError("网络异常,请检查网络后重试", "Common.NetworkError", 0);
}

interface Envelope {
  Code?: unknown;
  Message?: unknown;
  Data?: unknown;
}

/** Parse the body as JSON; null for empty or non-JSON (e.g. an HTML error page). */
async function readJSON(res: Response): Promise<Envelope | null> {
  let text: string;
  try {
    text = await res.text();
  } catch {
    return null;
  }
  if (!text) return null;
  try {
    const body: unknown = JSON.parse(text);
    return body && typeof body === "object" ? (body as Envelope) : null;
  } catch {
    return null;
  }
}

/** Unwrap a platform envelope, throwing an ApiError for any failure. */
export async function readEnvelope<T>(res: Response): Promise<T> {
  const body = await readJSON(res);
  if (!body) {
    throw res.ok
      ? apiError("服务响应格式异常,请稍后重试", `HTTP_${res.status}`, res.status)
      : apiError(`服务暂时不可用(HTTP ${res.status}),请稍后重试`, `HTTP_${res.status}`, res.status);
  }
  if (!res.ok || body.Code !== "OK") {
    const message = typeof body.Message === "string" && body.Message ? body.Message : `请求失败(HTTP ${res.status})`;
    const code = typeof body.Code === "string" && body.Code ? body.Code : `HTTP_${res.status}`;
    throw apiError(message, code, res.status);
  }
  return body.Data as T;
}
