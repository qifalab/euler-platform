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
    port: 5183,
    cors: true, // the base fetchs this entry cross-origin in dev
    proxy: {
      // Real BFF data → console-bff (03§4). Dev injects the seed account header
      // so the sub-app renders real resources standalone or in the sandbox.
      "/console": {
        target: "http://localhost:9200",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => {
          r.setHeader("X-Sc-Account-Id", "100123");
          r.setHeader("X-Sc-TraceId", `eci-${Date.now().toString(36)}`);
        }),
      },
      // Real provisioning → svc-orchestrator (03§4.3, the 开通 step). ECI flows
      // through the same saga as every product — productCode=sceci routes it to
      // the DriverK8s binding in provision.Registry.
      "/api/v1/orchestrator": {
        target: "http://localhost:9203",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => r.setHeader("X-Sc-Account-Id", "100123")),
      },
      // Product catalogue + 询价 → svc-catalog (01§7, the quote link). SCECI's
      // per-SECOND pricing rule lives here (M-7.1).
      "/api/v1/catalog": {
        target: "http://localhost:9207",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => r.setHeader("X-Sc-Account-Id", "100123")),
      },
      // Order creation/payment → svc-order (03§4.2.2).
      "/api/v1/orders": {
        target: "http://localhost:9204",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => r.setHeader("X-Sc-Account-Id", "100123")),
      },
    },
  },
});
