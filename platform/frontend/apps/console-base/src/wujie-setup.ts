/**
 * Wujie sub-app registration (02§3.4).
 * setupApp wires the sandbox, shared props (token/region/shared deps), and
 * keep-alive for each registry entry. Vue/Element Plus are injected as single
 * instances (externals, 02§6.5) — sub-apps never bundle their own Vue.
 */
import { setupApp, type cacheOptions } from "wujie";
import * as Vue from "vue";
import * as VueRouter from "vue-router";
import * as Pinia from "pinia";
import * as ElementPlus from "element-plus";
import { useAuthStore } from "./stores/auth";
import { useRegionStore } from "./stores/region";

/**
 * The theme in force when a sub-app is set up. The shell writes data-theme on
 * <html> and the --eu-* token set is CSS-variable based, so a live toggle
 * reaches sub-apps through the cascade; this value only seeds the `theme` prop
 * (which used to be hardcoded "light" even in dark mode).
 */
function currentTheme(): "light" | "dark" {
  const saved = localStorage.getItem("eu:theme");
  if (saved === "dark" || saved === "light") return saved;
  return window.matchMedia?.("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

/**
 * Shared deps the base injects into every sub-app (02§6.5).
 * token/regionId are exposed as getters (getToken/getRegion) so sub-apps read
 * the LIVE store value per request — a plain `token` prop would be a snapshot
 * frozen at setupApp time and go stale after silent refresh.
 */
function createSharedProps() {
  const auth = useAuthStore();
  const region = useRegionStore();
  return {
    /** Live token getter — always reflects the latest silent-refresh result. */
    getToken: () => auth.accessToken || undefined,
    /** Live region getter (see region store; changes also emit region:changed). */
    getRegion: () => region.regionId,
    /**
     * Refresh entry point for sub-apps: a sub-app's 401 asks the base to run
     * its single-flight silent refresh (the refresh cookie lives on the base's
     * origin and the store de-duplicates concurrent calls), then replays with
     * the returned token.
     */
    refreshToken: () => auth.silentRefresh(),
    regionId: region.regionId,
    theme: currentTheme(),
    shared: {
      vue: Vue,
      "vue-router": VueRouter,
      pinia: Pinia,
      "element-plus": ElementPlus,
    },
    env: (window as unknown as { __SC_ENV__?: Record<string, unknown> }).__SC_ENV__ ?? {},
  };
}

export function setupSubApp(entry: {
  appCode: string;
  appTitle: string;
  entryUrl: string;
  keepAlive: boolean;
}) {
  const config: cacheOptions = {
    name: entry.appCode,
    url: entry.entryUrl,
    exec: true, // pre-execute (02§4.3)
    alive: entry.keepAlive, // keep-alive for high-frequency categories (02§3.4)
    props: createSharedProps(),
    attrs: { "data-app": entry.appCode },
    // Wujie fetch hook: attach source id, timeout, and error reporting.
    fetch: (input: RequestInfo, init?: RequestInit) =>
      fetch(input, { ...init, credentials: "omit" }).catch((e: Error) => {
        console.error(`[console-base] sub-app ${entry.appCode} entry fetch failed`, e);
        throw e;
      }),
  };
  setupApp(config);
}
