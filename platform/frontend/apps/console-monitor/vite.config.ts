import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";
import { resolve } from "node:path";

// Sub-app build config (02§3.4). How the shared deps (Vue / Element Plus /
// @eu/*) are handled is decided by SHARED_EXTERNALS below.
// Externals sharing (02§6.5) is opt-in: EU_SHARED_EXTERNALS=1 leaves the
// shared deps as bare imports for a host that supplies them through an import
// map. No deployment provides one (not the Wujie shell, not this index.html),
// so such a build fails in the browser ("Failed to resolve module specifier
// 'vue'"). By default they are bundled — Wujie runs every sub-app in its own
// iframe realm, so a bundled Vue is never shared with the shell anyway.
const SHARED_EXTERNALS = [
  "vue", "vue-router", "element-plus",
  "@eu/tokens", "@eu/ui", "@eu/console-kit", "@eu/sdk", "@eu/wujie-bridge",
];
const external = process.env.EU_SHARED_EXTERNALS === "1" ? SHARED_EXTERNALS : [];

export default defineConfig({
  plugins: [vue()],
  base: "/",
  resolve: {
    alias: { "@": resolve(__dirname, "src") },
  },
  build: {
    rollupOptions: {
      external,
      output: {
        entryFileNames: "assets/index.js",
        assetFileNames: "assets/[name][extname]",
      },
    },
  },
  server: {
    port: 5179,
    cors: true, // the base fetches this entry cross-origin in dev
    proxy: {
      // Real BFF data → console-bff (03§4). Dev injects the seed account header
      // so the sub-app renders real resources standalone or in the sandbox.
      "/console": {
        target: "http://localhost:9200",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => {
          r.setHeader("X-Euler-Account-Id", "100123");
          r.setHeader("X-Euler-TraceId", `monitor-${Date.now().toString(36)}`);
        }),
      },
      // Real alert rules + metrics + M-9 SLO/chaos → svc-monitor (03§4.4.1).
      "/api/v1/monitor": {
        target: "http://localhost:9202",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => r.setHeader("X-Euler-Account-Id", "100123")),
      },
      // 告警中心 (phase-3 D-1) → alert-center.
      "/api/v1/alertcenter": {
        target: "http://localhost:9213",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => r.setHeader("X-Euler-Account-Id", "100123")),
      },
      // 异常检测 (phase-3 D-2) → svc-metering.
      "/api/v1/metering": {
        target: "http://localhost:9206",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => r.setHeader("X-Euler-Account-Id", "100123")),
      },
      // 地域与容灾 (phase-3 M-8) → svc-catalog.
      "/api/v1/catalog": {
        target: "http://localhost:9207",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => r.setHeader("X-Euler-Account-Id", "100123")),
      },
    },
  },
});
