// Nuxt config for the Euler Computing Platform marketing portal (02§8.1).
// SSR by default; route-level render-mode mixing (ssr/swr/csr) as pages land.
export default defineNuxtConfig({
  devtools: { enabled: true },
  ssr: true,
  // Google Cloud Glass: global design tokens (@sc/tokens) — --sc-* CSS variables,
  // body base styles (mesh background + Google Sans stack) and .sc-glass utilities.
  css: ["@sc/tokens/style.css"],
  app: {
    head: {
      htmlAttrs: { lang: "zh-CN" },
      title: "欧拉算力平台 — 云计算与 AI 基础设施",
      meta: [
        { charset: "utf-8" },
        { name: "viewport", content: "width=device-width, initial-scale=1" },
        {
          name: "description",
          content: "欧拉算力平台 — 云服务器、对象存储、块存储、专有网络、云数据库、云监控、弹性公网 IP。",
        },
      ],
    },
  },
  nitro: {
    compressPublicAssets: true,
    // Dev-only proxy: the SSR server and the browser fetch site content
    // (announcements from svc-notify :9211, catalogue from svc-catalog :9207)
    // through these paths. In production APISIX fronts the site and routes
    // /api/v1/* to the services on the same origin — no proxy needed there.
    devProxy: {
      "/api/v1/announcements": { target: "http://localhost:9211", changeOrigin: true },
      "/api/v1/catalog": { target: "http://localhost:9207", changeOrigin: true },
    },
  },
});
