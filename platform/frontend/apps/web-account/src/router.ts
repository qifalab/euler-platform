/**
 * web-account router (02§1.2 dual-form).
 * Unauthenticated routes (login/register/forgot) are public; account
 * management and real-name verification require a session (route guard).
 */
import { createRouter, createWebHistory, type RouteLocationNormalized, type RouteRecordRaw } from "vue-router";
import { useAccountAuth } from "./stores/auth";
import { inWujieSandbox } from "@eu/wujie-bridge";

const routes: RouteRecordRaw[] = [
  { path: "/", redirect: "/login" },
  { path: "/login", name: "login", component: () => import("./views/Login.vue"), meta: { public: true, title: "登录" } },
  { path: "/register", name: "register", component: () => import("./views/Register.vue"), meta: { public: true, title: "注册" } },
  { path: "/forgot", name: "forgot", component: () => import("./views/Forgot.vue"), meta: { public: true, title: "找回密码" } },
  // Authenticated (account management, dual-form sub-app routes).
  // /realname too: svc-iam answers every /api/realname/* call without a
  // session with 未认证, so a visitor is sent to login and back here (with its
  // own ?redirect kept) instead of filling in a form that cannot be submitted.
  { path: "/realname", name: "realname", component: () => import("./views/Realname.vue"), meta: { title: "实名认证" } },
  { path: "/profile", name: "profile", component: () => import("./views/Profile.vue"), meta: { title: "账号信息" } },
  { path: "/ram/users", name: "ram-users", component: () => import("./views/RamUsers.vue"), meta: { title: "RAM 用户" } },
  { path: "/ram/roles", name: "ram-roles", component: () => import("./views/RoleList.vue"), meta: { title: "RAM 角色" } },
  { path: "/ram/simulator", name: "ram-simulator", component: () => import("./views/PolicySimulator.vue"), meta: { title: "策略模拟器" } },
  { path: "/ak", name: "ak", component: () => import("./views/AccessKeys.vue"), meta: { title: "AccessKey 管理" } },
  { path: "/sts", name: "sts", component: () => import("./views/StsCredentials.vue"), meta: { title: "STS 临时凭证" } },
];

export const router = createRouter({ history: createWebHistory(), routes });

/**
 * Auth guard. The access token lives in memory only, so a reload or deep link
 * starts without one: restore the session first (refresh cookie, or the
 * console shell's token when embedded — single-flight) and only then decide.
 * Public pages restore once per page load so e.g. /register sees an existing
 * session; protected pages retry whenever the token is missing.
 */
export async function accountGuard(to: Pick<RouteLocationNormalized, "meta" | "fullPath">) {
  const auth = useAccountAuth();
  if (!auth.isAuthenticated && (!to.meta.public || !auth.sessionChecked)) {
    await auth.ensureSession();
  }
  // Embedded in the console the shell owns sign-in; never bounce to our login.
  if (!to.meta.public && !auth.isAuthenticated && !inWujieSandbox()) {
    return { name: "login", query: { redirect: to.fullPath } };
  }
  return true;
}

router.beforeEach((to) => {
  document.title = `${to.meta.title ?? ""} · 辰云账号`;
  return accountGuard(to);
});
