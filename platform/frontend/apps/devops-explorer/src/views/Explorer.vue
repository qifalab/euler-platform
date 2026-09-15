<script setup lang="ts">
/** OpenAPI Explorer — 在线调试台 (M-5.1, 03§9.4 rule ⑤).
 *
 *  This view never computes a signature itself. It posts the assembled request
 *  to svc-api-meta /api/v1/apimeta/explorer, which signs with pkg-go/cps1 — the
 *  SAME implementation the SDK ships and the gateway verifies (07§4.1, D4).
 *  The returned signature material and optional upstream response are rendered
 *  verbatim. "SDK / 文档 / 网关三处算法不漂移" is enforced by construction: the
 *  browser cannot drift an algorithm it does not run.
 */
import { ref, computed, onMounted, watch } from "vue";
import { createSDK } from "@eu/sdk";
import { PageHeader, StatusBadge } from "@eu/ui";

const sdk = createSDK({ baseURL: "" });

// --- action catalogue (the dropdown of signable Actions) ---------------------
interface ActionMeta {
  id: string;
  product_code: string;
  action_name: string;
  version: string;
  param_schema: Record<string, unknown> | string;
  error_codes: string[];
}
const actions = ref<ActionMeta[]>([]);

// --- request form -----------------------------------------------------------
const productCode = ref("euecs");
const actionName = ref("RunInstances");
const method = ref("POST");
const path = ref("/");
const region = ref("cn-north-1");
// AK/SK must be supplied by the user each session — never hardcode defaults
// (even test material) in page source.
const ak = ref("");
const sk = ref("");
const securityToken = ref("");
const body = ref('{"ImageId":"img-001","InstanceType":"s2.large"}');
// query rows: editable key/value pairs
const queryRows = ref<{ k: string; v: string }[]>([
  { k: "Action", v: "RunInstances" },
  { k: "Version", v: "2026-08-01" },
]);
const headerRows = ref<{ k: string; v: string }[]>([]);

// --- result -----------------------------------------------------------------
interface SignatureMaterial {
  method: string; host: string; path: string;
  query: Record<string, string>; body: string;
  region: string; service: string; date: string;
  authorization: string; signed_headers: string; signature: string;
  headers: Record<string, string>;
}
interface UpstreamResult {
  status_code: number;
  headers: Record<string, string>;
  body: string;
  error?: string;
}
interface ExplorerData {
  signature: SignatureMaterial;
  executed: boolean;
  upstream?: UpstreamResult;
}
const result = ref<ExplorerData | null>(null);
const loading = ref(false);
const error = ref<string | null>(null);

const queryMap = computed(() => {
  const m: Record<string, string> = {};
  for (const r of queryRows.value) if (r.k) m[r.k] = r.v;
  return m;
});
const headerMap = computed(() => {
  const m: Record<string, string> = {};
  for (const r of headerRows.value) if (r.k) m[r.k] = r.v;
  return m;
});

async function loadActions() {
  try {
    const res = await sdk.get<{ actions: ActionMeta[] } | ActionMeta[]>("/internal/actions");
    // explorer actions route may return either an envelope-unwrapped array or
    // {actions:[...]}; tolerate both.
    const list = Array.isArray(res.data) ? res.data : (res.data as { actions: ActionMeta[] }).actions ?? [];
    actions.value = list;
  } catch (e) {
    // Catalogue is a convenience; signing works without it.
    console.warn("[explorer] action catalogue unavailable", e);
  }
}
onMounted(loadActions);

// Selecting an Action from the catalogue pre-fills product/action/query/body.
function applyAction(a: ActionMeta) {
  productCode.value = a.product_code;
  actionName.value = a.action_name;
  queryRows.value = [
    { k: "Action", v: a.action_name },
    { k: "Version", v: a.version },
  ];
  // Derive a plausible body from the schema if it has required properties.
  const schema = typeof a.param_schema === "string" ? safeParse(a.param_schema) : a.param_schema;
  const props = (schema as { properties?: Record<string, unknown> })?.properties ?? {};
  const required = (schema as { required?: string[] })?.required ?? [];
  if (method.value === "POST" && required.length) {
    const sample: Record<string, string> = {};
    for (const k of required) {
      const desc = (props[k] as { description?: string })?.description ?? k;
      sample[k] = hintFromDesc(desc, k);
    }
    body.value = JSON.stringify(sample, null, 2);
  } else {
    body.value = "";
  }
}

function hintFromDesc(desc: string, key: string): string {
  const d = desc.toLowerCase();
  if (d.includes("spec") || key.toLowerCase().includes("type")) return "s2.large";
  if (key.toLowerCase().includes("image")) return "img-001";
  if (d.includes("name")) return "demo-resource";
  if (d.includes("cidr")) return "192.168.0.0/16";
  return "value";
}

async function sign() {
  if (!ak.value.trim() || !sk.value.trim()) {
    error.value = "请先填写 AccessKey ID 与 SecretAccessKey（必填）";
    return;
  }
  loading.value = true;
  error.value = null;
  result.value = null;
  try {
    const res = await sdk.post<ExplorerData>("/api/v1/apimeta/explorer", {
      product_code: productCode.value,
      action_name: actionName.value,
      method: method.value,
      path: path.value,
      query: queryMap.value,
      headers: headerMap.value,
      body: body.value,
      ak: ak.value,
      sk: sk.value,
      security_token: securityToken.value || undefined,
      region: region.value,
    });
    result.value = res.data;
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}

// When product changes, keep the Action query param in sync if it matches.
watch([productCode, actionName], () => {
  const row = queryRows.value.find((r) => r.k === "Action");
  if (row) row.v = actionName.value;
});

function addRow(rows: { k: string; v: string }[]) { rows.push({ k: "", v: "" }); }
function removeRow(rows: { k: string; v: string }[], i: number) { rows.splice(i, 1); }

const upstreamKind = computed(() => {
  if (!result.value?.upstream) return "info";
  const s = result.value.upstream.status_code;
  if (s >= 200 && s < 300) return "success";
  if (s >= 400 && s < 500) return "warning";
  if (s >= 500) return "danger";
  return "info";
});
const upstreamLabel = computed(() => {
  const u = result.value?.upstream;
  if (!u) return "";
  if (u.error) return "ERROR";
  return String(u.status_code);
});

function safeParse(s: string): Record<string, unknown> {
  try { return JSON.parse(s); } catch { return {}; }
}
function prettyJson(obj: unknown): string {
  try { return JSON.stringify(obj, null, 2); } catch { return String(obj); }
}
</script>

<template>
  <div class="explorer">
    <PageHeader title="OpenAPI Explorer" subtitle="在线调试产品 API 调用。签名由 svc-api-meta 用与 SDK/网关同一份 cps1 实现计算,浏览器不重算,杜绝三处漂移。" />

    <section class="explorer-catalogue" v-if="actions.length">
      <h3>API 目录</h3>
      <div class="action-chips">
        <button v-for="a in actions" :key="a.id"
          class="action-chip"
          :class="{ active: a.product_code === productCode && a.action_name === actionName }"
          @click="applyAction(a)">
          {{ a.id }}
        </button>
      </div>
    </section>

    <section class="explorer-form">
      <h3>请求参数</h3>
      <div class="form-grid">
        <label>产品 <input v-model="productCode" placeholder="euecs" /></label>
        <label>Action <input v-model="actionName" placeholder="RunInstances" /></label>
        <label>Method
          <select v-model="method"><option>GET</option><option>POST</option><option>PUT</option><option>DELETE</option></select>
        </label>
        <label>Path <input v-model="path" placeholder="/" /></label>
        <label>Region <input v-model="region" placeholder="cn-north-1" /></label>
      </div>

      <div class="form-rows">
        <div class="rows-header">Query 参数</div>
        <div v-for="(row, i) in queryRows" :key="'q' + i" class="row">
          <input v-model="row.k" placeholder="key" />
          <input v-model="row.v" placeholder="value" />
          <button class="row-del" @click="removeRow(queryRows, i)">✕</button>
        </div>
        <button class="row-add" @click="addRow(queryRows)">+ 添加 query</button>
      </div>

      <div class="form-rows">
        <div class="rows-header">额外 Headers (可选)</div>
        <div v-for="(row, i) in headerRows" :key="'h' + i" class="row">
          <input v-model="row.k" placeholder="header" />
          <input v-model="row.v" placeholder="value" />
          <button class="row-del" @click="removeRow(headerRows, i)">✕</button>
        </div>
        <button class="row-add" @click="addRow(headerRows)">+ 添加 header</button>
      </div>

      <div class="form-rows">
        <div class="rows-header">Body</div>
        <textarea v-model="body" rows="5" placeholder="JSON body (POST/PUT)"></textarea>
      </div>

      <details class="form-creds">
        <summary>凭证 (调试会话 AK/SK)</summary>
        <div class="form-grid">
          <label>AK（必填）<input v-model="ak" placeholder="请输入 AccessKey ID（EU 开头）" required /></label>
          <label>SK（必填）<input v-model="sk" type="password" placeholder="请输入 SecretAccessKey" required /></label>
          <label>Security Token (STS, 可选) <input v-model="securityToken" /></label>
        </div>
        <p class="form-creds-note">仅用于本次调试签名。生产环境 Explorer 应以控制台登录态换取用户自有 AK/SK,而非明文输入。</p>
      </details>

      <button class="explorer-sign-btn" :disabled="loading" @click="sign">
        {{ loading ? "签名中…" : "签名并发送" }}
      </button>
      <p v-if="error" class="explorer-error">失败:{{ error }}</p>
    </section>

    <section v-if="result" class="explorer-result">
      <h3>签名结果 <StatusBadge :status="result.executed ? 'running' : 'stopped'" :label="result.executed ? '已发送' : '仅签名'" /></h3>

      <div class="result-block">
        <div class="result-label">Authorization</div>
        <pre class="code-block">{{ result.signature.authorization }}</pre>
      </div>
      <div class="result-meta">
        <div><span>Host</span>{{ result.signature.host }}</div>
        <div><span>Service</span>{{ result.signature.service }}</div>
        <div><span>Region</span>{{ result.signature.region }}</div>
        <div><span>Date (x-cps-date)</span>{{ result.signature.date }}</div>
        <div><span>SignedHeaders</span>{{ result.signature.signed_headers }}</div>
        <div><span>Signature</span><code>{{ result.signature.signature }}</code></div>
      </div>

      <div class="result-block">
        <div class="result-label">待发送 Headers</div>
        <pre class="code-block">{{ prettyJson(result.signature.headers) }}</pre>
      </div>

      <div v-if="result.upstream" class="result-block upstream">
        <div class="result-label">上游响应 <StatusBadge :status="upstreamLabel.toLowerCase()" :label="upstreamLabel" /></div>
        <pre v-if="result.upstream.error" class="code-block upstream-error">{{ result.upstream.error }}</pre>
        <template v-else>
          <pre class="code-block">HTTP {{ result.upstream.status_code }}</pre>
          <div class="result-label sub">响应 Headers</div>
          <pre class="code-block">{{ prettyJson(result.upstream.headers) }}</pre>
          <div class="result-label sub">响应 Body</div>
          <pre class="code-block">{{ result.upstream.body }}</pre>
        </template>
      </div>
      <p v-else-if="result" class="explorer-hint">未配置 EULER_EXPLORER_TARGET,仅返回签名材料(源码开发模式)。配置后 Explorer 会将此签名请求代理到真实产品 API 并回传响应。</p>
    </section>
  </div>
</template>

<style scoped>
.explorer { padding: var(--eu-spacing-6); max-width: 1100px; }
.explorer-catalogue, .explorer-form, .explorer-result {
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  padding: var(--eu-spacing-5);
  margin-bottom: var(--eu-spacing-4);
}
.explorer h3 { font-size: 15px; color: var(--eu-text-primary); margin: 0 0 var(--eu-spacing-3); display: flex; align-items: center; gap: var(--eu-spacing-2); }
.action-chips { display: flex; flex-wrap: wrap; gap: var(--eu-spacing-2); }
.action-chip { border: 1px solid var(--eu-border); background: var(--eu-glass-bg); color: var(--eu-text-secondary); padding: 4px 10px; border-radius: var(--eu-radius-sm); font-size: 12px; cursor: pointer; transition: var(--eu-transition); font-family: monospace; }
.action-chip:hover { border-color: var(--eu-color-brand); color: var(--eu-color-brand); }
.action-chip.active { background: var(--eu-color-brand); color: #fff; border-color: var(--eu-color-brand); }
.form-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: var(--eu-spacing-3); margin-bottom: var(--eu-spacing-4); }
.form-grid label { display: flex; flex-direction: column; gap: 4px; font-size: 12px; color: var(--eu-text-secondary); }
.form-grid input, .form-grid select { padding: 6px 10px; border: 1px solid var(--eu-border); border-radius: var(--eu-radius-sm); background: var(--eu-glass-bg); color: var(--eu-text-primary); font-size: 13px; }
.form-rows { margin-bottom: var(--eu-spacing-3); }
.rows-header { font-size: 12px; color: var(--eu-text-secondary); margin-bottom: 6px; }
.row { display: grid; grid-template-columns: 1fr 1fr auto; gap: 6px; margin-bottom: 6px; }
.row input { padding: 6px 10px; border: 1px solid var(--eu-border); border-radius: var(--eu-radius-sm); background: var(--eu-glass-bg); color: var(--eu-text-primary); font-size: 13px; }
.row-del, .row-add { border: 1px solid var(--eu-border); background: var(--eu-glass-bg); color: var(--eu-text-secondary); border-radius: var(--eu-radius-sm); cursor: pointer; padding: 4px 10px; font-size: 12px; transition: var(--eu-transition); }
.row-del:hover { color: var(--eu-color-danger); border-color: var(--eu-color-danger); }
.row-add:hover { color: var(--eu-color-brand); border-color: var(--eu-color-brand); }
textarea { width: 100%; padding: 8px 10px; border: 1px solid var(--eu-border); border-radius: var(--eu-radius-sm); background: var(--eu-glass-bg); color: var(--eu-text-primary); font-size: 13px; font-family: monospace; resize: vertical; box-sizing: border-box; }
.form-creds { margin-bottom: var(--eu-spacing-4); border-top: 1px solid var(--eu-border); padding-top: var(--eu-spacing-3); }
.form-creds summary { font-size: 13px; color: var(--eu-text-secondary); cursor: pointer; }
.form-creds-note { font-size: 11px; color: var(--eu-text-secondary); margin-top: 6px; }
.explorer-sign-btn { background: var(--eu-color-brand); color: #fff; border: none; padding: 9px 20px; border-radius: var(--eu-radius-md); font-size: 14px; font-weight: 500; cursor: pointer; transition: var(--eu-transition); }
.explorer-sign-btn:hover:not(:disabled) { background: var(--eu-color-brand-hover); }
.explorer-sign-btn:disabled { opacity: 0.6; cursor: not-allowed; }
.explorer-error { color: var(--eu-color-danger); font-size: 13px; margin-top: var(--eu-spacing-2); }
.explorer-hint { color: var(--eu-text-secondary); font-size: 12px; margin-top: var(--eu-spacing-3); }
.result-block { margin-bottom: var(--eu-spacing-4); }
.result-label { font-size: 12px; color: var(--eu-text-secondary); margin-bottom: 4px; }
.result-label.sub { margin-top: var(--eu-spacing-3); }
.code-block { background: var(--eu-bg-container); border: 1px solid var(--eu-border); border-radius: var(--eu-radius-sm); padding: var(--eu-spacing-3); font-size: 12px; font-family: monospace; color: var(--eu-text-primary); white-space: pre-wrap; word-break: break-all; overflow-x: auto; }
.upstream-error { color: var(--eu-color-danger); }
.result-meta { display: grid; grid-template-columns: repeat(auto-fit, minmax(280px, 1fr)); gap: var(--eu-spacing-2); margin-bottom: var(--eu-spacing-4); }
.result-meta div { font-size: 12px; color: var(--eu-text-primary); display: flex; flex-direction: column; gap: 2px; }
.result-meta span { color: var(--eu-text-secondary); font-size: 11px; }
.result-meta code { font-family: monospace; font-size: 11px; word-break: break-all; }
</style>
