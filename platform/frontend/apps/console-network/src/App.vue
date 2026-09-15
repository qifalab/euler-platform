<script setup lang="ts">
/**
 * console-network — VPC list (EUVPC), the network category sub-app (02§7.2).
 * Demonstrates the declarative ResourceTable pattern from @eu/console-kit with
 * unified StatusBadge, polling on transitional states (Creating), and
 * EmptyGuide. Also surfaces a compact EIP (EUEIP) table below the main VPC list.
 */
import { computed, h, onMounted, onUnmounted, ref } from "vue";
import { ElButton } from "element-plus";
import { ResourceTable, useResourceTable } from "@eu/console-kit";
import { StatusBadge, EmptyGuide, PageHeader } from "@eu/ui";
import { createSDK, type EuError } from "@eu/sdk";
import VpcDetail from "./views/VpcDetail.vue";
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

type VpcRow = Record<string, unknown>;
type EipRow = Record<string, unknown>;

// In the real build @eu/sdk is generated from OpenAPI; here a typed fetcher.
const sdk = createSDK({ baseURL: "" });

// --- Main VPC list (EUVPC) ------------------------------------------------
// Demo fetcher — returns a fixed page so the list renders with no backend.
// Real deploy: sdk.get<Vpc[]>('/euvpc/vpcs', { regionId }).
const {
  rows: vpcRows,
  loading: vpcLoading,
  columns: vpcColumns,
  page: vpcPage,
  pageSize: vpcPageSize,
  total: vpcTotal,
  setPage: setVpcPage,
} = useResourceTable<VpcRow>({
  api: async () => {
    const res = await sdk.get<VpcRow[]>("/console/resources");
    const items = (res.data ?? [])
      .filter((r) => r.ProductCode === "euvpc")
      .map((r) => ({
        vpcId: r.ResourceId,
        vpcName: r.ResourceId,
        cidr: r.SpecCode,
        region: r.Region,
        subnetCount: 0,
        status: String(r.State ?? "").charAt(0).toUpperCase() + String(r.State ?? "").slice(1).toLowerCase(),
        createTime: (r.CreatedAt as string) || "—",
      }));
    return { items, total: items.length };
  },
  columns: [
    { key: "vpcId", title: "资源ID/名称", link: (r) => `#/euvpc/vpcs/${r.vpcId}` },
    { key: "status", title: "状态" },
    { key: "cidr", title: "规格" },
    { key: "region", title: "地域" },
    { key: "createTime", title: "创建时间" },
  ],
  polling: { interval: 10_000, when: (r) => r.some((x) => x.status === "Creating") },
  pageSize: 20,
});

// Wrap StatusBadge as a column render for the status column; surface the VPC
// name alongside the ID in the link column.
const vpcTableColumns = computed(() =>
  vpcColumns.value.map((c) =>
    c.key === "status"
      ? { ...c, render: (row: VpcRow) => h(StatusBadge, { status: String(row.status ?? "") }) }
      : c.key === "vpcName"
        ? { ...c, render: (row: VpcRow) => row.vpcName }
        : c,
  ),
);

// --- Secondary EIP table (EUEIP) -----------------------------------------
// Compact auxiliary list; real deploy: sdk.get<Eip[]>('/eueip/eips').
const {
  rows: eipRows,
  loading: eipLoading,
  columns: eipColumns,
} = useResourceTable<EipRow>({
  api: async () => {
    const res = await sdk.get<EipRow[]>("/console/resources");
    const items = (res.data ?? [])
      .filter((r) => r.ProductCode === "eueip")
      .map((r) => ({
        // The shared resource list carries no public IP field (prod: the eueip
        // detail API returns it). The old mapping put the SKU in the IP column
        // and the REGION in the bandwidth column — a dash is honest, a
        // mislabelled spec is not.
        eipId: r.ResourceId,
        ipAddress: "—",
        bandwidth: r.SpecCode ?? "—",
        bindInstance: "—",
        status: String(r.State ?? "").charAt(0).toUpperCase() + String(r.State ?? "").slice(1).toLowerCase(),
      }));
    return { items, total: items.length };
  },
  columns: [
    { key: "eipId", title: "弹性公网IP ID/名称", link: (r) => `#/eueip/eips/${r.eipId}` },
    { key: "ipAddress", title: "公网 IP" },
    { key: "status", title: "状态" },
    { key: "bandwidth", title: "规格" },
    { key: "bindInstance", title: "绑定实例" },
  ],
  pageSize: 20,
});

const eipTableColumns = computed(() =>
  eipColumns.value.map((c) =>
    c.key === "status"
      ? { ...c, render: (row: EipRow) => h(StatusBadge, { status: String(row.status ?? "") }) }
      : c,
  ),
);

// Suppress unused-warning for sdk/error wiring in the scaffold (real deploy
// wires onError → toast / page error bar, 02§7.5).
void sdk; void (null as unknown as EuError);
</script>

<template>
  <section class="vpc-app">
    <VpcDetail v-if="showDetail" :key="detailVpcId ?? ''" />
    <template v-else>
    <PageHeader title="专有网络 VPC">
      <template #actions>
        <ElButton type="primary">创建</ElButton>
      </template>
    </PageHeader>

    <ResourceTable
      :rows="vpcRows as Record<string, unknown>[]"
      :columns="vpcTableColumns"
      :loading="vpcLoading"
      :total="vpcTotal"
      :page="vpcPage"
      :page-size="vpcPageSize"
      @update:page="setVpcPage"
    />

    <EmptyGuide
      v-if="!vpcLoading && vpcRows.length === 0"
      title="暂无专有网络"
      description="创建您的第一个 VPC,规划云上私有网络与子网。"
      action-label="创建 VPC"
      action-href="#/euvpc/buy"
    />

    <header class="vpc-subheader">
      <h2>弹性公网 IP (EUEIP)</h2>
    </header>

    <ResourceTable
      :rows="eipRows as Record<string, unknown>[]"
      :columns="eipTableColumns"
      :loading="eipLoading"
    />

    <EmptyGuide
      v-if="!eipLoading && eipRows.length === 0"
      title="暂无弹性公网 IP"
      description="申请弹性公网 IP 并绑定到云资源,实现公网访问。"
      action-label="申请弹性公网 IP"
      action-href="#/eueip/buy"
    />
    </template>
  </section>
</template>

<style scoped>
.vpc-app { padding: 16px 24px; }
.vpc-subheader { margin: 24px 0 12px; }
.vpc-subheader h2 { font-size: var(--eu-font-size-lg); margin: 0; color: var(--eu-text-primary); }
</style>
