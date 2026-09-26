import { inject, type InjectionKey } from "vue";
import { api, getCSRFToken, CloudError } from "../api";

export type AppPermission =
  | "read"
  | "write"
  | "manage"
  | "secrets"
  | "review"
  | "admin";
export interface AppScope {
  actorId: string;
  actorName: string;
  email: string;
  emailVerified: boolean;
  provider: string;
  subject: string;
  tenantId: string;
  projectId: string;
  installationId: string;
  applicationId: string;
  permissions: AppPermission[];
}
export interface AppContext {
  scope: AppScope;
  basePath: string;
  publicBase: string;
  can(permission: AppPermission): boolean;
  request<T>(
    path: string,
    options?: {
      method?: string;
      body?: unknown;
      signal?: AbortSignal;
      timeoutMs?: number;
    },
  ): Promise<T>;
  upload<T>(path: string, body: FormData, method?: string): Promise<T>;
  download(path: string, filename: string): Promise<void>;
  notify(message: string, tone?: "success" | "error"): void;
}
export const appContextKey: InjectionKey<AppContext> = Symbol(
  "euler-application-context",
);
export function useApp(): AppContext {
  const value = inject(appContextKey);
  if (!value)
    throw new Error("Application must be mounted in the Euler workspace");
  return value;
}

// The selected context supplies a fixed same-origin prefix. Applications cannot
// turn this client into an arbitrary outbound proxy or choose their identity.
function relative(path: string) {
  if (
    !path.startsWith("/") ||
    path.startsWith("//") ||
    path.includes("\\") ||
    path.split(/[/?#]/).includes("..")
  )
    throw new Error("Invalid application path");
  return path;
}
export function createAppContext(
  scope: AppScope,
  notify: AppContext["notify"],
  signal: AbortSignal,
): AppContext {
  const basePath = `/api/v1/tenants/${encodeURIComponent(scope.tenantId)}/projects/${encodeURIComponent(scope.projectId)}/apps/${encodeURIComponent(scope.applicationId)}`;
  async function raw(path: string, options: RequestInit = {}) {
    const headers = new Headers(options.headers);
    if (!["GET", "HEAD"].includes(options.method ?? "GET"))
      headers.set("X-CSRF-Token", getCSRFToken());
    const response = await fetch(basePath + relative(path), {
      ...options,
      headers,
      credentials: "same-origin",
      signal,
    });
    if (!response.ok) {
      const data = await response.json().catch(() => null);
      if (response.status === 401)
        window.dispatchEvent(new Event("cloud:unauthorized"));
      throw new CloudError(
        data?.error?.message ?? "操作无法完成",
        response.status,
        data?.error?.code,
      );
    }
    return response;
  }
  return {
    scope,
    basePath,
    publicBase: `${location.origin}/public/${scope.applicationId}`,
    can: (permission) => scope.permissions.includes(permission),
    request: (path, options = {}) =>
      api(basePath + relative(path), {
        ...options,
        signal: options.signal
          ? AbortSignal.any([signal, options.signal])
          : signal,
      }),
    upload: async (path, body, method = "POST") =>
      (await raw(path, { method, body })).json(),
    download: async (path, filename) => {
      const response = await raw(path);
      const url = URL.createObjectURL(await response.blob());
      const anchor = document.createElement("a");
      anchor.href = url;
      anchor.download = filename;
      anchor.click();
      window.setTimeout(() => URL.revokeObjectURL(url), 1000);
    },
    notify,
  };
}
