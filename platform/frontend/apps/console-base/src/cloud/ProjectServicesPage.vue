<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from "vue";
import { RouterLink } from "vue-router";
import { api, errorMessage } from "./api";
import { useCloud } from "./context";
import CloudState from "./CloudState.vue";
interface Quota {
  mysqlMB: number;
  mysqlCount: number;
  postgresMB: number;
  postgresCount: number;
  storageBytes: number;
  buckets: number;
}
interface DatabaseUsage {
  quota: Quota;
  mysqlUsedBytes: number;
  mysqlCount: number;
  postgresUsedBytes: number;
  postgresCount: number;
  engines: Record<string, boolean>;
}
interface StorageUsage {
  quota: Quota;
  usedBytes: number;
  reservedBytes: number;
  bucketCount: number;
  objectCount: number;
  configured: boolean;
}
const { state, project, projectPath } = useCloud();
const database = ref<DatabaseUsage | null>(null),
  storage = ref<StorageUsage | null>(null);
const loading = ref(false),
  errors = ref<Record<string, string>>({});
let controller = new AbortController(),
  sequence = 0;
const enabled = computed(() =>
  state.installations.filter((i) => i.status === "enabled"),
);
const isEnabled = (id: string) =>
  enabled.value.some((i) => i.applicationId === id);
function bytes(n: number) {
  if (!n) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  const i = Math.min(4, Math.floor(Math.log(n) / Math.log(1024)));
  return `${(n / 1024 ** i).toLocaleString(undefined, { maximumFractionDigits: 1 })} ${units[i]}`;
}
function percent(used: number, total: number) {
  return total > 0
    ? Math.min(100, Math.max(0, (used / total) * 100))
    : used > 0
      ? 100
      : 0;
}
async function load() {
  const current = ++sequence;
  controller.abort();
  controller = new AbortController();
  const signal = controller.signal;
  database.value = null;
  storage.value = null;
  errors.value = {};
  loading.value = true;
  try {
    if (
      !state.projectId ||
      project.value?.status === "archived" ||
      state.installationsLoading
    )
      return;
    const ids = ["database", "storage"].filter(isEnabled);
    const result = await Promise.allSettled(
      ids.map((id) =>
        api<DatabaseUsage | StorageUsage>(projectPath(`/apps/${id}/quota`), {
          signal,
        }),
      ),
    );
    if (current !== sequence) return;
    result.forEach((r, i) => {
      const id = ids[i];
      if (r.status === "rejected") errors.value[id] = errorMessage(r.reason);
      else if (id === "database") database.value = r.value as DatabaseUsage;
      else storage.value = r.value as StorageUsage;
    });
  } finally {
    if (current === sequence) loading.value = false;
  }
}
watch(
  () => [
    state.tenantId,
    state.projectId,
    state.installationsLoading,
    state.installations.map((i) => `${i.applicationId}:${i.status}`).join(","),
  ],
  load,
  { immediate: true },
);
onBeforeUnmount(() => {
  ++sequence;
  controller.abort();
});
</script>
<template>
  <section class="page-heading">
    <div>
      <span class="eyebrow">RESOURCES & USAGE</span>
      <h1>资源与用量<span class="title-dot">.</span></h1>
      <p>查看当前项目的资源配额、已用容量与配置状态。</p>
    </div>
    <button class="button" :disabled="loading" @click="load">刷新用量</button>
  </section>
  <CloudState v-if="loading" kind="loading" title="正在读取资源用量" />
  <CloudState
    v-else-if="state.installationError"
    kind="error"
    title="项目应用暂不可用"
    :description="state.installationError"
  />
  <CloudState
    v-else-if="project?.status === 'archived'"
    kind="empty"
    title="项目已归档"
    description="恢复项目后可重新查看资源用量。已有资源继续保留。"
  />
  <template v-else>
    <div class="usage-grid">
      <section class="panel usage-card">
        <div class="panel-heading">
          <h2>云数据库</h2>
          <RouterLink to="/apps/database" class="text-link"
            >管理数据库 →</RouterLink
          >
        </div>
        <CloudState
          v-if="errors.database"
          kind="error"
          title="数据库用量读取失败"
          :description="errors.database"
          @retry="load"
        />
        <template v-else-if="database">
          <div
            v-for="engine in ['mysql', 'postgresql'] as const"
            :key="engine"
            class="usage-engine"
          >
            <h3>
              {{ engine === "mysql" ? "MySQL" : "PostgreSQL" }}
              <small>{{
                database.engines[engine] ? "引擎已配置" : "引擎尚未配置"
              }}</small>
            </h3>
            <p>
              <strong>{{
                bytes(
                  engine === "mysql"
                    ? database.mysqlUsedBytes
                    : database.postgresUsedBytes,
                )
              }}</strong>
              /
              {{
                bytes(
                  (engine === "mysql"
                    ? database.quota.mysqlMB
                    : database.quota.postgresMB) *
                    1024 *
                    1024,
                )
              }}
            </p>
            <progress
              :aria-label="`${engine} 容量使用率`"
              max="100"
              :value="
                percent(
                  engine === 'mysql'
                    ? database.mysqlUsedBytes
                    : database.postgresUsedBytes,
                  (engine === 'mysql'
                    ? database.quota.mysqlMB
                    : database.quota.postgresMB) *
                    1024 *
                    1024,
                )
              "
            />
            <p class="muted">
              数据库数量
              {{
                engine === "mysql"
                  ? database.mysqlCount
                  : database.postgresCount
              }}
              /
              {{
                engine === "mysql"
                  ? database.quota.mysqlCount
                  : database.quota.postgresCount
              }}
            </p>
          </div>
          <p class="muted note">
            容量来自最近一次资源检测。配置状态表示已设置服务连接，实时依赖健康由部署管理员检查。
          </p> </template
        ><CloudState
          v-else
          kind="empty"
          title="尚未启用云数据库"
          description="启用后可以在这里查看数据库容量与配额。"
        />
      </section>
      <section class="panel usage-card">
        <div class="panel-heading">
          <h2>对象存储</h2>
          <RouterLink to="/apps/storage" class="text-link"
            >管理存储 →</RouterLink
          >
        </div>
        <CloudState
          v-if="errors.storage"
          kind="error"
          title="存储用量读取失败"
          :description="errors.storage"
          @retry="load"
        />
        <template v-else-if="storage">
          <p class="service-status">
            {{ storage.configured ? "存储服务已配置" : "存储服务尚未配置" }}
          </p>
          <p class="usage-number">
            <strong>{{ bytes(storage.usedBytes) }}</strong> /
            {{ bytes(storage.quota.storageBytes) }}
          </p>
          <progress
            aria-label="存储容量使用率"
            max="100"
            :value="
              percent(
                storage.usedBytes + storage.reservedBytes,
                storage.quota.storageBytes,
              )
            "
          />
          <dl>
            <div>
              <dt>上传预留</dt>
              <dd>{{ bytes(storage.reservedBytes) }}</dd>
            </div>
            <div>
              <dt>存储桶</dt>
              <dd>{{ storage.bucketCount }} / {{ storage.quota.buckets }}</dd>
            </div>
            <div>
              <dt>对象数量</dt>
              <dd>{{ storage.objectCount.toLocaleString() }}</dd>
            </div>
          </dl>
          <p class="muted note">
            上传预留占用可用配额。版本保留策略与实际数据面用量可在存储工作台查看。
          </p> </template
        ><CloudState
          v-else
          kind="empty"
          title="尚未启用对象存储"
          description="启用后可以在这里查看存储桶、容量与上传预留。"
        />
      </section>
    </div>
    <section class="panel usage-card">
      <div class="panel-heading">
        <div>
          <h2>项目应用</h2>
          <p class="muted">启用状态与部署版本；应用中的外部服务需独立配置。</p>
        </div>
        <RouterLink to="/catalog" class="text-link">应用目录 →</RouterLink>
      </div>
      <div class="service-list">
        <RouterLink
          v-for="app in state.catalog"
          :key="app.id"
          :to="`/apps/${app.id}`"
          ><span class="service-dot" :style="{ background: app.color }" /><span
            ><strong>{{ app.name }}</strong
            ><small>{{
              app.version ? `v${app.version}` : "内置应用"
            }}</small></span
          ><span class="service-label">{{
            isEnabled(app.id) ? "已启用" : "未启用"
          }}</span></RouterLink
        >
      </div>
    </section>
  </template>
</template>
<style scoped>
.usage-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 24px;
  margin-bottom: 24px;
}
.usage-card {
  padding: 26px;
}
.usage-engine + .usage-engine {
  border-top: 1px solid var(--cloud-border);
  padding-top: 12px;
}
.usage-engine h3 {
  display: flex;
  gap: 12px;
  align-items: center;
  flex-wrap: wrap;
}
.usage-engine h3 small,
.service-status {
  font-size: 12px;
  font-weight: 400;
  color: var(--cloud-muted);
}
.usage-engine strong,
.usage-number strong {
  font-size: 24px;
}
progress {
  width: 100%;
  height: 9px;
  accent-color: var(--cloud-blue);
}
.note {
  font-size: 12px;
  line-height: 1.8;
}
dl div {
  display: flex;
  justify-content: space-between;
  padding: 12px 0;
  border-bottom: 1px solid var(--cloud-border);
}
dd {
  margin: 0;
}
.service-list {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px;
}
.service-list a {
  display: flex;
  align-items: center;
  gap: 12px;
  border: 1px solid var(--cloud-border);
  border-radius: 10px;
  padding: 16px;
  min-width: 0;
}
.service-list small {
  display: block;
  color: var(--cloud-muted);
  margin-top: 5px;
}
.service-dot {
  width: 9px;
  height: 9px;
  flex-shrink: 0;
  border-radius: 50%;
}
.service-label {
  margin-left: auto;
  font-size: 12px;
  white-space: nowrap;
}
@media (max-width: 800px) {
  .usage-grid,
  .service-list {
    grid-template-columns: 1fr;
  }
}
</style>
