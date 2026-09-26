<script setup lang="ts">
import { onMounted, onBeforeUnmount, ref, reactive } from "vue";
import { useApp } from "../shared";
import PackagesPanel from "../database/PackagesPanel.vue";
const app = useApp();
type Bucket = {
  id: string;
  name: string;
  displayName: string;
  description: string;
  access: string;
  status: string;
  usedBytes: number;
  objectCount: number;
  origins: string[];
};
type FileObject = {
  key: string;
  size: number;
  contentType: string;
  etag: string;
  modifiedAt: string;
  folder: boolean;
};
type Key = {
  id: string;
  name: string;
  accessKey: string;
  secretKey?: string;
  scopes: string[];
  active: boolean;
  expiresAt: string;
  lastUsedAt: string;
};
type Signature = {
  url: string;
  method: string;
  fields?: Record<string, string>;
};
type Overview = {
  items: Bucket[];
  quota: { storageBytes: number; buckets: number };
  usedBytes: number;
  objectCount: number;
  bucketCount: number;
  reservedBytes: number;
  configured: boolean;
  corsMode: "bucket" | "external";
};
type Log = {
  id: string;
  action: string;
  key: string;
  state: string;
  sizeChange: number;
  createdAt: string;
};
const tab = ref("buckets"),
  overview = ref<Overview | null>(null),
  bucket = ref<Bucket | null>(null),
  objects = ref<FileObject[]>([]),
  prefix = ref(""),
  after = ref(""),
  next = ref(""),
  history = ref<string[]>([]),
  hasMore = ref(false),
  selected = ref<string[]>([]),
  keys = ref<Key[]>([]),
  logs = ref<Log[]>([]),
  logOffset = ref(0),
  error = ref(""),
  loading = ref(false),
  busy = ref(false),
  detail = ref<FileObject | null>(null),
  keyOnce = ref<Key | null>(null),
  endpoint = ref("");
const bucketEditor = ref(false),
  bucketEditId = ref(""),
  corsEditor = ref(false),
  corsOrigins = ref(""),
  folderEditor = ref(false),
  folderName = ref(""),
  keyEditor = ref(false),
  uploadProgress = ref(""),
  fileInput = ref<HTMLInputElement | null>(null);
const bucketForm = reactive({ name: "", description: "", access: "private" }),
  keyForm = reactive({ name: "", scopes: ["read"], expiresAt: "" });
const accessLabels: Record<string, string> = {
  private: "私有",
  "public-read": "公开读",
  "public-read-write": "公开读写",
};
const statusLabels: Record<string, string> = {
  active: "可用",
  provisioning: "创建中",
  error: "需要核验",
  pending: "进行中",
  completed: "已完成",
  uncertain: "待核验",
};
const bytes = (n: number) =>
  n >= 1024 ** 3
    ? `${(n / 1024 ** 3).toFixed(2)} GB`
    : n >= 1024 ** 2
      ? `${(n / 1024 ** 2).toFixed(2)} MB`
      : `${n.toLocaleString()} B`;
const date = (s: string) => (s ? new Date(s).toLocaleString() : "—");
const activeRequests = new Set<XMLHttpRequest>();
let filesEpoch = 0;
onBeforeUnmount(() => {
  activeRequests.forEach((r) => r.abort());
  keyOnce.value = null;
});
async function load() {
  loading.value = true;
  error.value = "";
  try {
    overview.value = await app.request<Overview>("/");
    if (bucket.value) {
      bucket.value =
        overview.value.items.find((b) => b.id === bucket.value?.id) ?? null;
      if (bucket.value) await loadFiles();
    }
    if (app.can("manage")) {
      const value = await app.request<{ items: Key[]; endpoint: string }>(
        "/keys",
      );
      keys.value = value.items;
      endpoint.value = value.endpoint;
    }
    await loadLogs();
  } catch (e) {
    error.value = e instanceof Error ? e.message : "无法读取存储服务";
  } finally {
    loading.value = false;
  }
}
async function loadLogs() {
  logs.value = (
    await app.request<{ items: Log[] }>(
      `/stats/logs?limit=50&offset=${logOffset.value}`,
    )
  ).items;
}
async function loadFiles() {
  if (!bucket.value) return;
  const target = bucket.value.id,
    path = prefix.value,
    cursor = after.value,
    epoch = ++filesEpoch;
  const value = await app.request<{
    items: FileObject[];
    hasMore: boolean;
    next: string;
  }>(
    `/buckets/${target}/objects?prefix=${encodeURIComponent(path)}&after=${encodeURIComponent(cursor)}`,
  );
  if (
    epoch !== filesEpoch ||
    bucket.value?.id !== target ||
    prefix.value !== path ||
    after.value !== cursor
  )
    return;
  objects.value = value.items;
  hasMore.value = value.hasMore;
  next.value = value.next;
  selected.value = [];
}
async function act(
  fn: () => Promise<unknown>,
  message?: string,
  reload = true,
) {
  if (busy.value) return;
  busy.value = true;
  error.value = "";
  try {
    await fn();
    if (message) app.notify(message);
    if (reload) await load();
  } catch (e) {
    error.value = e instanceof Error ? e.message : "操作失败";
  } finally {
    busy.value = false;
  }
}
function openBucket(b: Bucket) {
  if (busy.value) return;
  objects.value = [];
  bucket.value = b;
  prefix.value = "";
  after.value = "";
  history.value = [];
  void act(loadFiles, undefined, false);
}
function navigate(path: string) {
  if (busy.value) return;
  objects.value = [];
  prefix.value = path;
  after.value = "";
  history.value = [];
  void act(loadFiles, undefined, false);
}
function page(forward: boolean) {
  if (busy.value) return;
  if (forward) {
    history.value.push(after.value);
    after.value = next.value;
  } else {
    after.value = history.value.pop() ?? "";
  }
  void act(loadFiles, undefined, false);
}
function editBucket(b?: Bucket) {
  bucketEditId.value = b?.id ?? "";
  Object.assign(bucketForm, {
    name: b?.displayName ?? "",
    description: b?.description ?? "",
    access: b?.access ?? "private",
  });
  bucketEditor.value = true;
}
async function saveBucket() {
  await act(async () => {
    await app.request(
      bucketEditId.value ? `/buckets/${bucketEditId.value}` : "/buckets",
      {
        method: bucketEditId.value ? "PUT" : "POST",
        body: bucketForm,
        timeoutMs: 120000,
      },
    );
    bucketEditor.value = false;
  }, "存储桶已保存");
}
async function removeBucket(b: Bucket) {
  const force = b.objectCount > 0;
  if (
    !confirm(
      `删除存储桶“${b.displayName}”${force ? "并永久删除其中全部文件" : ""}？`,
    )
  )
    return;
  await act(async () => {
    await app.request(`/buckets/${b.id}?force=${force}`, {
      method: "DELETE",
      timeoutMs: 120000,
    });
    if (bucket.value?.id === b.id) bucket.value = null;
  }, "存储桶已删除");
}
async function removeFile(o: FileObject) {
  if (
    !bucket.value ||
    !confirm(
      `删除“${o.key}”？${o.folder ? "此操作删除目录标记，文件需要单独选择删除。" : ""}`,
    )
  )
    return;
  await act(
    () =>
      app.request(
        `/buckets/${bucket.value!.id}/objects?key=${encodeURIComponent(o.key)}`,
        { method: "DELETE" },
      ),
    "对象已删除",
  );
}
async function removeBatch() {
  if (!bucket.value || !confirm(`删除选中的 ${selected.value.length} 个对象？`))
    return;
  await act(async () => {
    const value = await app.request<{ deleted: string[]; failed: string[] }>(
      `/buckets/${bucket.value!.id}/objects/delete-batch`,
      { method: "POST", body: { keys: selected.value } },
    );
    if (value.failed.length)
      throw new Error(
        `已删除 ${value.deleted.length} 个，${value.failed.length} 个未完成，请刷新后核验。`,
      );
  }, "选中对象已删除");
}
async function signed(o: FileObject, copyLink = false) {
  if (!bucket.value) return;
  await act(
    async () => {
      const value = await app.request<Signature>(
        `/buckets/${bucket.value!.id}/objects/presigned-download`,
        { method: "POST", body: { key: o.key, expires: 3600 } },
      );
      if (copyLink) {
        await copy(value.url);
      } else {
        const a = document.createElement("a");
        a.href = value.url;
        a.rel = "noopener noreferrer";
        a.target = "_blank";
        a.click();
      }
    },
    undefined,
    false,
  );
}
async function copyPublic(o: FileObject) {
  if (!bucket.value) return;
  await act(
    async () => {
      const value = await app.request<{ url: string }>(
        `/buckets/${bucket.value!.id}/objects/public-url?key=${encodeURIComponent(o.key)}`,
      );
      await copy(value.url);
    },
    undefined,
    false,
  );
}
async function info(o: FileObject) {
  if (!bucket.value) return;
  await act(
    async () => {
      detail.value = await app.request<FileObject>(
        `/buckets/${bucket.value!.id}/objects/info?key=${encodeURIComponent(o.key)}`,
      );
    },
    undefined,
    false,
  );
}
async function copy(s: string) {
  await navigator.clipboard.writeText(s);
  app.notify("已复制");
}
async function upload(event: Event) {
  const files = Array.from((event.target as HTMLInputElement).files ?? []);
  if (!files.length || !bucket.value || busy.value) return;
  const targetBucketID = bucket.value.id,
    targetPrefix = prefix.value;
  await act(async () => {
    for (const file of files) {
      uploadProgress.value = `${file.name} · 准备上传`;
      const reserved = await app.request<{
        uploadId: string;
        signature: Signature;
      }>(`/buckets/${targetBucketID}/objects/presigned-upload`, {
        method: "POST",
        body: {
          key: targetPrefix + file.name,
          size: file.size,
          contentType: file.type || "application/octet-stream",
        },
      });
      await new Promise<void>((resolve, reject) => {
        const xhr = new XMLHttpRequest();
        activeRequests.add(xhr);
        const form = new FormData();
        for (const [key, value] of Object.entries(
          reserved.signature.fields ?? {},
        ))
          form.append(key, value);
        form.append("file", file);
        xhr.open(reserved.signature.method, reserved.signature.url);
        xhr.timeout = 15 * 60 * 1000;
        xhr.upload.onprogress = (e) => {
          if (e.lengthComputable)
            uploadProgress.value = `${file.name} · ${Math.round((e.loaded / e.total) * 100)}%`;
        };
        xhr.onload = () => {
          activeRequests.delete(xhr);
          xhr.status >= 200 && xhr.status < 300
            ? resolve()
            : reject(new Error("文件直传失败，请检查 S3 CORS 和上传额度"));
        };
        xhr.onerror = xhr.ontimeout = () => {
          activeRequests.delete(xhr);
          reject(new Error("文件直传网络异常"));
        };
        xhr.onabort = () => {
          activeRequests.delete(xhr);
          reject(new Error("上传已取消"));
        };
        xhr.send(form);
      });
      uploadProgress.value = `${file.name} · 核对并发布`;
      await app.request(`/buckets/${targetBucketID}/objects/confirm-upload`, {
        method: "POST",
        body: { uploadId: reserved.uploadId },
        timeoutMs: 120000,
      });
    }
    uploadProgress.value = "";
  }, "文件上传完成");
  uploadProgress.value = "";
  if (fileInput.value) fileInput.value.value = "";
}
async function createKey() {
  await act(async () => {
    keyOnce.value = await app.request<Key>("/keys", {
      method: "POST",
      body: {
        ...keyForm,
        expiresAt: keyForm.expiresAt
          ? new Date(keyForm.expiresAt).toISOString()
          : "",
      },
    });
    keyEditor.value = false;
    keyForm.name = "";
  }, "密钥已创建，请保存一次性 Secret Key");
}
onMounted(load);
</script>
<template>
  <div class="storage-app">
    <header class="hero">
      <div>
        <span class="eyebrow">EULER OBJECT STORAGE</span>
        <h1>对象存储</h1>
        <p>项目文件、访问策略、程序化密钥与容量配额。</p>
      </div>
      <div class="actions">
        <button @click="load" :disabled="loading || busy">刷新</button
        ><button
          v-if="app.can('write')"
          class="primary"
          :disabled="!overview?.configured"
          @click="editBucket()"
        >
          创建存储桶
        </button>
      </div>
    </header>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <p v-if="overview && !overview.configured" class="notice">
      尚未配置 S3/MinIO 数据面，配置完成后可创建真实存储资源。
    </p>
    <section v-if="overview" class="metrics">
      <article>
        <span>已用容量</span><strong>{{ bytes(overview.usedBytes) }}</strong
        ><small>总配额 {{ bytes(overview.quota.storageBytes) }}</small
        ><progress
          :value="overview.usedBytes"
          :max="Math.max(1, overview.quota.storageBytes)"
        />
      </article>
      <article>
        <span>存储桶</span
        ><strong
          >{{ overview.bucketCount }} / {{ overview.quota.buckets }}</strong
        ><small>当前项目独立配额</small>
      </article>
      <article>
        <span>对象</span
        ><strong>{{ overview.objectCount.toLocaleString() }}</strong
        ><small>上传预留 {{ bytes(overview.reservedBytes) }}</small>
      </article>
    </section>
    <nav aria-label="对象存储功能">
      <button :class="{ current: tab === 'buckets' }" @click="tab = 'buckets'">
        存储桶与文件</button
      ><button
        :class="{ current: tab === 'packages' }"
        @click="tab = 'packages'"
      >
        配额与资源包</button
      ><button
        v-if="app.can('manage')"
        :class="{ current: tab === 'keys' }"
        @click="tab = 'keys'"
      >
        API 密钥</button
      ><button :class="{ current: tab === 'logs' }" @click="tab = 'logs'">
        使用日志
      </button>
    </nav>
    <section v-if="tab === 'buckets' && !bucket" class="panel">
      <header>
        <h2>存储桶</h2>
        <span class="muted">{{ overview?.items.length ?? 0 }} 个</span>
      </header>
      <div class="table">
        <table>
          <thead>
            <tr>
              <th>名称</th>
              <th>访问策略</th>
              <th>容量 / 对象</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="b in overview?.items" :key="b.id">
              <td>
                <button
                  class="text-link"
                  :disabled="b.status !== 'active'"
                  @click="openBucket(b)"
                >
                  {{ b.displayName }}</button
                ><small>{{ b.description }}</small
                ><small
                  ><code>{{ b.name }}</code></small
                >
              </td>
              <td>{{ accessLabels[b.access] }}</td>
              <td>
                {{ bytes(b.usedBytes)
                }}<small>{{ b.objectCount }} 个对象</small>
              </td>
              <td>{{ statusLabels[b.status] || b.status }}</td>
              <td class="actions">
                <button
                  :disabled="b.status !== 'active'"
                  @click="openBucket(b)"
                >
                  浏览文件</button
                ><button v-if="app.can('manage')" @click="editBucket(b)">
                  设置</button
                ><button
                  v-if="app.can('manage')"
                  class="danger"
                  :disabled="busy"
                  @click="removeBucket(b)"
                >
                  删除
                </button>
              </td>
            </tr>
            <tr v-if="!overview?.items.length">
              <td colspan="5" class="empty">
                还没有存储桶，创建后即可上传项目文件。
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>
    <section v-if="tab === 'buckets' && bucket" class="panel">
      <header>
        <div>
          <button
            class="text-link"
            :disabled="busy"
            @click="
              bucket = null;
              objects = [];
              filesEpoch++;
            "
          >
            ← 所有存储桶
          </button>
          <h2>{{ bucket.displayName }}</h2>
          <p class="muted">
            {{ accessLabels[bucket.access] }} · {{ bytes(bucket.usedBytes) }} ·
            {{ bucket.objectCount }} 个对象
          </p>
        </div>
        <div class="actions">
          <button
            v-if="app.can('write')"
            :disabled="busy"
            @click="
              act(
                () =>
                  app.request(`/buckets/${bucket!.id}/sync`, {
                    method: 'POST',
                  }),
                '统计已同步',
              )
            "
          >
            同步统计</button
          ><button
            v-if="app.can('manage') && overview?.corsMode !== 'external'"
            @click="
              corsOrigins = bucket.origins.join('\n');
              corsEditor = true;
            "
          >
            CORS</button
          ><button v-if="app.can('write')" @click="folderEditor = true">
            新建目录</button
          ><button
            v-if="app.can('write')"
            class="primary"
            :disabled="busy"
            @click="fileInput?.click()"
          >
            上传文件</button
          ><input
            ref="fileInput"
            type="file"
            multiple
            hidden
            @change="upload"
          />
        </div>
      </header>
      <p v-if="overview?.corsMode === 'external'" class="muted">
        跨域来源由数据面部署者统一管理，此服务不提供按桶 CORS 修改。
      </p>
      <p v-if="bucket.access === 'public-read-write'" class="notice">
        此桶允许公开读取和写入。公开写入使用 Euler
        文件入口，仍受当前项目配额限制。
      </p>
      <div class="breadcrumb">
        <button :disabled="busy" @click="navigate('')">根目录</button
        ><template
          v-for="(part, index) in prefix.split('/').filter(Boolean)"
          :key="index"
          ><span>/</span
          ><button
            :disabled="busy"
            @click="
              navigate(
                prefix
                  .split('/')
                  .slice(0, index + 1)
                  .join('/') + '/',
              )
            "
          >
            {{ part }}
          </button></template
        >
      </div>
      <p v-if="uploadProgress" role="status">{{ uploadProgress }}</p>
      <div v-if="selected.length && app.can('write')" class="actions">
        <span>已选择 {{ selected.length }} 个</span
        ><button class="danger" :disabled="busy" @click="removeBatch">
          删除所选</button
        ><button @click="selected = []">取消选择</button>
      </div>
      <div class="table">
        <table>
          <thead>
            <tr>
              <th v-if="app.can('write')">选择</th>
              <th>名称</th>
              <th>大小</th>
              <th>修改时间</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="o in objects" :key="o.key">
              <td v-if="app.can('write')">
                <input
                  v-model="selected"
                  type="checkbox"
                  :value="o.key"
                  :aria-label="`选择 ${o.key}`"
                />
              </td>
              <td>
                <button
                  v-if="o.folder"
                  class="text-link"
                  @click="navigate(o.key)"
                >
                  📁 {{ o.key.slice(prefix.length) }}</button
                ><span v-else>{{ o.key.slice(prefix.length) }}</span
                ><small>{{ o.contentType }}</small>
              </td>
              <td>{{ o.folder ? "—" : bytes(o.size) }}</td>
              <td>{{ date(o.modifiedAt) }}</td>
              <td>
                <div class="actions">
                  <button v-if="!o.folder" :disabled="busy" @click="signed(o)">
                    下载</button
                  ><button
                    v-if="!o.folder"
                    :disabled="busy"
                    @click="signed(o, true)"
                  >
                    签名分享</button
                  ><button
                    v-if="!o.folder && bucket.access !== 'private'"
                    :disabled="busy"
                    @click="copyPublic(o)"
                  >
                    公开链接</button
                  ><button @click="info(o)">详情</button
                  ><button
                    v-if="app.can('write')"
                    class="danger"
                    :disabled="busy"
                    @click="removeFile(o)"
                  >
                    删除
                  </button>
                </div>
              </td>
            </tr>
            <tr v-if="!objects.length">
              <td colspan="5" class="empty">当前目录没有文件。</td>
            </tr>
          </tbody>
        </table>
      </div>
      <div class="actions pagination">
        <button :disabled="!history.length || busy" @click="page(false)">
          上一页</button
        ><button :disabled="!hasMore || busy" @click="page(true)">
          下一页
        </button>
      </div>
    </section>
    <section v-if="tab === 'packages'" class="panel">
      <PackagesPanel kind="storage" />
    </section>
    <section v-if="tab === 'keys' && app.can('manage')" class="panel">
      <header>
        <div>
          <h2>项目 API 密钥</h2>
          <p class="muted">
            按读、写、删除分别授权。Secret Key 仅创建时显示一次。
          </p>
        </div>
        <button
          v-if="app.can('secrets')"
          class="primary"
          @click="keyEditor = true"
        >
          创建密钥
        </button>
      </header>
      <p class="endpoint">
        REST 数据入口 <code>{{ endpoint }}</code>
      </p>
      <p class="muted">
        使用 X-Access-Key 与 X-Secret-Key 请求头；此项目 REST
        接口不接受其他产品的 JWT 或密钥。
      </p>
      <div class="table">
        <table>
          <thead>
            <tr>
              <th>名称 / Access Key</th>
              <th>权限</th>
              <th>状态 / 有效期</th>
              <th>最近使用</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="k in keys" :key="k.id">
              <td>
                {{ k.name
                }}<small
                  ><code>{{ k.accessKey }}</code></small
                >
              </td>
              <td>
                {{
                  k.scopes
                    .map(
                      (s) =>
                        ({ read: "读取", write: "写入", delete: "删除" })[s] ??
                        s,
                    )
                    .join("、")
                }}
              </td>
              <td>
                {{ k.active ? "启用" : "停用"
                }}<small>{{
                  k.expiresAt ? date(k.expiresAt) : "不限有效期"
                }}</small>
              </td>
              <td>{{ date(k.lastUsedAt) }}</td>
              <td class="actions">
                <button
                  :disabled="busy"
                  @click="
                    act(
                      () =>
                        app.request(`/keys/${k.id}`, {
                          method: 'PUT',
                          body: { active: !k.active },
                        }),
                      '密钥状态已更新',
                    )
                  "
                >
                  {{ k.active ? "停用" : "启用" }}</button
                ><button
                  class="danger"
                  :disabled="busy"
                  @click="
                    act(
                      () => app.request(`/keys/${k.id}`, { method: 'DELETE' }),
                      '密钥已删除',
                    )
                  "
                >
                  删除
                </button>
              </td>
            </tr>
            <tr v-if="!keys.length">
              <td colspan="5">尚未创建程序化密钥。</td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>
    <section v-if="tab === 'logs'" class="panel">
      <header>
        <div>
          <h2>使用日志</h2>
          <p class="muted">
            待核验表示数据面操作结果尚未确认；请核对实际对象后处理。
          </p>
        </div>
      </header>
      <div class="table">
        <table>
          <thead>
            <tr>
              <th>操作</th>
              <th>对象</th>
              <th>容量变化</th>
              <th>状态</th>
              <th>时间</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="l in logs" :key="l.id">
              <td>{{ l.action }}</td>
              <td>
                <code>{{ l.key || "—" }}</code>
              </td>
              <td>
                {{ l.sizeChange >= 0 ? "+" : "−"
                }}{{ bytes(Math.abs(l.sizeChange)) }}
              </td>
              <td>{{ statusLabels[l.state] || l.state }}</td>
              <td>{{ date(l.createdAt) }}</td>
            </tr>
            <tr v-if="!logs.length">
              <td colspan="5">暂无操作记录。</td>
            </tr>
          </tbody>
        </table>
      </div>
      <div class="actions pagination">
        <button
          :disabled="!logOffset || busy"
          @click="
            logOffset = Math.max(0, logOffset - 50);
            act(loadLogs, undefined, false);
          "
        >
          上一页</button
        ><button
          :disabled="logs.length < 50 || busy"
          @click="
            logOffset += 50;
            act(loadLogs, undefined, false);
          "
        >
          下一页
        </button>
      </div>
    </section>
    <div v-if="bucketEditor" class="overlay">
      <section
        class="dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="bucket-editor"
      >
        <header>
          <h2 id="bucket-editor">
            {{ bucketEditId ? "存储桶设置" : "创建存储桶" }}
          </h2>
          <button @click="bucketEditor = false" aria-label="关闭">×</button>
        </header>
        <form @submit.prevent="saveBucket">
          <label
            >显示名称<input
              v-model="bucketForm.name"
              required
              maxlength="100" /></label
          ><label
            >说明<textarea
              v-model="bucketForm.description"
              maxlength="2000"
            /></label
          ><label
            >访问策略<select v-model="bucketForm.access">
              <option value="private">私有</option>
              <option v-if="app.can('manage')" value="public-read">
                公开读
              </option>
              <option v-if="app.can('manage')" value="public-read-write">
                公开读写
              </option>
            </select></label
          >
          <p v-if="bucketForm.access === 'public-read-write'" class="notice">
            公开写入允许未登录访客消耗本项目容量，请只用于明确需要公众提交文件的桶。
          </p>
          <button class="primary" :disabled="busy">保存</button>
          <p v-if="error" class="error" role="alert">{{ error }}</p>
        </form>
      </section>
    </div>
    <div v-if="corsEditor && bucket" class="overlay">
      <section
        class="dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="cors-editor"
      >
        <header>
          <h2 id="cors-editor">跨域来源</h2>
          <button @click="corsEditor = false" aria-label="关闭">×</button>
        </header>
        <form
          @submit.prevent="
            act(async () => {
              await app.request(`/buckets/${bucket!.id}/cors`, {
                method: 'POST',
                body: {
                  origins: corsOrigins
                    .split('\n')
                    .map((s) => s.trim())
                    .filter(Boolean),
                },
              });
              corsEditor = false;
            }, 'CORS 已更新')
          "
        >
          <label
            >每行一个来源<textarea
              v-model="corsOrigins"
              rows="5"
              required
              placeholder="https://console.example.com"
            />
          </label>
          <p class="muted">
            保留当前 Euler
            的来源，浏览器才能签名直传。公开数据面与人员会话分离。
          </p>
          <button :disabled="busy">保存来源</button>
        </form>
      </section>
    </div>
    <div v-if="folderEditor && bucket" class="overlay">
      <section
        class="dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="folder-editor"
      >
        <header>
          <h2 id="folder-editor">新建目录</h2>
          <button @click="folderEditor = false" aria-label="关闭">×</button>
        </header>
        <form
          @submit.prevent="
            act(async () => {
              await app.request(`/buckets/${bucket!.id}/objects/folder`, {
                method: 'POST',
                body: { key: prefix + folderName },
              });
              folderEditor = false;
              folderName = '';
            }, '目录已创建')
          "
        >
          <label
            >目录名称<input
              v-model="folderName"
              required
              maxlength="255"
              pattern="[^/\\]+" /></label
          ><button :disabled="busy">创建目录</button>
        </form>
      </section>
    </div>
    <div v-if="detail" class="overlay">
      <section
        class="dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="file-detail"
      >
        <header>
          <h2 id="file-detail">对象详情</h2>
          <button @click="detail = null" aria-label="关闭">×</button>
        </header>
        <dl>
          <dt>对象键</dt>
          <dd>{{ detail.key }}</dd>
          <dt>大小</dt>
          <dd>{{ bytes(detail.size) }}</dd>
          <dt>内容类型</dt>
          <dd>{{ detail.contentType || "—" }}</dd>
          <dt>ETag</dt>
          <dd>
            <code>{{ detail.etag }}</code>
          </dd>
          <dt>修改时间</dt>
          <dd>{{ date(detail.modifiedAt) }}</dd>
        </dl>
      </section>
    </div>
    <div v-if="keyEditor" class="overlay">
      <section
        class="dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="key-editor"
      >
        <header>
          <h2 id="key-editor">创建项目 API 密钥</h2>
          <button @click="keyEditor = false" aria-label="关闭">×</button>
        </header>
        <form @submit.prevent="createKey">
          <label
            >名称<input
              v-model="keyForm.name"
              required
              maxlength="64"
              placeholder="例如：生产上传服务"
          /></label>
          <fieldset>
            <legend>权限</legend>
            <label class="check"
              ><input
                v-model="keyForm.scopes"
                type="checkbox"
                value="read"
              />读取</label
            ><label class="check"
              ><input
                v-model="keyForm.scopes"
                type="checkbox"
                value="write"
              />写入</label
            ><label class="check"
              ><input
                v-model="keyForm.scopes"
                type="checkbox"
                value="delete"
              />删除</label
            >
          </fieldset>
          <label
            >有效期（可选）<input
              v-model="keyForm.expiresAt"
              type="datetime-local" /></label
          ><button class="primary" :disabled="busy || !keyForm.scopes.length">
            创建密钥
          </button>
        </form>
      </section>
    </div>
    <div v-if="keyOnce" class="overlay">
      <section
        class="dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="key-once"
      >
        <header>
          <h2 id="key-once">保存新密钥</h2>
          <button @click="keyOnce = null" aria-label="关闭">×</button>
        </header>
        <p class="notice">
          Secret Key 只显示这一次。关闭后无法再次查看；丢失时需删除并新建。
        </p>
        <dl>
          <dt>Access Key</dt>
          <dd>
            <code>{{ keyOnce.accessKey }}</code>
          </dd>
          <dt>Secret Key</dt>
          <dd>
            <code>{{ keyOnce.secretKey }}</code>
          </dd>
        </dl>
        <div class="actions">
          <button
            @click="
              copy(
                `Access Key: ${keyOnce.accessKey}\nSecret Key: ${keyOnce.secretKey}`,
              )
            "
          >
            复制密钥</button
          ><button @click="keyOnce = null">已安全保存</button>
        </div>
      </section>
    </div>
  </div>
</template>
<style scoped>
.storage-app {
  display: grid;
  gap: 22px;
}
.hero {
  padding: 24px;
  border: 1px solid var(--cloud-border);
  border-radius: 18px;
  background: linear-gradient(125deg, var(--cloud-panel), #0694a212);
}
header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 18px;
}
.eyebrow {
  font-size: 11px;
  font-weight: 700;
  letter-spacing: 2px;
  color: #078c9a;
}
h1 {
  font-size: 28px;
  margin: 5px 0;
}
h2 {
  font-size: 18px;
  margin: 0;
}
p {
  margin: 8px 0;
}
.muted,
small,
.hero p {
  color: var(--cloud-muted);
}
small {
  display: block;
  font-size: 12px;
}
.metrics {
  display: grid;
  grid-template-columns: 2fr 1fr 1fr;
  gap: 16px;
}
.metrics article {
  padding: 20px;
  border: 1px solid var(--cloud-border);
  border-radius: 14px;
  background: var(--cloud-panel);
}
.metrics strong {
  display: block;
  font-size: 26px;
  margin: 8px 0;
}
.metrics progress {
  width: 100%;
  height: 6px;
  accent-color: #0891b2;
}
.actions,
nav,
.breadcrumb {
  display: flex;
  gap: 8px;
  align-items: center;
  flex-wrap: wrap;
}
button,
a {
  padding: 8px 12px;
  border: 1px solid var(--cloud-border);
  border-radius: 8px;
  background: var(--cloud-panel);
  color: var(--cloud-text);
}
button:hover {
  background: var(--cloud-hover);
}
.current {
  background: var(--cloud-blue-soft);
  color: var(--cloud-blue);
}
.primary {
  background: #087f94;
  color: #fff;
}
.text-link {
  padding: 0;
  border: 0;
  background: none;
  color: var(--cloud-blue);
  text-align: left;
}
.danger,
.error {
  color: var(--cloud-danger);
}
.error,
.notice {
  padding: 12px;
  border-radius: 8px;
  background: var(--cloud-panel-soft);
}
.notice {
  color: var(--cloud-warning);
}
.panel {
  padding: 20px;
  border: 1px solid var(--cloud-border);
  border-radius: 14px;
  background: var(--cloud-panel);
  min-width: 0;
}
.panel > header {
  margin-bottom: 18px;
}
.table {
  overflow: auto;
}
table {
  width: 100%;
  border-collapse: collapse;
}
th,
td {
  text-align: left;
  padding: 12px;
  border-bottom: 1px solid var(--cloud-border);
  vertical-align: top;
}
th {
  font-size: 12px;
  color: var(--cloud-muted);
}
.empty {
  padding: 40px;
  text-align: center;
  color: var(--cloud-muted);
}
code {
  font-size: 12px;
  overflow-wrap: anywhere;
}
.endpoint {
  overflow-wrap: anywhere;
}
.breadcrumb {
  margin: 14px 0;
}
.breadcrumb button {
  padding: 3px 7px;
}
.pagination {
  justify-content: flex-end;
  margin-top: 16px;
}
input,
select,
textarea {
  padding: 10px;
  border: 1px solid var(--cloud-border);
  border-radius: 8px;
  background: var(--cloud-panel);
  color: var(--cloud-text);
}
input[type="checkbox"] {
  accent-color: #087f94;
}
.overlay {
  position: fixed;
  inset: 0;
  background: #10192a66;
  z-index: 80;
  display: grid;
  place-items: center;
  padding: 24px;
}
.dialog {
  width: 100%;
  max-width: 620px;
  max-height: 85vh;
  overflow: auto;
  padding: 24px;
  border-radius: 16px;
  background: var(--cloud-panel);
  box-shadow: var(--cloud-shadow);
}
form,
label {
  display: grid;
  gap: 8px;
}
form {
  gap: 18px;
  margin-top: 20px;
}
.check {
  display: inline-flex;
  align-items: center;
  margin-right: 14px;
}
fieldset {
  border: 1px solid var(--cloud-border);
  border-radius: 8px;
}
dl {
  display: grid;
  grid-template-columns: 95px 1fr;
  gap: 12px;
}
dt {
  color: var(--cloud-muted);
}
dd {
  margin: 0;
  overflow-wrap: anywhere;
}
@media (max-width: 720px) {
  .metrics {
    grid-template-columns: 1fr;
  }
  .hero,
  header {
    flex-direction: column;
    align-items: start;
  }
  .panel {
    padding: 14px;
  }
  .dialog {
    padding: 18px;
  }
}
</style>
