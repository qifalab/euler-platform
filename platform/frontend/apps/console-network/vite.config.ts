import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";
import { resolve } from "node:path";

// Sub-app build config (02§3.4): externals sharing — Vue/Element Plus/@eu/*
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
        "@eu/tokens", "@eu/ui", "@eu/console-kit", "@eu/sdk", "@eu/wujie-bridge",
      ],
      output: {
        entryFileNames: "assets/index.js",
        assetFileNames: "assets/[name][extname]",
      },
    },
  },
  server: {
    port: 5177,
    cors: true, // the base fetches this entry cross-origin in dev
    proxy: {
      // Real BFF data → console-bff (03§4). Dev injects the seed account header
      // so the sub-app renders real resources standalone or in the sandbox.
      "/console": {
        target: "http://localhost:9200",
        changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => {
          r.setHeader("X-Euler-Account-Id", "100123");
          r.setHeader("X-Euler-TraceId", `network-${Date.now().toString(36)}`);
        }),
      },
    },
  },
});
