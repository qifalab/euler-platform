/**
 * console-base — Wujie micro-frontend base (console-shell, 02§4).
 *
 * Boot sequence (02§4.2): fetch session + registry in parallel → resolve route →
 * fetch sub-app entry → sandbox exec → mount. Here the registry comes from the
 * runtime svc-api-meta endpoint when available, else a built-in fallback list.
 */
import { createApp } from "vue";
import { createPinia } from "pinia";
import ElementPlus from "element-plus";
import "element-plus/dist/index.css";

import "@eu/tokens/style.css";
import "@eu/ui/style.css";
import "@eu/console-kit/style.css";

import App from "./App.vue";
import { router } from "./router";
import { i18n } from "./i18n";
import { useRegistry, type RegistryApp } from "./registry";
import { useAuthStore } from "./stores/auth";
import { useSDK } from "./sdk";
import { setupSubApp } from "./wujie-setup";
import { preloadApp } from "wujie";
import { setCatalogClient } from "@eu/console-kit";

async function bootstrap() {
  const app = createApp(App);
  app.use(createPinia());
  app.use(router);
  app.use(ElementPlus);
  app.use(i18n);

  // The shell is not inside the Wujie sandbox, so console-kit's catalogue
  // metadata (the top-bar RegionSelector) must use the shell's auth-wired SDK.
  setCatalogClient(useSDK());

  // Establish the login state first: the registry endpoint is JWT-protected,
  // and registering sub-apps pre-auth would let a pre-executed sub-app fire
  // its first requests with no token at all (shared props carry a live
  // getToken()). silentRefresh is single-flight with the router guard's.
  const auth = useAuthStore();
  if (!auth.accessToken) await auth.silentRefresh();

  // Fetch the sub-app registry (runtime → fallback) before mounting, so the
  // router guard can resolve productCode → sub-app on first navigation.
  const registry = useRegistry();
  await registry.load();

  // Pre-register every sub-app with Wujie: setupApp wires the sandbox, shared
  // props, and keep-alive. Actual fetch/mount happens on first navigation.
  registry.apps.forEach((entry: RegistryApp) => setupSubApp(entry));

  app.mount("#app");

  // Preload high-frequency sub-apps in idle time (02§4.3). requestIdleCallback
  // is missing in Safari — a bare reference would throw ReferenceError, so
  // guard with typeof and fall back to setTimeout.
  const idle: (cb: () => void) => void =
    typeof requestIdleCallback === "function"
      ? (cb) => requestIdleCallback(cb)
      : (cb) => setTimeout(cb, 200);
  idle(() => {
    registry.apps
      .filter((a) => a.preload)
      .forEach((a) => void preloadApp({ name: a.appCode }));
  });
}

bootstrap();
