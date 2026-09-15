/**
 * web-billing — billing center, dual-form (02§1.2).
 * Wired to the console-bff bills endpoint in dev (proxy → :9200 with the dev
 * account header). In the Wujie sandbox it renders shell-less.
 */
import { createApp, type App as VueApp } from "vue";
import ElementPlus from "element-plus";
import "element-plus/dist/index.css";
import "@eu/tokens/style.css";
import "@eu/ui/style.css";
import "@eu/console-kit/style.css";
import App from "./App.vue";
import { router } from "./router";
import { createSDK } from "@eu/sdk";
import { inWujieSandbox } from "@eu/wujie-bridge";

let app: VueApp | null = null;

async function mount(el?: HTMLElement | string) {
  const root = el ? (typeof el === "string" ? document.querySelector(el) : el) : document.getElementById("app");
  if (!root) throw new Error("[web-billing] mount target not found");
  app = createApp(App);
  app.use(router);
  app.use(ElementPlus);
  app.provide("eu:sdk", createSDK({ baseURL: "" }));
  app.mount(root);
}

export async function unmount() { if (app) { app.unmount(); app = null; } }

if (!inWujieSandbox()) {
  document.addEventListener("DOMContentLoaded", () => void mount());
}
