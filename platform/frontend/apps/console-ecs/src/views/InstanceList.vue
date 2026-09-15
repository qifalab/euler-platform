<script setup lang="ts">
/**
 * Instance list (02§7.2). The compute sub-app's main view.
 * ResourceTable + StatusBadge + polling on transitional states.
 */
import { computed, h } from "vue";
import { useRouter } from "vue-router";
import { ElButton } from "element-plus";
import { ResourceTable, useResourceTable } from "@eu/console-kit";
import { StatusBadge, EmptyGuide, PageHeader } from "@eu/ui";
import { sdk } from "../sdk";
import "@eu/tokens/style.css";

type Row = Record<string, unknown>;
const router = useRouter();

// Real fetcher: console-bff /console/resources, filtered to euecs. The BFF
// returns the shared Resource shape (store.go); map to list columns.
const { rows, loading, error, columns, page, pageSize, total, setPage, refresh } = useResourceTable<Row>({
  api: async (_params, signal) => {
    const res = await sdk.get<Row[]>("/console/resources", { signal });
    const items = (res.data ?? [])
      .filter((r) => r.ProductCode === "euecs")
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
    <div v-if="error" class="ecs-error">
      <p class="ecs-error-msg">列表加载失败:{{ (error as Error)?.message ?? String(error) }}</p>
      <ElButton size="small" @click="refresh">重试</ElButton>
    </div>
    <template v-else>
      <ResourceTable :rows="rows" :columns="tableColumns" :loading="loading" :total="total" :page="page" :page-size="pageSize" @update:page="setPage" />
      <EmptyGuide v-if="!loading && rows.length === 0" title="暂无云服务器" description="创建您的第一台云服务器。" action-label="创建实例" action-href="#/buy" />
    </template>
  </section>
</template>

<style scoped>
.ecs-app { padding: 16px 24px; }
.ecs-error {
  padding: 32px; text-align: center;
  border: 1px solid var(--eu-border); border-radius: var(--eu-radius-lg);
}
.ecs-error-msg { color: var(--eu-color-danger); font-size: 13px; margin: 0 0 12px; }
</style>
