import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";
import { InstanceListView, type InstanceListConfig } from "@eu/console-kit";

/** /instances page — the shared list view, configured for this product (02§7.2). */
const instances: InstanceListConfig = {
  productCode: "euas",
  title: "弹性伸缩",
  createLabel: "创建伸缩组",
  idTitle: "伸缩组 ID/名称",
  placement: { key: "region", title: "地域" },
  charge: {},
  empty: {
    title: "暂无伸缩组",
    description: "创建您的第一个弹性伸缩组,按策略自动扩缩容。",
  },
};

const routes: RouteRecordRaw[] = [
  { path: "/", redirect: "/instances" },
  { path: "/instances", name: "instances", component: InstanceListView, props: { config: instances }, meta: { title: "弹性伸缩" } },
  { path: "/instances/:id", name: "instance-detail", component: () => import("./views/InstanceDetail.vue"), meta: { title: "实例详情" } },
  { path: "/buy", name: "buy", component: () => import("./views/BuyWizard.vue"), meta: { title: "创建伸缩组" } },
];

export const router = createRouter({ history: createWebHistory(), routes });
