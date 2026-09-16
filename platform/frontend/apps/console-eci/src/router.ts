import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";
import { InstanceListView, type InstanceListConfig } from "@eu/console-kit";

/** /instances page — the shared list view, configured for this product (02§7.2). */
const instances: InstanceListConfig = {
  productCode: "eueci",
  title: "弹性容器实例 ECI",
  createLabel: "创建实例",
  placement: { key: "zoneId", title: "可用区" },
  charge: {},
  empty: {
    title: "暂无容器实例",
    description: "创建您的第一个弹性容器实例,按秒计费,秒级拉起。",
  },
};

const routes: RouteRecordRaw[] = [
  { path: "/", redirect: "/instances" },
  { path: "/instances", name: "instances", component: InstanceListView, props: { config: instances }, meta: { title: "弹性容器实例 ECI" } },
  { path: "/instances/:id", name: "instance-detail", component: () => import("./views/InstanceDetail.vue"), meta: { title: "实例详情" } },
  { path: "/buy", name: "buy", component: () => import("./views/BuyWizard.vue"), meta: { title: "创建容器实例" } },
];

export const router = createRouter({ history: createWebHistory(), routes });
