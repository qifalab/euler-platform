import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";

const routes: RouteRecordRaw[] = [
  { path: "/", redirect: "/instances" },
  { path: "/instances", name: "instances", component: () => import("./views/InstanceList.vue"), meta: { title: "弹性负载均衡 SLB" } },
  { path: "/instances/:id", name: "instance-detail", component: () => import("./views/InstanceDetail.vue"), meta: { title: "实例详情" } },
  { path: "/buy", name: "buy", component: () => import("./views/BuyWizard.vue"), meta: { title: "创建负载均衡" } },
];

export const router = createRouter({ history: createWebHistory(), routes });
