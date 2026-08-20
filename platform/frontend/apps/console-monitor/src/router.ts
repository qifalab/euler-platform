import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";

const routes: RouteRecordRaw[] = [
  { path: "/", redirect: "/rules" },
  { path: "/rules", name: "rules", component: () => import("./views/RuleList.vue"), meta: { title: "监控规则" } },
  { path: "/alerts", name: "alerts", component: () => import("./views/AlertCenter.vue"), meta: { title: "告警中心" } },
  { path: "/anomaly", name: "anomaly", component: () => import("./views/AnomalyScan.vue"), meta: { title: "异常检测" } },
  { path: "/stability", name: "stability", component: () => import("./views/Stability.vue"), meta: { title: "稳定性平台" } },
  { path: "/regions", name: "regions", component: () => import("./views/RegionTopology.vue"), meta: { title: "地域与容灾" } },
];
export const router = createRouter({ history: createWebHistory(), routes });
