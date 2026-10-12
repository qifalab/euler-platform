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
    port: 5188,
    cors: true, // the base fetchs this entry cross-origin in dev
    proxy: {
      // Real BFF data → console-bff (03§4). Dev injects the seed account header
      // so the sub-app renders real resources standalone or in the sandbox.
      "/console": {
        target: "http://localhost:9200",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => {
          r.setHeader("X-Euler-Account-Id", "100123");
          r.setHeader("X-Euler-TraceId", `kafka-${Date.now().toString(36)}`);
        }),
      },
      // Real provisioning → svc-orchestrator (03§4.3, the 开通 step). EUKAFKA
      // flows through the same saga as every product — productCode=eukafka
      // routes it to the DriverK8s binding in provision.Registry.
      "/api/v1/orchestrator": {
        target: "http://localhost:9203",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => r.setHeader("X-Euler-Account-Id", "100123")),
      },
      // Product catalogue + 询价 → svc-catalog (01§7, the quote link). EUKAFKA's
      // prepaid (MONTH) / postpaid (HOUR) pricing rules live here (M-7.5).
      "/api/v1/catalog": {
        target: "http://localhost:9207",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => r.setHeader("X-Euler-Account-Id", "100123")),
      },
      // Order creation/payment → svc-order (03§4.2.2).
      "/api/v1/orders": {
        target: "http://localhost:9204",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => r.setHeader("X-Euler-Account-Id", "100123")),
      },
      // Checkout coupons (优惠券) → svc-order.
      "/api/v1/coupons": {
        target: "http://localhost:9204",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => r.setHeader("X-Euler-Account-Id", "100123")),
      },
    },
  },
});
