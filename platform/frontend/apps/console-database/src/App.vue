<script setup lang="ts">
/**
 * console-database — RDS instance list (EURDS / MySQL), the database category
 * sub-app (02§7.2 list-page pattern). Demonstrates the declarative ResourceTable
 * from @eu/console-kit with unified StatusBadge, polling on transitional
 * states (Restoring), and EmptyGuide when empty.
 */
import { computed, h, onMounted, onUnmounted, ref } from "vue";
import { ElButton } from "element-plus";
import { ResourceTable, useResourceTable } from "@eu/console-kit";
import { StatusBadge, EmptyGuide, PageHeader } from "@eu/ui";
import { createSDK, type EuError } from "@eu/sdk";
import InstanceDetail from "./views/InstanceDetail.vue";
import "@eu/tokens/style.css";

// Internal hash routing (no vue-router instance in this sub-app, mirroring the
// console-network reference 02§3.4). The list links to
// #/eurds/instances/<id>; when the hash matches that shape the detail page
// takes over. Before this the link only changed the URL — InstanceDetail
// existed but was never rendered, so every instance link was a dead end.
const hashRoute = ref(location.hash);
function onHash() { hashRoute.value = location.hash; }
onMounted(() => window.addEventListener("hashchange", onHash));
onUnmounted(() => window.removeEventListener("hashchange", onHash));
const detailInstanceId = computed(() => {
  const m = hashRoute.value.match(/eurds\/instances\/([^/?#]+)/);
  return m ? decodeURIComponent(m[1]) : null;
});
const showDetail = computed(() => detailInstanceId.value !== null);

type InstanceRow = Record<string, unknown>;

// In the real build @eu/sdk is generated from OpenAPI; here a typed fetcher.
const sdk = createSDK({ baseURL: "" });

// Real fetcher: console-bff /console/resources filtered to eurds. The BFF
// returns the shared Resource shape; the list maps SpecCode to the spec
// column (engine-specific fields await the svc-rds list endpoint).
const { rows, loading, columns, page, pageSize, total, setPage } = useResourceTable<InstanceRow>({
  api: async () => {
    const res = await sdk.get<InstanceRow[]>("/console/resources");
    const items = (res.data ?? [])
      .filter((r) => r.ProductCode === "eurds")
      .map((r) => ({
        instanceId: r.ResourceId,
        instanceName: r.ResourceId,
        engine: r.SpecCode,
        spec: r.SpecCode,
        region: r.Region,
        status: String(r.State ?? "").charAt(0).toUpperCase() + String(r.State ?? "").slice(1).toLowerCase(),
        createdTime: (r.CreatedAt as string) || "—",
        expiredTime: (r.ExpiredAt as string) || "—",
      }));
    return { items, total: items.length };
  },
  columns: [
    { key: "instanceId", title: "资源ID/名称", link: (r) => `#/eurds/instances/${r.instanceId}` },
    { key: "status", title: "状态" },
    { key: "spec", title: "规格" },
    { key: "region", title: "地域" },
    { key: "createdTime", title: "创建时间" },
    { key: "expiredTime", title: "到期时间" },
    { key: "action", title: "操作" },
  ],
  polling: { interval: 10_000, when: (r) => r.some((x) => x.status === "Restoring") },
  pageSize: 20,
});

// Wrap StatusBadge as a column render for the status column; surface the
// instance name alongside the ID in the link column; render text actions
// for the 操作 column (the scaffold ResourceTable interpolates render text).
const tableColumns = computed(() =>
  columns.value.map((c) =>
    c.key === "status"
      ? { ...c, render: (row: InstanceRow) => h(StatusBadge, { status: String(row.status ?? "") }) }
      : c.key === "instanceName"
        ? { ...c, render: (row: InstanceRow) => row.instanceName }
        : c.key === "action"
          ? { ...c, render: () => "重启 · 详情 · 删除" }
          : c,
  ),
);

// Suppress unused-warning for sdk/error wiring in the scaffold (real deploy
// wires onError → toast / page error bar, 02§7.5).
void sdk; void (null as unknown as EuError);
</script>

<template>
  <section class="rds-app">
    <InstanceDetail v-if="showDetail" :key="detailInstanceId ?? ''" />
    <template v-else>
    <PageHeader title="云数据库 RDS">
      <template #actions>
        <ElButton type="primary">创建</ElButton>
      </template>
    </PageHeader>

    <ResourceTable
      :rows="rows as Record<string, unknown>[]"
      :columns="tableColumns"
      :loading="loading"
      :total="total"
      :page="page"
      :page-size="pageSize"
      @update:page="setPage"
    />

    <EmptyGuide
      v-if="!loading && rows.length === 0"
      title="暂无云数据库实例"
      description="创建您的第一个 RDS 实例,开始使用托管 MySQL 数据库。"
      action-label="创建实例"
      action-href="#/eurds/buy"
    />
    </template>
  </section>
</template>

<style scoped>
.rds-app { padding: 16px 24px; }
</style>
