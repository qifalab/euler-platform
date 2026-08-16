<script setup lang="ts">
/**
 * console-database — RDS instance list (SCRDS / MySQL), the database category
 * sub-app (02§7.2 list-page pattern). Demonstrates the declarative ResourceTable
 * from @sc/console-kit with unified StatusBadge, polling on transitional
 * states (Restoring), and EmptyGuide when empty.
 */
import { computed, h } from "vue";
import { ElButton } from "element-plus";
import { ResourceTable, useResourceTable } from "@sc/console-kit";
import { StatusBadge, EmptyGuide, PageHeader } from "@sc/ui";
import { createSDK, type ScError } from "@sc/sdk";
import "@sc/tokens/style.css";

type InstanceRow = Record<string, unknown>;

// In the real build @sc/sdk is generated from OpenAPI; here a typed fetcher.
const sdk = createSDK({ baseURL: "" });

// Real fetcher: console-bff /console/resources filtered to scrds. The BFF
// returns the shared Resource shape; the list maps SpecCode to the spec
// column (engine-specific fields await the svc-rds list endpoint).
const { rows, loading, columns, page, pageSize, total, setPage } = useResourceTable<InstanceRow>({
  api: async () => {
    const res = await sdk.get<InstanceRow[]>("/console/resources");
    const items = (res.data ?? [])
      .filter((r) => r.ProductCode === "scrds")
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
    { key: "instanceId", title: "资源ID/名称", link: (r) => `#/scrds/instances/${r.instanceId}` },
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
void sdk; void (null as unknown as ScError);
</script>

<template>
  <section class="rds-app">
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
      action-href="#/scrds/buy"
    />
  </section>
</template>

<style scoped>
.rds-app { padding: 16px 24px; }
</style>
