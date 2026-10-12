/**
 * web-account auth store (02§5.1) — real SSO via svc-iam.
 * login → POST /api/auth/login (sets HttpOnly refresh cookie, returns access_token;
 *   MFA accounts get {mfaRequired, mfaToken} and finish via POST /api/auth/mfa/verify).
 * refresh → POST /api/auth/refresh (browser auto-sends the cookie).
 * logout → POST /api/auth/logout (revokes the session).
 * The access_token lives in memory only (02§5.1: never localStorage).
 */
import { defineStore } from "pinia";
import { bridge, inWujieSandbox, readSharedProps, subAppAuthOptions } from "@eu/wujie-bridge";
import { apiError, networkError, readEnvelope, type ApiError } from "../http";

export interface EuUser {
  id: number;
  /** Login name (the email) — the display name. */
  name: string;
  /** Verified legal name; "" until 实名认证 — never a display-name fallback. */
  realName?: string;
}

interface LoginResponse {
  accessToken: string;
  user: EuUser;
}

/** Password accepted, second factor still required (07§2.4). */
interface MfaChallenge {
  mfaRequired: true;
  mfaToken: string;
  user: EuUser;
}

export type LoginResult = { mfaRequired: boolean; user: EuUser };

/**
 * MFA failures that keep the challenge alive: a wrong or already-used code, the
 * attempt throttle (429), or a network blip. Anything else (expired/revoked
 * challenge, account gone) means the password step must be redone.
 */
const MFA_RETRYABLE = new Set([
  "Auth.MFAInvalidCode",
  "Auth.MFACodeReused",
  "Auth.TooManyAttempts",
  "Common.NetworkError",
]);

// Inside the console the shell owns the session: its live token getter and its
// single-flight refresh (the refresh cookie and its rotation belong to it).
const shellAuth = subAppAuthOptions();

async function authFetch(path: string, init?: RequestInit): Promise<Response> {
  try {
    return await fetch(path, {
      ...init,
      credentials: "include", // send/receive the HttpOnly refresh cookie
      headers: { "Content-Type": "application/json", "X-Requested-With": "XMLHttpRequest", ...(init?.headers ?? {}) },
    });
  } catch {
    throw networkError();
  }
}

// Single-flight guards. Module-level: promises do not belong in reactive state.
let refreshInFlight: Promise<string | undefined> | null = null;
let restoreInFlight: Promise<boolean> | null = null;

export const useAccountAuth = defineStore("accountAuth", {
  state: () => ({
    accessToken: "" as string,
    user: null as EuUser | null,
    /** Pending second factor after the password step of an MFA account. */
    mfa: null as { token: string; user: EuUser } | null,
    /** A session restore (refresh cookie / shell token) has been attempted. */
    sessionChecked: false,
  }),
  getters: {
    isAuthenticated: (s) => Boolean(s.accessToken),
    mfaPending: (s) => s.mfa !== null,
  },
  actions: {
    /** Install a fresh session and share it with the parent shell (02§5.4). */
    establish(accessToken: string, user: EuUser | null) {
      this.accessToken = accessToken;
      if (user) this.user = user;
      this.mfa = null;
      this.sessionChecked = true;
      if (inWujieSandbox()) bridge.emit("auth:token-refreshed", { accessToken, user });
    },
    /**
     * Password step. For an MFA account the backend answers
     * {mfaRequired, mfaToken, user} WITHOUT a session — the caller must then
     * collect the authenticator code and call verifyMfa().
     */
    async login(email: string, password: string): Promise<LoginResult> {
      this.mfa = null;
      const res = await authFetch("/api/auth/login", { method: "POST", body: JSON.stringify({ email, password }) });
      const data = await readEnvelope<LoginResponse | MfaChallenge>(res);
      if ("mfaRequired" in data && data.mfaRequired) {
        if (!data.mfaToken) throw apiError("登录响应异常,请重试", "Auth.MalformedResponse", res.status);
        this.mfa = { token: data.mfaToken, user: data.user };
        return { mfaRequired: true, user: data.user };
      }
      const ok = data as LoginResponse;
      if (!ok?.accessToken) throw apiError("登录响应异常,请重试", "Auth.MalformedResponse", res.status);
      this.establish(ok.accessToken, ok.user);
      return { mfaRequired: false, user: ok.user };
    },
    /** Second factor: POST /api/auth/mfa/verify {mfaToken, code} → session. */
    async verifyMfa(code: string): Promise<EuUser> {
      const pending = this.mfa;
      if (!pending) throw apiError("动态验证码校验已失效,请重新登录", "Auth.MFAChallengeMissing", 0);
      try {
        const res = await authFetch("/api/auth/mfa/verify", {
          method: "POST",
          body: JSON.stringify({ mfaToken: pending.token, code: code.trim() }),
        });
        const data = await readEnvelope<LoginResponse>(res);
        if (!data?.accessToken) throw apiError("登录响应异常,请重新登录", "Auth.MalformedResponse", res.status);
        this.establish(data.accessToken, data.user ?? pending.user);
        return data.user ?? pending.user;
      } catch (e) {
        if (!MFA_RETRYABLE.has((e as ApiError).code)) this.mfa = null;
        throw e;
      }
    },
    cancelMfa() {
      this.mfa = null;
    },
    /** Current access token; inside the console the shell's live token wins. */
    currentToken(): string | undefined {
      if (inWujieSandbox()) {
        const t = shellAuth.getToken();
        if (t && t !== this.accessToken) this.accessToken = t;
      }
      return this.accessToken || undefined;
    },
    /**
     * Single-flight silent refresh. Embedded: ask the shell (it owns the
     * refresh cookie; refreshing behind its back would race its rotation).
     * Standalone: redeem the HttpOnly refresh cookie directly.
     */
    refresh(): Promise<string | undefined> {
      if (!refreshInFlight) {
        const p = (async (): Promise<string | undefined> => {
          const shellRefresh = inWujieSandbox() ? readSharedProps().refreshToken : undefined;
          if (shellRefresh) {
            const t = await shellRefresh().catch(() => undefined);
            if (t) this.accessToken = t;
            return t || undefined;
          }
          let res: Response;
          try {
            res = await authFetch("/api/auth/refresh", { method: "POST" });
          } catch {
            return undefined;
          }
          if (!res.ok) {
            // No / expired / revoked session: drop the dead access token too.
            if (res.status === 401) {
              this.accessToken = "";
              this.user = null;
            }
            return undefined;
          }
          try {
            const data = await readEnvelope<{ accessToken: string }>(res);
            if (!data?.accessToken) return undefined;
            this.accessToken = data.accessToken;
            if (inWujieSandbox()) bridge.emit("auth:token-refreshed", { accessToken: data.accessToken });
            return data.accessToken;
          } catch {
            return undefined;
          }
        })();
        refreshInFlight = p;
        void p.finally(() => {
          if (refreshInFlight === p) refreshInFlight = null;
        });
      }
      return refreshInFlight;
    },
    /**
     * Restore the session after a reload or deep link (single-flight). The
     * access token lives in memory only (02§5.1), so it is re-minted from the
     * refresh cookie — or taken from the console shell when embedded — and the
     * user record (which refresh does not return) is loaded from the session.
     */
    ensureSession(): Promise<boolean> {
      if (!restoreInFlight) {
        const p = (async (): Promise<boolean> => {
          try {
            if (!this.currentToken()) await this.refresh();
            if (this.accessToken && !this.user) await this.loadSession();
          } finally {
            this.sessionChecked = true;
          }
          return this.isAuthenticated;
        })();
        restoreInFlight = p;
        void p.finally(() => {
          if (restoreInFlight === p) restoreInFlight = null;
        });
      }
      return restoreInFlight;
    },
    /** Fill in `user` from GET /api/auth/session. Best-effort. */
    async loadSession(): Promise<void> {
      try {
        const data = await this.request<{ user?: EuUser }>("/api/auth/session");
        if (data?.user) this.user = data.user;
      } catch {
        /* pages still work with the token alone */
      }
    },
    /**
     * Authenticated svc-iam call: bearer token, one silent refresh + replay on
     * 401 (svc-iam rejects a bad token before the handler runs, so the replay
     * is safe for any method), envelope unwrapped by readEnvelope.
     */
    async request<T>(path: string, init: RequestInit = {}): Promise<T> {
      const send = async (token: string | undefined): Promise<Response> => {
        const headers = new Headers(init.headers);
        headers.set("X-Requested-With", "XMLHttpRequest");
        if (init.body != null && !headers.has("Content-Type")) headers.set("Content-Type", "application/json");
        if (token) headers.set("Authorization", `Bearer ${token}`);
        try {
          return await fetch(path, { ...init, headers });
        } catch {
          throw networkError();
        }
      };
      let res = await send(this.currentToken());
      if (res.status === 401) {
        const fresh = await this.refresh();
        if (fresh) res = await send(fresh);
      }
      return readEnvelope<T>(res);
    },
    async logout() {
      try { await authFetch("/api/auth/logout", { method: "POST" }); } catch { /* ignore */ }
      this.accessToken = "";
      this.user = null;
      this.mfa = null;
      if (inWujieSandbox()) bridge.emit("auth:logout", {});
    },
    /**
     * Register a new account (真实写库,非 setTimeout). `code` is the mailed
     * verification code (POST /api/auth/register/send-code) — required by
     * svc-iam outside legacy mode. Returns the new account.
     */
    async register(email: string, password: string, code?: string): Promise<{ id: number; email: string; realNameStatus?: number }> {
      const body: Record<string, string> = { email, password };
      if (code && code.trim()) body.code = code.trim();
      const res = await authFetch("/api/auth/register", { method: "POST", body: JSON.stringify(body) });
      return readEnvelope<{ id: number; email: string; realNameStatus?: number }>(res);
    },
    /** Query real-name verification status (requires login). */
    async realnameStatus(): Promise<{ status: number; type: string; name: string; idCard: string }> {
      return this.request<{ status: number; type: string; name: string; idCard: string }>("/api/realname/status");
    },
    /** Submit real-name verification (individual/enterprise). Real backend update. */
    async verifyRealname(payload: {
      type: "individual" | "enterprise";
      name?: string; idCardNo?: string;
      enterpriseName?: string; creditCode?: string; legalPerson?: string;
    }): Promise<{ status: number; type: string; idCard: string }> {
      return this.request<{ status: number; type: string; idCard: string }>("/api/realname/verify", {
        method: "POST",
        body: JSON.stringify(payload),
      });
    },
  },
});
