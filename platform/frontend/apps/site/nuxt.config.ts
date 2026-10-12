// Public introduction for the application-cloud mainline. Works without the
// legacy IaaS catalogue services or an already deployed console.
export default defineNuxtConfig({
  devtools: { enabled: false },
  ssr: true,
  runtimeConfig: {
    public: {
      // NUXT_PUBLIC_CONSOLE_URL is deployment-specific; empty uses a docs CTA.
      consoleUrl: "",
    },
  },
  app: {
    head: {
      htmlAttrs: { lang: "zh-CN" },
      title: "欧拉应用云 — 团队、项目与应用的统一工作空间",
      meta: [
        { charset: "utf-8" },
        { name: "viewport", content: "width=device-width, initial-scale=1" },
        { name: "theme-color", content: "#fcfcfa" },
        {
          name: "description",
          content:
            "欧拉应用云围绕团队与项目连接 EID、Trust、WeAuth、数据库、对象存储、统计、活动与服务器安全工具，提供项目权限、应用连接和操作审计。支持自主部署。",
        },
      ],
    },
  },
  nitro: { compressPublicAssets: true },
});
