<script setup lang="ts">
/**
 * Instance list (02§7.2). The log-service sub-app's main view.
 * ResourceTable + StatusBadge + polling on transitional states. Filtered to
 * eulog — the same /console/resources endpoint the BFF serves for every
 * product; the productCode distinguishes them.
 */
import { computed, h } from "vue";
import { useRouter } from "vue-router";
import { ElButton } from "element-plus";
import { ResourceTable, useResourceTable } from "@eu/console-kit";
import { StatusBadge, EmptyGuide, PageHeader } from "@eu/ui";
import { createSDK } from "@eu/sdk";
import "@eu/tokens/style.css";

type Row = Record<string, unknown>;
const router = useRouter();
const sdk = createSDK({ baseURL: "" });

const { rows, loading, columns, page, pageSize, total, setPage } = useResourceTable<Row>({
  api: async () => {
    const res = await sdk.get<Row[]>("/console/resources");
    const items = (res.data ?? [])
      .filter((r) => r.ProductCode === "eulog")
      .map((r) => ({
        instanceId: r.ResourceId,
        instanceName: r.ResourceId,
        status: String(r.State ?? "").charAt(0).toUpperCase() + String(r.State ?? "").slice(1).toLowerCase(),
        spec: r.SpecCode,
        region: r.Region,
        chargeType: "按量",
      }));
    return { items, total: items.length };
  },
  columns: [
    { key: "instanceId", title: "实例 ID/名称" },
    { key: "status", title: "状态" },
    { key: "spec", title: "规格" },
    { key: "region", title: "地域" },
    { key: "chargeType", title: "计费" },
  ],
  polling: { interval: 10_000, when: (r) => r.some((x) => x.status === "Creating") },
  pageSize: 20,
});

const tableColumns = computed(() =>
  columns.value.map((c) => {
    if (c.key === "status") return { ...c, render: (row: Row) => h(StatusBadge, { status: String(row.status ?? "") }) };
    if (c.key === "instanceId") return { ...c, link: (row: Row) => `/instances/${row.instanceId}`, render: (row: Row) => `${row.instanceName} (${row.instanceId})` };
    return c;
  }),
);
</script>

<template>
  <section class="log-app">
    <PageHeader title="日志服务">
      <template #actions>
        <ElButton type="primary" @click="router.push('/buy')">创建日志服务</ElButton>
      </template>
    </PageHeader>
    <ResourceTable :rows="rows" :columns="tableColumns" :loading="loading" :total="total" :page="page" :page-size="pageSize" @update:page="setPage" />
    <EmptyGuide v-if="!loading && rows.length === 0" title="暂无日志服务实例" description="创建您的第一个日志服务,Vector 采集 + ClickHouse 存储,保留期自动清理。" action-label="创建日志服务" action-href="#/buy" />
  </section>
</template>

<style scoped>
.log-app { padding: 16px 24px; }
</style>
