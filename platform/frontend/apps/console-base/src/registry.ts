/**
 * Sub-app registry client (02§4.1).
 * Source of truth is svc-api-meta GET /api/v1/meta/console/apps; this falls
 * back to a built-in static list so the shell runs with zero backend.
 * Cached in sessionStorage for 5 minutes (02§4.1).
 */
import { defineStore } from "pinia";

export interface RegistryApp {
  appCode: string;
  appTitle: string;
  productCodes: string[];
  activeRules: string[];
  entryUrl: string;
  version: string;
  keepAlive: boolean;
  preload: boolean;
  gray?: { version: string; entryUrl: string; rule: { type: string; percent?: number } };
  status: string;
}

export interface Registry {
  apps: RegistryApp[];
  revision: number;
  ttl: number;
}

const CACHE_KEY = "sc:console:registry";
const CACHE_TTL = 5 * 60 * 1000;

// Built-in fallback (production pulls this from svc-api-meta). Mirrors the
// architecture doc's seven product-category sub-apps + billing/iam/ticket.
const fallback: Registry = {
  revision: 1,
  ttl: 300,
  apps: [
    { appCode: "console-ecs", appTitle: "云服务器 ECS", productCodes: ["scecs"], activeRules: ["/scecs"], entryUrl: "http://localhost:5174/", version: "0.1.0", keepAlive: true, preload: true, status: "online" },
    { appCode: "console-eci", appTitle: "弹性容器实例 ECI", productCodes: ["sceci"], activeRules: ["/sceci"], entryUrl: "http://localhost:5183/", version: "0.1.0", keepAlive: true, preload: false, status: "online" },
    { appCode: "console-lb", appTitle: "负载均衡 SLB", productCodes: ["sclb"], activeRules: ["/sclb"], entryUrl: "http://localhost:5184/", version: "0.1.0", keepAlive: false, preload: false, status: "online" },
    { appCode: "console-autoscaling", appTitle: "弹性伸缩 AS", productCodes: ["scas"], activeRules: ["/scas"], entryUrl: "http://localhost:5185/", version: "0.1.0", keepAlive: false, preload: false, status: "online" },
    { appCode: "console-backup", appTitle: "云备份", productCodes: ["scbackup"], activeRules: ["/scbackup"], entryUrl: "http://localhost:5186/", version: "0.1.0", keepAlive: false, preload: false, status: "online" },
    { appCode: "console-redis", appTitle: "云数据库 Redis", productCodes: ["scredis"], activeRules: ["/scredis"], entryUrl: "http://localhost:5187/", version: "0.1.0", keepAlive: false, preload: false, status: "online" },
    { appCode: "console-kafka", appTitle: "消息队列 Kafka", productCodes: ["sckafka"], activeRules: ["/sckafka"], entryUrl: "http://localhost:5188/", version: "0.1.0", keepAlive: false, preload: false, status: "online" },
    { appCode: "console-logservice", appTitle: "日志服务", productCodes: ["sclog"], activeRules: ["/sclog"], entryUrl: "http://localhost:5189/", version: "0.1.0", keepAlive: false, preload: false, status: "online" },
    { appCode: "console-storage", appTitle: "对象存储 OSS", productCodes: ["scoss"], activeRules: ["/scoss"], entryUrl: "http://localhost:5176/", version: "0.1.0", keepAlive: true, preload: false, status: "online" },
    { appCode: "console-network", appTitle: "专有网络 VPC", productCodes: ["scvpc", "sceip"], activeRules: ["/scvpc", "/sceip"], entryUrl: "http://localhost:5177/", version: "0.1.0", keepAlive: false, preload: false, status: "online" },
    { appCode: "console-database", appTitle: "云数据库 RDS", productCodes: ["scrds"], activeRules: ["/scrds"], entryUrl: "http://localhost:5178/", version: "0.1.0", keepAlive: false, preload: false, status: "online" },
    { appCode: "console-monitor", appTitle: "云监控", productCodes: ["scmon"], activeRules: ["/scmon"], entryUrl: "http://localhost:5179/", version: "0.1.0", keepAlive: false, preload: false, status: "online" },
    { appCode: "web-billing", appTitle: "费用中心", productCodes: [], activeRules: ["/billing"], entryUrl: "http://localhost:5180/", version: "0.1.0", keepAlive: false, preload: true, status: "online" },
    { appCode: "web-account", appTitle: "账号与访问控制", productCodes: [], activeRules: ["/account"], entryUrl: "http://localhost:5175/", version: "0.1.0", keepAlive: false, preload: false, status: "online" },
    { appCode: "web-ticket", appTitle: "工单支持", productCodes: [], activeRules: ["/ticket"], entryUrl: "http://localhost:5181/", version: "0.1.0", keepAlive: false, preload: false, status: "online" },
    { appCode: "devops-explorer", appTitle: "OpenAPI Explorer", productCodes: [], activeRules: ["/explorer"], entryUrl: "http://localhost:5182/", version: "0.1.0", keepAlive: false, preload: false, status: "online" },
  ],
};

export const useRegistry = defineStore("registry", {
  state: () => ({
    apps: [] as RegistryApp[],
    revision: 0,
    loaded: false,
  }),
  actions: {
    async load() {
      // 1) Session cache (5 min).
      try {
        const raw = sessionStorage.getItem(CACHE_KEY);
        if (raw) {
          const cached = JSON.parse(raw) as { data: Registry; ts: number };
          if (Date.now() - cached.ts < CACHE_TTL) {
            this.apply(cached.data);
            return;
          }
        }
      } catch { /* ignore corrupt cache */ }

      // 2) Runtime registry from svc-api-meta. The service returns the
      // platform envelope { Code, Data: { apps, revision, ttl } } (03§9.3);
      // unwrap Data before applying.
      try {
        const res = await fetch("/api/v1/meta/console/apps");
        if (res.ok) {
          const body = await res.json();
          const data = (body.Data ?? body) as Registry;
          if (data && Array.isArray(data.apps)) {
            this.apply(data);
            sessionStorage.setItem(CACHE_KEY, JSON.stringify({ data, ts: Date.now() }));
            return;
          }
        }
      } catch (e) {
        console.warn("[console-base] registry fetch failed, using fallback", e);
      }

      // 3) Built-in fallback (02§4.1: 保底可用).
      this.apply(fallback);
    },
    apply(data: Registry) {
      this.apps = Array.isArray(data?.apps) ? data.apps : [];
      this.revision = data?.revision ?? 0;
      this.loaded = true;
    },
    /** Resolve which sub-app owns a given path (first path segment match). */
    resolve(path: string): RegistryApp | undefined {
      const seg = "/" + (path.split("/").filter(Boolean)[0] ?? "");
      return this.apps.find((a) => a.activeRules.includes(seg));
    },
    productCodeOf(path: string): string | undefined {
      const seg = "/" + (path.split("/").filter(Boolean)[0] ?? "");
      return this.resolve(path)?.activeRules.find((r) => r === seg);
    },
  },
});
