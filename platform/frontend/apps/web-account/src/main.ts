/**
 * web-account — SSO account center (02§5, §1.2).
 * Dual-form: standalone site (account.*) + console sub-app. Detected via
 * @eu/wujie-bridge: in the sandbox it renders shell-less; standalone it
 * renders its own top bar. Auth endpoints hit the real svc-iam web-auth
 * service (vite proxy → :9101 in dev; APISIX → svc-iam in prod).
 */
import { createApp, type App as VueApp } from "vue";
import { createPinia } from "pinia";
import ElementPlus from "element-plus";
import "element-plus/dist/index.css";
import "@eu/tokens/style.css";
import "@eu/ui/style.css";

import App from "./App.vue";
import { router } from "./router";

let app: VueApp | null = null;

async function mount(el?: HTMLElement | string) {
  const root = el
    ? (typeof el === "string" ? document.querySelector(el) : el)
    : document.getElementById("app");
  if (!root) throw new Error("[web-account] mount target not found");

  app = createApp(App);
  app.use(createPinia());
  app.use(router);
  app.use(ElementPlus);
  app.mount(root);
}

export async function unmount() {
  if (app) { app.unmount(); app = null; }
}

// Standalone mode (02§6.6 degradation path): self-mount when not in sandbox.
import { inWujieSandbox } from "@eu/wujie-bridge";
if (!inWujieSandbox()) {
  document.addEventListener("DOMContentLoaded", () => void mount());
}
