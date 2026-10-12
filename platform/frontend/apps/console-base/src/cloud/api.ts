export type { Application, Installation, Summary, Resource } from "./types";

let csrfToken = "";
export function getCSRFToken() {
  return csrfToken;
}
export function setCSRFToken(token?: string) {
  csrfToken = token ?? "";
}

export class CloudError extends Error {
  constructor(
    message: string,
    public status = 0,
    public code = "request_failed",
  ) {
    super(message);
    this.name = "CloudError";
  }
}

/** Application-cloud uses its own server session; no bearer token or identity is supplied by the browser. */
export async function api<T>(
  path: string,
  options: {
    method?: string;
    body?: unknown;
    signal?: AbortSignal;
    timeoutMs?: number;
  } = {},
): Promise<T> {
  const controller = new AbortController();
  const abort = () => controller.abort();
  if (options.signal?.aborted) abort();
  options.signal?.addEventListener("abort", abort, { once: true });
  const timeout = window.setTimeout(
    abort,
    Math.max(1000, Math.min(options.timeoutMs ?? 20_000, 120_000)),
  );
  const method = options.method ?? "GET";
  const headers: Record<string, string> = { Accept: "application/json" };
  if (options.body !== undefined) headers["Content-Type"] = "application/json";
  if (!["GET", "HEAD", "OPTIONS"].includes(method.toUpperCase()))
    headers["X-CSRF-Token"] = csrfToken;
  try {
    const response = await fetch(path, {
      method,
      headers,
      credentials: "same-origin",
      body:
        options.body === undefined ? undefined : JSON.stringify(options.body),
      signal: controller.signal,
    });
    if (response.status === 204) return undefined as T;
    const payload = await response.json().catch(() => null);
    if (!response.ok) {
      if (response.status === 401 && path !== "/api/v1/session")
        window.dispatchEvent(new Event("cloud:unauthorized"));
      throw new CloudError(
        payload?.error?.message ??
          (response.status === 403
            ? "你没有执行此操作的权限，请联系团队管理员。"
            : "请求未能完成，请稍后重试。"),
        response.status,
        payload?.error?.code,
      );
    }
    if (payload === null)
      throw new CloudError(
        "服务返回了无法读取的数据，请稍后重试。",
        response.status,
      );
    return payload as T;
  } catch (error) {
    if (error instanceof CloudError) throw error;
    if (options.signal?.aborted) throw error;
    throw new CloudError(
      controller.signal.aborted
        ? "请求超时，请检查网络后重试。"
        : "暂时无法连接服务，请检查网络后重试。",
    );
  } finally {
    window.clearTimeout(timeout);
    options.signal?.removeEventListener("abort", abort);
  }
}

export function safeExternalURL(value?: string): string | undefined {
  if (!value) return undefined;
  try {
    const url = new URL(value);
    return ["https:", "http:"].includes(url.protocol) &&
      !url.username &&
      !url.password
      ? url.href
      : undefined;
  } catch {
    return undefined;
  }
}
export function errorMessage(error: unknown) {
  return error instanceof Error ? error.message : "操作未能完成，请重试。";
}
export function formatDate(value?: string) {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.getTime())
    ? "—"
    : new Intl.DateTimeFormat("zh-CN", {
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
      }).format(date);
}
export const roleLabels: Record<string, string> = {
  owner: "所有者",
  admin: "管理员",
  member: "成员",
  viewer: "只读成员",
};
