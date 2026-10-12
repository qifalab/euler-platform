<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from "vue";
import { api, errorMessage } from "./api";
import { useCloud } from "./context";
import CloudState from "./CloudState.vue";
import CloudModal from "./CloudModal.vue";
interface ProjectDetail {
  id: string;
  name: string;
  status: "active" | "archived";
  role: string;
  tenantId: string;
}
const { state, projectPath, canManageProject, refreshTenants, notify } =
  useCloud();
const detail = ref<ProjectDetail | null>(null),
  name = ref(""),
  loading = ref(true),
  error = ref(""),
  busy = ref(false),
  action = ref(""),
  confirmation = ref("");
const controller = new AbortController();
async function load() {
  loading.value = true;
  error.value = "";
  try {
    detail.value = await api<ProjectDetail>(projectPath(), {
      signal: controller.signal,
    });
    name.value = detail.value.name;
  } catch (e) {
    if (!controller.signal.aborted) error.value = errorMessage(e);
  } finally {
    loading.value = false;
  }
}
async function rename() {
  busy.value = true;
  try {
    await api(projectPath(), { method: "PATCH", body: { name: name.value } });
    await refreshTenants();
    await load();
    notify("项目名称已更新。");
  } catch (e) {
    notify(errorMessage(e), "error");
  } finally {
    busy.value = false;
  }
}
function confirmAction(value: string) {
  action.value = value;
  confirmation.value = "";
}
async function apply() {
  busy.value = true;
  try {
    await api(
      projectPath(action.value === "delete" ? "" : `/${action.value}`),
      {
        method: action.value === "delete" ? "DELETE" : "POST",
        body: { confirmation: confirmation.value },
      },
    );
    const deleted = action.value === "delete";
    action.value = "";
    await refreshTenants();
    if (state.projectId) await load();
    else detail.value = null;
    notify(deleted ? "空项目已删除。" : "项目状态已更新。");
  } catch (e) {
    notify(errorMessage(e), "error");
  } finally {
    busy.value = false;
  }
}
async function exportInventory() {
  busy.value = true;
  try {
    const value = await api(projectPath("/export"));
    const url = URL.createObjectURL(
      new Blob([JSON.stringify(value, null, 2)], { type: "application/json" }),
    );
    const a = document.createElement("a");
    a.href = url;
    a.download = `euler-project-${detail.value?.id}.json`;
    a.click();
    URL.revokeObjectURL(url);
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
      <span class="eyebrow">PROJECT SETTINGS</span>
      <h1>项目设置</h1>
      <p>维护项目资料、暂停应用访问，并保留已有资源。</p>
    </div>
  </section>
  <CloudState v-if="loading" kind="loading" title="正在读取项目设置" />
  <CloudState
    v-else-if="error"
    kind="error"
    title="项目设置加载失败"
    :description="error"
    @retry="load"
  />
  <div v-else-if="detail" class="stack">
    <form class="panel settings-panel" @submit.prevent="rename">
      <h2>基本信息</h2>
      <p class="muted">项目 ID：{{ detail.id }}</p>
      <label class="field"
        >项目名称<input
          v-model="name"
          required
          maxlength="100"
          :disabled="!canManageProject"
      /></label>
      <p>
        当前状态：<strong>{{
          detail.status === "archived" ? "已归档" : "正常运行"
        }}</strong>
      </p>
      <button
        v-if="canManageProject"
        class="button button-primary"
        :disabled="busy"
      >
        保存名称
      </button>
    </form>
    <section class="panel settings-panel">
      <h2>导出配置清单</h2>
      <p>
        下载项目信息、成员、应用与服务账号的配置清单。清单不包含密钥、认证材料、业务数据、数据库内容或对象文件。
      </p>
      <p class="muted">完整恢复需要部署级备份，配置清单不能代替备份。</p>
      <button
        class="button"
        :disabled="busy || !canManageProject"
        @click="exportInventory"
      >
        导出配置清单
      </button>
    </section>
    <section class="panel settings-panel">
      <h2>{{ detail.status === "archived" ? "恢复项目" : "归档项目" }}</h2>
      <p>
        归档后欧拉应用入口、公开业务接口、服务账号与后台任务暂停，新请求被拒绝。全部业务数据和外部资源保留，恢复后应用回到归档前的启用状态。
      </p>
      <p class="muted">
        归档不会撤回已建立的外部数据库连接、公开对象地址或已签发的下载链接。需要完全停止外部访问时，请同时在基础设施侧撤销连接和链接。
      </p>
      <button
        class="button"
        :disabled="busy || !canManageProject"
        @click="
          confirmAction(detail.status === 'archived' ? 'restore' : 'archive')
        "
      >
        {{ detail.status === "archived" ? "恢复项目" : "归档项目" }}
      </button>
    </section>
    <section class="panel settings-panel">
      <h2>删除空项目</h2>
      <p>
        只允许删除已经归档、从未保留应用或服务账号的空项目。包含应用或账号的项目需继续归档，防止业务数据、外部资源与审计记录失去归属。
      </p>
      <button
        class="button danger"
        :disabled="busy || !canManageProject || detail.status !== 'archived'"
        @click="confirmAction('delete')"
      >
        删除空项目
      </button>
    </section>
  </div>
  <CloudModal
    :model-value="Boolean(action)"
    :title="
      action === 'delete'
        ? '删除空项目'
        : action === 'archive'
          ? '归档项目'
          : '恢复项目'
    "
    @update:model-value="action = ''"
  >
    <form @submit.prevent="apply">
      <p>
        请输入项目名称 <strong>{{ detail?.name }}</strong> 确认操作。
      </p>
      <label class="field"
        >项目名称<input v-model="confirmation" required autocomplete="off"
      /></label>
      <div class="actions">
        <button class="button" type="button" @click="action = ''">取消</button
        ><button
          class="button button-primary"
          :disabled="busy || confirmation !== detail?.name"
        >
          确认{{
            action === "delete"
              ? "删除"
              : action === "archive"
                ? "归档"
                : "恢复"
          }}
        </button>
      </div>
    </form>
  </CloudModal>
</template>
<style scoped>
.settings-panel {
  padding: 26px;
}
.settings-panel .field {
  max-width: 480px;
}
.settings-panel p {
  line-height: 1.8;
}
.actions {
  display: flex;
  gap: 12px;
  justify-content: flex-end;
  margin-top: 24px;
}
.danger {
  color: #bc3434;
}
</style>
