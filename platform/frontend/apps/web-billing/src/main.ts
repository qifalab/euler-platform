/**
 * web-billing — billing center, dual-form (02§1.2).
 * Wired to the console-bff bills endpoint in dev (proxy → :9200 with the dev
 * account header). In the Wujie sandbox it renders shell-less.
 */
import { createApp, type App as VueApp } from "vue";
import ElementPlus from "element-plus";
import "element-plus/dist/index.css";
import "@sc/tokens/style.css";
import "@sc/ui/style.css";
import "@sc/console-kit/style.css";
import App from "./App.vue";
import { router } from "./router";
import { createSDK } from "@sc/sdk";
import { inWujieSandbox } from "@sc/wujie-bridge";

let app: VueApp | null = null;

async function mount(el?: HTMLElement | string) {
  const root = el ? (typeof el === "string" ? document.querySelector(el) : el) : document.getElementById("app");
  if (!root) throw new Error("[web-billing] mount target not found");
  app = createApp(App);
  app.use(router);
  app.use(ElementPlus);
  app.provide("sc:sdk", createSDK({ baseURL: "" }));
  app.mount(root);
}

export async function unmount() { if (app) { app.unmount(); app = null; } }

if (!inWujieSandbox()) {
  document.addEventListener("DOMContentLoaded", () => void mount());
}
