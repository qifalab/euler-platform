<script setup lang="ts">
/**
 * Instance list (02§7.2). The autoscaling sub-app's main view.
 * ResourceTable + StatusBadge + polling on transitional states. Filtered to
 * scas — the same /console/resources endpoint the BFF serves for every
 * product; the productCode distinguishes them.
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

const { rows, loading, columns, page, pageSize, total, setPage } = useResourceTable<Row>({
  api: async () => {
    const res = await sdk.get<Row[]>("/console/resources");
    const items = (res.data ?? [])
      .filter((r) => r.ProductCode === "scas")
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
    { key: "instanceId", title: "伸缩组 ID/名称" },
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
  <section class="scaling-app">
    <PageHeader title="弹性伸缩">
      <template #actions>
        <ElButton type="primary" @click="router.push('/buy')">创建伸缩组</ElButton>
      </template>
    </PageHeader>
    <ResourceTable :rows="rows" :columns="tableColumns" :loading="loading" :total="total" :page="page" :page-size="pageSize" @update:page="setPage" />
    <EmptyGuide v-if="!loading && rows.length === 0" title="暂无伸缩组" description="创建您的第一个弹性伸缩组,按策略自动扩缩容。" action-label="创建伸缩组" action-href="#/buy" />
  </section>
</template>

<style scoped>
.scaling-app { padding: 16px 24px; }
</style>
