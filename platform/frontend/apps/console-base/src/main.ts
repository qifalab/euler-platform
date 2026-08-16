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

import "@sc/tokens/style.css";
import "@sc/ui/style.css";
import "@sc/console-kit/style.css";

import App from "./App.vue";
import { router } from "./router";
import { i18n } from "./i18n";
import { useRegistry, type RegistryApp } from "./registry";
import { useAuthStore } from "./stores/auth";
import { setupSubApp } from "./wujie-setup";

async function bootstrap() {
  const app = createApp(App);
  app.use(createPinia());
  app.use(router);
  app.use(ElementPlus);
  app.use(i18n);

  // Fetch the sub-app registry (runtime → fallback) before mounting, so the
  // router guard can resolve productCode → sub-app on first navigation.
  const registry = useRegistry();
  await registry.load();

  // Pre-register every sub-app with Wujie: setupApp wires the sandbox, shared
  // props, and keep-alive. Actual fetch/mount happens on first navigation.
  registry.apps.forEach((entry: RegistryApp) => setupSubApp(entry));

  app.mount("#app");

  // Preload high-frequency sub-apps in idle time (02§4.3).
  requestIdleCallback?.(() => registry.apps.filter((a) => a.preload).forEach((a) => a.preload && void 0));
}

bootstrap();
