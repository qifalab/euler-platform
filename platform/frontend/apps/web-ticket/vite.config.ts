import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";
import { resolve } from "node:path";
// Externals sharing (02§6.5) is opt-in: EU_SHARED_EXTERNALS=1 leaves these
// as bare imports for a host that supplies them through an import map. No
// deployment provides one (neither this app's index.html nor the Wujie
// sandbox), so such a build cannot load in a browser ("Failed to resolve
// module specifier 'vue'"); and this app is also served standalone on its
// own domain. By default the shared deps are bundled — Wujie runs each sub-app in
// its own iframe realm, so a bundled Vue is never shared with the shell anyway.
const SHARED_EXTERNALS = ["vue","vue-router","element-plus","@eu/tokens","@eu/ui","@eu/sdk","@eu/wujie-bridge"];
const external = process.env.EU_SHARED_EXTERNALS === "1" ? SHARED_EXTERNALS : [];

export default defineConfig({
  plugins: [vue()],
  resolve: { alias: { "@": resolve(__dirname, "src") } },
  server: {
    port: 5181,
    cors: true,
    proxy: {
      "/api/v1/tickets": {
        target: "http://localhost:9209",
        changeOrigin: true,
        configure: (p) =>
          p.on("proxyReq", (r) => r.setHeader("X-Euler-Account-Id", "100123")),
      },
    },
  },
  build: {
    rollupOptions: { input: { main: resolve(__dirname, "index.html") },
      external },
  },
});
