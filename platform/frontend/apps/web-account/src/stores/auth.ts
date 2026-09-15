/**
 * web-account auth store (02§5.1) — real SSO via svc-iam.
 * login → POST /api/auth/login (sets HttpOnly refresh cookie, returns access_token).
 * refresh → POST /api/auth/refresh (browser auto-sends the cookie).
 * logout → POST /api/auth/logout (revokes the session).
 * The access_token lives in memory only (02§5.1: never localStorage).
 */
import { defineStore } from "pinia";
import { bridge, inWujieSandbox } from "@eu/wujie-bridge";

export interface EuUser {
  id: number;
  name: string;
  realName?: string;
}

interface LoginResponse {
  accessToken: string;
  user: EuUser;
}

async function authFetch(path: string, init?: RequestInit): Promise<Response> {
  return fetch(path, {
    ...init,
    credentials: "include", // send/receive the HttpOnly refresh cookie
    headers: { "Content-Type": "application/json", "X-Requested-With": "XMLHttpRequest", ...(init?.headers ?? {}) },
  });
}

async function readEnvelope<T>(res: Response): Promise<T> {
  const body = await res.json();
  if (!res.ok || body.Code !== "OK") {
    throw Object.assign(new Error(body.Message ?? "请求失败"), { code: body.Code ?? `HTTP_${res.status}`, status: res.status });
  }
  return body.Data as T;
}

export const useAccountAuth = defineStore("accountAuth", {
  state: () => ({
    accessToken: "" as string,
    user: null as EuUser | null,
  }),
  getters: { isAuthenticated: (s) => Boolean(s.accessToken) },
  actions: {
    async login(email: string, password: string) {
      const res = await authFetch("/api/auth/login", { method: "POST", body: JSON.stringify({ email, password }) });
      const data = await readEnvelope<LoginResponse>(res);
      this.accessToken = data.accessToken;
      this.user = data.user;
      // Notify the parent shell (console-base) so it shares the session
      // (02§5.4 auth:token-refreshed across the bridge).
      if (inWujieSandbox()) {
        bridge.emit("auth:token-refreshed", { accessToken: data.accessToken, user: data.user });
      }
      return data;
    },
    async refresh(): Promise<string | undefined> {
      try {
        const res = await authFetch("/api/auth/refresh", { method: "POST" });
        if (!res.ok) return undefined;
        const data = await readEnvelope<{ accessToken: string }>(res);
        this.accessToken = data.accessToken;
        if (inWujieSandbox()) bridge.emit("auth:token-refreshed", { accessToken: data.accessToken });
        return data.accessToken;
      } catch {
        return undefined;
      }
    },
    async logout() {
      try { await authFetch("/api/auth/logout", { method: "POST" }); } catch { /* ignore */ }
      this.accessToken = "";
      this.user = null;
      if (inWujieSandbox()) bridge.emit("auth:logout", {});
    },
    /** Register a new account (真实写库,非 setTimeout). Returns the new account id. */
    async register(email: string, password: string): Promise<{ id: number; email: string }> {
      const res = await authFetch("/api/auth/register", { method: "POST", body: JSON.stringify({ email, password }) });
      return readEnvelope<{ id: number; email: string }>(res);
    },
    /** Query real-name verification status (requires login). */
    async realnameStatus(): Promise<{ status: number; type: string; name: string; idCard: string }> {
      const res = await authFetch("/api/realname/status", {
        headers: this.accessToken ? { Authorization: `Bearer ${this.accessToken}` } : {},
      });
      return readEnvelope<{ status: number; type: string; name: string; idCard: string }>(res);
    },
    /** Submit real-name verification (individual/enterprise). Real backend update. */
    async verifyRealname(payload: {
      type: "individual" | "enterprise";
      name?: string; idCardNo?: string;
      enterpriseName?: string; creditCode?: string; legalPerson?: string;
    }): Promise<{ status: number; type: string; idCard: string }> {
      const res = await authFetch("/api/realname/verify", {
        method: "POST",
        headers: this.accessToken ? { Authorization: `Bearer ${this.accessToken}` } : {},
        body: JSON.stringify(payload),
      });
      return readEnvelope<{ status: number; type: string; idCard: string }>(res);
    },
  },
});
