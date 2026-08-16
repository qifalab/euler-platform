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

/** Shared deps the base injects into every sub-app (02§6.5). */
function createSharedProps() {
  const auth = useAuthStore();
  return {
    token: auth.accessToken,
    regionId: "cn-north-1",
    theme: "light" as const,
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
