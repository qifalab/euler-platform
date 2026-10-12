<script setup lang="ts">
/**
 * Rule list (监控规则) — svc-monitor /api/v1/monitor/rules (03§4.4.1).
 * Demonstrates the declarative ResourceTable pattern from @eu/console-kit with
 * unified StatusBadge, polling on transitional states, and empty-guide.
 *
 * Row actions go to svc-monitor directly: 编辑 → /rules/:id/edit (RuleForm),
 * 启用/禁用 → PUT /api/v1/monitor/rules/{id} {status, version}, 删除 →
 * DELETE /api/v1/monitor/rules/{id} (confirmed). Create → /rules/create.
 */
import { computed, h, ref } from "vue";
import { useRouter } from "vue-router";
import { ElButton, ElMessage, ElMessageBox } from "element-plus";
import { ResourceTable, useResourceTable, paginate } from "@eu/console-kit";
import { StatusBadge, EmptyGuide, PageHeader } from "@eu/ui";
import { sdk } from "../sdk";
import { RULE_STATUS_DISABLED, RULE_STATUS_ENABLED, comparisonSymbol, displayThreshold, ruleErrorMessage } from "../ruleForm";

const router = useRouter();

type RuleRow = Record<string, unknown>;

const { rows, loading, error, columns, page, pageSize, total, setPage, refresh } = useResourceTable<RuleRow>({
  api: async (params, signal) => {
    const res = await sdk.get<RuleRow[]>("/api/v1/monitor/rules", { signal });
    const items = (res.data ?? []).map((r) => ({
      ruleId: r.ruleId,
      ruleName: `${r.productCode} · ${r.metric}`,
      resourceScope: `${r.productCode}/${r.resourceType}`,
      metric: r.metric,
      threshold: `${comparisonSymbol(Number(r.comparisonOperator))} ${displayThreshold(String(r.threshold ?? ""))}`.trim(),
      duration: `${r.period}s × ${r.evalPeriods}`,
      contact: (r.notificationChannels as string[] | undefined)?.join("/") ?? "—",
      status: r.status === 1 ? "Enabled" : "Disabled",
      statusCode: r.status,
      version: r.version,
    }));
    // The endpoint returns every rule: page client-side, total = full count.
    return paginate(items, params);
  },
  columns: [
    { key: "ruleName", title: "规则名", link: (r) => `#/eumon/dashboard/${r.ruleId}` },
    { key: "resourceScope", title: "资源范围" },
    { key: "metric", title: "指标" },
    { key: "threshold", title: "阈值" },
    { key: "duration", title: "持续" },
    { key: "contact", title: "通知联系人" },
    { key: "status", title: "状态" },
    { key: "operation", title: "操作" },
  ],
  // No polling: a rule's status is Enabled/Disabled (r.status 1/0) and has no
  // transitional phase, so the previous "Evaluating" predicate could never fire.
  pageSize: 20,
});

// --- row actions ---
const acting = ref<number | null>(null);

function goCreate() {
  void router.push("/rules/create");
}

/** 启用 / 禁用: PUT the new status with the row version (optimistic lock). */
async function toggleRule(row: RuleRow) {
  const enabled = Number(row.statusCode) === RULE_STATUS_ENABLED;
  const id = Number(row.ruleId);
  acting.value = id;
  try {
    await sdk.put(`/api/v1/monitor/rules/${id}`, {
      status: enabled ? RULE_STATUS_DISABLED : RULE_STATUS_ENABLED,
      version: Number(row.version),
    });
    ElMessage.success(enabled ? "规则已禁用" : "规则已启用");
  } catch (e) {
    ElMessage.error(ruleErrorMessage(e));
  } finally {
    acting.value = null;
    void refresh();
  }
}

async function deleteRule(row: RuleRow) {
  try {
    await ElMessageBox.confirm(`删除后该规则将停止告警且不可恢复,确定删除「${String(row.ruleName)}」吗?`, "删除规则", {
      type: "warning", confirmButtonText: "确定删除", cancelButtonText: "取消",
    });
  } catch {
    return; // 用户取消
  }
  const id = Number(row.ruleId);
  acting.value = id;
  try {
    await sdk.del(`/api/v1/monitor/rules/${id}`);
    ElMessage.success("规则已删除");
  } catch (e) {
    ElMessage.error(ruleErrorMessage(e));
  } finally {
    acting.value = null;
    void refresh();
  }
}

function rowActions(row: RuleRow) {
  const busy = acting.value !== null;
  const enabled = Number(row.statusCode) === RULE_STATUS_ENABLED;
  const btn = (label: string, onClick: () => void, type: "primary" | "danger" = "primary") =>
    h(ElButton, { link: true, type, size: "small", disabled: busy, loading: acting.value === Number(row.ruleId) && label !== "编辑", onClick }, () => label);
  return h("span", { class: "mon-rules-ops" }, [
    btn("编辑", () => void router.push(`/rules/${String(row.ruleId)}/edit`)),
    btn(enabled ? "禁用" : "启用", () => void toggleRule(row)),
    btn("删除", () => void deleteRule(row), "danger"),
  ]);
}

// EmptyGuide renders its action as a plain <a href>; in this history-mode
// sub-app the click is routed through vue-router instead of reloading.
const createHref = computed(() => router.resolve("/rules/create").href);
function onEmptyAction(e: MouseEvent) {
  if ((e.target as HTMLElement | null)?.closest(".eu-empty-action")) {
    e.preventDefault();
    goCreate();
  }
}

// Wrap StatusBadge as a column render for the status column; the operation
// column renders the row action buttons (ResourceTable renders VNodes).
const tableColumns = computed(() =>
  columns.value.map((c) =>
    c.key === "status"
      ? { ...c, render: (row: RuleRow) => h(StatusBadge, { status: String(row.status ?? "") }) }
      : c.key === "operation"
        ? { ...c, render: rowActions }
        : c,
  ),
);
</script>

<template>
  <section class="mon-rules">
    <PageHeader title="监控规则">
      <template #actions>
        <ElButton type="primary" @click="goCreate">创建规则</ElButton>
      </template>
    </PageHeader>

    <!-- A failed load must not read as "暂无告警规则". -->
    <div v-if="error" class="mon-rules-error">
      <p class="mon-rules-error-msg">规则加载失败:{{ (error as Error)?.message ?? String(error) }}</p>
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
        <div @click.capture="onEmptyAction">
          <EmptyGuide title="暂无告警规则" description="创建您的第一条监控告警规则,及时掌握资源异常。" action-label="创建规则" :action-href="createHref" />
        </div>
      </template>
    </ResourceTable>
    </template>
  </section>
</template>

<style scoped>
.mon-rules { padding: 0; }
.mon-rules-ops { display: inline-flex; gap: 4px; }
.mon-rules-error {
  padding: 32px; text-align: center;
  border: 1px solid var(--eu-border); border-radius: var(--eu-radius-lg);
}
.mon-rules-error-msg { color: var(--eu-color-danger); font-size: 13px; margin: 0 0 12px; }
</style>
