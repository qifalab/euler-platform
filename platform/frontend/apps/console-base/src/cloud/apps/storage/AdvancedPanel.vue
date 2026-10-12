<script setup lang="ts">
import { ref, reactive } from "vue";
import { useApp } from "../shared";
import MultipartPanel from "./MultipartPanel.vue";
const props = defineProps<{ bucketId: string; prefix: string }>();
const emit = defineEmits<{ changed: [] }>();
const app = useApp();
type Version = { key: string; versionId: string; size: number; modifiedAt: string; isLatest: boolean; deleteMarker: boolean };
type Rule = { id: string; prefix: string; enabled: boolean; expirationDays: number; noncurrentDays: number };
const open = ref(false), tab = ref("versions"), error = ref(""), busy = ref(false), versions = ref<Version[]>([]), next = ref(""), filter = ref(""), versioning = ref("Disabled"), rules = ref<Rule[]>([]);
const transfer = reactive({ source: "", target: "", move: false });
const base = () => `/buckets/${props.bucketId}`;
const statusLabel: Record<string, string> = { Enabled: "已启用", Suspended: "已暂停", Disabled: "未启用" };
async function run(fn: () => Promise<void>) { if (busy.value) return; busy.value = true; error.value = ""; try { await fn(); } catch (e) { error.value = e instanceof Error ? e.message : "操作失败"; } finally { busy.value = false; } }
async function loadVersions(more = false) {
  const v = await app.request<{ items: Version[]; next: string }>(`${base()}/versions?prefix=${encodeURIComponent(filter.value)}&cursor=${encodeURIComponent(more ? next.value : "")}`);
  versions.value = more ? versions.value.concat(v.items) : v.items; next.value = v.next;
}
async function load() {
  versioning.value = (await app.request<{ status: string }>(`${base()}/versioning`)).status;
  await loadVersions();
  if (app.can("manage")) rules.value = (await app.request<{ items: Rule[] }>(`${base()}/lifecycle`)).items;
}
function toggle() { open.value = !open.value; if (open.value) void run(load); }
async function restore(v: Version) {
  if (!confirm(`将“${v.key}”恢复为此版本？已有当前版本会被替换；启用版本管理时会保留历史。`)) return;
  await run(async () => {
    let expectedETag = "";
    try { expectedETag = (await app.request<{ etag: string }>(`${base()}/objects/info?key=${encodeURIComponent(v.key)}`)).etag; }
    catch (e) { if ((e as { status?: number }).status !== 404) throw e; }
    await app.request(`${base()}/versions/restore`, { method: "POST", body: { key: v.key, versionId: v.versionId, expectedETag } });
    await loadVersions(); emit("changed"); app.notify("历史版本已恢复");
  });
}
async function remove(v: Version) {
  if (!confirm(`永久删除“${v.key}”的版本 ${v.versionId}？此操作无法恢复。删除当前版本或删除标记，可能重新显示更早的版本。`)) return;
  await run(async () => { await app.request(`${base()}/versions?key=${encodeURIComponent(v.key)}&versionId=${encodeURIComponent(v.versionId)}`, { method: "DELETE" }); await loadVersions(); emit("changed"); });
}
async function saveVersioning(status: string) {
  if (!confirm(status === "Enabled" ? "启用版本管理？历史版本将占用项目容量。" : "暂停产生新版本？已有历史版本继续保留并占用容量。")) return;
  await run(async () => { versioning.value = (await app.request<{ status: string }>(`${base()}/versioning`, { method: "PUT", body: { status } })).status; app.notify("版本策略已保存"); });
}
function addRule() { rules.value.push({ id: crypto.randomUUID(), prefix: "", enabled: true, expirationDays: 0, noncurrentDays: 30 }); }
async function saveRules() {
  if (!confirm("保存自动清理规则？到期文件和历史版本将由 S3 服务按其调度执行删除，请确认保留期限。")) return;
  await run(async () => { rules.value = (await app.request<{ items: Rule[] }>(`${base()}/lifecycle`, { method: "PUT", body: { items: rules.value } })).items; app.notify("生命周期规则已保存"); });
}
async function transferFile() { await run(async () => { await app.request(`${base()}/objects/transfer`, { method: "POST", body: transfer, timeoutMs: 120000 }); app.notify(transfer.move ? "文件已移动" : "文件已复制"); emit("changed"); await loadVersions(); }); }
</script>
<template>
  <MultipartPanel v-if="app.can('write')" :key="bucketId" :bucket-id="bucketId" :prefix="prefix" @changed="emit('changed')" />
  <section class="advanced"><button class="toggle" :aria-expanded="open" @click="toggle">{{ open ? "收起" : "展开" }}版本、生命周期与文件迁移</button>
    <div v-if="open"><nav><button @click="tab = 'versions'">历史版本与回收</button><button v-if="app.can('manage')" @click="tab = 'policy'">版本与生命周期</button><button v-if="app.can('write')" @click="tab = 'transfer'">复制 / 移动</button></nav>
      <p v-if="error" role="alert" class="error">{{ error }}</p>
      <div v-if="tab === 'versions'"><p>版本管理{{ statusLabel[versioning] }}。列表包含删除标记，可查找已删除文件。历史版本也占用存储配额。</p><form @submit.prevent="run(() => loadVersions())"><input v-model="filter" placeholder="按文件路径前缀查找" aria-label="版本路径前缀" /><button :disabled="busy">查询</button></form>
        <div class="table"><table><thead><tr><th>文件</th><th>版本</th><th>状态 / 大小</th><th>时间</th><th>操作</th></tr></thead><tbody><tr v-for="v in versions" :key="v.key + v.versionId"><td>{{ v.key }}</td><td><code>{{ v.versionId }}</code></td><td>{{ v.deleteMarker ? "删除标记" : (v.size / 1024).toFixed(1) + " KB" }} {{ v.isLatest ? "· 当前" : "" }}</td><td>{{ new Date(v.modifiedAt).toLocaleString() }}</td><td><button v-if="!v.deleteMarker && app.can('write')" :disabled="busy" @click="restore(v)">恢复</button><button v-if="app.can('manage')" :disabled="busy" @click="remove(v)">永久删除版本</button></td></tr><tr v-if="!versions.length"><td colspan="5">没有匹配版本。</td></tr></tbody></table></div><button v-if="next" :disabled="busy" @click="run(() => loadVersions(true))">加载更多</button></div>
      <div v-if="tab === 'policy' && app.can('manage')"><h3>版本管理 · {{ statusLabel[versioning] }}</h3><p>暂停后不会删除历史版本。建议启用版本管理后，再配置历史版本保留期限。</p><button :disabled="busy || versioning === 'Enabled'" @click="saveVersioning('Enabled')">启用版本</button><button :disabled="busy || versioning !== 'Enabled'" @click="saveVersioning('Suspended')">暂停版本</button>
        <h3>生命周期规则</h3><p>路径前缀留空代表全部文件。天数 0 表示不执行该项。规则由底层 S3 服务执行，生效时间取决于服务调度。</p>
        <fieldset v-for="(rule, i) in rules" :key="rule.id"><legend>{{ rule.id }}</legend><label><input v-model="rule.enabled" type="checkbox" />启用</label><label>文件路径前缀<input v-model="rule.prefix" placeholder="例如 logs/" /></label><label>当前文件过期天数<input v-model.number="rule.expirationDays" type="number" min="0" max="36500" /></label><label>旧版本永久删除天数<input v-model.number="rule.noncurrentDays" type="number" min="0" max="36500" /></label><button :disabled="busy" @click="rules.splice(i, 1)">移除此规则</button></fieldset>
        <button :disabled="busy || rules.length >= 20" @click="addRule">添加规则</button><button :disabled="busy" @click="saveRules">保存生命周期</button>
      </div>
      <form v-if="tab === 'transfer' && app.can('write')" @submit.prevent="transferFile"><h3>桶内复制 / 移动</h3><p>目标文件必须不存在。移动会先复制，确认成功后删除源文件；启用版本管理时，源文件历史仍保留。</p><label>源文件路径<input v-model="transfer.source" required placeholder="reports/source.pdf" /></label><label>目标文件路径<input v-model="transfer.target" required placeholder="archive/source.pdf" /></label><label><input v-model="transfer.move" type="checkbox" />移动文件（复制成功后删除源文件）</label><button :disabled="busy">{{ transfer.move ? "移动文件" : "复制文件" }}</button></form>
    </div>
  </section>
</template>
<style scoped>
.advanced{border:1px solid #dae3ef;border-radius:12px;margin:18px 0;padding:16px}.toggle{font-weight:650;color:#2855a6}nav{display:flex;gap:8px;flex-wrap:wrap;margin:16px 0}p{font-size:13px;color:#536278;line-height:1.65}button{background:#fff;border:1px solid #cbd5e1;border-radius:7px;padding:8px 12px;cursor:pointer;margin:3px}button:disabled{opacity:.55;cursor:default}input{border:1px solid #cbd5e1;padding:8px;border-radius:6px}label{display:flex;align-items:center;gap:10px;margin:10px 0;flex-wrap:wrap}fieldset{border:1px solid #dfe7f1;border-radius:8px;margin:12px 0;padding:12px}.table{overflow:auto}table{border-collapse:collapse;width:100%;font-size:13px}th,td{text-align:left;padding:12px;border-bottom:1px solid #e2e8f0;vertical-align:top}code{font-size:11px;overflow-wrap:anywhere}.error{color:#b42318}
</style>
