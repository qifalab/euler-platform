import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";
import { resolve } from "node:path";

// devops-explorer sub-app (M-5.1, 02§3.4). Externals share Vue/Element Plus/@eu/*
// with the base in Wujie mode; standalone dev resolves its own. The Explorer
// signs through svc-api-meta's /api/v1/apimeta/explorer (the SAME cps1 the SDK
// uses), so this app never computes a signature itself — it posts params and
// renders the returned signature material + optional upstream response.
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
  resolve: { alias: { "@": resolve(__dirname, "src") } },
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
    port: 5182,
    cors: true,
    proxy: {
      // Explorer signing + action catalogue (/api/v1/apimeta/explorer/actions)
      // → svc-api-meta (03§9.4). Dev injects the seed account header the
      // handler requires (gateway-injected in prod).
      "/api/v1/apimeta": {
        target: "http://localhost:9201",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => {
          r.setHeader("X-Euler-Account-Id", "100123");
          r.setHeader("X-Euler-TraceId", `explorer-${Date.now().toString(36)}`);
        }),
      },
    },
  },
});
