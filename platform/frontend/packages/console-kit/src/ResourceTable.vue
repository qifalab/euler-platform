<script setup lang="ts">
/**
 * ResourceTable — the declarative list-page component (02§7.2).
 * Renders the columns/filters/polling model from useResourceTable. Uses
 * Element Plus el-table under the hood (externals, not bundled).
 */
import { ElTable, ElTableColumn, ElPagination, ElEmpty } from "element-plus";
import { StatusBadge } from "@sc/ui";
import type { Column } from "./useResourceTable";

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
</script>

<template>
  <div class="sc-resource-table">
    <ElTable :data="rows" v-loading="loading" stripe style="width: 100%">
      <ElTableColumn
        v-for="col in columns"
        :key="col.key"
        :prop="col.key"
        :label="col.title"
      >
        <template #default="{ row }">
          <a v-if="col.link" :href="col.link(row as Record<string, unknown>)" class="sc-rt-link">
            {{ col.render ? col.render(row as Record<string, unknown>) : row[col.key] }}
          </a>
          <span v-else-if="col.key === 'status'">
            <StatusBadge :status="String(row[col.key] ?? '')" />
          </span>
          <span v-else>{{ col.render ? col.render(row as Record<string, unknown>) : row[col.key] }}</span>
        </template>
      </ElTableColumn>
    </ElTable>
    <ElEmpty v-if="!loading && rows.length === 0" description="暂无资源" />
    <div v-if="(total ?? 0) > (pageSize ?? 20)" class="sc-rt-pagination">
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
.sc-resource-table {
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  overflow: hidden;
}
/* Let the glass show through; hover highlight uses the brand-soft token. */
.sc-resource-table .el-table {
  --el-table-bg-color: transparent;
  --el-table-tr-bg-color: transparent;
  --el-table-row-hover-bg-color: var(--sc-color-brand-soft);
}
.sc-resource-table .el-table th.el-table__cell { background-color: var(--sc-glass-bg-soft); }
.sc-rt-link { color: var(--sc-color-brand); text-decoration: none; }
.sc-rt-link:hover { color: var(--sc-color-brand-hover); }
.sc-rt-pagination { display: flex; justify-content: flex-end; padding: 12px; }
</style>
