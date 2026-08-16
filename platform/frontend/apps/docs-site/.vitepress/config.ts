import { defineConfig } from "vitepress";

// StarCloud docs site config (02§8.2). Each product has five slots:
// 简介 / 计费说明 / 快速入门 / API 参考 / 操作指南. API ref is auto-generated
// from OpenAPI in the real deploy; here it's hand-written markdown.
export default defineConfig({
  title: "辰云文档",
  description: "辰云(StarCloud)云平台官方文档",
  lang: "zh-CN",
  lastUpdated: true,
  cleanUrls: true,
  themeConfig: {
    nav: [
      { text: "产品", link: "/products/scecs/intro" },
      { text: "快速入门", link: "/products/scecs/quickstart" },
      { text: "API 参考", link: "/products/scecs/api" },
      { text: "控制台", link: "https://console.starcloud.cn" },
    ],
    sidebar: {
      "/products/scecs/": [
        {
          text: "云服务器 ECS",
          items: [
            { text: "产品简介", link: "/products/scecs/intro" },
            { text: "计费说明", link: "/products/scecs/billing" },
            { text: "快速入门", link: "/products/scecs/quickstart" },
            { text: "API 参考", link: "/products/scecs/api" },
            { text: "操作指南", link: "/products/scecs/guide" },
          ],
        },
      ],
    },
    socialLinks: [{ icon: "github", link: "https://starcloud.cn" }],
    footer: { message: "© 辰云 StarCloud", copyright: "starcloud.cn" },
    search: { provider: "local" },
  },
});
