import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";
import { resolve } from "node:path";

// web-billing dual-form: standalone on billing.* + console sub-app (02§1.2).
export default defineConfig({
  plugins: [vue()],
  resolve: { alias: { "@": resolve(__dirname, "src") } },
  server: {
    port: 5180,
    cors: true,
    proxy: { "/console": { target: "http://localhost:9200", changeOrigin: true,
      configure: (p) => p.on("proxyReq", (r) => r.setHeader("X-Sc-Account-Id", "100123")) },
      "/api/v1/orders": { target: "http://localhost:9204", changeOrigin: true,
        configure: (p) => p.on("proxyReq", (r) => r.setHeader("X-Sc-Account-Id", "100123")) } },
  },
  build: {
    rollupOptions: { input: { main: resolve(__dirname, "index.html") },
      external: ["vue","vue-router","element-plus","@sc/tokens","@sc/ui","@sc/console-kit","@sc/sdk","@sc/wujie-bridge"] },
  },
});
