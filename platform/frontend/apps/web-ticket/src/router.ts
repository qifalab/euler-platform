import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";
import { inWujieSandbox } from "@sc/wujie-bridge";
const routes: RouteRecordRaw[] = [
  { path: "/", redirect: "/list" },
  { path: "/list", name: "list", component: () => import("./views/TicketList.vue"), meta: { title: "工单" } },
  { path: "/create", name: "create", component: () => import("./views/CreateTicket.vue"), meta: { title: "提交工单" } },
];
export const router = createRouter({ history: createWebHistory(), routes });
