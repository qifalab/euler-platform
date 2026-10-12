<script setup lang="ts">
import { computed, onMounted, onBeforeUnmount, ref } from "vue";
import { api, errorMessage, formatDate } from "./api";
import { useCloud } from "./context";
import type { AuditEvent, List } from "./types";
import CloudIcon from "./CloudIcon.vue";
import CloudState from "./CloudState.vue";
const { state, tenantPath, canManageTeam } = useCloud();
const events = ref<AuditEvent[]>([]);
const loading = ref(true);
const error = ref("");
const query = ref("");
const scope = ref(state.projectId ? "project" : "team");
let controller: AbortController | undefined;
const filtered = computed(() =>
  events.value.filter((event) =>
    `${event.summary} ${event.action} ${event.actorId} ${event.targetId}`
      .toLowerCase()
      .includes(query.value.toLowerCase()),
  ),
);
async function load() {
  controller?.abort();
  controller = new AbortController();
  const signal = controller.signal;
  loading.value = true;
  error.value = "";
  try {
    events.value = (
      await api<List<AuditEvent>>(
        tenantPath(
          `/audit?limit=100${scope.value === "project" && state.projectId ? `&projectId=${encodeURIComponent(state.projectId)}` : ""}`,
        ),
        { signal },
      )
    ).items;
  } catch (err) {
    if (!signal.aborted) error.value = errorMessage(err);
  } finally {
    if (!signal.aborted) loading.value = false;
  }
}
onMounted(load);
onBeforeUnmount(() => controller?.abort());
</script>
<template>
  <section class="page-heading">
    <div>
      <span class="eyebrow">AUDIT LOG</span>
      <h1>每一步，都有迹可循</h1>
      <p>查看团队和项目中的真实操作记录，最多显示最近 100 条。</p>
    </div>
    <button class="button" :disabled="loading" @click="load">
      <CloudIcon name="refresh" :size="17" />刷新记录
    </button>
  </section>
  <section class="panel">
    <div class="panel-heading">
      <div class="filter-tabs">
        <button
          :class="{ active: scope === 'project' }"
          :disabled="!state.projectId"
          @click="
            scope = 'project';
            load();
          "
        >
          当前项目</button
        ><button
          v-if="canManageTeam"
          :class="{ active: scope === 'team' }"
          @click="
            scope = 'team';
            load();
          "
        >
          整个团队
        </button>
      </div>
      <label class="search-field"
        ><CloudIcon name="search" :size="17" /><input
          v-model="query"
          aria-label="筛选审计记录"
          placeholder="筛选操作、成员或资源"
      /></label>
    </div>
    <CloudState
      v-if="loading"
      kind="loading"
      title="正在读取审计记录"
    /><CloudState
      v-else-if="error"
      kind="error"
      title="审计记录加载失败"
      :description="error"
      @retry="load"
    />
    <div v-else-if="filtered.length" class="table-wrap">
      <table class="data-table">
        <thead>
          <tr>
            <th>操作</th>
            <th>操作者</th>
            <th>目标</th>
            <th>时间</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="event in filtered" :key="event.id">
            <td>
              <strong>{{ event.summary || event.action }}</strong
              ><small>{{ event.action }}</small>
            </td>
            <td>
              <code>{{ event.actorId }}</code>
            </td>
            <td>
              <code>{{ event.targetId || "—" }}</code>
            </td>
            <td class="nowrap">{{ formatDate(event.createdAt) }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <CloudState
      v-else
      kind="empty"
      :title="query ? '没有匹配的操作记录' : '暂无操作记录'"
      :description="
        query ? '调整筛选词后再试。' : '团队、项目和应用的变更会出现在这里。'
      "
    />
  </section>
</template>
