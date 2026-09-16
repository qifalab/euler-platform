import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";
import { InstanceListView, type InstanceListConfig } from "@eu/console-kit";

/** /instances page — the shared list view, configured for this product (02§7.2). */
const instances: InstanceListConfig = {
  productCode: "eulog",
  title: "日志服务",
  createLabel: "创建日志服务",
  placement: { key: "region", title: "地域" },
  charge: {},
  empty: {
    title: "暂无日志服务实例",
    description: "创建您的第一个日志服务,Vector 采集 + ClickHouse 存储,保留期自动清理。",
  },
};

const routes: RouteRecordRaw[] = [
  { path: "/", redirect: "/instances" },
  { path: "/instances", name: "instances", component: InstanceListView, props: { config: instances }, meta: { title: "日志服务" } },
  { path: "/instances/:id", name: "instance-detail", component: () => import("./views/InstanceDetail.vue"), meta: { title: "详情" } },
  { path: "/buy", name: "buy", component: () => import("./views/BuyWizard.vue"), meta: { title: "创建日志服务" } },
];

export const router = createRouter({ history: createWebHistory(), routes });
