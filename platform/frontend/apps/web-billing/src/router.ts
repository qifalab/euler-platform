import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";
import { inWujieSandbox } from "@sc/wujie-bridge";

const routes: RouteRecordRaw[] = [
  { path: "/", redirect: "/bills" },
  { path: "/bills", name: "bills", component: () => import("./views/Bills.vue"), meta: { title: "账单" } },
  { path: "/orders", name: "orders", component: () => import("./views/Orders.vue"), meta: { title: "订单" } },
  { path: "/renew", name: "renew", component: () => import("./views/Renew.vue"), meta: { title: "续费管理" } },
  { path: "/resource-packs", name: "resource-packs", component: () => import("./views/ResourcePackages.vue"), meta: { title: "资源包" } },
  { path: "/invoices", name: "invoices", component: () => import("./views/Invoices.vue"), meta: { title: "发票" } },
  { path: "/cost-analysis", name: "cost-analysis", component: () => import("./views/CostAnalysis.vue"), meta: { title: "成本分析" } },
];

export const router = createRouter({ history: createWebHistory(), routes });
