<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from "vue";
import { api, errorMessage, formatDate } from "./api";
import { useCloud } from "./context";
import type { List, Member } from "./types";
import CloudState from "./CloudState.vue";
interface Grant {
  id: string;
  applicationId: string;
  userId: string;
  displayName: string;
  permission: string;
  createdAt: number;
}
const { state, tenantPath, projectPath, notify } = useCloud();
const grants = ref<Grant[]>([]),
  members = ref<Member[]>([]);
const loading = ref(true),
  error = ref(""),
  busy = ref(false);
const applicationId = ref("trust"),
  userId = ref(""),
  permission = ref("review");
const controller = new AbortController();
async function load() {
  if (!state.session?.platformAdmin) {
    loading.value = false;
    return;
  }
  loading.value = true;
  error.value = "";
  try {
    const [g, team, project] = await Promise.all([
      api<List<Grant>>(
        `/api/v1/platform/grants?tenantId=${encodeURIComponent(state.tenantId)}&projectId=${encodeURIComponent(state.projectId)}`,
        { signal: controller.signal },
      ),
      api<List<Member>>(tenantPath("/members"), { signal: controller.signal }),
      api<List<Member>>(projectPath("/members"), { signal: controller.signal }),
    ]);
    grants.value = g.items;
    const ids = new Set(project.items.map((m) => m.userId));
    members.value = team.items.filter(
      (m) => ids.has(m.userId) || ["owner", "admin"].includes(m.role),
    );
    if (!members.value.some((m) => m.userId === userId.value))
      userId.value = members.value[0]?.userId ?? "";
  } catch (e) {
    if (!controller.signal.aborted) error.value = errorMessage(e);
  } finally {
    loading.value = false;
  }
}
async function grant() {
  busy.value = true;
  try {
    await api("/api/v1/platform/grants", {
      method: "POST",
      body: {
        tenantId: state.tenantId,
        projectId: state.projectId,
        applicationId: applicationId.value,
        userId: userId.value,
        permission: permission.value,
      },
    });
    await load();
    notify("应用权限已授予。");
  } catch (e) {
    notify(errorMessage(e), "error");
  } finally {
    busy.value = false;
  }
}
async function revoke(g: Grant) {
  busy.value = true;
  try {
    await api(`/api/v1/platform/grants/${g.id}`, { method: "DELETE" });
    await load();
    notify("权限已撤销。");
  } catch (e) {
    notify(errorMessage(e), "error");
  } finally {
    busy.value = false;
  }
}
onMounted(load);
onBeforeUnmount(() => controller.abort());
</script>
<template>
  <section class="page-heading">
    <div>
      <span class="eyebrow">APPLICATION PERMISSIONS</span>
      <h1>应用审核与运营权限</h1>
      <p>为当前项目成员单独授予认证审核和产品运营权限。</p>
    </div>
  </section>
  <CloudState
    v-if="!state.session?.platformAdmin"
    kind="empty"
    title="仅平台管理员可以分配这些权限"
    description="普通项目权限可在成员与邀请中管理。"
  />
  <CloudState v-else-if="loading" kind="loading" title="正在读取应用授权" />
  <CloudState
    v-else-if="error"
    kind="error"
    title="授权信息加载失败"
    :description="error"
    @retry="load"
  />
  <div v-else class="stack">
    <form class="panel permission-form" @submit.prevent="grant">
      <h2>授予权限</h2>
      <div class="permission-fields">
        <label class="field"
          >应用<select v-model="applicationId" aria-label="应用">
            <option v-for="a in state.catalog" :key="a.id" :value="a.id">
              {{ a.name }}
            </option>
          </select></label
        ><label class="field"
          >项目成员<select v-model="userId" aria-label="项目成员" required>
            <option v-for="m in members" :key="m.userId" :value="m.userId">
              {{ m.displayName }} · {{ m.userId.slice(-6) }}
            </option>
          </select></label
        ><label class="field"
          >权限<select v-model="permission" aria-label="权限">
            <option value="review">认证审核</option>
            <option value="admin">产品运营</option>
          </select></label
        ><button class="button button-primary" :disabled="busy || !userId">
          授予权限
        </button>
      </div>
      <p class="muted">
        认证审核用于 EID / Trust
        的申请审阅；产品运营用于方案、资源包等管理。授权限于所选项目和应用。
      </p>
    </form>
    <section class="panel">
      <div class="panel-heading">
        <h2>当前授权</h2>
        <span class="muted">{{ grants.length }} 项</span>
      </div>
      <div v-if="grants.length" class="permission-list">
        <article v-for="g in grants" :key="g.id">
          <div>
            <strong>{{ g.displayName }}</strong>
            <p class="muted">
              {{ state.catalog.find((a) => a.id === g.applicationId)?.name }} ·
              {{ g.permission === "review" ? "认证审核" : "产品运营" }}
            </p>
            <small class="muted">{{
              formatDate(new Date(g.createdAt).toISOString())
            }}</small>
          </div>
          <button class="button" :disabled="busy" @click="revoke(g)">
            撤销授权
          </button>
        </article>
      </div>
      <CloudState
        v-else
        kind="empty"
        title="当前项目还没有单独授权"
        description="团队或项目管理员身份不会自动产生认证审核权。"
      />
    </section>
  </div>
</template>
<style scoped>
.permission-form {
  padding: 26px;
}
.permission-fields {
  display: grid;
  grid-template-columns: 1fr 1fr 1fr auto;
  gap: 16px;
  align-items: end;
  margin: 22px 0;
}
.permission-list article {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 18px 24px;
  border-top: 1px solid var(--cloud-border);
}
@media (max-width: 900px) {
  .permission-fields {
    grid-template-columns: 1fr 1fr;
  }
}
@media (max-width: 550px) {
  .permission-fields {
    grid-template-columns: 1fr;
  }
}
</style>
