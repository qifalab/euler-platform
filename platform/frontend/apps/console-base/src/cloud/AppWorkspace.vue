<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from "vue";
import { RouterLink, useRoute } from "vue-router";
import { api, errorMessage } from "./api";
import { useCloud } from "./context";
import type { AppScope } from "./apps/shared";
import CloudIcon from "./CloudIcon.vue";
import CloudState from "./CloudState.vue";
import CloudModal from "./CloudModal.vue";
import AppModule from "./AppModule.vue";
import "./apps/native.css";
const route = useRoute();
const {
  state,
  tenant,
  project,
  projectPath,
  canManageProject,
  refreshInstallations,
  notify,
} = useCloud();
const application = computed(() =>
  state.catalog.find((a) => a.id === route.params.applicationId),
);
const installation = computed(() =>
  state.installations.find((i) => i.applicationId === application.value?.id),
);
const icons: Record<string, string> = {
  eid: "shield",
  trust: "check",
  weauth: "key",
  database: "database",
  storage: "storage",
  statistics: "chart",
  lottery: "gift",
  witshield: "shield",
};
const scope = ref<AppScope>();
const loading = ref(false),
  error = ref(""),
  busy = ref(false),
  disableOpen = ref(false);
let controller = new AbortController();
async function load() {
  controller.abort();
  controller = new AbortController();
  const signal = controller.signal;
  scope.value = undefined;
  error.value = "";
  loading.value = false;
  if (installation.value?.status !== "enabled" || !application.value) return;
  loading.value = true;
  try {
    const value = await api<AppScope>(
      projectPath(`/apps/${application.value.id}/_context`),
      { signal },
    );
    if (!signal.aborted) scope.value = value;
  } catch (e) {
    if (!signal.aborted) error.value = errorMessage(e);
  } finally {
    if (!signal.aborted) loading.value = false;
  }
}
async function enable() {
  if (!application.value || busy.value) return;
  busy.value = true;
  try {
    if (installation.value)
      await api(projectPath(`/installations/${installation.value.id}`), {
        method: "PATCH",
        body: { status: "enabled" },
      });
    else
      await api(projectPath("/installations"), {
        method: "POST",
        body: { applicationId: application.value.id },
      });
    await refreshInstallations();
    notify("应用已启用，可以开始使用。");
  } catch (e) {
    notify(errorMessage(e), "error");
  } finally {
    busy.value = false;
  }
}
async function disable() {
  if (!installation.value || busy.value) return;
  busy.value = true;
  try {
    await api(projectPath(`/installations/${installation.value.id}`), {
      method: "PATCH",
      body: { status: "disabled" },
    });
    disableOpen.value = false;
    await refreshInstallations();
    notify("应用已停用，现有数据已保留。");
  } catch (e) {
    notify(errorMessage(e), "error");
  } finally {
    busy.value = false;
  }
}
watch(
  () =>
    `${state.tenantId}:${state.projectId}:${application.value?.id}:${installation.value?.status}`,
  load,
  { immediate: true },
);
onBeforeUnmount(() => controller.abort());
</script>
<template>
  <template v-if="application">
    <nav class="app-breadcrumb" aria-label="当前位置">
      <RouterLink to="/">{{ tenant?.name }} / {{ project?.name }}</RouterLink
      ><CloudIcon name="arrow" :size="13" /><span>{{ application.name }}</span>
    </nav>
    <section class="page-heading app-workspace-heading">
      <div class="row">
        <span class="app-mark" :data-app="application.id"
          ><CloudIcon :name="icons[application.id] ?? 'layers'" :size="26"
        /></span>
        <div>
          <span class="eyebrow">{{ application.category }}</span>
          <h1>{{ application.name }}</h1>
          <p>{{ application.description }}</p>
        </div>
      </div>
      <button
        v-if="canManageProject && installation?.status === 'enabled'"
        class="button"
        @click="disableOpen = true"
      >
        应用设置
      </button>
    </section>
    <CloudState
      v-if="state.installationsLoading || loading"
      kind="loading"
      title="正在读取应用空间"
    />
    <CloudState
      v-else-if="state.installationError || error"
      kind="error"
      title="应用暂时无法打开"
      :description="state.installationError || error"
      @retry="load"
    />
    <section
      v-else-if="installation?.status !== 'enabled'"
      class="panel app-activation"
    >
      <span class="app-mark large" :data-app="application.id"
        ><CloudIcon :name="icons[application.id] ?? 'layers'" :size="32"
      /></span>
      <h2>{{ installation ? "此应用已暂停使用" : "在当前项目启用应用" }}</h2>
      <p>
        在 {{ project?.name }} 中使用{{
          application.name
        }}。应用数据归属于这个项目，你的账号和项目权限由欧拉统一管理。
      </p>
      <button
        v-if="canManageProject"
        class="button button-primary"
        :disabled="busy"
        @click="enable"
      >
        {{ busy ? "正在启用…" : installation ? "重新启用" : "启用应用" }}
      </button>
      <p v-else class="muted">请联系项目管理员启用。</p>
    </section>
    <AppModule
      v-else-if="scope"
      :key="`${scope.installationId}:${scope.permissions.join(',')}`"
      :scope="scope"
    />
    <CloudModal
      v-model="disableOpen"
      title="应用设置"
      description="停用会关闭当前项目的控制台及由欧拉提供的业务入口，保留现有数据。已签发的短期链接、数据库连接和存储桶访问策略按各自配置继续生效。"
      ><div class="stack">
        <p>当前应用 · {{ application.name }}</p>
        <p>所属项目 · {{ project?.name }}</p>
        <div class="modal-actions">
          <button class="button" :disabled="busy" @click="disableOpen = false">
            保持启用</button
          ><button
            class="button button-danger"
            :disabled="busy"
            @click="disable"
          >
            {{ busy ? "正在停用…" : "停用应用" }}
          </button>
        </div>
      </div></CloudModal
    >
  </template>
  <CloudState v-else kind="empty" title="没有找到此应用"
    ><RouterLink to="/catalog" class="button"
      >返回应用目录</RouterLink
    ></CloudState
  >
</template>
