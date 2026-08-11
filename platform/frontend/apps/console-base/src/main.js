/**
 * console-base — Wujie micro-frontend base (console-shell analog).
 *
 * This is the source-only scaffold of the StarCloud console shell. In the real
 * architecture the sub-app entries come from the runtime registry
 * (svc-api-meta GET /api/v1/meta/console/apps), with a build-time
 * fallback-registry. Here we keep a small static registry so the scaffold
 * runs with zero backend.
 *
 * Sub-app list mirrors the architecture doc (02-frontend-architecture.md §1.2):
 * the seven product-category console sub-apps plus billing/iam/ticket.
 */
import { createApp } from "vue";
import App from "./App.vue";

// Minimal Wujie facade so the scaffold is importable without installing deps.
// Swap these stubs for `import { setupApp } from "wujie"` when real deps land.
const wujieBridge = {
  setupApp: (config) => {
    console.debug("[console-base] setupApp", config.name, config.url);
    // Render a placeholder when the sub-app has no real build yet.
    const el = document.getElementById(`sub-app-${config.name}`);
    if (el) {
      el.innerHTML =
        `<div class="sub-app-placeholder">` +
        `<p><strong>${config.title}</strong> (${config.name})</p>` +
        `<p>子应用尚未构建 — 请在 registry 中配置真实的 entryUrl。</p>` +
        `</div>`;
    }
    return Promise.resolve();
  },
  preloadApp: () => {},
  startApp: (config) => {
    console.debug("[console-base] startApp", config.name);
    return wujieBridge.setupApp(config);
  },
};

/** Static fallback registry (production pulls this from svc-api-meta). */
const fallbackRegistry = {
  revision: 1,
  ttl: 300,
  apps: [
    {
      appCode: "console-ecs",
      appTitle: "云服务器 ECS",
      productCodes: ["scecs", "scsas"],
      activeRules: ["/scecs", "/scsas"],
      entryUrl: "/apps/console-ecs/index.html",
      version: "0.1.0",
      keepAlive: true,
      preload: true,
    },
    {
      appCode: "console-oss",
      appTitle: "对象存储 OSS",
      productCodes: ["scoss"],
      activeRules: ["/scoss"],
      entryUrl: "/apps/console-oss/index.html",
      version: "0.1.0",
      keepAlive: true,
      preload: false,
    },
    {
      appCode: "console-rds",
      appTitle: "云数据库 RDS",
      productCodes: ["scrds"],
      activeRules: ["/scrds"],
      entryUrl: "/apps/console-rds/index.html",
      version: "0.1.0",
      keepAlive: false,
      preload: false,
    },
    {
      appCode: "console-vpc",
      appTitle: "专有网络 VPC",
      productCodes: ["scvpc", "sceip"],
      activeRules: ["/scvpc", "/sceip"],
      entryUrl: "/apps/console-vpc/index.html",
      version: "0.1.0",
      keepAlive: false,
      preload: false,
    },
    {
      appCode: "console-monitor",
      appTitle: "云监控",
      productCodes: ["scmon"],
      activeRules: ["/scmon"],
      entryUrl: "/apps/console-monitor/index.html",
      version: "0.1.0",
      keepAlive: false,
      preload: false,
    },
    {
      appCode: "console-billing",
      appTitle: "费用中心",
      productCodes: [],
      activeRules: ["/billing"],
      entryUrl: "/apps/console-billing/index.html",
      version: "0.1.0",
      keepAlive: false,
      preload: true,
    },
    {
      appCode: "console-iam",
      appTitle: "账号与访问控制 IAM",
      productCodes: [],
      activeRules: ["/account"],
      entryUrl: "/apps/console-iam/index.html",
      version: "0.1.0",
      keepAlive: false,
      preload: false,
    },
    {
      appCode: "console-ticket",
      appTitle: "工单支持",
      productCodes: [],
      activeRules: ["/ticket"],
      entryUrl: "/apps/console-ticket/index.html",
      version: "0.1.0",
      keepAlive: false,
      preload: false,
    },
  ],
};

async function fetchRegistry() {
  // Try the runtime registry; fall back to the built-in static list.
  try {
    const res = await fetch("/api/v1/meta/console/apps");
    if (res.ok) return await res.json();
  } catch (e) {
    console.warn("[console-base] registry fetch failed, using fallback", e);
  }
  return fallbackRegistry;
}

async function bootstrap() {
  const registry = await fetchRegistry();
  const container = document.getElementById("app");
  if (container) {
    // Build placeholder slots for each sub-app so the shell layout renders.
    registry.apps.forEach((app) => {
      const slot = document.createElement("div");
      slot.id = `sub-app-${app.appCode}`;
      slot.className = "sub-app-container";
      slot.dataset.app = app.appCode;
      container.appendChild(slot);
    });
  }

  // In a real build this would setupApp() + startApp() per registered app.
  // For the scaffold we preload keep-alive apps and defer actual mounts to
  // the sub-apps' own dev servers.
  const shell = createApp(App, { apps: registry.apps });
  shell.mount("#app");

  registry.apps
    .filter((app) => app.preload)
    .forEach((app) => wujieBridge.preloadApp({ name: app.appCode }));
}

bootstrap();
