import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";
import { isOperator } from "./marketplace";
const routes: RouteRecordRaw[] = [
  { path: "/", redirect: "/storefront" },
  { path: "/storefront", name: "storefront", component: () => import("./views/Storefront.vue"), meta: { title: "商品目录" } },
  { path: "/publish", name: "publish", component: () => import("./views/PublishListing.vue"), meta: { title: "发布商品" } },
  // Operator-only desks (svc-marketplace answers 403 Marketplace.NotOperator).
  { path: "/review", name: "review", component: () => import("./views/ReviewDesk.vue"), meta: { title: "上架审核", operator: true } },
  { path: "/settlement", name: "settlement", component: () => import("./views/Settlement.vue"), meta: { title: "订单结算", operator: true } },
];
export const router = createRouter({ history: createWebHistory(), routes });

router.beforeEach(async (to) => {
  if (to.meta.operator && !(await isOperator())) return { name: "storefront" };
  return true;
});
