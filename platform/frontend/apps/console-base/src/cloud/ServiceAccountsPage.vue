<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { api, errorMessage, formatDate } from "./api";
import { useCloud } from "./context";
import CloudState from "./CloudState.vue";
import CloudModal from "./CloudModal.vue";
interface Account {
  id: string;
  name: string;
  scopes: { applicationId: string; operations: string[] }[];
  expiresAt: string;
  createdAt: string;
  revokedAt?: string;
  lastUsedAt?: string;
}
const { state, projectPath, canManageProject, notify } = useCloud();
const accounts = ref<Account[]>([]),
  loading = ref(true),
  error = ref(""),
  busy = ref(false),
  secret = ref("");
const name = ref(""),
  applicationId = ref("statistics"),
  operations = ref(["read"]),
  days = ref(30),
  revokeTarget = ref<Account | null>(null);
const controller = new AbortController();
const applications = computed(() =>
  state.catalog.filter((a) =>
    ["weauth", "database", "storage", "statistics", "lottery"].includes(a.id),
  ),
);
const operationLabels: Record<string, string> = {
  read: "读取资源",
  write: "创建与更新",
  manage: "管理与删除",
  secrets: "读取与轮换资源密钥",
};
const availableOperations = computed(() =>
  Object.fromEntries(
    Object.entries(operationLabels).filter(([op]) =>
      applicationId.value === "statistics"
        ? ["read", "manage"].includes(op)
        : ["storage", "lottery"].includes(applicationId.value)
          ? op !== "secrets"
          : true,
    ),
  ),
);
watch(applicationId, () => {
  operations.value = operations.value.filter(
    (op) => op in availableOperations.value,
  );
  if (!operations.value.length) operations.value = ["read"];
});
async function load() {
  if (!canManageProject.value) {
    loading.value = false;
    return;
  }
  loading.value = true;
  error.value = "";
  try {
    accounts.value = (
      await api<{ items: Account[] }>(projectPath("/service-accounts"), {
        signal: controller.signal,
      })
    ).items;
  } catch (e) {
    if (!controller.signal.aborted) error.value = errorMessage(e);
  } finally {
    loading.value = false;
  }
}
async function create() {
  busy.value = true;
  try {
    const out = await api<{ secret: string }>(
      projectPath("/service-accounts"),
      {
        method: "POST",
        body: {
          name: name.value,
          expiresAt: new Date(Date.now() + days.value * 86400000).toISOString(),
          scopes: [
            {
              applicationId: applicationId.value,
              operations: operations.value,
            },
          ],
        },
      },
    );
    secret.value = out.secret;
    name.value = "";
    await load();
  } catch (e) {
    notify(errorMessage(e), "error");
  } finally {
    busy.value = false;
  }
}
async function rotate(a: Account) {
  busy.value = true;
  try {
    const out = await api<{ secret: string }>(
      projectPath(`/service-accounts/${a.id}/rotate`),
      { method: "POST" },
    );
    secret.value = out.secret;
    await load();
    notify("旧密钥已立即失效，请更新调用端。");
  } catch (e) {
    notify(errorMessage(e), "error");
  } finally {
    busy.value = false;
  }
}
async function revoke() {
  if (!revokeTarget.value) return;
  busy.value = true;
  try {
    await api(projectPath(`/service-accounts/${revokeTarget.value.id}`), {
      method: "DELETE",
    });
    revokeTarget.value = null;
    await load();
    notify("服务账号已撤销。");
  } catch (e) {
    notify(errorMessage(e), "error");
  } finally {
    busy.value = false;
  }
}
function expired(a: Account) {
  return new Date(a.expiresAt).getTime() <= Date.now();
}
async function copy() {
  try {
    await navigator.clipboard.writeText(secret.value);
    notify("密钥已复制。");
  } catch {
    notify("请手动复制密钥。", "error");
  }
}
onMounted(load);
onBeforeUnmount(() => {
  controller.abort();
  secret.value = "";
});
</script>
<template>
  <section class="page-heading">
    <div>
      <span class="eyebrow">SERVICE ACCOUNTS</span>
      <h1>服务账号</h1>
      <p>为自动化程序授予当前项目的应用权限，独立管理密钥与有效期。</p>
    </div>
  </section>
  <CloudState
    v-if="!canManageProject"
    kind="empty"
    title="仅项目管理员可以管理服务账号"
  />
  <CloudState v-else-if="loading" kind="loading" title="正在读取服务账号" />
  <CloudState
    v-else-if="error"
    kind="error"
    title="服务账号加载失败"
    :description="error"
    @retry="load"
  />
  <div v-else class="stack">
    <form class="panel account-form" @submit.prevent="create">
      <h2>创建服务账号</h2>
      <div class="account-fields">
        <label class="field"
          >名称<input
            v-model="name"
            required
            maxlength="80"
            placeholder="例如：访问统计报表任务"
        /></label>
        <label class="field"
          >应用<select v-model="applicationId" aria-label="应用">
            <option v-for="a in applications" :key="a.id" :value="a.id">
              {{ a.name }}
            </option>
          </select></label
        >
        <label class="field"
          >有效期<select v-model="days" aria-label="有效期">
            <option :value="7">7 天</option>
            <option :value="30">30 天</option>
            <option :value="90">90 天</option>
            <option :value="365">365 天</option>
          </select></label
        >
      </div>
      <fieldset>
        <legend>允许的操作</legend>
        <label v-for="(label, op) in availableOperations" :key="op"
          ><input v-model="operations" type="checkbox" :value="op" />{{
            label
          }}</label
        >
      </fieldset>
      <p class="muted">
        各项权限独立授予。服务账号不参与个人认证、人工审核与修复审批；无法分配资源包或创建其他账号。
      </p>
      <button
        class="button button-primary"
        :disabled="busy || !operations.length"
      >
        创建并显示密钥
      </button>
    </form>
    <section class="panel">
      <div class="panel-heading">
        <h2>项目服务账号</h2>
        <span class="muted">{{ accounts.length }} 个</span>
      </div>
      <div v-if="accounts.length" class="account-list">
        <article v-for="a in accounts" :key="a.id">
          <div>
            <strong>{{ a.name }}</strong
            ><span class="badge">{{
              a.revokedAt ? "已撤销" : expired(a) ? "已到期" : "有效"
            }}</span>
            <p
              class="muted"
              v-for="scope in a.scopes"
              :key="scope.applicationId"
            >
              {{
                state.catalog.find((c) => c.id === scope.applicationId)?.name ||
                scope.applicationId
              }}
              ·
              {{ scope.operations.map((op) => operationLabels[op]).join("、") }}
            </p>
            <small class="muted"
              >到期 {{ formatDate(a.expiresAt) }} ·
              {{
                a.lastUsedAt
                  ? `上次调用 ${formatDate(a.lastUsedAt)}`
                  : "尚未调用"
              }}</small
            >
          </div>
          <div class="account-actions" v-if="!a.revokedAt">
            <button
              class="button"
              :disabled="busy || expired(a)"
              @click="rotate(a)"
            >
              轮换密钥</button
            ><button class="button" :disabled="busy" @click="revokeTarget = a">
              撤销
            </button>
          </div>
        </article>
      </div>
      <CloudState
        v-else
        kind="empty"
        title="还没有服务账号"
        description="根据实际调用需要授予最少权限。"
      />
    </section>
    <section class="panel account-form">
      <h2>接入方式</h2>
      <p>
        在服务端使用 Bearer 密钥调用专用机器 API。请求不应携带浏览器
        Cookie、Origin 或 CSRF Token。
      </p>
      <code
        >/api/v1/machine/tenants/{{ state.tenantId }}/projects/{{
          state.projectId
        }}/apps/&lt;应用&gt;/&lt;资源路径&gt;</code
      >
      <p class="muted">
        密钥只在创建和轮换时显示一次。请存入部署环境的秘密管理服务。
      </p>
    </section>
  </div>
  <CloudModal
    :model-value="Boolean(secret)"
    title="保存服务账号密钥"
    @update:model-value="secret = ''"
  >
    <p>密钥仅显示这一次。关闭后无法找回；可以再次轮换生成新密钥。</p>
    <textarea
      class="secret"
      :value="secret"
      readonly
      aria-label="服务账号密钥"
    />
    <div class="account-actions">
      <button class="button button-primary" @click="copy">复制密钥</button
      ><button class="button" @click="secret = ''">已保存，关闭</button>
    </div>
  </CloudModal>
  <CloudModal
    v-if="revokeTarget"
    :model-value="true"
    title="撤销服务账号"
    @update:model-value="revokeTarget = null"
    ><p>撤销 {{ revokeTarget.name }} 后，使用该密钥的新请求将立即被拒绝。</p>
    <button class="button button-primary" :disabled="busy" @click="revoke">
      确认撤销
    </button></CloudModal
  >
</template>
<style scoped>
.account-form {
  padding: 24px;
}
.account-fields {
  display: grid;
  grid-template-columns: 2fr 1fr 1fr;
  gap: 16px;
  margin: 20px 0;
}
fieldset {
  border: 1px solid var(--cloud-border);
  border-radius: 10px;
  padding: 16px;
  display: flex;
  flex-wrap: wrap;
  gap: 18px;
}
fieldset label {
  display: flex;
  gap: 8px;
  align-items: center;
}
.account-list article {
  display: flex;
  justify-content: space-between;
  gap: 20px;
  padding: 22px;
  border-top: 1px solid var(--cloud-border);
}
.account-actions {
  display: flex;
  gap: 10px;
  align-items: center;
  flex-wrap: wrap;
}
.badge {
  margin-left: 12px;
  font-size: 12px;
  color: var(--cloud-muted);
}
code {
  display: block;
  overflow-wrap: anywhere;
}
.secret {
  width: 100%;
  min-height: 100px;
  margin: 16px 0;
  font-family: monospace;
}
@media (max-width: 760px) {
  .account-fields {
    grid-template-columns: 1fr;
  }
  .account-list article {
    flex-direction: column;
  }
}
</style>
