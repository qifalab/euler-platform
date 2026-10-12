<script setup lang="ts">
import { computed, onMounted, onBeforeUnmount, ref } from "vue";
import { useApp } from "../shared";

type Rules = {
  minLength?: number;
  maxLength?: number;
  min?: number;
  max?: number;
  pattern?: string;
  maxBytes?: number;
  accept?: string[];
};
type Field = {
  id: string;
  name: string;
  label: string;
  type: string;
  required: boolean;
  description: string;
  options?: string[];
  validations: Rules;
};
type Scheme = {
  id: string;
  name: string;
  description: string;
  status: string;
  fields: Field[];
  version: number;
  createdAt?: string;
  updatedAt?: string;
};
type Material = {
  id: string;
  name: string;
  type: string;
  size: number;
  fieldName: string;
  createdAt: string;
};
type History = {
  id: string;
  action: string;
  status: string;
  actorId: string;
  reason?: string;
  createdAt: string;
  version: number;
};
type Submission = {
  id: string;
  schemeId: string;
  schemeName: string;
  actorId: string;
  actorName?: string;
  email?: string;
  status: string;
  version: number;
  data?: Record<string, string | number>;
  fields?: Field[];
  materials?: Material[];
  history?: History[];
  reason?: string;
  createdAt: string;
  updatedAt: string;
  reviewedAt?: string;
};
type User = {
  id: string;
  name: string;
  email: string;
  createdAt?: string;
  lastSeen?: string;
};
type Key = {
  id: string;
  name: string;
  schemeIds: string[];
  actorIds: string[];
  details: boolean;
  expiresAt: string;
  revokedAt: string;
  createdAt: string;
};
type Notice = {
  id: string;
  submissionId: string;
  channel: string;
  status: string;
  attempts: number;
  lastError: string;
  createdAt: string;
};
type Page<T> = { items: T[]; total: number; page: number; limit: number };
const app = useApp();
const view = ref("overview"),
  busy = ref(false),
  loading = ref(true),
  error = ref("");
const schemes = ref<Scheme[]>([]),
  mine = ref<Submission[]>([]),
  queue = ref<Submission[]>([]),
  users = ref<User[]>([]),
  keys = ref<Key[]>([]),
  notices = ref<Notice[]>([]);
const stats = ref<Record<string, number>>({}),
  channels = ref<Record<string, boolean>>({});
const unsubmitted = ref<Material[]>([]);
const queuePage = ref(0),
  queueTotal = ref(0),
  userPage = ref(0),
  userTotal = ref(0);
const queueStatus = ref("pending"),
  queueScheme = ref(""),
  queueSearch = ref(""),
  queueActor = ref(""),
  userSearch = ref("");
const editor = ref<Scheme | null>(null),
  formScheme = ref<Scheme | null>(null),
  formData = ref<Record<string, string | number>>({}),
  formVersion = ref(0),
  formFiles = ref<Material[]>([]);
const detail = ref<Submission | null>(null),
  reviewMode = ref(false),
  reviewReason = ref(""),
  preview = ref<{ file: Material; review: boolean } | null>(null);
const keyDialog = ref(false),
  issuedToken = ref(""),
  keyName = ref(""),
  keySchemes = ref<string[]>([]),
  keyActors = ref<string[]>([]),
  keyDetails = ref(false),
  keyExpiry = ref("");
const confirmAction = ref<{
  title: string;
  text: string;
  action: () => Promise<void>;
} | null>(null);
const apiToken = ref(""),
  apiActor = ref(app.scope.actorId),
  apiScheme = ref(""),
  apiResult = ref<{ verified: boolean; status: string } | null>(null);
const testController = new AbortController();
function closeKeyDialog() {
  if (!busy.value) {
    keyDialog.value = false;
    issuedToken.value = "";
  }
}
async function copyToken() {
  try {
    await navigator.clipboard.writeText(issuedToken.value);
    app.notify("密钥已复制", "success");
  } catch {
    app.notify("请手动选择并复制密钥", "error");
  }
}
onBeforeUnmount(() => testController.abort());
const activeSchemes = computed(() =>
  schemes.value.filter((s) => s.status === "active"),
);
const tabs = computed(() => [
  { id: "overview", title: "我的认证" },
  { id: "schemes", title: "认证方案" },
  { id: "history", title: "申请记录" },
  ...(app.can("review")
    ? [
        { id: "queue", title: "审核工作台" },
        { id: "insights", title: "统计与用户" },
      ]
    : []),
  ...(app.can("admin")
    ? [
        { id: "keys", title: "查询密钥" },
        { id: "notifications", title: "通知记录" },
        { id: "docs", title: "API 文档" },
      ]
    : []),
]);
const labels: Record<string, string> = {
  active: "可申请",
  inactive: "已停用",
  pending: "待审核",
  approved: "已通过",
  rejected: "需补充材料",
  not_submitted: "未申请",
  submit: "首次提交",
  resubmit: "重新提交",
  approve: "审核通过",
  reject: "审核拒绝",
  sent: "发送成功",
  failed: "发送失败",
  unknown: "结果待核实",
  sending: "发送中",
};
const fieldTypes = [
  { id: "text", name: "文本" },
  { id: "longText", name: "长文本" },
  { id: "email", name: "电子邮箱" },
  { id: "phone", name: "电话" },
  { id: "number", name: "数字" },
  { id: "date", name: "日期" },
  { id: "select", name: "单选" },
  { id: "image", name: "图片" },
  { id: "file", name: "文件" },
];
function label(value: string) {
  return labels[value] ?? value;
}
function date(value?: string) {
  return value ? new Date(value).toLocaleString("zh-CN") : "—";
}
function size(n: number) {
  return n > 1048576
    ? `${(n / 1048576).toFixed(1)} MiB`
    : `${Math.ceil(n / 1024)} KiB`;
}
function message(e: unknown) {
  return e instanceof Error ? e.message : "操作暂时无法完成，请重试";
}
async function run(fn: () => Promise<void>) {
  if (busy.value) return;
  busy.value = true;
  error.value = "";
  try {
    await fn();
  } catch (e) {
    error.value = message(e);
    app.notify(error.value, "error");
  } finally {
    busy.value = false;
  }
}
async function refresh() {
  const data = await app.request<{
    schemes: Scheme[];
    submissions: Submission[];
  }>("/bootstrap");
  schemes.value = data.schemes;
  mine.value = data.submissions;
  unsubmitted.value = (
    await app.request<{ items: Material[] }>("/materials")
  ).items;
}
async function loadQueue() {
  const q = new URLSearchParams({
    page: String(queuePage.value),
    limit: "20",
    status: queueStatus.value,
    schemeId: queueScheme.value,
    search: queueSearch.value,
    actorId: queueActor.value,
  });
  const data = await app.request<Page<Submission>>(`/review/submissions?${q}`);
  queue.value = data.items;
  queueTotal.value = data.total;
}
async function loadUsers() {
  const q = new URLSearchParams({
    page: String(userPage.value),
    limit: "20",
    search: userSearch.value,
  });
  const data = await app.request<Page<User>>(`/review/users?${q}`);
  users.value = data.items;
  userTotal.value = data.total;
}
async function loadKeys() {
  keys.value = (await app.request<{ items: Key[] }>("/keys")).items;
}
async function loadNotices() {
  const data = await app.request<{
    items: Notice[];
    channels: Record<string, boolean>;
  }>("/notifications");
  notices.value = data.items;
  channels.value = data.channels;
}
async function changeView(id: string) {
  view.value = id;
  await run(async () => {
    if (id === "queue") await loadQueue();
    if (id === "insights") {
      stats.value = await app.request<Record<string, number>>("/review/stats");
      await loadUsers();
    }
    if (id === "keys") await loadKeys();
    if (id === "notifications") await loadNotices();
  });
}
onMounted(async () => {
  try {
    await refresh();
  } catch (e) {
    error.value = message(e);
  } finally {
    loading.value = false;
  }
});
function mineFor(id: string) {
  return mine.value.find((s) => s.schemeId === id);
}
function addField() {
  if (!editor.value) return;
  const suffix = crypto.randomUUID().replaceAll("-", "").slice(0, 12);
  editor.value.fields.push({
    id: `fld_${suffix}`,
    name: `field_${suffix}`,
    label: "",
    type: "text",
    required: false,
    description: "",
    validations: {},
  });
}
function editScheme(s?: Scheme) {
  error.value = "";
  editor.value = s
    ? (JSON.parse(JSON.stringify(s)) as Scheme)
    : {
        id: "",
        name: "",
        description: "",
        status: "active",
        fields: [],
        version: 0,
      };
  if (!s) addField();
}
function moveField(index: number, direction: number) {
  const fields = editor.value?.fields;
  if (!fields) return;
  const next = index + direction;
  if (next < 0 || next >= fields.length) return;
  [fields[index], fields[next]] = [fields[next]!, fields[index]!];
}
function setOptions(field: Field, event: Event) {
  field.options = (event.target as HTMLTextAreaElement).value
    .split("\n")
    .filter(Boolean);
}
async function saveScheme() {
  await run(async () => {
    const s = editor.value;
    if (!s) return;
    for (const field of s.fields)
      field.validations = Object.fromEntries(
        Object.entries(field.validations).filter(
          ([, value]) => value !== "" && value !== null && value !== undefined,
        ),
      ) as Rules;
    await app.request(s.id ? `/schemes/${s.id}` : "/schemes", {
      method: s.id ? "PUT" : "POST",
      body: s,
    });
    editor.value = null;
    await refresh();
    app.notify("认证方案已保存", "success");
  });
}
function confirmDelete(s: Scheme) {
  confirmAction.value = {
    title: "删除认证方案",
    text: `确定删除「${s.name}」？已有申请的方案应停用以保留记录。`,
    action: async () => {
      await app.request(`/schemes/${s.id}`, { method: "DELETE" });
      await refresh();
    },
  };
}
async function toggleScheme(s: Scheme) {
  await run(async () => {
    await app.request(`/schemes/${s.id}`, {
      method: "PUT",
      body: { ...s, status: s.status === "active" ? "inactive" : "active" },
    });
    await refresh();
  });
}
async function startApplication(s: Scheme) {
  await run(async () => {
    const current = await app.request<Scheme>(`/schemes/${s.id}`);
    formScheme.value = current;
    formData.value = {};
    formFiles.value = [];
    formVersion.value = 0;
    const previous = mineFor(s.id);
    if (previous) {
      const old = await app.request<Submission>(`/submissions/${previous.id}`);
      formVersion.value = old.version;
      formData.value = Object.fromEntries(
        current.fields
          .filter((f) => Object.hasOwn(old.data ?? {}, f.name))
          .map((f) => [f.name, old.data![f.name]!]),
      );
      formFiles.value = old.materials ?? [];
    }
  });
}
function fileFor(field: Field) {
  return formFiles.value.find((f) => f.id === formData.value[field.name]);
}
async function chooseFile(field: Field, event: Event) {
  const input = event.target as HTMLInputElement;
  const file = input.files?.[0];
  if (!file || !formScheme.value) return;
  if (file.size > (field.validations.maxBytes || 10 * 1024 * 1024)) {
    error.value = "文件超过允许大小";
    input.value = "";
    return;
  }
  await run(async () => {
    const body = new FormData();
    body.set("schemeId", formScheme.value!.id);
    body.set("fieldName", field.name);
    body.set("file", file);
    const uploaded = await app.upload<Material>("/materials", body);
    formData.value[field.name] = uploaded.id;
    formFiles.value.push(uploaded);
    unsubmitted.value.unshift(uploaded);
  });
}
async function removeFile(field: Field) {
  const file = fileFor(field);
  if (!file) return;
  await run(async () => {
    const old = mineFor(formScheme.value!.id);
    if (!old) await app.request(`/materials/${file.id}`, { method: "DELETE" });
    delete formData.value[field.name];
    formFiles.value = formFiles.value.filter((f) => f.id !== file.id);
    if (!old)
      unsubmitted.value = unsubmitted.value.filter((f) => f.id !== file.id);
  });
}
async function submit() {
  await run(async () => {
    const s = formScheme.value;
    if (!s) return;
    const data = Object.fromEntries(
      Object.entries(formData.value).filter(([, value]) => value !== ""),
    );
    const result = await app.request<Submission>("/submissions", {
      method: "POST",
      body: {
        schemeId: s.id,
        schemeVersion: s.version,
        version: formVersion.value,
        data,
      },
    });
    formScheme.value = null;
    await refresh();
    detail.value = result;
    reviewMode.value = false;
    app.notify("材料已提交，等待审核", "success");
  });
}
async function showSubmission(id: string, review = false) {
  await run(async () => {
    detail.value = await app.request<Submission>(
      `${review ? "/review" : ""}/submissions/${id}`,
    );
    reviewMode.value = review;
    reviewReason.value = "";
  });
}
async function applyReview(status: string) {
  const sub = detail.value;
  if (!sub) return;
  await run(async () => {
    detail.value = await app.request<Submission>(
      `/review/submissions/${sub.id}`,
      {
        method: "POST",
        body: { status, version: sub.version, reason: reviewReason.value },
      },
    );
    await refresh();
    await loadQueue();
    reviewReason.value = "";
    app.notify("审核结果已保存", "success");
  });
}
function reviewConfirm(status: string) {
  if (status === "rejected" && !reviewReason.value.trim()) {
    error.value = "请填写拒绝原因";
    return;
  }
  confirmAction.value = {
    title: status === "approved" ? "确认通过认证" : "确认拒绝认证",
    text: "该决定会更新当前认证结果并写入不可省略的审核历史。请确认已检查当前版本材料。",
    action: async () => {
      const sub = detail.value!;
      detail.value = await app.request<Submission>(
        `/review/submissions/${sub.id}`,
        {
          method: "POST",
          body: { status, version: sub.version, reason: reviewReason.value },
        },
      );
      await refresh();
      await loadQueue();
      reviewReason.value = "";
    },
  };
}
function detailFile(field: Field) {
  return detail.value?.materials?.find(
    (m) => m.id === detail.value?.data?.[field.name],
  );
}
function materialPath(file: Material, review = reviewMode.value) {
  return `${review ? "/review" : ""}/materials/${file.id}`;
}
function deleteUnsubmitted(file: Material) {
  confirmAction.value = {
    title: "删除未提交材料",
    text: `删除「${file.name}」后无法恢复。已提交的材料不会在此列表中显示。`,
    action: async () => {
      await app.request(`/materials/${file.id}`, { method: "DELETE" });
      await refresh();
    },
  };
}
async function download(file: Material, review = reviewMode.value) {
  await run(() => app.download(materialPath(file, review), file.name));
}
async function performConfirmation() {
  const value = confirmAction.value;
  if (!value) return;
  await run(async () => {
    await value.action();
    confirmAction.value = null;
    app.notify("操作已完成", "success");
  });
}
async function openKeyDialog() {
  keyName.value = "";
  keySchemes.value = [];
  keyActors.value = [app.scope.actorId];
  keyDetails.value = false;
  issuedToken.value = "";
  keyExpiry.value = new Date(Date.now() + 30 * 86400000)
    .toISOString()
    .slice(0, 16);
  keyDialog.value = true;
  if (app.can("review")) await run(loadUsers);
}
async function issueKey() {
  await run(async () => {
    const result = await app.request<{ token: string; key: Key }>("/keys", {
      method: "POST",
      body: {
        name: keyName.value,
        schemeIds: keySchemes.value,
        actorIds: keyActors.value,
        details: keyDetails.value,
        expiresAt: new Date(keyExpiry.value).toISOString(),
      },
    });
    issuedToken.value = result.token;
    await loadKeys();
  });
}
function revokeKey(k: Key) {
  confirmAction.value = {
    title: "撤销查询密钥",
    text: `「${k.name}」撤销后立即失效，无法恢复。`,
    action: async () => {
      await app.request(`/keys/${k.id}`, { method: "DELETE" });
      await loadKeys();
    },
  };
}
function retryNotice(n: Notice) {
  confirmAction.value = {
    title: "重试生命周期通知",
    text: "若上次结果未知，请先检查群消息。重试可能产生重复通知。",
    action: async () => {
      await app.request(`/notifications/${n.id}/retry`, {
        method: "POST",
        body: {},
      });
      await loadNotices();
    },
  };
}
async function testAPI() {
  await run(async () => {
    const q = new URLSearchParams({
      schemeId: apiScheme.value,
      actorId: apiActor.value,
    });
    const response = await fetch(`${app.publicBase}/verification/status?${q}`, {
      headers: { Authorization: `Bearer ${apiToken.value}` },
      signal: testController.signal,
      credentials: "omit",
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error?.message ?? "查询失败");
    apiResult.value = result.data;
  });
}
</script>

<template>
  <section class="trust-app" aria-label="Trust 信任中心">
    <header class="trust-heading">
      <div>
        <p class="eyebrow">TRUST · 信任中心</p>
        <h1>每一份认证，都有清楚的来路</h1>
        <p>在当前项目完成材料提交、认证审核与结果查询。</p>
      </div>
      <button
        class="secondary"
        :disabled="busy || loading"
        @click="run(refresh)"
      >
        刷新
      </button>
    </header>
    <nav class="tabs" aria-label="Trust 功能">
      <button
        v-for="tab in tabs"
        :key="tab.id"
        :class="{ active: view === tab.id }"
        :aria-current="view === tab.id ? 'page' : undefined"
        @click="changeView(tab.id)"
      >
        {{ tab.title }}
      </button>
    </nav>
    <p v-if="error" class="alert error" role="alert">
      {{ error }} <button class="text" @click="error = ''">关闭</button>
    </p>
    <div v-if="loading" class="empty" role="status">正在加载认证工作区…</div>
    <template v-else>
      <div v-if="view === 'overview'" class="overview">
        <div class="metric-row">
          <article>
            <span>可用方案</span><strong>{{ activeSchemes.length }}</strong>
          </article>
          <article>
            <span>待审核</span
            ><strong>{{
              mine.filter((s) => s.status === "pending").length
            }}</strong>
          </article>
          <article>
            <span>已通过</span
            ><strong>{{
              mine.filter((s) => s.status === "approved").length
            }}</strong>
          </article>
          <article>
            <span>需补充材料</span
            ><strong>{{
              mine.filter((s) => s.status === "rejected").length
            }}</strong>
          </article>
        </div>
        <div class="section-title">
          <h2>选择认证方案</h2>
          <span>资格属于申请人，审核权限单独授予。</span>
        </div>
        <div class="scheme-grid">
          <article v-for="s in activeSchemes" :key="s.id" class="scheme-card">
            <div class="card-top">
              <span class="badge" :class="mineFor(s.id)?.status">{{
                label(mineFor(s.id)?.status ?? "not_submitted")
              }}</span
              ><span>{{ s.fields.length }} 项资料</span>
            </div>
            <h3>{{ s.name }}</h3>
            <p>
              {{
                s.description || "按方案要求填写资料，提交后由审核人员处理。"
              }}
            </p>
            <p v-if="mineFor(s.id)?.reason" class="rejection">
              {{ mineFor(s.id)?.reason }}
            </p>
            <div class="actions">
              <button
                v-if="app.can('write') && mineFor(s.id)?.status !== 'approved'"
                :disabled="busy"
                @click="startApplication(s)"
              >
                {{ mineFor(s.id) ? "完善并重新提交" : "开始认证" }}</button
              ><button
                v-if="mineFor(s.id)"
                class="secondary"
                @click="showSubmission(mineFor(s.id)!.id)"
              >
                申请详情</button
              ><button v-else class="text" @click="view = 'schemes'">
                查看要求
              </button>
            </div>
          </article>
        </div>
        <div v-if="!activeSchemes.length" class="empty">
          <h3>当前项目还没有可用方案</h3>
          <p>具有 Trust 运营权限的成员可以创建并启用认证方案。</p>
          <button v-if="app.can('admin')" @click="editScheme()">
            创建认证方案
          </button>
        </div>
      </div>
      <div v-if="view === 'schemes'">
        <div class="section-title">
          <div>
            <h2>认证方案</h2>
            <p>
              字段、顺序与校验规则在这里维护。已有申请会保留提交时的字段快照。
            </p>
          </div>
          <button v-if="app.can('admin')" @click="editScheme()">
            创建方案
          </button>
        </div>
        <div v-if="!schemes.length" class="empty">暂无认证方案。</div>
        <article v-for="s in schemes" :key="s.id" class="panel scheme-list">
          <div class="section-title">
            <div>
              <h3>
                {{ s.name }}
                <span class="badge" :class="s.status">{{
                  label(s.status)
                }}</span>
              </h3>
              <p>{{ s.description }}</p>
            </div>
            <div class="actions">
              <button
                v-if="
                  app.can('write') &&
                  s.status === 'active' &&
                  mineFor(s.id)?.status !== 'approved'
                "
                @click="startApplication(s)"
              >
                填写申请</button
              ><template v-if="app.can('admin')"
                ><button class="secondary" @click="editScheme(s)">编辑</button
                ><button
                  class="secondary"
                  :disabled="busy"
                  @click="toggleScheme(s)"
                >
                  {{ s.status === "active" ? "停用" : "启用" }}</button
                ><button class="danger text" @click="confirmDelete(s)">
                  删除
                </button></template
              >
            </div>
          </div>
          <ol class="requirements">
            <li v-for="f in s.fields" :key="f.name">
              <strong>{{ f.label }}</strong
              ><span
                >{{ fieldTypes.find((t) => t.id === f.type)?.name }} ·
                {{ f.required ? "必填" : "选填" }}</span
              >
              <p v-if="f.description">{{ f.description }}</p>
              <small v-if="f.options?.length"
                >选项：{{ f.options.join("、") }}</small
              >
            </li>
          </ol>
        </article>
      </div>
      <div v-if="view === 'history'">
        <div class="section-title">
          <h2>我的申请记录</h2>
          <span>{{ mine.length }} 项申请</span>
        </div>
        <div v-if="!mine.length" class="empty">
          还没有申请，先选择一个认证方案。
        </div>
        <div v-else class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>方案</th>
                <th>状态</th>
                <th>最近更新</th>
                <th>审核反馈</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="s in mine" :key="s.id">
                <td>{{ s.schemeName }}</td>
                <td>
                  <span class="badge" :class="s.status">{{
                    label(s.status)
                  }}</span>
                </td>
                <td>{{ date(s.updatedAt) }}</td>
                <td>{{ s.reason || "—" }}</td>
                <td>
                  <button class="text" @click="showSubmission(s.id)">
                    详情与历史
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
      <section
        v-if="view === 'history' && unsubmitted.length"
        class="panel"
        style="margin-top: 24px"
      >
        <h3>尚未提交的材料</h3>
        <p>
          关闭填写窗口后，已上传的材料会保留在这里。可以下载或删除不再使用的材料。
        </p>
        <div v-for="file in unsubmitted" :key="file.id" class="file-row">
          <span>{{ file.name }} · {{ size(file.size) }}</span
          ><button class="text" @click="preview = { file, review: false }">
            预览</button
          ><button class="text" @click="download(file, false)">下载</button
          ><button
            v-if="app.can('write')"
            class="text danger"
            @click="deleteUnsubmitted(file)"
          >
            删除
          </button>
        </div>
      </section>
      <div v-if="view === 'queue' && app.can('review')">
        <div class="section-title">
          <h2>审核工作台</h2>
          <span>仅显示当前项目的申请与材料。</span>
        </div>
        <form
          class="filters"
          @submit.prevent="
            queuePage = 0;
            run(loadQueue);
          "
        >
          <label
            >状态<select v-model="queueStatus">
              <option value="">全部状态</option>
              <option value="pending">待审核</option>
              <option value="approved">已通过</option>
              <option value="rejected">已拒绝</option>
            </select></label
          ><label
            >方案<select v-model="queueScheme">
              <option value="">全部方案</option>
              <option v-for="s in schemes" :key="s.id" :value="s.id">
                {{ s.name }}
              </option>
            </select></label
          ><label class="grow"
            >申请人<input
              v-model="queueSearch"
              placeholder="姓名、邮箱或申请人编号" /></label
          ><button :disabled="busy">查询</button
          ><button
            v-if="queueActor"
            type="button"
            class="secondary"
            @click="
              queueActor = '';
              run(loadQueue);
            "
          >
            取消指定申请人
          </button>
        </form>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>申请人</th>
                <th>方案</th>
                <th>状态</th>
                <th>更新时间</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="s in queue" :key="s.id">
                <td>
                  <strong>{{ s.actorName || s.actorId }}</strong
                  ><small>{{ s.email }}</small>
                </td>
                <td>{{ s.schemeName }}</td>
                <td>
                  <span class="badge" :class="s.status">{{
                    label(s.status)
                  }}</span>
                </td>
                <td>{{ date(s.updatedAt) }}</td>
                <td>
                  <button @click="showSubmission(s.id, true)">
                    查看与审核
                  </button>
                </td>
              </tr>
              <tr v-if="!queue.length">
                <td colspan="5" class="empty">没有符合条件的申请。</td>
              </tr>
            </tbody>
          </table>
        </div>
        <div class="pagination">
          <span>共 {{ queueTotal }} 条</span
          ><button
            class="secondary"
            :disabled="!queuePage || busy"
            @click="
              queuePage--;
              run(loadQueue);
            "
          >
            上一页</button
          ><span>第 {{ queuePage + 1 }} 页</span
          ><button
            class="secondary"
            :disabled="(queuePage + 1) * 20 >= queueTotal || busy"
            @click="
              queuePage++;
              run(loadQueue);
            "
          >
            下一页
          </button>
        </div>
      </div>
      <div v-if="view === 'insights' && app.can('review')">
        <div class="metric-row">
          <article
            v-for="metric in [
              { key: 'users', name: 'Trust 用户' },
              { key: 'schemes', name: '活跃方案' },
              { key: 'total', name: '全部申请' },
              { key: 'pending', name: '待审核' },
              { key: 'approved', name: '已通过' },
              { key: 'rejected', name: '已拒绝' },
            ]"
            :key="metric.key"
          >
            <span>{{ metric.name }}</span
            ><strong>{{ stats[metric.key] ?? 0 }}</strong>
          </article>
        </div>
        <div class="section-title">
          <h2>项目 Trust 用户</h2>
          <span>包括已进入 Trust、尚未提交申请的用户。</span>
        </div>
        <form
          class="filters"
          @submit.prevent="
            userPage = 0;
            run(loadUsers);
          "
        >
          <input
            v-model="userSearch"
            aria-label="搜索用户"
            placeholder="姓名、邮箱或申请人编号"
          /><button :disabled="busy">搜索</button>
        </form>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>用户</th>
                <th>邮箱</th>
                <th>首次访问</th>
                <th>最近访问</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="u in users" :key="u.id">
                <td>
                  {{ u.name || u.id }}<small>{{ u.id }}</small>
                </td>
                <td>{{ u.email || "—" }}</td>
                <td>{{ date(u.createdAt) }}</td>
                <td>{{ date(u.lastSeen) }}</td>
                <td>
                  <button
                    class="text"
                    @click="
                      queueActor = u.id;
                      queueStatus = '';
                      queuePage = 0;
                      changeView('queue');
                    "
                  >
                    查看申请
                  </button>
                </td>
              </tr>
              <tr v-if="!users.length">
                <td colspan="5" class="empty">没有符合条件的用户。</td>
              </tr>
            </tbody>
          </table>
        </div>
        <div class="pagination">
          <span>共 {{ userTotal }} 人</span
          ><button
            class="secondary"
            :disabled="!userPage || busy"
            @click="
              userPage--;
              run(loadUsers);
            "
          >
            上一页</button
          ><button
            class="secondary"
            :disabled="(userPage + 1) * 20 >= userTotal || busy"
            @click="
              userPage++;
              run(loadUsers);
            "
          >
            下一页
          </button>
        </div>
      </div>
      <div v-if="view === 'keys' && app.can('admin')">
        <div class="section-title">
          <div>
            <h2>应用查询密钥</h2>
            <p>
              每把密钥固定到当前项目，并限制方案、申请人、有效期和可读取的数据。
            </p>
          </div>
          <button v-if="app.can('secrets')" @click="openKeyDialog">
            签发密钥
          </button>
        </div>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>名称</th>
                <th>授权范围</th>
                <th>权限</th>
                <th>有效期</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="k in keys" :key="k.id">
                <td>
                  {{ k.name }}<small>{{ k.id }}</small>
                </td>
                <td>
                  {{ k.schemeIds.length }} 个方案 /
                  {{ k.actorIds.length }} 位申请人
                </td>
                <td>
                  {{ k.details ? "状态、已通过详情和材料" : "仅认证状态" }}
                </td>
                <td>{{ k.revokedAt ? "已撤销" : date(k.expiresAt) }}</td>
                <td>
                  <button
                    v-if="!k.revokedAt && app.can('secrets')"
                    class="danger text"
                    @click="revokeKey(k)"
                  >
                    撤销
                  </button>
                </td>
              </tr>
              <tr v-if="!keys.length">
                <td colspan="5" class="empty">尚未签发查询密钥。</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
      <div v-if="view === 'notifications' && app.can('admin')">
        <div class="section-title">
          <div>
            <h2>生命周期通知</h2>
            <p>
              企业微信：{{ channels.wecom ? "已配置" : "未配置" }} · 飞书：{{
                channels.feishu ? "已配置" : "未配置"
              }}。渠道由部署者配置，消息不包含申请材料。
            </p>
          </div>
          <button class="secondary" :disabled="busy" @click="run(loadNotices)">
            刷新结果
          </button>
        </div>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>时间</th>
                <th>申请编号</th>
                <th>渠道</th>
                <th>发送结果</th>
                <th>次数</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="n in notices" :key="n.id">
                <td>{{ date(n.createdAt) }}</td>
                <td>{{ n.submissionId }}</td>
                <td>{{ n.channel === "wecom" ? "企业微信" : "飞书" }}</td>
                <td>
                  {{ label(n.status) }}<small>{{ n.lastError }}</small>
                </td>
                <td>{{ n.attempts }}</td>
                <td>
                  <button
                    v-if="n.status !== 'sent'"
                    class="secondary"
                    @click="retryNotice(n)"
                  >
                    核实后重试
                  </button>
                </td>
              </tr>
              <tr v-if="!notices.length">
                <td colspan="6" class="empty">
                  暂无通知记录。配置渠道后，新提交、重提和审核会记录发送结果。
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
      <div v-if="view === 'docs' && app.can('admin')" class="docs">
        <h2>认证查询 API</h2>
        <p>
          使用查询密钥通过 Authorization
          请求头访问。请求范围由密钥确定；身份参数必须属于签发时选定的申请人。停用
          Trust 或撤销密钥后立即拒绝访问。
        </p>
        <article class="panel">
          <h3>认证状态</h3>
          <code
            >GET
            {{
              app.publicBase
            }}/verification/status?schemeId=方案编号&amp;actorId=申请人编号</code
          >
          <p>
            返回 verified 和 pending / approved / rejected / not_submitted。
          </p>
          <h3>已通过的认证详情</h3>
          <code
            >GET
            {{
              app.publicBase
            }}/verification/details?schemeId=方案编号&amp;actorId=申请人编号</code
          >
          <p>
            需要详情读取授权，仅返回已通过申请。材料使用返回的材料编号单独访问。
          </p>
          <code>GET {{ app.publicBase }}/materials/材料编号</code>
          <p>
            请求头：Authorization: Bearer 查询密钥。密钥不会显示在
            URL、历史记录或列表里。
          </p>
        </article>
        <form class="panel form-grid" @submit.prevent="testAPI">
          <h3 class="full">测试状态查询</h3>
          <label class="full"
            >查询密钥<input
              v-model="apiToken"
              type="password"
              autocomplete="off"
              required /></label
          ><label
            >认证方案<select v-model="apiScheme" required>
              <option value="" disabled>选择方案</option>
              <option v-for="s in activeSchemes" :key="s.id" :value="s.id">
                {{ s.name }}
              </option>
            </select></label
          ><label>申请人编号<input v-model="apiActor" required /></label>
          <div class="full"><button :disabled="busy">查询认证状态</button></div>
          <p v-if="apiResult" class="alert full">
            认证结果：{{ label(apiResult.status) }} ·
            {{ apiResult.verified ? "已验证" : "尚未验证" }}
          </p>
        </form>
      </div>
    </template>

    <div v-if="editor" class="overlay" @click.self="!busy && (editor = null)">
      <section
        class="dialog wide"
        role="dialog"
        aria-modal="true"
        aria-label="编辑认证方案"
      >
        <header>
          <h2>{{ editor.id ? "编辑认证方案" : "创建认证方案" }}</h2>
          <button class="text" :disabled="busy" @click="editor = null">
            关闭
          </button>
        </header>
        <form @submit.prevent="saveScheme">
          <div class="form-grid">
            <label
              >方案名称<input
                v-model="editor.name"
                maxlength="120"
                required /></label
            ><label
              >状态<select v-model="editor.status" aria-label="状态">
                <option value="active">启用</option>
                <option value="inactive">停用</option>
              </select></label
            ><label class="full"
              >方案说明<textarea
                v-model="editor.description"
                rows="3"
                maxlength="10000"
              />
            </label>
          </div>
          <h3>需要申请人提供的资料</h3>
          <article
            v-for="(f, index) in editor.fields"
            :key="f.id"
            class="field-editor"
          >
            <div class="section-title">
              <strong>字段 {{ index + 1 }}</strong>
              <div class="actions">
                <button
                  type="button"
                  class="text"
                  :disabled="index === 0"
                  @click="moveField(index, -1)"
                >
                  上移</button
                ><button
                  type="button"
                  class="text"
                  :disabled="index === editor.fields.length - 1"
                  @click="moveField(index, 1)"
                >
                  下移</button
                ><button
                  type="button"
                  class="danger text"
                  :disabled="editor.fields.length === 1"
                  @click="editor.fields.splice(index, 1)"
                >
                  删除
                </button>
              </div>
            </div>
            <div class="form-grid">
              <label
                >字段标题<input
                  v-model="f.label"
                  required
                  maxlength="120" /></label
              ><label
                >字段类型<select v-model="f.type" aria-label="字段类型">
                  <option
                    v-for="type in fieldTypes"
                    :key="type.id"
                    :value="type.id"
                  >
                    {{ type.name }}
                  </option>
                </select></label
              ><label
                >字段标识<input
                  v-model="f.name"
                  pattern="[A-Za-z_][A-Za-z0-9_]{0,99}"
                  required /></label
              ><label class="check"
                ><input v-model="f.required" type="checkbox" />必须填写</label
              ><label class="full"
                >填写提示<input
                  v-model="f.description"
                  maxlength="2000" /></label
              ><label v-if="f.type === 'select'" class="full"
                >选项，每行一项<textarea
                  :value="f.options?.join('\n')"
                  required
                  rows="3"
                  @input="setOptions(f, $event)"
                /></label
              ><template v-if="f.type === 'file' || f.type === 'image'"
                ><label
                  >最大文件大小（字节，最多 10485760）<input
                    v-model.number="f.validations.maxBytes"
                    type="number"
                    min="1"
                    max="10485760"
                    placeholder="默认 10 MiB" /></label
                ><label
                  >允许的文件类型<select
                    v-model="f.validations.accept"
                    aria-label="允许的文件类型"
                    multiple
                  >
                    <option value="image/png">PNG</option>
                    <option value="image/jpeg">JPEG</option>
                    <option value="image/gif">GIF</option>
                    <option value="image/webp">WebP</option>
                    <option v-if="f.type === 'file'" value="application/pdf">
                      PDF
                    </option>
                    <option v-if="f.type === 'file'" value="text/plain">
                      纯文本
                    </option>
                  </select></label
                ></template
              ><template v-else-if="f.type === 'number'"
                ><label
                  >最小值<input
                    v-model.number="f.validations.min"
                    type="number"
                    step="any" /></label
                ><label
                  >最大值<input
                    v-model.number="f.validations.max"
                    type="number"
                    step="any" /></label></template
              ><template v-else
                ><label
                  >最少字符<input
                    v-model.number="f.validations.minLength"
                    type="number"
                    min="0"
                    max="20000" /></label
                ><label
                  >最多字符<input
                    v-model.number="f.validations.maxLength"
                    type="number"
                    min="1"
                    max="20000" /></label
                ><label class="full"
                  >格式规则（可选，正则表达式）<input
                    v-model="f.validations.pattern"
                    maxlength="500"
                    placeholder="例如 ^[0-9]{11}$" /></label
              ></template>
            </div>
          </article>
          <button
            type="button"
            class="secondary"
            :disabled="editor.fields.length >= 100"
            @click="addField"
          >
            添加字段
          </button>
          <p v-if="error" class="alert error">{{ error }}</p>
          <footer>
            <button
              type="button"
              class="secondary"
              :disabled="busy"
              @click="editor = null"
            >
              取消</button
            ><button :disabled="busy">
              {{ busy ? "正在保存…" : "保存方案" }}
            </button>
          </footer>
        </form>
      </section>
    </div>
    <div
      v-if="formScheme"
      class="overlay"
      @click.self="!busy && (formScheme = null)"
    >
      <section
        class="dialog"
        role="dialog"
        aria-modal="true"
        aria-label="提交认证材料"
      >
        <header>
          <div>
            <p class="eyebrow">认证申请</p>
            <h2>{{ formScheme.name }}</h2>
          </div>
          <button class="text" :disabled="busy" @click="formScheme = null">
            关闭
          </button>
        </header>
        <p>{{ formScheme.description }}</p>
        <p v-if="formVersion" class="alert">
          重新提交会使申请回到待审核，之前的审核决定保留在历史中。
        </p>
        <form @submit.prevent="submit">
          <div
            v-for="f in formScheme.fields"
            :key="f.name"
            class="dynamic-field"
          >
            <label :for="'trust-' + f.name"
              >{{ f.label }}
              <span v-if="f.required" class="required">*</span></label
            >
            <p v-if="f.description" class="hint">{{ f.description }}</p>
            <template v-if="f.type === 'file' || f.type === 'image'"
              ><input
                :id="'trust-' + f.name"
                type="file"
                :required="f.required && !formData[f.name]"
                :disabled="busy"
                :accept="
                  f.validations.accept?.join(',') ||
                  (f.type === 'image'
                    ? 'image/png,image/jpeg,image/gif,image/webp'
                    : 'image/png,image/jpeg,image/gif,image/webp,application/pdf,text/plain')
                "
                @change="chooseFile(f, $event)"
              />
              <div v-if="fileFor(f)" class="file-row">
                <span
                  >{{ fileFor(f)!.name }} · {{ size(fileFor(f)!.size) }}</span
                ><button
                  type="button"
                  class="text"
                  @click="preview = { file: fileFor(f)!, review: false }"
                >
                  预览</button
                ><button type="button" class="text" @click="removeFile(f)">
                  移除
                </button>
              </div>
              <small
                >最多
                {{
                  size(f.validations.maxBytes || 10485760)
                }}，材料加密保存。</small
              ></template
            ><select
              v-else-if="f.type === 'select'"
              :id="'trust-' + f.name"
              v-model="formData[f.name]"
              :required="f.required"
            >
              <option value="">请选择</option>
              <option v-for="option in f.options" :key="option" :value="option">
                {{ option }}
              </option></select
            ><textarea
              v-else-if="f.type === 'longText'"
              :id="'trust-' + f.name"
              v-model="formData[f.name]"
              :required="f.required"
              :minlength="f.validations.minLength"
              :maxlength="f.validations.maxLength || 20000"
              rows="5"
            /><input
              v-else-if="f.type === 'number'"
              :id="'trust-' + f.name"
              v-model.number="formData[f.name]"
              type="number"
              step="any"
              :min="f.validations.min"
              :max="f.validations.max"
              :required="f.required"
            /><input
              v-else
              :id="'trust-' + f.name"
              v-model="formData[f.name]"
              :type="
                f.type === 'phone'
                  ? 'tel'
                  : ['email', 'date'].includes(f.type)
                    ? f.type
                    : 'text'
              "
              :required="f.required"
              :minlength="f.validations.minLength"
              :maxlength="f.validations.maxLength || 2000"
            />
          </div>
          <p v-if="error" class="alert error">{{ error }}</p>
          <footer>
            <button
              type="button"
              class="secondary"
              :disabled="busy"
              @click="formScheme = null"
            >
              取消</button
            ><button :disabled="busy">
              {{ busy ? "正在提交…" : "确认提交材料" }}
            </button>
          </footer>
        </form>
      </section>
    </div>
    <div v-if="detail" class="overlay" @click.self="!busy && (detail = null)">
      <section
        class="dialog wide"
        role="dialog"
        aria-modal="true"
        aria-label="认证申请详情"
      >
        <header>
          <div>
            <p class="eyebrow">{{ reviewMode ? "审核工作台" : "我的申请" }}</p>
            <h2>{{ detail.schemeName }}</h2>
          </div>
          <button class="text" :disabled="busy" @click="detail = null">
            关闭
          </button>
        </header>
        <div class="detail-meta">
          <span class="badge" :class="detail.status">{{
            label(detail.status)
          }}</span
          ><span>{{ detail.actorName }} · {{ detail.email }}</span
          ><span>更新于 {{ date(detail.updatedAt) }}</span>
        </div>
        <p v-if="detail.reason" class="alert rejection">
          审核反馈：{{ detail.reason }}
        </p>
        <h3>申请材料</h3>
        <dl class="details-grid">
          <div v-for="f in detail.fields" :key="f.name">
            <dt>{{ f.label }}</dt>
            <dd v-if="f.type === 'file' || f.type === 'image'">
              <template v-if="detailFile(f)"
                ><span
                  >{{ detailFile(f)!.name }} ·
                  {{ size(detailFile(f)!.size) }}</span
                >
                <div class="actions">
                  <button
                    class="secondary"
                    @click="
                      preview = { file: detailFile(f)!, review: reviewMode }
                    "
                  >
                    预览</button
                  ><button class="text" @click="download(detailFile(f)!)">
                    下载
                  </button>
                </div></template
              ><span v-else>未提供</span>
            </dd>
            <dd v-else>{{ detail.data?.[f.name] ?? "未填写" }}</dd>
          </div>
        </dl>
        <section v-if="reviewMode" class="review-box">
          <h3>
            {{ detail.status === "pending" ? "审核决定" : "修改审核决定" }}
          </h3>
          <p>
            改判会影响当前资格结果，并保留之前的决定。普通项目管理员没有此权限。
          </p>
          <label
            >拒绝原因<textarea
              v-model="reviewReason"
              rows="3"
              maxlength="10000"
              placeholder="拒绝时必填，请写清需要补充的资料。"
            />
          </label>
          <div class="actions">
            <button :disabled="busy" @click="reviewConfirm('approved')">
              通过认证</button
            ><button
              class="danger"
              :disabled="busy"
              @click="reviewConfirm('rejected')"
            >
              拒绝并说明原因
            </button>
          </div>
        </section>
        <h3>完整处理历史</h3>
        <ol class="timeline">
          <li v-for="h in detail.history" :key="h.id">
            <div>
              <strong>{{ label(h.action) }}</strong
              ><time>{{ date(h.createdAt) }}</time>
            </div>
            <p v-if="h.reason">{{ h.reason }}</p>
            <small>操作人 {{ h.actorId }} · 申请版本 {{ h.version }}</small>
          </li>
        </ol>
        <p v-if="!detail.history?.length" class="empty">没有历史记录。</p>
        <p v-if="error" class="alert error">{{ error }}</p>
      </section>
    </div>
    <div
      v-if="preview"
      class="overlay preview-overlay"
      @click.self="preview = null"
    >
      <section
        class="dialog wide"
        role="dialog"
        aria-modal="true"
        aria-label="材料预览"
      >
        <header>
          <h2>{{ preview.file.name }}</h2>
          <button class="text" @click="preview = null">关闭预览</button>
        </header>
        <iframe
          :src="
            app.basePath +
            materialPath(preview.file, preview.review) +
            '?preview=1'
          "
          :title="preview.file.name"
          sandbox="allow-same-origin"
        />
        <p>若浏览器无法预览此格式，可以下载后查看。</p>
        <button @click="download(preview.file, preview.review)">
          下载材料
        </button>
      </section>
    </div>
    <div v-if="keyDialog" class="overlay" @click.self="closeKeyDialog">
      <section
        class="dialog"
        role="dialog"
        aria-modal="true"
        aria-label="签发查询密钥"
      >
        <header>
          <h2>签发查询密钥</h2>
          <button
            class="text"
            :disabled="busy"
            @click="
              keyDialog = false;
              issuedToken = '';
            "
          >
            关闭
          </button>
        </header>
        <div v-if="issuedToken">
          <p class="alert">
            密钥仅显示这一次，请交给获授权的服务保存。丢失后应撤销并重新签发。
          </p>
          <textarea
            :value="issuedToken"
            readonly
            rows="4"
            aria-label="新签发的查询密钥"
          /><button @click="copyToken">复制密钥</button>
        </div>
        <form v-else @submit.prevent="issueKey">
          <label
            >用途名称<input v-model="keyName" required maxlength="100" /></label
          ><label
            >有效期<input v-model="keyExpiry" type="datetime-local" required
          /></label>
          <fieldset>
            <legend>允许的认证方案</legend>
            <label v-for="s in activeSchemes" :key="s.id" class="check"
              ><input v-model="keySchemes" type="checkbox" :value="s.id" />{{
                s.name
              }}</label
            >
          </fieldset>
          <fieldset>
            <legend>允许的申请人</legend>
            <label class="check"
              ><input
                v-model="keyActors"
                type="checkbox"
                :value="app.scope.actorId"
              />本人 {{ app.scope.actorName }}</label
            ><template v-if="app.can('review')"
              ><div class="filters">
                <input v-model="userSearch" placeholder="搜索申请人" /><button
                  type="button"
                  class="secondary"
                  @click="
                    userPage = 0;
                    run(loadUsers);
                  "
                >
                  搜索
                </button>
              </div>
              <label
                v-for="u in users.filter((u) => u.id !== app.scope.actorId)"
                :key="u.id"
                class="check"
                ><input v-model="keyActors" type="checkbox" :value="u.id" />{{
                  u.name || u.id
                }}
                · {{ u.email }}</label
              >
              <div class="actions">
                <button
                  type="button"
                  class="text"
                  :disabled="!userPage"
                  @click="
                    userPage--;
                    run(loadUsers);
                  "
                >
                  上一页</button
                ><button
                  type="button"
                  class="text"
                  :disabled="(userPage + 1) * 20 >= userTotal"
                  @click="
                    userPage++;
                    run(loadUsers);
                  "
                >
                  下一页
                </button>
              </div></template
            >
          </fieldset>
          <label v-if="app.can('review')" class="check"
            ><input
              v-model="keyDetails"
              type="checkbox"
            />允许读取已通过详情与材料</label
          >
          <p v-if="error" class="alert error">{{ error }}</p>
          <footer>
            <button :disabled="busy || !keySchemes.length || !keyActors.length">
              签发并显示一次
            </button>
          </footer>
        </form>
      </section>
    </div>
    <div v-if="confirmAction" class="overlay confirmation">
      <section class="dialog compact" role="alertdialog" aria-modal="true">
        <h2>{{ confirmAction.title }}</h2>
        <p>{{ confirmAction.text }}</p>
        <p v-if="error" class="alert error">{{ error }}</p>
        <footer>
          <button
            class="secondary"
            :disabled="busy"
            @click="confirmAction = null"
          >
            取消</button
          ><button :disabled="busy" @click="performConfirmation">
            {{ busy ? "正在处理…" : "确认" }}
          </button>
        </footer>
      </section>
    </div>
  </section>
</template>

<style scoped>
.trust-app {
  --trust: #0d766e;
  --line: var(--eu-border, #dce5e5);
  --surface: var(--eu-surface, #fff);
  --ink: var(--eu-text, #172a31);
  color: var(--ink);
  max-width: 1400px;
  margin: auto;
  padding: 8px 0 40px;
  font-size: 14px;
}
.trust-heading {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 24px;
  padding: 28px 30px;
  background: linear-gradient(120deg, #e7f5ef, #edf5fc);
  border: 1px solid #d6e8e1;
  border-radius: 18px;
  margin-bottom: 22px;
}
.trust-heading h1 {
  font-size: 28px;
  margin: 8px 0 12px;
  letter-spacing: -0.04em;
}
.trust-heading p {
  color: #49616a;
  margin: 0;
}
.eyebrow {
  font-size: 11px;
  font-weight: 750;
  letter-spacing: 0.17em;
  color: var(--trust) !important;
}
.tabs {
  display: flex;
  gap: 4px;
  flex-wrap: wrap;
  border-bottom: 1px solid var(--line);
  margin-bottom: 25px;
}
.tabs button {
  background: none;
  color: #62727a;
  border-radius: 8px 8px 0 0;
  border-bottom: 3px solid transparent;
  padding: 13px 17px;
}
.tabs button.active {
  color: var(--trust);
  border-color: var(--trust);
  background: #edf8f5;
}
.trust-app h2 {
  font-size: 21px;
  line-height: 1.4;
  margin: 0 0 8px;
}
.trust-app h3 {
  font-size: 17px;
  line-height: 1.5;
  margin: 12px 0;
}
.trust-app p {
  line-height: 1.65;
}
.trust-app button {
  font: inherit;
  font-weight: 600;
  border: 1px solid transparent;
  border-radius: 8px;
  background: var(--trust);
  color: #fff;
  cursor: pointer;
  padding: 9px 14px;
  white-space: nowrap;
}
.trust-app button:hover {
  filter: brightness(0.95);
}
.trust-app button:disabled {
  opacity: 0.5;
  cursor: default;
}
.trust-app button.secondary {
  border-color: var(--line);
  background: var(--surface);
  color: var(--ink);
}
.trust-app button.text {
  background: none;
  color: var(--trust);
  padding: 6px 8px;
}
.trust-app button.danger {
  background: #a73132;
}
.trust-app button.danger.text {
  background: none;
  color: #a73132;
}
.trust-app button:focus-visible,
.trust-app input:focus-visible,
.trust-app select:focus-visible,
.trust-app textarea:focus-visible {
  outline: 3px solid #74bfb7;
  outline-offset: 2px;
}
.section-title {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 20px;
  margin: 20px 0;
}
.section-title p,
.section-title > span {
  color: #65747c;
  margin: 4px 0;
  font-size: 13px;
}
.actions {
  display: flex;
  gap: 8px;
  align-items: center;
  flex-wrap: wrap;
}
.metric-row {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
  gap: 14px;
  margin-bottom: 30px;
}
.metric-row article {
  padding: 20px 22px;
  background: var(--surface);
  border: 1px solid var(--line);
  border-radius: 12px;
}
.metric-row span {
  display: block;
  color: #637780;
  font-size: 12px;
}
.metric-row strong {
  font-size: 34px;
  font-weight: 650;
  display: block;
  margin-top: 10px;
}
.scheme-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
  gap: 18px;
}
.scheme-card,
.panel {
  background: var(--surface);
  border: 1px solid var(--line);
  border-radius: 13px;
  padding: 22px;
}
.scheme-card {
  display: flex;
  flex-direction: column;
}
.scheme-card > p {
  color: #61727b;
  flex: 1;
}
.card-top {
  display: flex;
  justify-content: space-between;
  align-items: center;
  color: #75858b;
  font-size: 12px;
}
.badge {
  display: inline-block;
  font-size: 11px;
  line-height: 1.3;
  white-space: nowrap;
  font-weight: 650;
  border-radius: 99px;
  padding: 5px 9px;
  background: #eff2f5;
  color: #586975;
}
.badge.approved,
.badge.active {
  background: #e1f5eb;
  color: #146743;
}
.badge.pending {
  background: #fff2cd;
  color: #946415;
}
.badge.rejected {
  background: #fce9e5;
  color: #a74332;
}
.rejection {
  color: #984d2f !important;
}
.scheme-list {
  margin-bottom: 16px;
}
.requirements {
  padding-left: 22px;
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(230px, 1fr));
  gap: 18px;
}
.requirements li {
  padding-left: 4px;
}
.requirements span {
  display: block;
  font-size: 12px;
  color: #6a7b83;
  margin: 5px 0;
}
.requirements p {
  margin: 4px 0;
}
.filters {
  display: flex;
  align-items: end;
  gap: 12px;
  flex-wrap: wrap;
  margin: 20px 0;
}
.grow {
  flex: 1;
}
.trust-app label {
  display: flex;
  flex-direction: column;
  gap: 7px;
  font-size: 13px;
  font-weight: 600;
}
.trust-app input,
.trust-app select,
.trust-app textarea {
  font: inherit;
  box-sizing: border-box;
  width: 100%;
  border: 1px solid #cad5d9;
  border-radius: 7px;
  background: var(--surface);
  color: var(--ink);
  padding: 10px 11px;
  line-height: 1.5;
  min-width: 0;
}
.trust-app select[multiple] {
  min-height: 100px;
}
.trust-app textarea {
  resize: vertical;
}
.trust-app input[type="checkbox"] {
  width: 16px;
  height: 16px;
  accent-color: var(--trust);
}
.trust-app .check {
  display: flex;
  flex-direction: row;
  align-items: center;
  gap: 9px;
  font-weight: 450;
  margin: 10px 0;
}
.table-wrap {
  overflow: auto;
  border: 1px solid var(--line);
  border-radius: 10px;
  background: var(--surface);
}
table {
  border-collapse: collapse;
  width: 100%;
  text-align: left;
}
th {
  font-size: 12px;
  color: #62757d;
  background: #f4f8f8;
  font-weight: 650;
}
th,
td {
  padding: 14px 16px;
  border-bottom: 1px solid var(--line);
  line-height: 1.5;
}
tbody tr:last-child td {
  border-bottom: 0;
}
td small {
  display: block;
  color: #74878f;
  font-size: 11px;
  margin-top: 4px;
}
.pagination {
  display: flex;
  justify-content: flex-end;
  align-items: center;
  gap: 12px;
  margin-top: 15px;
  color: #65777f;
}
.empty {
  text-align: center;
  padding: 44px 24px;
  color: #6a7c86;
}
.alert {
  background: #edf5fa;
  color: #345566;
  border: 1px solid #d5e6f0;
  padding: 12px 16px;
  border-radius: 9px;
  line-height: 1.6;
}
.alert.error {
  background: #fff1ef;
  color: #a83230;
  border-color: #f0cfca;
}
.overlay {
  position: fixed;
  inset: 0;
  background: #102d3b70;
  z-index: 1000;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 22px;
  backdrop-filter: blur(3px);
}
.dialog {
  background: var(--surface);
  border-radius: 16px;
  width: min(650px, 100%);
  max-height: 90vh;
  overflow: auto;
  padding: 26px 30px;
  box-shadow: 0 24px 70px #10202e40;
  box-sizing: border-box;
}
.dialog.wide {
  width: min(940px, 100%);
}
.dialog.compact {
  width: min(460px, 100%);
}
.dialog header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 20px;
  margin-bottom: 20px;
}
.dialog footer {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
  border-top: 1px solid var(--line);
  padding-top: 20px;
  margin-top: 25px;
}
.dialog form > label {
  margin: 18px 0;
}
.form-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 18px;
}
.full {
  grid-column: 1/-1;
}
.field-editor {
  background: #f5f9f8;
  border: 1px solid var(--line);
  padding: 18px;
  border-radius: 11px;
  margin-bottom: 14px;
}
.field-editor .section-title {
  margin: 0 0 15px;
}
.dynamic-field {
  margin: 24px 0;
}
.dynamic-field > label {
  font-weight: 650;
}
.hint {
  color: #718189;
  font-size: 12px;
  margin: 6px 0;
}
.required {
  color: #a23531;
}
.file-row {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  padding: 10px;
  background: #edf7f2;
  border-radius: 7px;
  margin: 8px 0;
}
.dynamic-field small {
  display: block;
  color: #718189;
  margin-top: 7px;
}
.detail-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 14px;
  align-items: center;
  color: #637781;
  font-size: 12px;
}
.details-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 15px;
  margin: 18px 0 28px;
}
.details-grid > div {
  border: 1px solid var(--line);
  border-radius: 9px;
  padding: 16px;
  min-width: 0;
}
.details-grid dt {
  color: #6a7c84;
  font-size: 12px;
  margin-bottom: 8px;
}
.details-grid dd {
  margin: 0;
  line-height: 1.7;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
.review-box {
  background: #f1f7f5;
  border: 1px solid #d5e7e0;
  padding: 20px;
  border-radius: 11px;
}
.review-box p {
  color: #667c79;
  font-size: 12px;
}
.review-box .actions {
  margin-top: 14px;
}
.timeline {
  list-style: none;
  padding: 0 0 0 20px;
  border-left: 2px solid #c6dfd5;
  margin-left: 8px;
}
.timeline li {
  padding: 5px 0 20px;
  position: relative;
}
.timeline li:before {
  content: "";
  position: absolute;
  left: -26px;
  top: 11px;
  width: 9px;
  height: 9px;
  border-radius: 50%;
  background: var(--trust);
}
.timeline li > div {
  display: flex;
  justify-content: space-between;
  gap: 15px;
}
.timeline time,
.timeline small {
  color: #71828c;
  font-size: 11px;
}
.timeline p {
  margin: 6px 0;
}
.preview-overlay {
  z-index: 1100;
}
.preview-overlay iframe {
  border: 1px solid var(--line);
  border-radius: 8px;
  width: 100%;
  height: 60vh;
}
.confirmation {
  z-index: 1200;
}
.docs {
  max-width: 900px;
}
.docs code {
  display: block;
  background: #edf3f5;
  padding: 13px;
  border-radius: 7px;
  overflow-wrap: anywhere;
  font-size: 12px;
  color: #345568;
}
.docs .panel {
  margin: 22px 0;
}
.trust-app fieldset {
  border: 1px solid var(--line);
  border-radius: 9px;
  margin: 20px 0;
  padding: 15px;
}
.trust-app legend {
  font-weight: 650;
  padding: 0 8px;
}
@media (max-width: 700px) {
  .trust-heading {
    padding: 22px;
    align-items: flex-start;
  }
  .trust-heading h1 {
    font-size: 23px;
  }
  .section-title {
    align-items: flex-start;
    flex-direction: column;
    gap: 12px;
  }
  .form-grid,
  .details-grid {
    grid-template-columns: 1fr;
  }
  .full {
    grid-column: auto;
  }
  .dialog {
    padding: 20px;
    max-height: 94vh;
  }
  .overlay {
    padding: 10px;
  }
  .tabs button {
    padding: 10px 12px;
  }
  .filters > * {
    flex: 1 1 130px;
  }
  .metric-row {
    grid-template-columns: 1fr 1fr;
  }
  .timeline li > div {
    flex-direction: column;
    gap: 4px;
  }
}
</style>
