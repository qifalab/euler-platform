<script setup lang="ts">
import { computed, reactive, ref, watch } from "vue";
import { useCloud } from "./context";
import { api, errorMessage, formatDate } from "./api";
type Rule = {
  id: string;
  name: string;
  source: string;
  filterId: string;
  action: string;
  targetApp: string;
  targetProjectId: string;
  templateId: string;
  enabled: boolean;
};
type Execution = {
  id: string;
  ruleId: string;
  state: string;
  attempts: number;
  error: string;
  createdAt: number;
};
type Notification = {
  id: string;
  title: string;
  body: string;
  read: boolean;
  createdAt: number;
};
const cloud = useCloud();
const rules = ref<Rule[]>([]),
  executions = ref<Execution[]>([]),
  notifications = ref<Notification[]>([]);
const loading = ref(false),
  busy = ref(false),
  error = ref("");
const sources = ref<{ id: string; name: string }[]>([]),
  templates = ref<{ id: string; name: string }[]>([]);
const form = reactive({
  name: "",
  source: "trust.status",
  filterId: "",
  action: "package",
  targetProjectId: "",
  targetApp: "database",
  templateId: "",
});
const labels: Record<string, string> = {
  "trust.status": "Trust 认证结果",
  "eid.status": "EID 已确认成员",
  "lottery.won": "活动中奖",
  pending: "等待执行",
  running: "执行中",
  succeeded: "已完成",
  failed: "执行失败",
  cancelled: "规则已暂停",
  skipped: "当前不符合条件",
};
const failed = computed(
  () => executions.value.filter((x) => x.state === "failed").length,
);
let sequence = 0,
  sourceSequence = 0,
  templateSequence = 0;
async function load() {
  const current = ++sequence;
  if (!cloud.state.projectId) return;
  loading.value = true;
  error.value = "";
  try {
    const base = cloud.projectPath("/automation");
    const [n, r, x] = await Promise.all([
      api<{ items: Notification[] }>(base + "/notifications"),
      cloud.canManageProject.value
        ? api<{ items: Rule[] }>(base + "/rules")
        : Promise.resolve({ items: [] }),
      cloud.canManageProject.value
        ? api<{ items: Execution[] }>(base + "/executions")
        : Promise.resolve({ items: [] }),
    ]);
    if (current !== sequence) return;
    notifications.value = n.items;
    rules.value = r.items;
    executions.value = x.items;
  } catch (e) {
    if (current === sequence) error.value = errorMessage(e);
  } finally {
    if (current === sequence) loading.value = false;
  }
}
async function loadSources() {
  const current = ++sourceSequence;
  sources.value = [];
  form.filterId = "";
  if (!cloud.state.projectId) return;
  if (form.source === "eid.status") {
    sources.value = [{ id: "club", name: "本项目已确认录取的成员" }];
    form.filterId = "club";
    return;
  }
  try {
    const app = form.source === "trust.status" ? "trust" : "lottery";
    const path = app === "trust" ? "/schemes" : "/rooms";
    const out = await api<{
      items?: { id: string; name: string }[];
      schemes?: { id: string; name: string }[];
      rooms?: { id: string; name: string }[];
    }>(cloud.projectPath("/apps/" + app + path));
    if (current === sourceSequence)
      sources.value = out.items ?? out.schemes ?? out.rooms ?? [];
  } catch (e) {
    if (current === sourceSequence) error.value = errorMessage(e);
  }
}
async function loadTemplates() {
  const current = ++templateSequence;
  templates.value = [];
  form.templateId = "";
  if (form.action !== "package" || !form.targetProjectId) return;
  try {
    const out = await api<{
      items: { id: string; name: string; active: boolean }[];
    }>(
      `/api/v1/tenants/${encodeURIComponent(cloud.state.tenantId)}/projects/${encodeURIComponent(form.targetProjectId)}/apps/${form.targetApp}/admin/templates`,
    );
    if (current === templateSequence)
      templates.value = out.items.filter((x) => x.active);
  } catch (e) {
    if (current === templateSequence) error.value = errorMessage(e);
  }
}
async function create() {
  busy.value = true;
  error.value = "";
  try {
    await api(cloud.projectPath("/automation/rules"), {
      method: "POST",
      body: { ...form },
    });
    form.name = "";
    cloud.notify("联动规则已启用，仅处理此后的业务事件");
    await load();
  } catch (e) {
    error.value = errorMessage(e);
  } finally {
    busy.value = false;
  }
}
async function toggle(rule: Rule) {
  busy.value = true;
  try {
    await api(
      cloud.projectPath(`/automation/rules/${encodeURIComponent(rule.id)}`),
      { method: "PATCH", body: { enabled: !rule.enabled } },
    );
    await load();
  } catch (e) {
    error.value = errorMessage(e);
  } finally {
    busy.value = false;
  }
}
async function retry(id: string) {
  busy.value = true;
  try {
    await api(
      cloud.projectPath(
        `/automation/executions/${encodeURIComponent(id)}/retry`,
      ),
      { method: "POST" },
    );
    await load();
  } catch (e) {
    error.value = errorMessage(e);
  } finally {
    busy.value = false;
  }
}
async function read(id: string) {
  try {
    await api(
      cloud.projectPath(
        `/automation/notifications/${encodeURIComponent(id)}/read`,
      ),
      { method: "POST" },
    );
    await load();
  } catch (e) {
    error.value = errorMessage(e);
  }
}
watch(
  () => [cloud.state.tenantId, cloud.state.projectId],
  () => {
    rules.value = [];
    executions.value = [];
    notifications.value = [];
    sources.value = [];
    templates.value = [];
    ++sourceSequence;
    ++templateSequence;
    form.targetProjectId = cloud.state.projectId;
    void load();
    if (cloud.canManageProject.value) {
      void loadSources();
      void loadTemplates();
    }
  },
  { immediate: true },
);
watch(() => form.source, loadSources);
watch(() => [form.targetProjectId, form.targetApp, form.action], loadTemplates);
</script>

<template>
  <section class="automation-page">
    <header class="page-header">
      <div>
        <p class="eyebrow">APPLICATION WORKFLOWS</p>
        <h1>应用联动</h1>
        <p class="muted">
          将认证、成员资格与活动结果，转为明确的服务权益和通知。
        </p>
      </div>
      <button class="button" :disabled="loading" @click="load">刷新记录</button>
    </header>
    <p v-if="error" class="inline-error" role="alert">{{ error }}</p>
    <p v-if="!cloud.state.projectId" class="empty-state">
      请先选择团队与项目。
    </p>
    <template v-else>
      <article class="panel">
        <h2>我的通知</h2>
        <p v-if="!notifications.length" class="muted">
          有新的认证或活动结果时，会在这里收到规则发送的通知。
        </p>
        <div v-for="n in notifications" :key="n.id" class="notice-row">
          <div>
            <strong>{{ n.title }}</strong>
            <p>{{ n.body }}</p>
            <small>{{ formatDate(new Date(n.createdAt).toISOString()) }}</small>
          </div>
          <button v-if="!n.read" class="button" @click="read(n.id)">
            标为已读</button
          ><span v-else class="muted">已读</span>
        </div>
      </article>
      <template v-if="cloud.canManageProject.value">
        <div class="workflow-layout">
          <form class="panel" @submit.prevent="create">
            <h2>创建联动规则</h2>
            <p class="muted">
              创建人需拥有来源审核/管理权限；赠送资源包还需目标应用运营授权。
            </p>
            <div class="form-grid">
              <label class="field full"
                >规则名称<input
                  aria-label="规则名称"
                  v-model="form.name"
                  required
                  maxlength="100"
                  placeholder="例如：认证通过后发放开发资源"
              /></label>
              <label class="field"
                >事件来源<select aria-label="事件来源" v-model="form.source">
                  <option value="trust.status">Trust 认证结果</option>
                  <option value="eid.status">EID 已确认成员</option>
                  <option value="lottery.won">活动中奖</option>
                </select></label
              >
              <label class="field"
                >{{
                  form.source === "trust.status"
                    ? "认证方案"
                    : form.source === "lottery.won"
                      ? "活动"
                      : "成员资格"
                }}<select
                  :aria-label="
                    form.source === 'trust.status'
                      ? '认证方案'
                      : form.source === 'lottery.won'
                        ? '活动'
                        : '成员资格'
                  "
                  v-model="form.filterId"
                  required
                >
                  <option value="" disabled>请选择</option>
                  <option v-for="s in sources" :key="s.id" :value="s.id">
                    {{ s.name }}
                  </option>
                </select></label
              >
              <label class="field full"
                >执行动作<select aria-label="执行动作" v-model="form.action">
                  <option value="package">授予项目资源包</option>
                  <option value="notify">向本人发送站内通知</option>
                </select></label
              >
              <template v-if="form.action === 'package'"
                ><label class="field"
                  >目标项目<select
                    aria-label="目标项目"
                    v-model="form.targetProjectId"
                    required
                  >
                    <option
                      v-for="p in cloud.state.projects"
                      :key="p.id"
                      :value="p.id"
                    >
                      {{ p.name }}
                    </option>
                  </select></label
                ><label class="field"
                  >资源应用<select
                    aria-label="资源应用"
                    v-model="form.targetApp"
                  >
                    <option value="database">云数据库</option>
                    <option value="storage">对象存储</option>
                  </select></label
                ><label class="field full"
                  >资源包模板<select
                    aria-label="资源包模板"
                    v-model="form.templateId"
                    required
                  >
                    <option value="" disabled>请选择已启用的模板</option>
                    <option v-for="t in templates" :key="t.id" :value="t.id">
                      {{ t.name }}
                    </option>
                  </select></label
                ></template
              >
            </div>
            <p class="policy-note">
              资源赠送作用于指定项目。受益人须仍具有该项目访问权限，每人每条规则仅赠送一次，保持模板有效期；资格撤销后终止对应权益，保留已有资源。认证不会授予管理或审核权限。
            </p>
            <button class="button button-primary" :disabled="busy">
              启用规则
            </button>
          </form>
          <article class="panel">
            <h2>
              已配置规则 <small>{{ rules.length }}</small>
            </h2>
            <p v-if="!rules.length" class="muted">
              尚无自动联动。创建规则后，新的业务事件将自动执行。
            </p>
            <div v-for="r in rules" :key="r.id" class="rule-row">
              <div>
                <strong>{{ r.name }}</strong>
                <p>
                  {{ labels[r.source] }} →
                  {{ r.action === "package" ? "项目资源包" : "站内通知" }}
                </p>
                <small>{{ r.enabled ? "运行中" : "已暂停" }}</small>
              </div>
              <button class="button" :disabled="busy" @click="toggle(r)">
                {{ r.enabled ? "暂停" : "启用" }}
              </button>
            </div>
            <p class="muted">暂停阻止后续执行，已发放权益仍按原有效期生效。</p>
          </article>
        </div>
        <article class="panel">
          <h2>
            执行记录 <small v-if="failed">{{ failed }} 项失败待处理</small>
          </h2>
          <p v-if="!executions.length" class="muted">
            暂无执行记录。规则不追溯创建前的历史事件。
          </p>
          <div class="table-wrap">
            <table v-if="executions.length">
              <thead>
                <tr>
                  <th>规则</th>
                  <th>状态</th>
                  <th>尝试次数</th>
                  <th>时间</th>
                  <th>说明</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="x in executions" :key="x.id">
                  <td>
                    {{ rules.find((r) => r.id === x.ruleId)?.name ?? x.ruleId }}
                  </td>
                  <td>{{ labels[x.state] ?? x.state }}</td>
                  <td>{{ x.attempts }}</td>
                  <td>{{ formatDate(new Date(x.createdAt).toISOString()) }}</td>
                  <td>{{ x.error || "—" }}</td>
                  <td>
                    <button
                      v-if="x.state === 'failed'"
                      class="button"
                      :disabled="busy"
                      @click="retry(x.id)"
                    >
                      重试
                    </button>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <p class="muted">
            执行前重新检查当前授权与应用状态；暂时失败最多自动重试 5
            次。修复授权或配置后可手动重试。
          </p>
        </article>
      </template>
    </template>
  </section>
</template>
<style scoped>
.automation-page {
  display: grid;
  gap: 24px;
}
.automation-page .panel {
  padding: 24px;
}
.page-header,
.notice-row,
.rule-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
}
.eyebrow {
  font-size: 12px;
  letter-spacing: 0.12em;
  color: var(--cloud-blue);
}
.workflow-layout {
  display: grid;
  grid-template-columns: minmax(0, 1.3fr) minmax(0, 1fr);
  gap: 24px;
}
.notice-row,
.rule-row {
  padding: 18px 0;
  border-bottom: 1px solid var(--cloud-border);
}
.notice-row p,
.rule-row p {
  margin: 6px 0;
}
.policy-note {
  font-size: 13px;
  line-height: 1.8;
  background: var(--cloud-blue-soft);
  border-radius: 12px;
  padding: 16px;
  color: var(--cloud-text);
}
h2 small,
.muted {
  font-size: 13px;
  color: var(--cloud-muted);
}
.table-wrap {
  overflow: auto;
}
.form-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 18px;
}
.field {
  display: grid;
  gap: 8px;
  font-size: 14px;
}
.field.full {
  grid-column: 1/-1;
}
.field input,
.field select {
  font: inherit;
  width: 100%;
  min-width: 0;
  padding: 10px 12px;
  border: 1px solid var(--cloud-border);
  border-radius: 9px;
  background: var(--cloud-panel);
  color: var(--cloud-text);
}
.field input:focus,
.field select:focus {
  outline: 2px solid var(--cloud-blue);
  outline-offset: 2px;
}
table {
  width: 100%;
  border-collapse: collapse;
  text-align: left;
  font-size: 13px;
}
th,
td {
  padding: 14px 10px;
  border-bottom: 1px solid var(--cloud-border);
}
.inline-error {
  padding: 14px;
  color: var(--cloud-danger);
  border: 1px solid currentColor;
  border-radius: 10px;
}
@media (max-width: 900px) {
  .workflow-layout {
    grid-template-columns: 1fr;
  }
  .page-header {
    align-items: flex-start;
  }
}
@media (max-width: 560px) {
  .automation-page .panel {
    padding: 18px;
  }
  .form-grid {
    grid-template-columns: 1fr;
  }
  .notice-row,
  .rule-row {
    align-items: flex-start;
  }
}
</style>
