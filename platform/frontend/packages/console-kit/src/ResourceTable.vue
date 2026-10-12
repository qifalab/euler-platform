<script setup lang="ts">
/**
 * ResourceTable — the declarative list-page component (02§7.2).
 * Renders the columns/filters/polling model from useResourceTable. Uses
 * Element Plus el-table under the hood (externals, not bundled).
 *
 * Links: in a history-mode sub-app (vue-router installed) an app path such as
 * "/instances/x" is navigated through the router — a plain <a href> click made
 * the browser reload the whole sandbox iframe. The href stays real (resolved by
 * the router, base included) so middle-click / open-in-new-tab still work.
 * Hash links of the hash-routed sub-apps stay plain anchors.
 *
 * Cells: a column's `render` may return text or a VNode (e.g. the action
 * buttons of an 操作 column); a VNode is rendered as-is, anything else is
 * interpolated as text.
 *
 * Empty state: exactly one. A page passes its EmptyGuide (with the next step)
 * in the `empty` slot and it replaces the table; without one the table shows a
 * generic "暂无资源" in its body (el-table's own "No Data" is never shown).
 */
import { inject, isVNode, type VNode } from "vue";
import { routerKey } from "vue-router";
import { ElTable, ElTableColumn, ElPagination, ElEmpty } from "element-plus";
import { StatusBadge } from "@eu/ui";
import type { Column } from "./useResourceTable";
import { routesInApp } from "./navigation";

const props = defineProps<{
  rows: Record<string, unknown>[];
  columns: Column[];
  loading?: boolean;
  total?: number;
  page?: number;
  pageSize?: number;
}>();
const emit = defineEmits<{
  "update:page": [p: number];
  "refresh": [];
}>();
defineSlots<{ empty?: () => unknown }>();

// null in the hash-routed sub-apps, which install no router.
const router = inject(routerKey, null);

/** The cell content: the column's render output, else the raw value. */
function cellOf(col: Column, row: Record<string, unknown>): unknown {
  return col.render ? col.render(row) : row[col.key];
}

function linkOf(col: Column, row: Record<string, unknown>): string {
  return col.link ? col.link(row) : "";
}

function linkHref(to: string): string {
  return router && to.startsWith("/") && !to.startsWith("//") ? router.resolve(to).href : to;
}

function onLinkClick(e: MouseEvent, to: string) {
  if (!router || !routesInApp(to, e)) return;
  e.preventDefault();
  void router.push(to);
}
</script>

<template>
  <div class="eu-resource-table">
    <ElTable v-if="loading || rows.length > 0 || !$slots.empty" :data="rows" v-loading="loading" stripe style="width: 100%">
      <ElTableColumn
        v-for="col in columns"
        :key="col.key"
        :prop="col.key"
        :label="col.title"
      >
        <template #default="{ row }">
          <a
            v-if="col.link"
            :href="linkHref(linkOf(col, row as Record<string, unknown>))"
            class="eu-rt-link"
            @click="onLinkClick($event, linkOf(col, row as Record<string, unknown>))"
          >
            {{ col.render ? col.render(row as Record<string, unknown>) : row[col.key] }}
          </a>
          <span v-else-if="col.key === 'status'">
            <StatusBadge :status="String(row[col.key] ?? '')" />
          </span>
          <template v-else>
            <component :is="(cellOf(col, row) as VNode)" v-if="col.render && isVNode(cellOf(col, row))" />
            <span v-else>{{ cellOf(col, row) }}</span>
          </template>
        </template>
      </ElTableColumn>
      <template #empty>
        <ElEmpty v-if="!loading" description="暂无资源" :image-size="80" />
      </template>
    </ElTable>
    <slot v-else name="empty" />
    <div v-if="(total ?? 0) > (pageSize ?? 20)" class="eu-rt-pagination">
      <ElPagination
        :current-page="page ?? 1"
        :page-size="pageSize ?? 20"
        :total="total ?? 0"
        layout="prev, pager, next"
        @update:current-page="(p: number) => emit('update:page', p)"
      />
    </div>
  </div>
</template>

<style>
/* Soft-glass container; the el-table inside stays readable (tokens only). */
.eu-resource-table {
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  overflow: hidden;
}
/* Let the glass show through; hover highlight uses the brand-soft token. */
.eu-resource-table .el-table {
  --el-table-bg-color: transparent;
  --el-table-tr-bg-color: transparent;
  --el-table-row-hover-bg-color: var(--eu-color-brand-soft);
}
.eu-resource-table .el-table th.el-table__cell { background-color: var(--eu-glass-bg-soft); }
/* el-table wraps its empty slot in a 50%-wide, 60px-line-height span. */
.eu-resource-table .el-table__empty-text { width: auto; line-height: normal; }
.eu-rt-link { color: var(--eu-color-brand); text-decoration: none; }
.eu-rt-link:hover { color: var(--eu-color-brand-hover); }
.eu-rt-pagination { display: flex; justify-content: flex-end; padding: 12px; }
</style>
