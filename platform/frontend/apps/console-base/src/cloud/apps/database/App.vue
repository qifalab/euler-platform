<script setup lang="ts">
import { ref, reactive, onMounted } from "vue";
import { useApp } from "../shared";
import PackagesPanel from "./PackagesPanel.vue";
const app = useApp();
type Database = {
  id: string;
  name: string;
  type: string;
  database: string;
  username: string;
  status: string;
  sizeBytes: number;
  createdAt: string;
  lastError?: string;
};
type Overview = {
  quota: {
    mysqlMB: number;
    mysqlCount: number;
    postgresMB: number;
    postgresCount: number;
  };
  mysqlUsedBytes: number;
  mysqlCount: number;
  postgresUsedBytes: number;
  postgresCount: number;
  engines: Record<string, boolean>;
  items: Database[];
};
type Credentials = {
  host: string;
  port: number;
  database: string;
  username: string;
  password: string;
  tools: { name: string; url: string }[];
};
type Operation = {
  id: string;
  resourceId: string;
  action: string;
  state: string;
  createdAt: string;
};
const tab = ref("databases"),
  view = ref<Overview | null>(null),
  ops = ref<Operation[]>([]),
  busy = ref(false),
  loading = ref(true),
  error = ref(""),
  createOpen = ref(false),
  filter = ref("all"),
  connection = ref<Credentials | null>(null),
  selected = ref("");
const form = reactive({ name: "", type: "mysql" });
const labels: Record<string, string> = {
  active: "运行中",
  readonly: "只读（配额限制）",
  provisioning: "创建中",
  error: "需要核验",
  pending: "处理中",
  completed: "已完成",
  uncertain: "结果待核验",
};
const bytes = (v: number) => `${(v / 1024 / 1024).toFixed(2)} MB`;
async function load() {
  error.value = "";
  loading.value = true;
  try {
    view.value = await app.request<Overview>("/");
    if (app.can("admin"))
      ops.value = (
        await app.request<{ items: Operation[] }>("/admin/operations")
      ).items;
  } catch (e) {
    error.value = e instanceof Error ? e.message : "无法读取数据库";
  } finally {
    loading.value = false;
  }
}
async function act(fn: () => Promise<unknown>, message: string) {
  if (busy.value) return;
  busy.value = true;
  error.value = "";
  try {
    await fn();
    app.notify(message);
    await load();
  } catch (e) {
    error.value = e instanceof Error ? e.message : "操作失败";
  } finally {
    busy.value = false;
  }
}
async function create() {
  await act(async () => {
    await app.request("/databases", {
      method: "POST",
      body: form,
      timeoutMs: 120000,
    });
    createOpen.value = false;
    form.name = "";
  }, "数据库已创建");
}
async function reveal(db: Database) {
  selected.value = db.name;
  connection.value = null;
  await act(async () => {
    connection.value = await app.request<Credentials>(
      `/databases/${db.id}/credentials`,
    );
  }, "连接凭据已读取，此操作已记录");
}
async function remove(db: Database) {
  if (!confirm(`永久删除数据库“${db.name}”及全部数据？此操作无法撤销。`))
    return;
  await act(
    () =>
      app.request(`/databases/${db.id}`, {
        method: "DELETE",
        timeoutMs: 120000,
      }),
    "数据库已删除",
  );
}
async function rotate(db: Database) {
  if (!confirm(`重置“${db.name}”的密码？旧密码将立即失效，应用连接需更新。`))
    return;
  connection.value = null;
  await act(
    () =>
      app.request(`/databases/${db.id}/password`, {
        method: "POST",
        timeoutMs: 120000,
      }),
    "密码已轮换，请重新查看连接信息",
  );
}
async function copy(s: string) {
  try {
    await navigator.clipboard.writeText(s);
    app.notify("已复制");
  } catch {
    app.notify("复制失败，请手动选择文本", "error");
  }
}
onMounted(load);
</script>
<template>
  <div class="database-app">
    <header class="hero">
      <div>
        <span class="eyebrow">EULER DATABASE</span>
        <h1>数据库</h1>
        <p>当前项目的 MySQL 与 PostgreSQL、连接凭据和容量配额。</p>
      </div>
      <div class="actions">
        <button @click="load" :disabled="busy || loading">刷新</button
        ><button
          v-if="app.can('write')"
          class="primary"
          @click="createOpen = true"
        >
          创建数据库
        </button>
      </div>
    </header>
    <p v-if="error" role="alert" class="error">{{ error }}</p>
    <div v-if="view" class="metrics">
      <article>
        <span>MySQL</span
        ><strong
          >{{ view.mysqlCount }} / {{ view.quota.mysqlCount }}
          <small>个数据库</small></strong
        >
        <p>{{ bytes(view.mysqlUsedBytes) }} / {{ view.quota.mysqlMB }} MB</p>
        <small>{{ view.engines.mysql ? "引擎已配置" : "尚未配置引擎" }}</small>
      </article>
      <article>
        <span>PostgreSQL</span
        ><strong
          >{{ view.postgresCount }} / {{ view.quota.postgresCount }}
          <small>个数据库</small></strong
        >
        <p>
          {{ bytes(view.postgresUsedBytes) }} / {{ view.quota.postgresMB }} MB
        </p>
        <small>{{
          view.engines.postgresql ? "引擎已配置" : "尚未配置引擎"
        }}</small>
      </article>
    </div>
    <nav aria-label="数据库功能">
      <button
        :class="{ current: tab === 'databases' }"
        @click="tab = 'databases'"
      >
        数据库</button
      ><button
        :class="{ current: tab === 'packages' }"
        @click="tab = 'packages'"
      >
        资源包{{ app.can("admin") ? "与运营" : "" }}</button
      ><button
        v-if="app.can('admin')"
        :class="{ current: tab === 'operations' }"
        @click="tab = 'operations'"
      >
        运行维护
      </button>
    </nav>
    <section v-if="tab === 'databases'" class="panel">
      <header>
        <h2>数据库列表</h2>
        <select v-model="filter" aria-label="按引擎筛选">
          <option value="all">全部引擎</option>
          <option value="mysql">MySQL</option>
          <option value="postgresql">PostgreSQL</option>
        </select>
      </header>
      <p v-if="loading">正在读取数据库…</p>
      <div v-else class="table">
        <table>
          <thead>
            <tr>
              <th>名称</th>
              <th>引擎与库名</th>
              <th>状态</th>
              <th>已用空间</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="db in view?.items.filter(
                (d) => filter === 'all' || d.type === filter,
              )"
              :key="db.id"
            >
              <td>
                <strong>{{ db.name }}</strong
                ><small>{{
                  new Date(db.createdAt).toLocaleDateString()
                }}</small>
              </td>
              <td>
                {{ db.type === "mysql" ? "MySQL" : "PostgreSQL"
                }}<small
                  ><code>{{ db.database }}</code></small
                >
              </td>
              <td>
                <span :class="['status', db.status]">{{
                  labels[db.status] || db.status
                }}</span
                ><small v-if="db.lastError">{{ db.lastError }}</small>
              </td>
              <td>{{ bytes(db.sizeBytes) }}</td>
              <td>
                <div class="actions">
                  <button
                    v-if="app.can('secrets')"
                    :disabled="
                      busy || !['active', 'readonly'].includes(db.status)
                    "
                    @click="reveal(db)"
                  >
                    连接信息</button
                  ><button
                    v-if="app.can('secrets')"
                    :disabled="
                      busy || !['active', 'readonly'].includes(db.status)
                    "
                    @click="rotate(db)"
                  >
                    重置密码</button
                  ><button
                    v-if="app.can('manage')"
                    :disabled="busy"
                    class="danger"
                    @click="remove(db)"
                  >
                    删除
                  </button>
                </div>
              </td>
            </tr>
            <tr v-if="!view?.items.length">
              <td colspan="5" class="empty">
                项目尚未创建数据库。配置真实引擎后即可创建。
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <p class="muted">
        容量由引擎定时采集。超出容量配额时将收回写入权限，扩容或清理后恢复。
      </p>
    </section>
    <section v-if="tab === 'packages'" class="panel">
      <PackagesPanel kind="database" />
    </section>
    <section v-if="tab === 'operations' && app.can('admin')" class="panel">
      <header>
        <div>
          <h2>运行维护</h2>
          <p class="muted">
            仅操作当前项目。待核验表示外部引擎结果尚未确认，保留资源和配额，避免重复创建。
          </p>
        </div>
        <div class="actions">
          <button
            :disabled="busy"
            @click="
              act(
                () =>
                  app.request('/admin/check-quota', {
                    method: 'POST',
                    timeoutMs: 120000,
                  }),
                '用量与配额权限已检查',
              )
            "
          >
            检查配额</button
          ><button
            :disabled="busy"
            @click="
              act(
                () =>
                  app.request('/admin/fix-pg-permissions', {
                    method: 'POST',
                    timeoutMs: 120000,
                  }),
                'PostgreSQL 权限已复核',
              )
            "
          >
            修复 PG 权限
          </button>
        </div>
      </header>
      <div class="table">
        <table>
          <thead>
            <tr>
              <th>操作</th>
              <th>资源</th>
              <th>状态</th>
              <th>时间</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="op in ops" :key="op.id">
              <td>{{ op.action }}</td>
              <td>
                <code>{{ op.resourceId }}</code>
              </td>
              <td>{{ labels[op.state] || op.state }}</td>
              <td>{{ new Date(op.createdAt).toLocaleString() }}</td>
            </tr>
            <tr v-if="!ops.length">
              <td colspan="4">暂无引擎操作。</td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>
    <div v-if="createOpen" class="overlay">
      <section
        role="dialog"
        aria-modal="true"
        aria-labelledby="create-title"
        class="dialog"
      >
        <header>
          <h2 id="create-title">创建数据库</h2>
          <button @click="createOpen = false" aria-label="关闭">×</button>
        </header>
        <form @submit.prevent="create">
          <label
            >名称<input
              v-model="form.name"
              maxlength="100"
              required
              placeholder="例如：生产业务库" /></label
          ><label
            >数据库引擎<select v-model="form.type">
              <option value="mysql" :disabled="!view?.engines.mysql">
                MySQL{{ view?.engines.mysql ? "" : "（未配置）" }}
              </option>
              <option value="postgresql" :disabled="!view?.engines.postgresql">
                PostgreSQL{{ view?.engines.postgresql ? "" : "（未配置）" }}
              </option>
            </select></label
          >
          <p class="muted">
            将创建项目专属数据库与独立数据库用户。密码由系统生成并加密保存。
          </p>
          <button class="primary" :disabled="busy || !view?.engines[form.type]">
            {{ busy ? "正在创建…" : "创建数据库" }}
          </button>
          <p v-if="error" role="alert" class="error">{{ error }}</p>
        </form>
      </section>
    </div>
    <div v-if="connection" class="overlay">
      <section
        role="dialog"
        aria-modal="true"
        aria-labelledby="connection-title"
        class="dialog"
      >
        <header>
          <h2 id="connection-title">{{ selected }} · 连接信息</h2>
          <button @click="connection = null" aria-label="关闭">×</button>
        </header>
        <p class="muted">
          凭据可直接访问数据库。关闭此面板后不会保存在浏览器本地。
        </p>
        <dl>
          <template
            v-for="(value, key) in {
              主机: connection.host,
              端口: connection.port,
              数据库: connection.database,
              用户名: connection.username,
              密码: connection.password,
            }"
            :key="key"
            ><dt>{{ key }}</dt>
            <dd>
              <code>{{ value }}</code
              ><button @click="copy(String(value))">复制</button>
            </dd></template
          >
        </dl>
        <div v-if="connection.tools.length" class="actions">
          <a
            v-for="tool in connection.tools"
            :key="tool.name"
            :href="tool.url"
            target="_blank"
            rel="noopener noreferrer"
            >打开 {{ tool.name }} ↗</a
          >
        </div>
        <p v-if="connection.tools.length" class="muted">
          数据库管理工具单独部署；请复制上面的数据库密码登录。
        </p>
      </section>
    </div>
  </div>
</template>
<style scoped>
.database-app {
  display: grid;
  gap: 22px;
}
.hero {
  padding: 24px;
  border-radius: 18px;
  background: linear-gradient(
    125deg,
    var(--cloud-panel),
    var(--cloud-blue-soft)
  );
  border: 1px solid var(--cloud-border);
}
header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 16px;
}
.eyebrow {
  letter-spacing: 2px;
  color: var(--cloud-blue);
  font-size: 11px;
  font-weight: 700;
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
small,
.muted,
.hero p {
  color: var(--cloud-muted);
}
small {
  display: block;
  font-size: 12px;
}
.metrics {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 16px;
}
.metrics article {
  padding: 20px;
  background: var(--cloud-panel);
  border: 1px solid var(--cloud-border);
  border-radius: 14px;
}
.metrics strong {
  display: block;
  font-size: 26px;
  margin-top: 8px;
}
.metrics strong small {
  display: inline;
  font-weight: 400;
}
nav,
.actions {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}
.current {
  color: var(--cloud-blue);
  background: var(--cloud-blue-soft);
}
button,
a {
  padding: 8px 12px;
  border: 1px solid var(--cloud-border);
  border-radius: 8px;
  background: var(--cloud-panel);
  color: var(--cloud-text);
  text-decoration: none;
}
button:hover,
a:hover {
  background: var(--cloud-hover);
}
.primary {
  background: var(--cloud-blue);
  color: #fff;
}
.danger,
.error {
  color: var(--cloud-danger);
}
.error {
  padding: 12px;
  background: var(--cloud-panel-soft);
  border-radius: 8px;
}
.panel {
  padding: 20px;
  background: var(--cloud-panel);
  border: 1px solid var(--cloud-border);
  border-radius: 14px;
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
  padding: 12px;
  text-align: left;
  border-bottom: 1px solid var(--cloud-border);
  vertical-align: top;
}
th {
  font-size: 12px;
  color: var(--cloud-muted);
}
code {
  font-size: 12px;
  overflow-wrap: anywhere;
}
.empty {
  padding: 40px;
  text-align: center;
  color: var(--cloud-muted);
}
.status {
  font-size: 12px;
  padding: 4px 8px;
  border-radius: 6px;
  background: var(--cloud-panel-soft);
}
.status.active {
  color: var(--cloud-success);
}
.status.error,
.status.readonly {
  color: var(--cloud-warning);
}
select,
input {
  padding: 10px;
  border: 1px solid var(--cloud-border);
  border-radius: 8px;
  background: var(--cloud-panel);
  color: var(--cloud-text);
}
.overlay {
  position: fixed;
  inset: 0;
  z-index: 80;
  background: #10192a66;
  display: grid;
  place-items: center;
  padding: 24px;
}
.dialog {
  max-width: 620px;
  width: 100%;
  max-height: 85vh;
  overflow: auto;
  padding: 24px;
  background: var(--cloud-panel);
  border-radius: 16px;
  box-shadow: var(--cloud-shadow);
}
.dialog form,
label {
  display: grid;
  gap: 8px;
}
.dialog form {
  margin-top: 20px;
  gap: 18px;
}
dl {
  display: grid;
  grid-template-columns: 80px 1fr;
  gap: 12px;
}
dt {
  color: var(--cloud-muted);
}
dd {
  margin: 0;
  display: flex;
  justify-content: space-between;
  gap: 10px;
  align-items: center;
}
.dialog .actions {
  margin-top: 20px;
}
@media (max-width: 680px) {
  .metrics {
    grid-template-columns: 1fr;
  }
  .hero,
  header {
    align-items: start;
    flex-direction: column;
  }
  .dialog {
    padding: 18px;
  }
  dd {
    min-width: 0;
  }
  .panel {
    padding: 14px;
  }
}
</style>
