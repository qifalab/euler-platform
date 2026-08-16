import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";
import { resolve } from "node:path";

// devops-explorer sub-app (M-5.1, 02§3.4). Externals share Vue/Element Plus/@sc/*
// with the base in Wujie mode; standalone dev resolves its own. The Explorer
// signs through svc-api-meta's /api/v1/apimeta/explorer (the SAME cps1 the SDK
// uses), so this app never computes a signature itself — it posts params and
// renders the returned signature material + optional upstream response.
export default defineConfig({
  plugins: [vue()],
  base: "/",
  resolve: { alias: { "@": resolve(__dirname, "src") } },
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
    port: 5182,
    cors: true,
    proxy: {
      // Explorer signing + action catalogue → svc-api-meta (03§9.4). Dev injects
      // the seed account header the handler requires (gateway-injected in prod).
      "/api/v1/apimeta": {
        target: "http://localhost:9201",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => {
          r.setHeader("X-Sc-Account-Id", "100123");
          r.setHeader("X-Sc-TraceId", `explorer-${Date.now().toString(36)}`);
        }),
      },
      // Action metadata (the dropdown) reuses the internal list endpoint.
      "/internal/actions": {
        target: "http://localhost:9201",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => r.setHeader("X-Sc-Account-Id", "100123")),
      },
    },
  },
});
