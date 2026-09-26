<script setup lang="ts">
import { computed, ref } from "vue";
import { RouterLink } from "vue-router";
import { useCloud } from "./context";
import CloudIcon from "./CloudIcon.vue";
import CloudState from "./CloudState.vue";
const { state } = useCloud();
const query = ref("");
const category = ref("全部应用");
const categories = computed(() => [
  "全部应用",
  ...new Set(state.catalog.map((app) => app.category)),
]);
const visible = computed(() =>
  state.catalog.filter(
    (app) =>
      (category.value === "全部应用" || app.category === category.value) &&
      `${app.name} ${app.description}`
        .toLowerCase()
        .includes(query.value.trim().toLowerCase()),
  ),
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
const installed = (id: string) =>
  state.installations.find((item) => item.applicationId === id);
const modeLabels: Record<string, string> = {
  native: "统一工作空间",
  identity: "身份服务",
  bearer: "API 连接",
  access_key_pair: "存储连接",
  external: "独立实例",
};
</script>
<template>
  <section class="page-heading">
    <div>
      <span class="eyebrow">APPLICATIONS</span>
      <h1>让应用在这里协作</h1>
      <p>身份、数据与应用工具，在同一个工作空间中使用。</p>
    </div>
    <span class="subtle-count">{{ state.catalog.length }} 个应用</span>
  </section>
  <div class="catalog-toolbar">
    <div class="filter-tabs" aria-label="应用分类">
      <button
        v-for="item in categories"
        :key="item"
        :class="{ active: category === item }"
        @click="category = item"
      >
        {{ item }}
      </button>
    </div>
    <label class="search-field"
      ><CloudIcon name="search" :size="18" /><input
        v-model="query"
        aria-label="搜索应用"
        placeholder="搜索应用或能力"
    /></label>
  </div>
  <div v-if="visible.length" class="catalog-grid">
    <article
      v-for="app in visible"
      :key="app.id"
      :data-testid="`application-card-${app.id}`"
      class="application-card"
      :data-app="app.id"
    >
      <div class="app-card-top">
        <span class="app-mark" :data-app="app.id"
          ><CloudIcon :name="icons[app.id] ?? 'layers'" :size="26" /></span
        ><span
          v-if="installed(app.id)"
          class="badge"
          :class="
            installed(app.id)?.status === 'enabled' ? 'success' : 'neutral'
          "
          >{{
            installed(app.id)?.status === "enabled"
              ? "项目已启用"
              : "项目已停用"
          }}</span
        ><span v-else class="app-category">{{ app.category }}</span>
      </div>
      <h2>{{ app.name }}</h2>
      <p>{{ app.description }}</p>
      <div class="app-card-footer">
        <span class="mode-label"
          ><span class="tiny-dot" />{{
            modeLabels[app.connectionMode] ?? app.connectionMode
          }}</span
        ><RouterLink class="text-link" :to="`/apps/${app.id}`"
          >{{ installed(app.id) ? "打开应用" : "启用应用"
          }}<CloudIcon name="arrow" :size="16"
        /></RouterLink>
      </div>
    </article>
  </div>
  <CloudState
    v-else
    kind="empty"
    title="没有找到匹配的应用"
    description="试试其他关键词，或切换到全部应用。"
    ><button
      class="button"
      @click="
        query = '';
        category = '全部应用';
      "
    >
      清除筛选
    </button></CloudState
  >
  <div class="catalog-footnote">
    <CloudIcon name="info" :size="17" />
    <p>
      各应用使用你的欧拉账号与当前项目权限。审核和运营权限由平台管理员单独授予。
    </p>
  </div>
</template>
