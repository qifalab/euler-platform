/**
 * Auth store (02§5.1/§5.3) — real SSO via svc-iam.
 *
 * Dual-token: access_token in memory only (never localStorage); refresh_token
 * is an HttpOnly cookie the browser attaches to /api/auth/* — JS can't read it.
 * Permission snapshot: product-level action set from /api/auth/session;
 * resource-level checks are always backend-decided, this is UI gating (02§5.3).
 */
import { defineStore } from "pinia";
import { bus } from "wujie";

export interface PermissionSnapshot {
  actions: string[];
}

interface SessionResponse {
  user: { id: number; name: string; realName?: string };
  permissions: PermissionSnapshot;
  realNameStatus: number; // 0 unverified, 1 individual, 2 enterprise (07§2.3)
}

export const useAuthStore = defineStore("auth", {
  state: () => ({
    accessToken: "" as string,
    user: null as { id: number; name: string; realName?: string } | null,
    permissions: { actions: [] } as PermissionSnapshot,
    realNameStatus: 0 as number,
    refreshing: null as Promise<string | undefined> | null,
  }),
  getters: {
    isAuthenticated: (s) => Boolean(s.accessToken),
    /** Real-name verified (required before opening resources, 01§4.5/07§2.3). */
    isRealNameVerified: (s) => s.realNameStatus >= 1,
    /** UI gate only — resource-level authz is backend-decided (02§5.3). */
    can: (s) => (action: string) => s.permissions.actions.includes(action),
  },
  actions: {
    setToken(token: string) {
      this.accessToken = token;
    },
    /** Load user + permission snapshot from /api/auth/session (02§5.3). */
    async fetchSession() {
      if (!this.accessToken) return;
      try {
        const res = await fetch("/api/auth/session", {
          headers: { Authorization: `Bearer ${this.accessToken}`, "X-Requested-With": "XMLHttpRequest" },
        });
        if (!res.ok) return;
        const body = await res.json();
        if (body.Code === "OK" && body.Data) {
          const d = body.Data as SessionResponse;
          this.user = d.user;
          this.permissions = d.permissions;
          this.realNameStatus = d.realNameStatus ?? 0;
        }
      } catch { /* ignore — UI gating degrades gracefully */ }
    },
    /** Silent refresh — single-flight: concurrent 401s share one refresh. */
    silentRefresh(): Promise<string | undefined> {
      if (this.refreshing) return this.refreshing;
      this.refreshing = (async () => {
        try {
          const res = await fetch("/api/auth/refresh", {
            method: "POST",
            credentials: "include", // send the HttpOnly refresh cookie
            headers: { "X-Requested-With": "XMLHttpRequest" }, // CSRF double-submit
          });
          if (res.ok) {
            const body = await res.json();
            if (body.Code === "OK" && body.Data?.accessToken) {
              this.accessToken = body.Data.accessToken;
              // Broadcast the fresh token to every active sub-app (02§5.4).
              bus.$emit("auth:token-refreshed", { token: this.accessToken });
              await this.fetchSession();
              return this.accessToken;
            }
          }
          // Refresh failed — not authenticated. Real deploy: bounce to
          // account.starcloud.cn (02§5.3). In dev the overview still renders
          // via the BFF proxy's injected account header.
          return undefined;
        } catch {
          return undefined;
        } finally {
          this.refreshing = null;
        }
      })();
      return this.refreshing;
    },
    async logout() {
      try {
        await fetch("/api/auth/logout", {
          method: "POST",
          credentials: "include",
          headers: { "X-Requested-With": "XMLHttpRequest" },
        });
      } catch { /* ignore */ }
      this.accessToken = "";
      this.user = null;
      this.permissions = { actions: [] };
    },
  },
});
