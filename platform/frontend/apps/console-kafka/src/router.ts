import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";
import { InstanceListView, type InstanceListConfig } from "@eu/console-kit";

/** /instances page — the shared list view, configured for this product (02§7.2). */
const instances: InstanceListConfig = {
  productCode: "eukafka",
  title: "托管 Kafka",
  createLabel: "创建实例",
  placement: { key: "zoneId", title: "可用区" },
  charge: { prepaid: true },
  empty: {
    title: "暂无 Kafka 实例",
    description: "创建您的第一个托管 Kafka,broker 跨可用区,平台运维经验产品化。",
  },
};

const routes: RouteRecordRaw[] = [
  { path: "/", redirect: "/instances" },
  { path: "/instances", name: "instances", component: InstanceListView, props: { config: instances }, meta: { title: "托管 Kafka" } },
  { path: "/instances/:id", name: "instance-detail", component: () => import("./views/InstanceDetail.vue"), meta: { title: "实例详情" } },
  { path: "/buy", name: "buy", component: () => import("./views/BuyWizard.vue"), meta: { title: "创建 Kafka 实例" } },
];

export const router = createRouter({ history: createWebHistory(), routes });
