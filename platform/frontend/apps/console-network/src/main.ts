/**
 * console-network — Wujie sub-app entry (02§3.4).
 * Exports the Wujie lifecycle hooks consumed by the bridge. In the real
 * architecture these are called by the base when it mounts this sub-app; when
 * run standalone (independent-run / degradation path, 02§6.6), the module
 * self-mounts.
 *
 * Shared deps (Vue/Element Plus) resolve from injected props in Wujie mode, or
 * from the sub-app's own node_modules in standalone mode (the shim below).
 */
import { createApp, type App as VueApp } from "vue";
import ElementPlus from "element-plus";
import "element-plus/dist/index.css";
import "@sc/tokens/style.css";
import "@sc/ui/style.css";
import "@sc/console-kit/style.css";
import App from "./App.vue";
import { inWujieSandbox, readSharedProps } from "@sc/wujie-bridge";

let app: VueApp | null = null;

export async function mount(el: HTMLElement | string) {
  const host = typeof el === "string" ? document.querySelector(el) : el;
  const root = host ?? document.getElementById("app");
  if (!root) throw new Error("[console-network] mount target not found");

  // Standalone mode: resolve shared deps from own node_modules. In Wujie mode
  // the base injects them via props; here we just create a fresh Vue app —
  // the externals resolve from this bundle's imports in standalone, and from
  // the injected globals in the sandbox (handled by the @sc/* shim layer).
  app = createApp(App);
  app.use(ElementPlus);

  // Read region/token the base injected (no-op in standalone).
  const props = readSharedProps();
  app.provide("sc:props", props);

  app.mount(root);
}

export async function unmount() {
  if (app) {
    app.unmount();
    app = null;
  }
}

export async function beforeLoad() {
  console.debug("[console-network] beforeLoad");
}

// Standalone mode (02§6.6 degradation path): self-mount when not in the sandbox.
if (!inWujieSandbox()) {
  document.addEventListener("DOMContentLoaded", () => {
    const el = document.getElementById("app");
    if (el) void mount(el);
  });
}
