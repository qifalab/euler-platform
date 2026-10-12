/** web-marketplace — 云市场, M-10 生态门户 (09§5.2). */
import { createApp, type App as VueApp } from "vue";
import ElementPlus from "element-plus";
import "element-plus/dist/index.css";
import "@eu/tokens/style.css";
import "@eu/ui/style.css";
import App from "./App.vue";
import { router } from "./router";
import { bootstrapSubApp } from "@eu/wujie-bridge";

let app: VueApp | null = null;
async function mount(el?: HTMLElement | string) {
  const root = el ? (typeof el === "string" ? document.querySelector(el) : el) : document.getElementById("app");
  if (!root) throw new Error("[web-marketplace] mount target not found");
  app = createApp(App); app.use(router); app.use(ElementPlus); app.mount(root);
}
export async function unmount() { if (app) { app.unmount(); app = null; } }
// Wujie drives mount/unmount via __WUJIE_MOUNT/__WUJIE_UNMOUNT; standalone
// self-mounts once the DOM is ready.
bootstrapSubApp(() => mount(), unmount);
