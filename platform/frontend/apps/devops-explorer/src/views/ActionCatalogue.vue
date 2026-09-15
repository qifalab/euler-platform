<script setup lang="ts">
/** Action catalogue — the registered OpenAPI surface (M-5.1, 03§9.4 rule ①).
 *  A route is not mounted on the gateway until its metadata is registered in
 *  svc-api-meta; this view lists what is registered so the Explorer dropdown
 *  and docs reflect the real gate. */
import { ref, onMounted } from "vue";
import { createSDK } from "@eu/sdk";
import { PageHeader } from "@eu/ui";

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
.catalogue { padding: var(--eu-spacing-6); max-width: 1200px; }
.catalogue-toolbar { display: flex; gap: var(--eu-spacing-3); margin-bottom: var(--eu-spacing-4); }
.catalogue-toolbar input { flex: 1; max-width: 360px; padding: 6px 10px; border: 1px solid var(--eu-border); border-radius: var(--eu-radius-sm); background: var(--eu-glass-bg); color: var(--eu-text-primary); font-size: 13px; }
.catalogue-refresh { border: 1px solid var(--eu-border); background: var(--eu-glass-bg); color: var(--eu-text-secondary); padding: 6px 14px; border-radius: var(--eu-radius-sm); cursor: pointer; font-size: 13px; transition: var(--eu-transition); }
.catalogue-refresh:hover { color: var(--eu-color-brand); border-color: var(--eu-color-brand); }
.catalogue-error, .catalogue-loading, .catalogue-empty { color: var(--eu-text-secondary); padding: var(--eu-spacing-6); }
.catalogue-error { color: var(--eu-color-danger); }
.catalogue-table-card { background: var(--eu-glass-bg-soft); border: 1px solid var(--eu-glass-border); border-radius: var(--eu-radius-lg); box-shadow: var(--eu-shadow-sm); overflow: hidden; }
.catalogue-table { width: 100%; border-collapse: collapse; background: transparent; }
.catalogue-table th, .catalogue-table td { padding: var(--eu-spacing-3) var(--eu-spacing-4); text-align: left; border-bottom: 1px solid var(--eu-border); font-size: 13px; color: var(--eu-text-primary); }
.catalogue-table th { background: var(--eu-glass-bg-soft); color: var(--eu-text-secondary); font-size: 12px; font-weight: 500; }
.catalogue-table tbody tr:hover { background: var(--eu-color-brand-soft); }
.catalogue-table tbody tr:last-child td { border-bottom: none; }
.mono { font-family: monospace; }
.schema-cell, .err-cell { font-size: 12px; color: var(--eu-text-secondary); }
</style>
