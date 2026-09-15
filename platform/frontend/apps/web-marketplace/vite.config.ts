import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";
import { resolve } from "node:path";
export default defineConfig({
  plugins: [vue()],
  resolve: { alias: { "@": resolve(__dirname, "src") } },
  server: {
    port: 5190,
    cors: true,
    proxy: {
      "/api/v1/marketplace": {
        target: "http://localhost:9212",
        changeOrigin: true,
        configure: (p) =>
          p.on("proxyReq", (r) => r.setHeader("X-Euler-Account-Id", "100123")),
      },
    },
  },
  build: {
    rollupOptions: { input: { main: resolve(__dirname, "index.html") },
      external: ["vue","vue-router","element-plus","@eu/tokens","@eu/ui","@eu/sdk","@eu/wujie-bridge"] },
  },
});
