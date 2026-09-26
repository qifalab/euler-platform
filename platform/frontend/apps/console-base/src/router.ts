import { createRouter, createWebHistory } from "vue-router";
export const router = createRouter({
  history: createWebHistory(),
  scrollBehavior: () => ({ top: 0 }),
  routes: [
    {
      path: "/",
      component: () => import("./cloud/OverviewPage.vue"),
      meta: { title: "项目总览", project: true },
    },
    {
      path: "/catalog",
      component: () => import("./cloud/CatalogPage.vue"),
      meta: { title: "应用目录" },
    },
    {
      path: "/apps/:applicationId",
      component: () => import("./cloud/AppWorkspace.vue"),
      meta: { title: "应用详情", project: true },
    },
    {
      path: "/members",
      component: () => import("./cloud/MembersPage.vue"),
      meta: { title: "成员与邀请" },
    },
    {
      path: "/identity",
      component: () => import("./cloud/IdentityPage.vue"),
      meta: { title: "身份与认证", project: true },
    },
    {
      path: "/permissions",
      component: () => import("./cloud/PermissionsPage.vue"),
      meta: { title: "应用权限", project: true },
    },
    {
      path: "/audit",
      component: () => import("./cloud/AuditPage.vue"),
      meta: { title: "操作审计" },
    },
    {
      path: "/join",
      component: () => import("./cloud/JoinPage.vue"),
      meta: { title: "接受团队邀请" },
    },
    {
      path: "/:pathMatch(.*)*",
      component: () => import("./cloud/NotFoundPage.vue"),
      meta: { title: "页面不存在" },
    },
  ],
});
router.afterEach((to) => {
  document.title = `${to.meta.title ?? "控制台"} · 欧拉应用云`;
});
