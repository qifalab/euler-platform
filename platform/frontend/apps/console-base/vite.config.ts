import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";
import { resolve } from "node:path";

// console-base (the shell) is the host. It bundles Vue/Element Plus/@eu/*
// as the single source of shared instances, injected into sub-apps via props
// (02§6.5 externals sharing — no runtime Module Federation in phase 1).
export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      "@": resolve(__dirname, "src"),
    },
  },
  server: {
    port: 5173,
    // Sub-apps run on their own dev ports; the shell fetches their entries
    // cross-origin, so enable CORS for the sub-app origin during dev.
    cors: true,
    proxy: {
      // Console aggregation endpoints → console-bff (03§4). The BFF requires the
      // gateway-injected X-Euler-Account-Id header; in dev we inject a fixed seed
      // account (100123 matches the BFF's seeded data) so the console renders
      // real data without the APISIX gateway.
      "/console": {
        target: "http://localhost:9200",
        changeOrigin: true,
        configure: (proxy) => {
          proxy.on("proxyReq", (proxyReq) => {
            proxyReq.setHeader("X-Euler-Account-Id", "100123");
            proxyReq.setHeader("X-Euler-TraceId", `dev-${Date.now().toString(36)}`);
          });
        },
      },
      // Real SSO → svc-iam web-auth (02§5.1). In dev the proxy forwards to
      // :9101; the HttpOnly refresh cookie carries via credentials:include.
      "/api/auth": { target: "http://localhost:9101", changeOrigin: true },
      "/api/realname": { target: "http://localhost:9101", changeOrigin: true },
      // Runtime registry → svc-api-meta (02§4.1).
      "/api/v1/meta": {
        target: "http://localhost:9201",
        changeOrigin: true,
      },
      // Catalogue metadata (regions/images/placement) → svc-catalog — the
      // RegionSelector and wizard pickers read live region inventory.
      "/api/v1/catalog": {
        target: "http://localhost:9207",
        changeOrigin: true,
      },
    },
  },
});
