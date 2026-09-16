import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";
import { InstanceListView, type InstanceListConfig } from "@eu/console-kit";

/** /instances page — the shared list view, configured for this product (02§7.2). */
const instances: InstanceListConfig = {
  productCode: "eulb",
  title: "弹性负载均衡 SLB",
  createLabel: "创建实例",
  placement: { key: "region", title: "地域" },
  charge: {},
  empty: {
    title: "暂无负载均衡",
    description: "创建您的第一个负载均衡,四层/七层入口,按量计费。",
  },
};

const routes: RouteRecordRaw[] = [
  { path: "/", redirect: "/instances" },
  { path: "/instances", name: "instances", component: InstanceListView, props: { config: instances }, meta: { title: "弹性负载均衡 SLB" } },
  { path: "/instances/:id", name: "instance-detail", component: () => import("./views/InstanceDetail.vue"), meta: { title: "实例详情" } },
  { path: "/buy", name: "buy", component: () => import("./views/BuyWizard.vue"), meta: { title: "创建负载均衡" } },
];

export const router = createRouter({ history: createWebHistory(), routes });
