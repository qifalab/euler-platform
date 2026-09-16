<script lang="ts">
/**
 * InstanceListConfig — the per-product configuration of the shared list page.
 *
 * Every product sub-app's /instances route is the same page over the same BFF
 * endpoint (/console/resources) with different copy, columns and billing
 * labels; this config carries exactly those differences and nothing else.
 */
export interface InstanceListConfig {
  /** Product code the resource list is filtered by (e.g. eukafka). */
  productCode: string;
  /** Page heading (e.g. 托管 Kafka). */
  title: string;
  /** Primary-action label, reused by the empty state (e.g. 创建实例). */
  createLabel: string;
  /** Header of the id/name column; default 实例 ID/名称. */
  idTitle?: string;
  /** Placement column — ZONAL products show 可用区 (zoneId), REGIONAL ones 地域. */
  placement?: { key: "region" | "zoneId"; title: string };
  /**
   * Billing column. `prepaid: true` maps the row's ChargeType
   * (PREPAID → 包年包月); without it the column is the postpaid-only constant
   * 按量. Omit to hide the column entirely.
   */
  charge?: { title?: string; prepaid?: boolean };
  /** Extra columns mapped from the raw BFF row (e.g. 到期时间 ← ExpiredAt). */
  extraColumns?: Array<{ key: string; title: string; from: string; fallback?: string }>;
  /** Empty-state copy. */
  empty: { title: string; description: string };
}
</script>

<script setup lang="ts">
/**
 * InstanceListView — the shared instance list page (02§7.2).
 *
 * ResourceTable + StatusBadge + polling on transitional states, filtered to the
 * configured productCode from the /console/resources endpoint the BFF serves
 * for every product. The eight product sub-apps used to carry byte-identical
 * copies of this page; they now pass their InstanceListConfig instead, so a fix
 * (stale-response handling, polling policy, the error state) lands once.
 */
import { computed, h } from "vue";
import { useRouter } from "vue-router";
import { ElButton } from "element-plus";
import { EmptyGuide, PageHeader, StatusBadge } from "@eu/ui";
import { createSDK } from "@eu/sdk";
import { subAppAuthOptions } from "@eu/wujie-bridge";
import ResourceTable from "./ResourceTable.vue";
import { useResourceTable, type Column } from "./useResourceTable";

const props = defineProps<{ config: InstanceListConfig }>();
const cfg = props.config;

type Row = Record<string, unknown>;
const router = useRouter();

// Auth-wired client (02§9.1): the sub-app reads the base's LIVE token through
// the Wujie bridge, so the list is loaded with the same credential every other
// call in the app uses.
const sdk = createSDK({ baseURL: "", ...subAppAuthOptions() });

function buildColumns(c: InstanceListConfig): Column<Row>[] {
  const cols: Column<Row>[] = [
    { key: "instanceId", title: c.idTitle ?? "实例 ID/名称" },
    { key: "status", title: "状态" },
    { key: "spec", title: "规格" },
  ];
  if (c.placement) cols.push({ key: c.placement.key, title: c.placement.title });
  if (c.charge) cols.push({ key: "chargeType", title: c.charge.title ?? "计费" });
  for (const extra of c.extraColumns ?? []) cols.push({ key: extra.key, title: extra.title });
  return cols;
}

const { rows, loading, error, columns, page, pageSize, total, setPage, refresh } = useResourceTable<Row>({
  api: async (_params, signal) => {
    const res = await sdk.get<Row[]>("/console/resources", { signal });
    const items = (res.data ?? [])
      .filter((r) => r.ProductCode === cfg.productCode)
      .map((r) => {
        const row: Row = {
          instanceId: r.ResourceId,
          instanceName: r.ResourceId,
          status: String(r.State ?? "").charAt(0).toUpperCase() + String(r.State ?? "").slice(1).toLowerCase(),
          spec: r.SpecCode,
        };
        if (cfg.placement) row[cfg.placement.key] = r.Region;
        if (cfg.charge) row.chargeType = cfg.charge.prepaid && r.ChargeType === "PREPAID" ? "包年包月" : "按量";
        for (const extra of cfg.extraColumns ?? []) row[extra.key] = (r[extra.from] as string) || extra.fallback || "—";
        return row;
      });
    return { items, total: items.length };
  },
  columns: buildColumns(cfg),
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
  <section class="eu-instance-view">
    <PageHeader :title="config.title">
      <template #actions>
        <ElButton type="primary" @click="router.push('/buy')">{{ config.createLabel }}</ElButton>
      </template>
    </PageHeader>
    <div v-if="error" class="eu-iv-error">
      <p class="eu-iv-error-msg">列表加载失败:{{ (error as Error)?.message ?? String(error) }}</p>
      <ElButton size="small" @click="refresh">重试</ElButton>
    </div>
    <template v-else>
      <ResourceTable :rows="rows" :columns="tableColumns" :loading="loading" :total="total" :page="page" :page-size="pageSize" @update:page="setPage" />
      <EmptyGuide v-if="!loading && rows.length === 0" :title="config.empty.title" :description="config.empty.description" :action-label="config.createLabel" action-href="#/buy" />
    </template>
  </section>
</template>

<style scoped>
.eu-instance-view { padding: 16px 24px; }
.eu-iv-error {
  padding: 32px; text-align: center;
  border: 1px solid var(--eu-border); border-radius: var(--eu-radius-lg);
}
.eu-iv-error-msg { color: var(--eu-color-danger); font-size: 13px; margin: 0 0 12px; }
</style>
