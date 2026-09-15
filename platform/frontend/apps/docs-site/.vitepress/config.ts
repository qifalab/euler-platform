import { defineConfig } from "vitepress";

// Euler docs site config (02§8.2). Each product has five slots:
// 简介 / 计费说明 / 快速入门 / API 参考 / 操作指南. API ref is auto-generated
// from OpenAPI in the real deploy; here it's hand-written markdown.
export default defineConfig({
  title: "辰云文档",
  description: "辰云(Euler)云平台官方文档",
  lang: "zh-CN",
  lastUpdated: true,
  cleanUrls: true,
  themeConfig: {
    nav: [
      { text: "产品", link: "/products/euecs/intro" },
      { text: "快速入门", link: "/products/euecs/quickstart" },
      { text: "API 参考", link: "/products/euecs/api" },
      { text: "控制台", link: "https://console.euler.emoera.com" },
    ],
    sidebar: {
      "/products/euecs/": [
        {
          text: "云服务器 ECS",
          items: [
            { text: "产品简介", link: "/products/euecs/intro" },
            { text: "计费说明", link: "/products/euecs/billing" },
            { text: "快速入门", link: "/products/euecs/quickstart" },
            { text: "API 参考", link: "/products/euecs/api" },
            { text: "操作指南", link: "/products/euecs/guide" },
          ],
        },
      ],
    },
    socialLinks: [{ icon: "github", link: "https://euler.emoera.com" }],
    footer: { message: "© 辰云 Euler", copyright: "euler.emoera.com" },
    search: { provider: "local" },
  },
});
