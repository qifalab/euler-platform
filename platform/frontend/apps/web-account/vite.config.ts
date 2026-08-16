import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";
import { resolve } from "node:path";

// web-account is a dual-form site (02§1.2): standalone site on account.* +
// console sub-app entry. In standalone mode it renders its own top bar; in the
// Wujie sandbox it renders shell-less (detected via @sc/wujie-bridge).
export default defineConfig({
  plugins: [vue()],
  resolve: { alias: { "@": resolve(__dirname, "src") } },
  server: {
    port: 5175,
    cors: true,
    // Real SSO: forward /api/auth/* and /api/realname/* to svc-iam web-auth
    // (02§5.1/§5.2). The refresh_token HttpOnly cookie is set on /api/auth
    // path and carries back via credentials:include.
    proxy: {
      "/api/auth": { target: "http://localhost:9101", changeOrigin: true },
      "/api/realname": { target: "http://localhost:9101", changeOrigin: true },
      // Account / RAM / AK management (svc-iam; 02§5, 07§2.2). Profile, RAM
      // users, and AccessKey CRUD are served by svc-iam on :9101.
      "/api/account": { target: "http://localhost:9101", changeOrigin: true },
      "/api/ram": { target: "http://localhost:9101", changeOrigin: true },
      "/api/ak": { target: "http://localhost:9101", changeOrigin: true },
    },
  },
  build: {
    rollupOptions: {
      input: { main: resolve(__dirname, "index.html") },
      external: ["vue", "vue-router", "element-plus", "@sc/tokens", "@sc/ui", "@sc/sdk", "@sc/wujie-bridge"],
    },
  },
});
