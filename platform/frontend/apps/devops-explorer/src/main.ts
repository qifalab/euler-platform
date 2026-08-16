/**
 * devops-explorer — Wujie sub-app entry (02§3.4), mirrors console-ecs.
 * Exports the Wujie lifecycle hooks; standalone-mode self-mounts when not in
 * the sandbox (02§6.6 degradation path).
 */
import { createApp, type App as VueApp } from "vue";
import ElementPlus from "element-plus";
import "element-plus/dist/index.css";
import "@sc/tokens/style.css";
import "@sc/ui/style.css";
import "@sc/console-kit/style.css";
import App from "./App.vue";
import { router } from "./router";
import { inWujieSandbox, readSharedProps } from "@sc/wujie-bridge";

let app: VueApp | null = null;

export async function mount(el: HTMLElement | string) {
  const host = typeof el === "string" ? document.querySelector(el) : el;
  const root = host ?? document.getElementById("app");
  if (!root) throw new Error("[devops-explorer] mount target not found");

  app = createApp(App);
  app.use(ElementPlus);
  app.use(router);

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
  console.debug("[devops-explorer] beforeLoad");
}

if (!inWujieSandbox()) {
  document.addEventListener("DOMContentLoaded", () => {
    const el = document.getElementById("app");
    if (el) void mount(el);
  });
}
