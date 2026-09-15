/**
 * web-account router (02§1.2 dual-form).
 * Unauthenticated routes (login/register/forgot/realname) are public;
 * account-management routes require a token (route guard).
 */
import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";
import { useAccountAuth } from "./stores/auth";
import { inWujieSandbox } from "@eu/wujie-bridge";

const routes: RouteRecordRaw[] = [
  { path: "/", redirect: "/login" },
  { path: "/login", name: "login", component: () => import("./views/Login.vue"), meta: { public: true, title: "登录" } },
  { path: "/register", name: "register", component: () => import("./views/Register.vue"), meta: { public: true, title: "注册" } },
  { path: "/forgot", name: "forgot", component: () => import("./views/Forgot.vue"), meta: { public: true, title: "找回密码" } },
  { path: "/realname", name: "realname", component: () => import("./views/Realname.vue"), meta: { public: true, title: "实名认证" } },
  // Authenticated (account management, dual-form sub-app routes).
  { path: "/profile", name: "profile", component: () => import("./views/Profile.vue"), meta: { title: "账号信息" } },
  { path: "/ram/users", name: "ram-users", component: () => import("./views/RamUsers.vue"), meta: { title: "RAM 用户" } },
  { path: "/ram/roles", name: "ram-roles", component: () => import("./views/RoleList.vue"), meta: { title: "RAM 角色" } },
  { path: "/ram/simulator", name: "ram-simulator", component: () => import("./views/PolicySimulator.vue"), meta: { title: "策略模拟器" } },
  { path: "/ak", name: "ak", component: () => import("./views/AccessKeys.vue"), meta: { title: "AccessKey 管理" } },
  { path: "/sts", name: "sts", component: () => import("./views/StsCredentials.vue"), meta: { title: "STS 临时凭证" } },
];

export const router = createRouter({ history: createWebHistory(), routes });

router.beforeEach((to) => {
  document.title = `${to.meta.title ?? ""} · 辰云账号`;
  const auth = useAccountAuth();
  // In Wujie mode the parent owns the session; trust it for sub-app nav.
  if (!to.meta.public && !auth.isAuthenticated && !inWujieSandbox()) {
    return { name: "login", query: { redirect: to.fullPath } };
  }
  return true;
});
