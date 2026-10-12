// Nuxt config for the Euler Computing Platform marketing portal (02§8.1).
// SSR by default; route-level render-mode mixing (ssr/swr/csr) as pages land.
export default defineNuxtConfig({
  devtools: { enabled: true },
  ssr: true,
  // Google Cloud Glass: global design tokens (@eu/tokens) — --eu-* CSS variables,
  // body base styles (mesh background + Google Sans stack) and .eu-glass utilities.
  css: ["@eu/tokens/style.css"],
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
  // Origin the SSR server uses for /api/v1/* (announcements, catalogue).
  // A relative useFetch("/api/v1/...") on the server resolves to Nitro itself:
  // the devProxy below exists only under `nuxt dev`, and in production APISIX
  // fronts only the browser's requests — so server-rendered pages 404'd and
  // shipped empty. The browser keeps same-origin relative URLs. Override at
  // runtime with NUXT_API_ORIGIN (e.g. the in-cluster gateway address).
  runtimeConfig: {
    apiOrigin: "https://www.euler.emoera.com",
  },
  $development: {
    // `nuxt dev --port 3000`: SSR goes through the dev server, whose devProxy
    // forwards to the local services.
    runtimeConfig: { apiOrigin: "http://localhost:3000" },
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
