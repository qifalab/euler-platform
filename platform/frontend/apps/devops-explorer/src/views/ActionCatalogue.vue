<script setup lang="ts">
/** Action catalogue — the registered OpenAPI surface (M-5.1, 03§9.4 rule ①).
 *  A route is not mounted on the gateway until its metadata is registered in
 *  svc-api-meta; this view lists what is registered so the Explorer dropdown
 *  and docs reflect the real gate. */
import { ref, onMounted } from "vue";
import { createSDK } from "@sc/sdk";
import { PageHeader } from "@sc/ui";

const sdk = createSDK({ baseURL: "" });
const actions = ref<ActionMeta[]>([]);
const loading = ref(true);
const error = ref<string | null>(null);
const filter = ref("");

interface ActionMeta {
  id: string;
  product_code: string;
  action_name: string;
  version: string;
  param_schema: Record<string, unknown> | string;
  error_codes: string[];
}

async function load() {
  loading.value = true;
  error.value = null;
  try {
    const res = await sdk.get<ActionMeta[] | { actions: ActionMeta[] }>("/internal/actions");
    actions.value = Array.isArray(res.data) ? res.data : (res.data as { actions: ActionMeta[] }).actions ?? [];
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}
onMounted(load);

const filtered = () => {
  const f = filter.value.toLowerCase();
  if (!f) return actions.value;
  return actions.value.filter((a) => a.id.toLowerCase().includes(f) || a.product_code.toLowerCase().includes(f));
};
function schemaSummary(s: Record<string, unknown> | string): string {
  const obj = typeof s === "string" ? safeParse(s) : s;
  const req = (obj as { required?: string[] })?.required ?? [];
  const props = (obj as { properties?: Record<string, unknown> })?.properties ?? {};
  const keys = Object.keys(props);
  if (req.length) return "required: " + req.join(", ");
  if (keys.length) return "props: " + keys.join(", ");
  return "—";
}
function safeParse(s: string): Record<string, unknown> {
  try { return JSON.parse(s); } catch { return {}; }
}
</script>

<template>
  <div class="catalogue">
    <PageHeader title="API 目录" subtitle="已登记的 OpenAPI Action。未登记元数据的 Action 不允许在网关开通路由(03§9.4 规则①)。" />
    <div class="catalogue-toolbar">
      <input v-model="filter" placeholder="按产品或 Action 过滤…" />
      <button class="catalogue-refresh" @click="load">刷新</button>
    </div>
    <p v-if="error" class="catalogue-error">加载失败:{{ error }}(请确认 svc-api-meta 在 :9201)</p>
    <p v-if="loading" class="catalogue-loading">加载中…</p>
    <div v-else-if="filtered().length" class="catalogue-table-card">
      <table class="catalogue-table">
        <thead><tr><th>Action ID</th><th>产品</th><th>版本</th><th>参数 Schema</th><th>错误码</th></tr></thead>
        <tbody>
          <tr v-for="a in filtered()" :key="a.id">
            <td class="mono">{{ a.id }}</td>
            <td>{{ a.product_code }}</td>
            <td>{{ a.version }}</td>
            <td class="schema-cell">{{ schemaSummary(a.param_schema) }}</td>
            <td class="err-cell">{{ (a.error_codes || []).join(", ") || "—" }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-else class="catalogue-empty">暂无已登记 Action</p>
  </div>
</template>

<style scoped>
.catalogue { padding: var(--sc-spacing-6); max-width: 1200px; }
.catalogue-toolbar { display: flex; gap: var(--sc-spacing-3); margin-bottom: var(--sc-spacing-4); }
.catalogue-toolbar input { flex: 1; max-width: 360px; padding: 6px 10px; border: 1px solid var(--sc-border); border-radius: var(--sc-radius-sm); background: var(--sc-glass-bg); color: var(--sc-text-primary); font-size: 13px; }
.catalogue-refresh { border: 1px solid var(--sc-border); background: var(--sc-glass-bg); color: var(--sc-text-secondary); padding: 6px 14px; border-radius: var(--sc-radius-sm); cursor: pointer; font-size: 13px; transition: var(--sc-transition); }
.catalogue-refresh:hover { color: var(--sc-color-brand); border-color: var(--sc-color-brand); }
.catalogue-error, .catalogue-loading, .catalogue-empty { color: var(--sc-text-secondary); padding: var(--sc-spacing-6); }
.catalogue-error { color: var(--sc-color-danger); }
.catalogue-table-card { background: var(--sc-glass-bg-soft); border: 1px solid var(--sc-glass-border); border-radius: var(--sc-radius-lg); box-shadow: var(--sc-shadow-sm); overflow: hidden; }
.catalogue-table { width: 100%; border-collapse: collapse; background: transparent; }
.catalogue-table th, .catalogue-table td { padding: var(--sc-spacing-3) var(--sc-spacing-4); text-align: left; border-bottom: 1px solid var(--sc-border); font-size: 13px; color: var(--sc-text-primary); }
.catalogue-table th { background: var(--sc-glass-bg-soft); color: var(--sc-text-secondary); font-size: 12px; font-weight: 500; }
.catalogue-table tbody tr:hover { background: var(--sc-color-brand-soft); }
.catalogue-table tbody tr:last-child td { border-bottom: none; }
.mono { font-family: monospace; }
.schema-cell, .err-cell { font-size: 12px; color: var(--sc-text-secondary); }
</style>
