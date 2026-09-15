/**
 * @eu/wujie-bridge — micro-frontend communication bridge (02§5.4/6.4).
 *
 * Wraps the raw Wujie bus so callers depend on a stable, typed API rather than
 * the global `window.$wujie`. Handles three concerns:
 *  - shared-props injection (main → sub-app, read-only): token, region, theme
 *  - session events: auth:token-refreshed / auth:logout / region:changed
 *  - dual-mode detection: whether the app is running inside the Wujie sandbox
 *
 * Principle from the architecture doc: "能走 URL 与后端就不走总线" — the bus is
 * only for session/layout-level events, not business state.
 */

/** Shared props the base injects into every sub-app (02§3.4 createSharedProps). */
export interface SharedProps {
  /** @deprecated snapshot token — prefer getToken(), which never goes stale. */
  token?: string;
  /** Live token getter injected by the base (reads the auth store per call). */
  getToken?: () => string | undefined;
  /** Current region id, e.g. cn-north-1 (00 附录A). */
  regionId?: string;
  /** Live region getter injected by the base (reads the region store per call). */
  getRegion?: () => string | undefined;
  /**
   * Ask the base to run its single-flight silent refresh and return the new
   * access token. The base owns the refresh cookie; a sub-app cannot refresh
   * itself, so a 401 in a sub-app must go through this.
   */
  refreshToken?: () => Promise<string | undefined>;
  /** Theme token set on :root by the base. */
  theme?: "light" | "dark";
  /** Shared dependency instances (Vue/Pinia/Element Plus) — externals, not MF. */
  shared?: Record<string, unknown>;
  /** Runtime environment injected by the base (window.__SC_ENV__). */
  env?: Record<string, unknown>;
}

/** Session/layout-level bus events (02§5.4 event table). */
export type BridgeEvent =
  | "auth:token-refreshed"
  | "auth:logout"
  | "auth:switch-account"
  | "region:changed";

/** Sub-app lifecycle hooks the bridge calls. */
export interface SubAppHooks {
  beforeLoad?: (props: SharedProps) => SharedProps | void;
  beforeMount?: (props: SharedProps) => void;
  afterMount?: (props: SharedProps) => void;
  beforeUnmount?: (props: SharedProps) => void;
  activated?: (props: SharedProps) => void;
  deactivated?: (props: SharedProps) => void;
}

/** Detect whether the current code runs inside a Wujie sandbox. */
export function inWujieSandbox(): boolean {
  return (
    typeof window !== "undefined" &&
    Boolean((window as unknown as { __POWERED_BY_WUJIE__?: boolean }).__POWERED_BY_WUJIE__)
  );
}

/** Read props injected by the base; returns an empty object when standalone. */
export function readSharedProps(): SharedProps {
  if (!inWujieSandbox()) return {};
  const w = (window as unknown as { $wujie?: { props?: SharedProps } }).$wujie;
  return w?.props ?? {};
}

type Listener = (payload: unknown) => void;

/**
 * Typed bus facade. In sandbox mode it proxies to the live Wujie bus; in
 * standalone mode it falls back to a local EventTarget so the same sub-app code
 * runs unchanged outside the shell (the degradation path, 02§6.6).
 */
class Bridge {
  private local: EventTarget | null = null;

  private bus(): { $on?: (e: string, fn: Listener) => void; $emit?: (e: string, p: unknown) => void; $off?: (e: string, fn: Listener) => void } | EventTarget | null {
    if (inWujieSandbox()) {
      return (window as unknown as { $wujie?: { bus: { $on(e: string, fn: Listener): void; $emit(e: string, p: unknown): void; $off(e: string, fn: Listener): void } } }).$wujie?.bus ?? null;
    }
    if (!this.local) this.local = new EventTarget();
    return this.local;
  }

  on(event: BridgeEvent, fn: Listener): () => void {
    const b = this.bus();
    const wujieBus = b as { $on?: (e: string, fn: Listener) => void; $off?: (e: string, fn: Listener) => void };
    if (wujieBus.$on) {
      wujieBus.$on(event, fn);
      return () => wujieBus.$off?.(event, fn);
    }
    const t = b as EventTarget;
    const wrap = (e: Event) => fn((e as CustomEvent).detail);
    t.addEventListener(event, wrap);
    return () => t.removeEventListener(event, wrap);
  }

  emit(event: BridgeEvent, payload?: unknown): void {
    const b = this.bus();
    const wujieBus = b as { $emit?: (e: string, p: unknown) => void };
    if (wujieBus.$emit) {
      wujieBus.$emit(event, payload);
      return;
    }
    (b as EventTarget).dispatchEvent(new CustomEvent(event, { detail: payload }));
  }
}

export const bridge = new Bridge();

// --- SDK auth wiring for sub-apps (02§5.4) ----------------------------------
// Cache the freshest token pushed by the base over the bus, so getToken()
// works even for props snapshots injected before the last silent refresh.
let refreshedToken: string | undefined;
bridge.on("auth:token-refreshed", (payload) => {
  const p = payload as { token?: string; accessToken?: string } | undefined;
  refreshedToken = p?.token ?? p?.accessToken;
});
bridge.on("auth:logout", () => {
  refreshedToken = undefined;
});

/**
 * Standard `createSDK` auth options for a sub-app: a live getToken that
 * prefers the base's injected getter, then the bus-refreshed token, then the
 * legacy snapshot prop; and an onUnauthorized that re-reads the base's live
 * token (the base owns silent refresh — sub-apps never refresh themselves).
 */
export function subAppAuthOptions(): {
  getToken: () => string | undefined;
  onUnauthorized: () => Promise<string | undefined>;
} {
  const getToken = () => {
    const props = readSharedProps();
    return props.getToken?.() ?? refreshedToken ?? props.token;
  };
  return {
    getToken,
    // A 401 means the access token expired: ask the base to run its
    // single-flight silent refresh and replay with the fresh token. Returning
    // the current token here (as this used to) made the SDK replay the very
    // credential that was just rejected, so the retry could only fail.
    onUnauthorized: async () => {
      const props = readSharedProps();
      if (props.refreshToken) {
        const fresh = await props.refreshToken();
        if (fresh) return fresh;
      }
      // Standalone (no base) or the refresh was rejected: hand back what we
      // have — the caller will surface the 401 rather than silently looping.
      return getToken();
    },
  };
}

/** Latest region id: live getter → bus event payload handled by callers → snapshot. */
export function currentRegionId(): string | undefined {
  const props = readSharedProps();
  return props.getRegion?.() ?? props.regionId;
}
