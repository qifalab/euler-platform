import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";
import { inWujieSandbox } from "@sc/wujie-bridge";
const routes: RouteRecordRaw[] = [
  { path: "/", redirect: "/storefront" },
  { path: "/storefront", name: "storefront", component: () => import("./views/Storefront.vue"), meta: { title: "商品目录" } },
  { path: "/publish", name: "publish", component: () => import("./views/PublishListing.vue"), meta: { title: "发布商品" } },
  { path: "/review", name: "review", component: () => import("./views/ReviewDesk.vue"), meta: { title: "上架审核" } },
  { path: "/settlement", name: "settlement", component: () => import("./views/Settlement.vue"), meta: { title: "订单结算" } },
];
export const router = createRouter({ history: createWebHistory(), routes });
