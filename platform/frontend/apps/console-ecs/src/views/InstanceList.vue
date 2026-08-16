<script setup lang="ts">
/**
 * Instance list (02§7.2). The compute sub-app's main view.
 * ResourceTable + StatusBadge + polling on transitional states.
 */
import { computed, h } from "vue";
import { useRouter } from "vue-router";
import { ElButton } from "element-plus";
import { ResourceTable, useResourceTable } from "@sc/console-kit";
import { StatusBadge, EmptyGuide, PageHeader } from "@sc/ui";
import { createSDK } from "@sc/sdk";
import "@sc/tokens/style.css";

type Row = Record<string, unknown>;
const router = useRouter();
const sdk = createSDK({ baseURL: "" });

// Real fetcher: console-bff /console/resources, filtered to scecs. The BFF
// returns the shared Resource shape (store.go); map to list columns.
const { rows, loading, columns, page, pageSize, total, setPage } = useResourceTable<Row>({
  api: async () => {
    const res = await sdk.get<Row[]>("/console/resources");
    const items = (res.data ?? [])
      .filter((r) => r.ProductCode === "scecs")
      .map((r) => ({
        instanceId: r.ResourceId,
        instanceName: r.ResourceId,
        status: String(r.State ?? "").charAt(0).toUpperCase() + String(r.State ?? "").slice(1).toLowerCase(),
        spec: r.SpecCode,
        zoneId: r.Region,
        expiredTime: (r.ExpiredAt as string) || "—",
      }));
    return { items, total: items.length };
  },
  columns: [
    { key: "instanceId", title: "实例 ID/名称" },
    { key: "status", title: "状态" },
    { key: "spec", title: "规格" },
    { key: "zoneId", title: "可用区" },
    { key: "expiredTime", title: "到期时间" },
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
  <section class="ecs-app">
    <PageHeader title="云服务器 ECS">
      <template #actions>
        <ElButton type="primary" @click="router.push('/buy')">创建实例</ElButton>
      </template>
    </PageHeader>
    <ResourceTable :rows="rows" :columns="tableColumns" :loading="loading" :total="total" :page="page" :page-size="pageSize" @update:page="setPage" />
    <EmptyGuide v-if="!loading && rows.length === 0" title="暂无云服务器" description="创建您的第一台云服务器。" action-label="创建实例" action-href="#/buy" />
  </section>
</template>

<style scoped>
.ecs-app { padding: 16px 24px; }
</style>
