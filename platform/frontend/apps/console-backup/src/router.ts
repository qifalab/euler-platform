import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";
import { InstanceListView, type InstanceListConfig } from "@eu/console-kit";

/** /instances page — the shared list view, configured for this product (02§7.2). */
const instances: InstanceListConfig = {
  productCode: "eubackup",
  title: "云备份",
  createLabel: "创建备份策略",
  idTitle: "策略 ID/名称",
  placement: { key: "zoneId", title: "可用区" },
  charge: {},
  empty: {
    title: "暂无备份策略",
    description: "创建您的第一个备份策略,定时快照,保留期自动清理。",
  },
};

const routes: RouteRecordRaw[] = [
  { path: "/", redirect: "/instances" },
  { path: "/instances", name: "instances", component: InstanceListView, props: { config: instances }, meta: { title: "云备份" } },
  { path: "/instances/:id", name: "instance-detail", component: () => import("./views/InstanceDetail.vue"), meta: { title: "详情" } },
  { path: "/buy", name: "buy", component: () => import("./views/BuyWizard.vue"), meta: { title: "创建备份策略" } },
];

export const router = createRouter({ history: createWebHistory(), routes });
