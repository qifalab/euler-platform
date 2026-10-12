/**
 * Account-center origin for the login / real-name bounces (02§5.3).
 *
 * Environment differences are not baked into the build (02§9.3): the runtime
 * `window.__SC_ENV__` injected by index.html wins, then a build-time
 * VITE_ACCOUNT_BASE, then the per-mode default. The previous hardcoded
 * http://localhost:5175 sent every production login bounce to localhost.
 */
export const PROD_ACCOUNT_BASE = "https://account.euler.emoera.com";
export const DEV_ACCOUNT_BASE = "http://localhost:5175";

function clean(v: unknown): string | undefined {
  if (typeof v !== "string") return undefined;
  const s = v.trim().replace(/\/+$/, "");
  return s || undefined;
}

/** Pure resolution (runtime env → build env → mode default), for tests. */
export function resolveAccountBase(
  scEnv: Record<string, unknown> | undefined,
  buildEnv: { VITE_ACCOUNT_BASE?: string; DEV?: boolean },
): string {
  return (
    clean(scEnv?.accountBase) ??
    clean(scEnv?.ACCOUNT_BASE) ??
    clean(buildEnv.VITE_ACCOUNT_BASE) ??
    (buildEnv.DEV ? DEV_ACCOUNT_BASE : PROD_ACCOUNT_BASE)
  );
}

/** The account-center origin for this deployment (read per call). */
export function accountBase(): string {
  const scEnv = (window as unknown as { __SC_ENV__?: Record<string, unknown> }).__SC_ENV__;
  return resolveAccountBase(scEnv, { VITE_ACCOUNT_BASE: import.meta.env.VITE_ACCOUNT_BASE, DEV: import.meta.env.DEV });
}
