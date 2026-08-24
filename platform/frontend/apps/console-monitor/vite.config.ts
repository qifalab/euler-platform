import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";
import { resolve } from "node:path";

// Sub-app build config (02§3.4): externals sharing — Vue/Element Plus/@sc/*
// are NOT bundled here; the base injects single instances via props. The
// sub-app shims resolve them from injected shared deps in Wujie mode, or from
// its own node_modules in standalone mode (degradation path).
export default defineConfig({
  plugins: [vue()],
  base: "/",
  resolve: {
    alias: { "@": resolve(__dirname, "src") },
  },
  build: {
    rollupOptions: {
      external: [
        "vue", "vue-router", "element-plus",
        "@sc/tokens", "@sc/ui", "@sc/console-kit", "@sc/sdk", "@sc/wujie-bridge",
      ],
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
          r.setHeader("X-Sc-Account-Id", "100123");
          r.setHeader("X-Sc-TraceId", `monitor-${Date.now().toString(36)}`);
        }),
      },
      // Real alert rules + metrics + M-9 SLO/chaos → svc-monitor (03§4.4.1).
      "/api/v1/monitor": {
        target: "http://localhost:9202",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => r.setHeader("X-Sc-Account-Id", "100123")),
      },
      // 告警中心 (phase-3 D-1) → alert-center.
      "/api/v1/alertcenter": {
        target: "http://localhost:9213",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => r.setHeader("X-Sc-Account-Id", "100123")),
      },
      // 异常检测 (phase-3 D-2) → svc-metering.
      "/api/v1/metering": {
        target: "http://localhost:9206",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => r.setHeader("X-Sc-Account-Id", "100123")),
      },
      // 地域与容灾 (phase-3 M-8) → svc-catalog.
      "/api/v1/catalog": {
        target: "http://localhost:9207",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => r.setHeader("X-Sc-Account-Id", "100123")),
      },
    },
  },
});
