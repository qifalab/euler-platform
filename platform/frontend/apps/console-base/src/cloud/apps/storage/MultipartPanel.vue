<script setup lang="ts">
import { onMounted, onBeforeUnmount, ref } from "vue";
import { useApp } from "../shared";
const props = defineProps<{ bucketId: string; prefix: string }>();
const emit = defineEmits<{ changed: [] }>();
const app = useApp();
type Part = { number: number; size: number; sha256: string };
type Upload = { id: string; key: string; size: number; state: string; partSize: number; expiresAt: string; parts: Part[] };
const sessions = ref<Upload[]>([]), error = ref(""), progress = ref(""), busy = ref(false), paused = ref(false), input = ref<HTMLInputElement | null>(null), resume = ref<Upload | null>(null);
let stopped = false;
onBeforeUnmount(() => { stopped = true; });
const base = () => `/buckets/${props.bucketId}/multipart`;
async function refresh() { sessions.value = (await app.request<{ items: Upload[] }>(base())).items; }
onMounted(() => refresh().catch(e => { error.value = e.message; }));
function choose(session: Upload | null) { resume.value = session; input.value?.click(); }
async function upload(event: Event) {
  const file = (event.target as HTMLInputElement).files?.[0];
  if (!file || busy.value) return;
  busy.value = true; paused.value = false; error.value = "";
  try {
    if (!file.size) throw new Error("空文件请使用普通上传。");
    let session = resume.value;
    if (session && session.size !== file.size) throw new Error("文件大小与上传会话不一致，请选择原文件。");
    if (!session) {
      const key = props.prefix + file.name;
      let expectedETag = "";
      try {
        const old = await app.request<{ etag: string }>(`/buckets/${props.bucketId}/objects/info?key=${encodeURIComponent(key)}`);
        if (!confirm(`“${key}”已存在，确认替换？启用版本管理后会保留旧版本。`)) return;
        expectedETag = old.etag;
      } catch (e) { if ((e as { status?: number }).status !== 404) throw e; }
      session = await app.request<Upload>(base(), { method: "POST", body: { key, size: file.size, contentType: file.type || "application/octet-stream", expectedETag } });
    } else {
      session = await app.request<Upload>(`${base()}/${session.id}`);
    }
    const total = Math.ceil(file.size / session.partSize);
    for (let n = 1; n <= total; n++) {
      if (stopped || paused.value) break;
      progress.value = `${file.name} · 校验分片 ${n}/${total}`;
      const blob = file.slice((n - 1) * session.partSize, n * session.partSize);
      const digest = await crypto.subtle.digest("SHA-256", await blob.arrayBuffer());
      const sha256 = Array.from(new Uint8Array(digest), v => v.toString(16).padStart(2, "0")).join("");
      const old = session.parts.find(p => p.number === n);
      if (old) {
        if (old.sha256 !== sha256) throw new Error(`原文件内容已改变（分片 ${n}）；请中止会话后重新上传。`);
        continue;
      }
      progress.value = `${file.name} · 上传分片 ${n}/${total}`;
      const form = new FormData(); form.append("sha256", sha256); form.append("file", blob, `${n}.part`);
      await app.upload(`${base()}/${session.id}/parts/${n}`, form, "PUT");
    }
    if (!stopped && !paused.value) {
      progress.value = `${file.name} · 合并并核对容量`;
      await app.request(`${base()}/${session.id}/complete`, { method: "POST", timeoutMs: 120000 });
      app.notify("文件已发布"); emit("changed");
    }
  } catch (e) { error.value = e instanceof Error ? e.message : "上传失败；可以选择原文件续传。"; }
  finally {
    busy.value = false; progress.value = "";
    if (input.value) input.value.value = "";
    if (!stopped) await refresh().catch(e => { error.value = e.message; });
  }
}
async function abort(session: Upload) {
  if (!confirm(`中止“${session.key}”并删除已上传分片？`)) return;
  busy.value = true; error.value = "";
  try { await app.request(`${base()}/${session.id}`, { method: "DELETE" }); await refresh(); emit("changed"); }
  catch (e) { error.value = e instanceof Error ? e.message : "中止失败"; }
  finally { busy.value = false; }
}
</script>
<template>
  <section class="multipart">
    <header><div><h3>分片上传与续传</h3><p>每片 8 MB；中断后选择原文件，逐片校验后继续。会话保留 24 小时。</p></div><button :disabled="busy" @click="choose(null)">选择文件分片上传</button><input ref="input" type="file" hidden @change="upload" /></header>
    <p v-if="error" role="alert" class="error">{{ error }}</p>
    <p v-if="progress" role="status">{{ progress }} <button :disabled="paused" @click="paused = true">{{ paused ? "当前分片结束后暂停" : "暂停" }}</button></p>
    <ul v-if="sessions.length"><li v-for="session in sessions" :key="session.id"><div><strong>{{ session.key }}</strong><small>{{ session.parts.length }} / {{ Math.ceil(session.size / session.partSize) }} 片 · 到期 {{ new Date(session.expiresAt).toLocaleString() }}</small></div><button :disabled="busy" @click="choose(session)">选择原文件续传</button><button :disabled="busy" @click="abort(session)">中止</button></li></ul>
    <p v-else class="muted">没有未完成的分片上传。</p>
  </section>
</template>
<style scoped>
.multipart{border:1px solid #dae3ef;border-radius:12px;padding:18px;margin:18px 0;background:#f7faff}header,li{display:flex;align-items:center;gap:12px;flex-wrap:wrap}header>div,li>div{flex:1}h3{margin:0 0 6px}p,small{color:#536278;font-size:13px}small{display:block;margin-top:5px}ul{padding:0;list-style:none}li{padding:12px 0;border-top:1px solid #dfe7f1}button{border:1px solid #cbd5e1;border-radius:7px;background:white;padding:8px 12px;cursor:pointer}button:disabled{opacity:.55;cursor:default}.error{color:#b42318}
</style>
