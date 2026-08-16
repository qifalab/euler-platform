<script setup lang="ts">
/**
 * console-storage — OSS Bucket list (SCOSS), the storage category sub-app (02§7.2).
 * Demonstrates the declarative ResourceTable pattern from @sc/console-kit with
 * unified StatusBadge, polling on transitional states, and empty-guide.
 */
import { computed, h } from "vue";
import { ElButton } from "element-plus";
import { ResourceTable, useResourceTable } from "@sc/console-kit";
import { StatusBadge, EmptyGuide, PageHeader } from "@sc/ui";
import { createSDK, type ScError } from "@sc/sdk";
import "@sc/tokens/style.css";

type BucketRow = Record<string, unknown>;

// In the real build @sc/sdk is generated from OpenAPI; here a typed fetcher.
const sdk = createSDK({ baseURL: "" });

// Real fetcher: console-bff /console/resources, filtered to scoss. The BFF
// returns the shared Resource shape (store.go); map to list columns. Fields
// not surfaced by the resource list (objectCount/size/access) fall back to a
// dash so the existing columns/template keep rendering.
const { rows, loading, columns, page, pageSize, total, setPage } = useResourceTable<BucketRow>({
  api: async () => {
    const res = await sdk.get<BucketRow[]>("/console/resources");
    const items = (res.data ?? [])
      .filter((r) => r.ProductCode === "scoss")
      .map((r) => ({
        bucketName: r.ResourceId,
        status: String(r.State ?? "").charAt(0).toUpperCase() + String(r.State ?? "").slice(1).toLowerCase(),
        storageClass: r.SpecCode,
        region: r.Region,
        objectCount: "—",
        size: "—",
        access: "—",
        createTime: r.CreatedAt ?? "—",
      }));
    return { items, total: items.length };
  },
  columns: [
    { key: "bucketName", title: "存储桶名称", link: (r) => `#/scoss/buckets/${r.bucketName}` },
    { key: "status", title: "状态" },
    { key: "storageClass", title: "存储类型" },
    { key: "region", title: "地域" },
    { key: "objectCount", title: "对象数量" },
    { key: "size", title: "存储用量" },
    { key: "access", title: "读写权限" },
    { key: "createTime", title: "创建时间" },
  ],
  polling: { interval: 10_000, when: (r) => r.some((x) => x.status === "Creating") },
  pageSize: 20,
});

// Wrap StatusBadge as a column render for the status column.
const tableColumns = computed(() =>
  columns.value.map((c) =>
    c.key === "status"
      ? { ...c, render: (row: BucketRow) => h(StatusBadge, { status: String(row.status ?? "") }) }
      : c,
  ),
);

// Suppress unused-warning for sdk/error wiring in the scaffold (real deploy
// wires onError → toast / page error bar, 02§7.5).
void sdk; void (null as unknown as ScError);
</script>

<template>
  <section class="oss-app">
    <PageHeader title="对象存储 OSS">
      <template #actions>
        <ElButton type="primary">创建存储桶</ElButton>
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
      title="暂无存储桶"
      description="创建您的第一个存储桶,开始使用对象存储服务。"
      action-label="创建存储桶"
      action-href="#/scoss/create"
    />
  </section>
</template>

<style scoped>
.oss-app { padding: 16px 24px; }
</style>
