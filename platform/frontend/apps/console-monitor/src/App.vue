<script setup lang="ts">
/**
 * console-monitor — alert rule list (SCMON), the monitor category sub-app (02§7.2).
 * Demonstrates the declarative ResourceTable pattern from @sc/console-kit with
 * unified StatusBadge, polling on transitional states, and empty-guide. The
 * monitor list is an alert-rule list (监控规则), not a resource list.
 */
import { computed, h } from "vue";
import { ElButton } from "element-plus";
import { ResourceTable, useResourceTable } from "@sc/console-kit";
import { StatusBadge, EmptyGuide, PageHeader } from "@sc/ui";
import { createSDK, type ScError } from "@sc/sdk";
import "@sc/tokens/style.css";

type RuleRow = Record<string, unknown>;

// In the real build @sc/sdk is generated from OpenAPI; here a typed fetcher.
const sdk = createSDK({ baseURL: "" });

// Real fetcher: svc-monitor /api/v1/monitor/rules (03§4.4.1). Returns the
// account's alert rules from the monitor service; map to list columns.
const { rows, loading, columns, page, pageSize, total, setPage } = useResourceTable<RuleRow>({
  api: async () => {
    const res = await sdk.get<RuleRow[]>("/api/v1/monitor/rules");
    const items = (res.data ?? []).map((r) => ({
      ruleId: r.ruleId,
      ruleName: `${r.productCode} · ${r.metric}`,
      resourceScope: `${r.productCode}/${r.resourceType}`,
      metric: r.metric,
      threshold: r.threshold,
      duration: `${r.period}s × ${r.evalPeriods}`,
      contact: (r.notificationChannels as string[] | undefined)?.join("/") ?? "—",
      status: r.status === 1 ? "Enabled" : "Disabled",
    }));
    return { items, total: items.length };
  },
  columns: [
    { key: "ruleName", title: "规则名", link: (r) => `#/scmon/dashboard/${r.ruleId}` },
    { key: "resourceScope", title: "资源范围" },
    { key: "metric", title: "指标" },
    { key: "threshold", title: "阈值" },
    { key: "duration", title: "持续" },
    { key: "contact", title: "通知联系人" },
    { key: "status", title: "状态" },
    { key: "operation", title: "操作" },
  ],
  // Polling is wired per the list-page pattern (02§7.2); it only fires while a
  // transitional state (评估中) exists. The demo rules are all 启用/禁用, so
  // polling stays dormant — it activates once a rule enters an evaluating state.
  polling: { interval: 10_000, when: (r) => r.some((x) => x.status === "Evaluating") },
  pageSize: 20,
});

// Wrap StatusBadge as a column render for the status column; render the action
// set for the operation column (real deploy wires ElButton/dropdown, 02§7.5).
const tableColumns = computed(() =>
  columns.value.map((c) =>
    c.key === "status"
      ? { ...c, render: (row: RuleRow) => h(StatusBadge, { status: String(row.status ?? "") }) }
      : c.key === "operation"
        ? { ...c, render: (row: RuleRow) => (String(row.status) === "Active" ? "编辑 · 禁用" : "编辑 · 启用") }
        : c,
  ),
);

// Suppress unused-warning for sdk/error wiring in the scaffold (real deploy
// wires onError → toast / page error bar, 02§7.5).
void sdk; void (null as unknown as ScError);
</script>

<template>
  <section class="mon-app">
    <PageHeader title="云监控">
      <template #actions>
        <ElButton type="primary">创建规则</ElButton>
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
      title="暂无告警规则"
      description="创建您的第一条监控告警规则,及时掌握资源异常。"
      action-label="创建规则"
      action-href="#/scmon/rules/create"
    />
  </section>
</template>

<style scoped>
.mon-app { padding: 16px 24px; }
</style>
