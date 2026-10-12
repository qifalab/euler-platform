<script setup lang="ts">
/**
 * console-network — VPC list (EUVPC), the network category sub-app (02§7.2).
 * Demonstrates the declarative ResourceTable pattern from @eu/console-kit with
 * unified StatusBadge, polling on transitional states (Creating), and
 * EmptyGuide. Also surfaces a compact EIP (EUEIP) table below the main VPC list.
 *
 * Both lists read the orchestrator's resource records (GET
 * /api/v1/orchestrator/resources), which carry the customer `attributes` set
 * at creation (name / cidrBlock / bandwidthMbps); the resource id and SKU
 * remain the fallback when a resource has none. Create flows: #/euvpc/buy
 * (VpcBuy) and #/eueip/buy (EipBuy).
 */
import { computed, h, onMounted, onUnmounted, ref, watch } from "vue";
import { ElButton } from "element-plus";
import { ResourceTable, useResourceTable, paginate, isTransitionalState, formatTime, attributesOf } from "@eu/console-kit";
import { StatusBadge, EmptyGuide, PageHeader } from "@eu/ui";
import { sdk } from "./sdk";
import VpcDetail from "./views/VpcDetail.vue";
import VpcBuy from "./views/VpcBuy.vue";
import EipBuy from "./views/EipBuy.vue";
import "@eu/tokens/style.css";

// Internal hash routing (no vue-router instance in this sub-app, mirroring the
// console-ecs reference 02§3.4). The list links to #/euvpc/vpcs/<id>; when the
// hash matches a VPC detail route we render VpcDetail, otherwise the list.
const hashRoute = ref(location.hash);
function onHash() { hashRoute.value = location.hash; }
onMounted(() => window.addEventListener("hashchange", onHash));
onUnmounted(() => window.removeEventListener("hashchange", onHash));
const detailVpcId = computed(() => {
  const m = hashRoute.value.match(/euvpc\/vpcs\/([^/?#]+)/);
  return m ? decodeURIComponent(m[1]) : null;
});
const showDetail = computed(() => detailVpcId.value !== null);
/** Create pages: #/euvpc/buy and #/eueip/buy. */
const buyPage = computed<"vpc" | "eip" | null>(() => {
  if (/^#\/euvpc\/buy(?:[/?]|$)/.test(hashRoute.value)) return "vpc";
  if (/^#\/eueip\/buy(?:[/?]|$)/.test(hashRoute.value)) return "eip";
  return null;
});
function goBuy(page: "euvpc" | "eueip") {
  location.hash = `#/${page}/buy`;
}

/** An orchestrator resource record (camelCase, with `attributes`). */
type OrchResource = Record<string, unknown>;
const titleCase = (s: unknown) => String(s ?? "").charAt(0).toUpperCase() + String(s ?? "").slice(1).toLowerCase();

type VpcRow = Record<string, unknown>;
type EipRow = Record<string, unknown>;

// --- Main VPC list (EUVPC) ------------------------------------------------
const {
  rows: vpcRows,
  loading: vpcLoading,
  error: vpcError,
  columns: vpcColumns,
  page: vpcPage,
  pageSize: vpcPageSize,
  total: vpcTotal,
  setPage: setVpcPage,
  refresh: refreshVpcs,
} = useResourceTable<VpcRow>({
  api: async (params, signal) => {
    const res = await sdk.get<OrchResource[]>("/api/v1/orchestrator/resources", { signal });
    const items = (res.data ?? [])
      .filter((r) => r.productCode === "euvpc")
      .map((r) => {
        const attrs = attributesOf(r);
        return {
          vpcId: r.resourceId,
          vpcName: attrs.name || r.resourceId,
          cidr: attrs.cidrBlock || "—",
          spec: r.specCode,
          region: r.region,
          subnetCount: 0,
          status: titleCase(r.state),
          createTime: formatTime(r.createdAt as string | undefined),
        };
      });
    // The endpoint returns every resource: page client-side, total = full count.
    return paginate(items, params);
  },
  columns: [
    { key: "vpcId", title: "资源ID/名称", link: (r) => `#/euvpc/vpcs/${r.vpcId}` },
    { key: "status", title: "状态" },
    { key: "cidr", title: "IPv4 网段" },
    { key: "spec", title: "规格" },
    { key: "region", title: "地域" },
    { key: "createTime", title: "创建时间" },
  ],
  polling: { interval: 10_000, when: (r) => r.some((x) => isTransitionalState(x.status)) },
  pageSize: 20,
});

// Wrap StatusBadge as a column render for the status column; surface the VPC
// name alongside the ID in the link column.
const vpcTableColumns = computed(() =>
  vpcColumns.value.map((c) =>
    c.key === "status"
      ? { ...c, render: (row: VpcRow) => h(StatusBadge, { status: String(row.status ?? "") }) }
      : c.key === "vpcId"
        ? { ...c, render: (row: VpcRow) => (row.vpcName === row.vpcId ? String(row.vpcId) : `${String(row.vpcName)} (${String(row.vpcId)})`) }
        : c,
  ),
);

// --- Secondary EIP table (EUEIP) -----------------------------------------
// Compact auxiliary list over the same orchestrator records.
const {
  rows: eipRows,
  loading: eipLoading,
  error: eipError,
  columns: eipColumns,
  page: eipPage,
  pageSize: eipPageSize,
  total: eipTotal,
  setPage: setEipPage,
  refresh: refreshEips,
} = useResourceTable<EipRow>({
  api: async (params, signal) => {
    const res = await sdk.get<OrchResource[]>("/api/v1/orchestrator/resources", { signal });
    const items = (res.data ?? [])
      .filter((r) => r.productCode === "eueip")
      .map((r) => {
        const attrs = attributesOf(r);
        // The resource record carries no public IP field (prod: the eueip
        // detail API returns it) — a dash is honest, a mislabelled spec is not.
        return {
          eipId: r.resourceId,
          eipName: attrs.name || r.resourceId,
          ipAddress: "—",
          bandwidth: attrs.bandwidthMbps ? `${attrs.bandwidthMbps} Mbit/s` : "—",
          spec: r.specCode ?? "—",
          bindInstance: "—",
          status: titleCase(r.state),
        };
      });
    return paginate(items, params);
  },
  columns: [
    { key: "eipId", title: "弹性公网IP ID/名称", link: (r) => `#/eueip/eips/${r.eipId}` },
    { key: "ipAddress", title: "公网 IP" },
    { key: "status", title: "状态" },
    { key: "bandwidth", title: "带宽" },
    { key: "spec", title: "规格" },
    { key: "bindInstance", title: "绑定实例" },
  ],
  polling: { interval: 10_000, when: (r) => r.some((x) => isTransitionalState(x.status)) },
  pageSize: 20,
});

// Back from a create page (or a detail page): reload both lists so a new
// resource shows up right away (polling then follows it out of CREATING).
watch(() => buyPage.value !== null || showDetail.value, (away, wasAway) => {
  if (wasAway && !away) {
    void refreshVpcs();
    void refreshEips();
  }
});

const eipTableColumns = computed(() =>
  eipColumns.value.map((c) =>
    c.key === "status"
      ? { ...c, render: (row: EipRow) => h(StatusBadge, { status: String(row.status ?? "") }) }
      : c.key === "eipId"
        ? { ...c, render: (row: EipRow) => (row.eipName === row.eipId ? String(row.eipId) : `${String(row.eipName)} (${String(row.eipId)})`) }
        : c,
  ),
);
</script>

<template>
  <section class="vpc-app">
    <VpcBuy v-if="buyPage === 'vpc'" />
    <EipBuy v-else-if="buyPage === 'eip'" />
    <VpcDetail v-else-if="showDetail" :key="detailVpcId ?? ''" />
    <template v-else>
    <PageHeader title="专有网络 VPC">
      <template #actions>
        <ElButton type="primary" @click="goBuy('euvpc')">创建</ElButton>
      </template>
    </PageHeader>

    <!-- A failed load must not read as "暂无专有网络". -->
    <div v-if="vpcError" class="vpc-error">
      <p class="vpc-error-msg">VPC 列表加载失败:{{ (vpcError as Error)?.message ?? String(vpcError) }}</p>
      <ElButton size="small" @click="refreshVpcs">重试</ElButton>
    </div>
    <template v-else>
    <ResourceTable
      :rows="vpcRows as Record<string, unknown>[]"
      :columns="vpcTableColumns"
      :loading="vpcLoading"
      :total="vpcTotal"
      :page="vpcPage"
      :page-size="vpcPageSize"
      @update:page="setVpcPage"
    >
      <template #empty>
        <EmptyGuide title="暂无专有网络" description="创建您的第一个 VPC,规划云上私有网络与子网。" action-label="创建 VPC" action-href="#/euvpc/buy" />
      </template>
    </ResourceTable>
    </template>

    <header class="vpc-subheader">
      <h2>弹性公网 IP (EUEIP)</h2>
      <ElButton size="small" type="primary" @click="goBuy('eueip')">申请弹性公网 IP</ElButton>
    </header>

    <div v-if="eipError" class="vpc-error">
      <p class="vpc-error-msg">弹性公网 IP 列表加载失败:{{ (eipError as Error)?.message ?? String(eipError) }}</p>
      <ElButton size="small" @click="refreshEips">重试</ElButton>
    </div>
    <template v-else>
    <ResourceTable
      :rows="eipRows as Record<string, unknown>[]"
      :columns="eipTableColumns"
      :loading="eipLoading"
      :total="eipTotal"
      :page="eipPage"
      :page-size="eipPageSize"
      @update:page="setEipPage"
    >
      <template #empty>
        <EmptyGuide title="暂无弹性公网 IP" description="申请弹性公网 IP 并绑定到云资源,实现公网访问。" action-label="申请弹性公网 IP" action-href="#/eueip/buy" />
      </template>
    </ResourceTable>
    </template>
    </template>
  </section>
</template>

<style scoped>
.vpc-app { padding: 16px 24px; }
.vpc-subheader { margin: 24px 0 12px; display: flex; align-items: center; justify-content: space-between; gap: 12px; }
.vpc-subheader h2 { font-size: var(--eu-font-size-lg); margin: 0; color: var(--eu-text-primary); }
.vpc-error {
  padding: 32px; text-align: center;
  border: 1px solid var(--eu-border); border-radius: var(--eu-radius-lg);
}
.vpc-error-msg { color: var(--eu-color-danger); font-size: 13px; margin: 0 0 12px; }
</style>
