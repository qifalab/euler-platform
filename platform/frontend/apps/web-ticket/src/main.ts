/** web-ticket — ticket support, dual-form (02§1.2). */
import { createApp, type App as VueApp } from "vue";
import ElementPlus from "element-plus";
import "element-plus/dist/index.css";
import "@eu/tokens/style.css";
import "@eu/ui/style.css";
import App from "./App.vue";
import { router } from "./router";
import { inWujieSandbox } from "@eu/wujie-bridge";

let app: VueApp | null = null;
async function mount(el?: HTMLElement | string) {
  const root = el ? (typeof el === "string" ? document.querySelector(el) : el) : document.getElementById("app");
  if (!root) throw new Error("[web-ticket] mount target not found");
  app = createApp(App); app.use(router); app.use(ElementPlus); app.mount(root);
}
export async function unmount() { if (app) { app.unmount(); app = null; } }
if (!inWujieSandbox()) { document.addEventListener("DOMContentLoaded", () => void mount()); }
