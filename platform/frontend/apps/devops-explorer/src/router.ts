import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";

const routes: RouteRecordRaw[] = [
  { path: "/", redirect: "/explorer" },
  { path: "/explorer", name: "explorer", component: () => import("./views/Explorer.vue"), meta: { title: "API 在线调试" } },
  { path: "/actions", name: "actions", component: () => import("./views/ActionCatalogue.vue"), meta: { title: "API 目录" } },
  { path: "/community", name: "community", component: () => import("./views/Community.vue"), meta: { title: "开发者社区" } },
];

export const router = createRouter({ history: createWebHistory(), routes });
