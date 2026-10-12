<script setup lang="ts">
/**
 * console-database — RDS instance list (EURDS / MySQL), the database category
 * sub-app (02§7.2 list-page pattern). Demonstrates the declarative ResourceTable
 * from @eu/console-kit with unified StatusBadge, polling on transitional
 * lifecycle states (CREATING/UPGRADING/RELEASING...), and EmptyGuide when empty.
 * Create flow: #/eurds/buy (RdsBuy).
 */
import { computed, h, onMounted, onUnmounted, ref, watch } from "vue";
import { ElButton } from "element-plus";
import { ResourceTable, useResourceTable, paginate, isTransitionalState, formatTime, attributesOf } from "@eu/console-kit";
import { StatusBadge, EmptyGuide, PageHeader } from "@eu/ui";
import { sdk } from "./sdk";
import InstanceDetail from "./views/InstanceDetail.vue";
import RdsBuy from "./views/RdsBuy.vue";
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
/** Create page: #/eurds/buy. */
const showBuy = computed(() => /^#\/eurds\/buy(?:[/?]|$)/.test(hashRoute.value));
function goBuy() {
  location.hash = "#/eurds/buy";
}

type InstanceRow = Record<string, unknown>;

// Real fetcher: the orchestrator's resource records filtered to eurds. They
// carry the customer attributes set at creation (name, engineVersion); the
// resource id / SKU remain the fallback.
const { rows, loading, error, columns, page, pageSize, total, setPage, refresh } = useResourceTable<InstanceRow>({
  api: async (params, signal) => {
    const res = await sdk.get<InstanceRow[]>("/api/v1/orchestrator/resources", { signal });
    const items = (res.data ?? [])
      .filter((r) => r.productCode === "eurds")
      .map((r) => {
        const attrs = attributesOf(r);
        return {
          instanceId: r.resourceId,
          instanceName: attrs.name || r.resourceId,
          engine: attrs.engineVersion ? `MySQL ${attrs.engineVersion}` : "—",
          spec: r.specCode,
          region: r.region,
          status: String(r.state ?? "").charAt(0).toUpperCase() + String(r.state ?? "").slice(1).toLowerCase(),
          createdTime: formatTime(r.createdAt as string | undefined),
          expiredTime: formatTime(r.expiredAt as string | undefined),
        };
      });
    // The endpoint returns every resource: page client-side, total = full count.
    return paginate(items, params);
  },
  columns: [
    { key: "instanceId", title: "资源ID/名称", link: (r) => `#/eurds/instances/${r.instanceId}` },
    { key: "status", title: "状态" },
    { key: "engine", title: "引擎" },
    { key: "spec", title: "规格" },
    { key: "region", title: "地域" },
    { key: "createdTime", title: "创建时间" },
    { key: "expiredTime", title: "到期时间" },
    { key: "action", title: "操作" },
  ],
  // The orchestrator never reports "Restoring": poll on the lifecycle states
  // that are actually transitional (CREATING, UPGRADING, RELEASING, ...).
  polling: { interval: 10_000, when: (r) => r.some((x) => isTransitionalState(x.status)) },
  pageSize: 20,
});

// Wrap StatusBadge as a column render for the status column; surface the
// instance name alongside the ID in the link column; render text actions
// for the 操作 column (the scaffold ResourceTable interpolates render text).
const tableColumns = computed(() =>
  columns.value.map((c) =>
    c.key === "status"
      ? { ...c, render: (row: InstanceRow) => h(StatusBadge, { status: String(row.status ?? "") }) }
      : c.key === "instanceId"
        ? { ...c, render: (row: InstanceRow) => (row.instanceName === row.instanceId ? String(row.instanceId) : `${String(row.instanceName)} (${String(row.instanceId)})`) }
        : c.key === "action"
          ? { ...c, render: () => "重启 · 详情 · 删除" }
          : c,
  ),
);

// Back from the create / detail page: reload so a new instance shows at once.
watch(() => showBuy.value || showDetail.value, (away, wasAway) => {
  if (wasAway && !away) void refresh();
});
</script>

<template>
  <section class="rds-app">
    <RdsBuy v-if="showBuy" />
    <InstanceDetail v-else-if="showDetail" :key="detailInstanceId ?? ''" />
    <template v-else>
    <PageHeader title="云数据库 RDS">
      <template #actions>
        <ElButton type="primary" @click="goBuy">创建</ElButton>
      </template>
    </PageHeader>

    <!-- A failed load must not read as "暂无云数据库实例". -->
    <div v-if="error" class="rds-error">
      <p class="rds-error-msg">列表加载失败:{{ (error as Error)?.message ?? String(error) }}</p>
      <ElButton size="small" @click="refresh">重试</ElButton>
    </div>
    <template v-else>
    <ResourceTable
      :rows="rows as Record<string, unknown>[]"
      :columns="tableColumns"
      :loading="loading"
      :total="total"
      :page="page"
      :page-size="pageSize"
      @update:page="setPage"
    >
      <template #empty>
        <EmptyGuide title="暂无云数据库实例" description="创建您的第一个 RDS 实例,开始使用托管 MySQL 数据库。" action-label="创建实例" action-href="#/eurds/buy" />
      </template>
    </ResourceTable>
    </template>
    </template>
  </section>
</template>

<style scoped>
.rds-app { padding: 16px 24px; }
.rds-error {
  padding: 32px; text-align: center;
  border: 1px solid var(--eu-border); border-radius: var(--eu-radius-lg);
}
.rds-error-msg { color: var(--eu-color-danger); font-size: 13px; margin: 0 0 12px; }
</style>
