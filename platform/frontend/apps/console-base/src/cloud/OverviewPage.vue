<script setup lang="ts">
import { computed, watch, onBeforeUnmount, ref } from "vue";
import { RouterLink } from "vue-router";
import { api, errorMessage, formatDate, roleLabels } from "./api";
import { useCloud } from "./context";
import type { AuditEvent, List } from "./types";
import CloudIcon from "./CloudIcon.vue";
import CloudState from "./CloudState.vue";
const { state, tenant, project, tenantPath, refreshInstallations } = useCloud();
const events = ref<AuditEvent[]>([]);
const auditError = ref("");
const auditLoading = ref(true);
let controller = new AbortController();
let auditSequence = 0;
const enabled = computed(() =>
  state.installations.filter((item) => item.status === "enabled"),
);
async function loadAudit() {
  const sequence = ++auditSequence;
  controller.abort();
  controller = new AbortController();
  const signal = controller.signal;
  events.value = [];
  if (!state.projectId) {
    auditLoading.value = false;
    return;
  }
  auditLoading.value = true;
  auditError.value = "";
  try {
    const result = (
      await api<List<AuditEvent>>(
        tenantPath(
          `/audit?projectId=${encodeURIComponent(state.projectId)}&limit=5`,
        ),
        { signal },
      )
    ).items;
    if (sequence === auditSequence) events.value = result;
  } catch (error) {
    if (sequence === auditSequence && !signal.aborted)
      auditError.value = errorMessage(error);
  } finally {
    if (sequence === auditSequence) auditLoading.value = false;
  }
}
watch(() => [state.tenantId, state.projectId], loadAudit, { immediate: true });
onBeforeUnmount(() => controller.abort());
</script>
<template>
  <section class="page-heading">
    <div>
      <span class="eyebrow">PROJECT OVERVIEW</span>
      <h1>{{ project?.name ?? "项目总览" }}<span class="title-dot">.</span></h1>
      <p>团队的应用、资源与协作，在一个工作空间里有序展开。</p>
    </div>
    <RouterLink to="/catalog" class="button button-primary"
      ><CloudIcon name="plus" :size="18" />添加应用</RouterLink
    >
  </section>
  <div class="overview-hero">
    <div>
      <span class="workspace-label"
        ><CloudIcon name="folder" :size="17" />当前项目</span
      >
      <h2>从一个工作空间，<br />开始下一件事。</h2>
      <p>
        属于 {{ tenant?.name }} ·
        {{ roleLabels[project?.role ?? tenant?.role ?? "viewer"] }}
      </p>
    </div>
    <div class="orbit-illustration" aria-hidden="true">
      <div class="orbit orbit-one" />
      <div class="orbit orbit-two" />
      <span class="orbit-center">E</span
      ><span class="orbit-product orbit-a" data-app="weauth"
        ><CloudIcon name="key" :size="25" /></span
      ><span class="orbit-product orbit-b" data-app="database"
        ><CloudIcon name="database" :size="25" /></span
      ><span class="orbit-product orbit-c" data-app="storage"
        ><CloudIcon name="storage" :size="23" /></span
      ><span class="orbit-product orbit-d" data-app="eid"
        ><CloudIcon name="shield" :size="24"
      /></span>
    </div>
    <div class="hero-bottom">
      <span class="tiny-dot" />项目与资源独立授权<span
        class="hero-project-id"
        >{{ project?.id }}</span
      >
    </div>
  </div>
  <div class="metric-grid">
    <article class="metric-card">
      <span>已启用应用</span
      ><strong
        >{{
          state.installationError || state.installationsLoading
            ? "—"
            : enabled.length
        }}<small>个</small></strong
      ><CloudIcon name="layers" />
    </article>
    <article class="metric-card">
      <span>可用应用</span
      ><strong
        >{{
          state.installationError || state.installationsLoading
            ? "—"
            : state.catalog.length
        }}<small>个</small></strong
      ><CloudIcon name="link" />
      <p>按需启用，独立配置服务依赖</p>
    </article>
    <article class="metric-card">
      <span>可访问项目</span
      ><strong>{{ state.projects.length }}<small>个</small></strong
      ><CloudIcon name="folder" />
    </article>
  </div>
  <section class="panel getting-started">
    <div>
      <h2>让项目开始运转</h2>
      <p class="muted">从启用应用到联动业务，每一步都有明确入口。</p>
    </div>
    <div class="start-links">
      <RouterLink to="/catalog"
        ><b>01</b
        ><span
          >选择应用<small>{{
            enabled.length
              ? `已启用 ${enabled.length} 个应用`
              : "为项目启用第一个应用"
          }}</small></span
        ></RouterLink
      >
      <RouterLink to="/services"
        ><b>02</b
        ><span>检查资源<small>查看配额与服务配置</small></span></RouterLink
      >
      <RouterLink to="/members"
        ><b>03</b
        ><span>邀请协作者<small>安排团队与项目权限</small></span></RouterLink
      >
      <RouterLink to="/automation"
        ><b>04</b
        ><span
          >连接业务流程<small>审核结果、资源权益与通知</small></span
        ></RouterLink
      >
    </div>
  </section>
  <div class="overview-columns">
    <section class="panel">
      <div class="panel-heading">
        <div>
          <h2>项目应用</h2>
          <p class="muted">应用与数据均属于当前项目</p>
        </div>
        <RouterLink class="text-link" to="/catalog"
          >浏览目录<CloudIcon name="arrow" :size="16"
        /></RouterLink>
      </div>
      <CloudState
        v-if="state.installationsLoading"
        kind="loading"
        title="正在读取项目应用"
      /><CloudState
        v-else-if="state.installationError"
        kind="error"
        title="项目应用加载失败"
        :description="state.installationError"
        @retry="refreshInstallations().catch(() => {})"
      />
      <div v-else-if="state.installations.length" class="project-app-list">
        <RouterLink
          v-for="item in state.installations"
          :key="item.id"
          :to="`/apps/${item.applicationId}`"
          class="project-app-row"
          ><span class="app-mark small" :data-app="item.applicationId"
            ><CloudIcon name="layers"
          /></span>
          <div>
            <strong>{{
              state.catalog.find((app) => app.id === item.applicationId)
                ?.name ?? item.applicationId
            }}</strong
            ><span>{{
              item.status === "disabled"
                ? "已停用"
                : state.catalog.find((app) => app.id === item.applicationId)
                      ?.connectionMode === "native"
                  ? "已启用 · 进入应用工作台"
                  : item.connection?.configured
                    ? "已保存连接"
                    : "等待配置连接"
            }}</span>
          </div>
          <CloudIcon name="arrow" :size="18"
        /></RouterLink>
      </div>
      <CloudState
        v-else
        kind="empty"
        title="这里即将有你的第一个应用"
        description="从目录启用应用，在当前项目开始使用。"
        ><RouterLink to="/catalog" class="button"
          >选择应用<CloudIcon name="arrow" :size="16" /></RouterLink
      ></CloudState>
    </section>
    <section class="panel">
      <div class="panel-heading">
        <div>
          <h2>最近动态</h2>
          <p class="muted">当前项目的操作记录</p>
        </div>
        <RouterLink to="/audit" class="text-link">全部</RouterLink>
      </div>
      <CloudState
        v-if="auditLoading"
        kind="loading"
        title="正在读取动态"
      /><CloudState
        v-else-if="auditError"
        kind="error"
        title="动态暂不可用"
        :description="auditError"
        @retry="loadAudit"
      />
      <div v-else-if="events.length" class="activity-list">
        <div v-for="event in events" :key="event.id" class="activity-item">
          <span class="activity-dot" />
          <div>
            <p>{{ event.summary || event.action }}</p>
            <span>{{ formatDate(event.createdAt) }}</span>
          </div>
        </div>
      </div>
      <CloudState
        v-else
        kind="empty"
        title="项目还没有操作记录"
        description="应用、连接与资源的变更会记录在这里。"
      />
    </section>
  </div>
</template>

<style scoped>
.getting-started {
  padding: 24px;
  margin-bottom: 24px;
}
.start-links {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 14px;
  margin-top: 18px;
}
.start-links a {
  display: flex;
  gap: 12px;
  border: 1px solid var(--cloud-border);
  border-radius: 10px;
  padding: 16px;
  line-height: 1.7;
}
.start-links b {
  color: var(--cloud-blue);
}
.start-links small {
  display: block;
  color: var(--cloud-muted);
  font-size: 12px;
}
@media (max-width: 1050px) {
  .start-links {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}
@media (max-width: 600px) {
  .start-links {
    grid-template-columns: 1fr;
  }
}
</style>
