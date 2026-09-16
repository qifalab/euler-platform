import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";
import { InstanceListView, type InstanceListConfig } from "@eu/console-kit";

/** /instances page — the shared list view, configured for this product (02§7.2). */
const instances: InstanceListConfig = {
  productCode: "euecs",
  title: "云服务器 ECS",
  createLabel: "创建实例",
  placement: { key: "zoneId", title: "可用区" },
  extraColumns: [{ key: "expiredTime", title: "到期时间", from: "ExpiredAt", fallback: "—" }],
  empty: {
    title: "暂无云服务器",
    description: "创建您的第一台云服务器。",
  },
};

const routes: RouteRecordRaw[] = [
  { path: "/", redirect: "/instances" },
  { path: "/instances", name: "instances", component: InstanceListView, props: { config: instances }, meta: { title: "云服务器 ECS" } },
  { path: "/instances/:id", name: "instance-detail", component: () => import("./views/InstanceDetail.vue"), meta: { title: "实例详情" } },
  { path: "/buy", name: "buy", component: () => import("./views/BuyWizard.vue"), meta: { title: "购买实例" } },
];

export const router = createRouter({ history: createWebHistory(), routes });
