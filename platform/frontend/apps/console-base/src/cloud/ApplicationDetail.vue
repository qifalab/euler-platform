<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref, watch } from "vue";
import { useRoute, RouterLink } from "vue-router";
import {
  api,
  safeExternalURL as safeUrl,
  errorMessage as messageOf,
  formatDate,
  type Application,
  type Installation,
  type Summary,
  type Resource,
} from "./api";
import { useCloud } from "./context";
import CloudIcon from "./CloudIcon.vue";
import CloudState from "./CloudState.vue";
import CloudModal from "./CloudModal.vue";

const route = useRoute();
const cloud = useCloud();
const applicationId = computed(() => String(route.params.applicationId ?? ""));
const application = computed<Application | undefined>(() =>
  cloud.state.catalog.find((item) => item.id === applicationId.value),
);
const installation = computed<Installation | undefined>(() =>
  cloud.state.installations.find(
    (item) =>
      item.applicationId === applicationId.value &&
      item.tenantId === cloud.state.tenantId &&
      item.projectId === cloud.state.projectId,
  ),
);
const enabled = computed(() => installation.value?.status === "enabled");
const contextKey = computed(
  () =>
    `${cloud.state.tenantId}/${cloud.state.projectId}/${applicationId.value}`,
);
const contextLoading = computed(
  () => cloud.state.contextLoading || cloud.state.installationsLoading,
);
const canManageConnection = computed(() =>
  Boolean(
    cloud.canManageProject.value &&
      cloud.state.projectId &&
      !contextLoading.value,
  ),
);
const canWriteResources = computed(() =>
  Boolean(
    cloud.tenant.value &&
      cloud.tenant.value.role !== "viewer" &&
      (cloud.canManageProject.value ||
        cloud.project.value?.role === "member") &&
      cloud.state.projectId &&
      !contextLoading.value,
  ),
);

const summary = ref<Summary | null>(null);
const resources = ref<Resource[]>([]);
const summaryLoading = ref(false);
const resourcesLoading = ref(false);
const summaryError = ref("");
const resourcesError = ref("");
const resourcesUnsupported = ref(false);
const busy = ref("");
const connectionOpen = ref(false);
const createOpen = ref(false);
const confirmOpen = ref(false);
const formError = ref("");
const confirmation = ref<{
  kind: "connection" | "resource";
  resource?: Resource;
} | null>(null);
const connectionForm = reactive({
  baseUrl: "",
  credential: "",
  accessKey: "",
  secretKey: "",
});
const siteForm = reactive({ name: "", domains: "" });

let requestVersion = 0;
let mutationVersion = 0;
let readController: AbortController | null = null;
let writeController: AbortController | null = null;

// A response belongs to the selected project, never merely to the current component.
function isCurrent(key: string, version: number) {
  return key === contextKey.value && version === requestVersion;
}
function isUnsupported(error: unknown) {
  const value = error as {
    code?: string;
    status?: number;
    error?: { code?: string };
  } | null;
  return (
    value?.code?.toLowerCase() === "unsupported" ||
    value?.error?.code?.toLowerCase() === "unsupported" ||
    value?.status === 501
  );
}
function pathFor(item: Installation, suffix = "") {
  return `${cloud.projectPath("/installations")}/${encodeURIComponent(item.id)}${suffix}`;
}
const summaryLabels: Record<string, string> = {
  ready: "连接正常",
  unconfigured: "待配置",
  unavailable: "暂不可用",
  unsupported: "暂不支持",
};
const summaryTone = computed(() =>
  summary.value?.state === "ready"
    ? "success"
    : summary.value?.state === "unavailable"
      ? "danger"
      : "warning",
);
const verificationLabels: Record<string, string> = {
  verified: "已验证",
  unverified: "未验证",
  pending: "审核中",
  rejected: "未通过",
  approved: "已通过",
  individual: "个人",
  enterprise: "企业",
  personal: "个人",
  organization: "组织",
};
const connectionModeLabels: Record<string, string> = {
  identity: "使用当前登录身份",
  bearer: "使用独立账号访问令牌",
  access_key_pair: "使用访问密钥对",
  external: "外部应用入口",
};
const connectionModeLabel = computed(
  () =>
    connectionModeLabels[application.value?.connectionMode ?? ""] ??
    "按应用要求配置连接",
);
const identityConnection = computed(
  () => application.value?.connectionMode === "identity",
);
const keyPairConnection = computed(
  () => application.value?.connectionMode === "access_key_pair",
);
const bearerConnection = computed(
  () => application.value?.connectionMode === "bearer",
);
const capabilities = computed(() => application.value?.capabilities ?? []);
const canCreateSite = computed(
  () =>
    applicationId.value === "weauth" &&
    capabilities.value.includes("resources:create"),
);
const canDeleteResource = computed(() =>
  capabilities.value.includes("resources:delete"),
);
const appIcons: Record<string, string> = {
  eid: "key",
  trust: "shield",
  weauth: "globe",
  database: "database",
  storage: "storage",
  statistics: "chart",
  lottery: "gift",
  witshield: "shield",
};
const appIcon = computed(() => appIcons[applicationId.value] ?? "layers");

async function loadDetails() {
  readController?.abort();
  const version = ++requestVersion;
  const key = contextKey.value;
  const item = installation.value;
  summary.value = null;
  resources.value = [];
  summaryError.value = "";
  resourcesError.value = "";
  resourcesUnsupported.value = false;
  summaryLoading.value = false;
  resourcesLoading.value = false;
  if (!item || !enabled.value) return;
  readController = new AbortController();
  const signal = readController.signal;
  const path = pathFor(item);
  summaryLoading.value = true;
  resourcesLoading.value = true;
  await Promise.allSettled([
    api<Summary>(`${path}/summary`, { signal })
      .then((value) => {
        if (isCurrent(key, version)) summary.value = value;
      })
      .catch((error: unknown) => {
        if (!isCurrent(key, version) || signal.aborted) return;
        if (isUnsupported(error))
          summary.value = {
            state: "unsupported",
            message: messageOf(error),
          } as Summary;
        else summaryError.value = messageOf(error);
      })
      .finally(() => {
        if (isCurrent(key, version)) summaryLoading.value = false;
      }),
    api<{ items: Resource[] }>(`${path}/resources`, { signal })
      .then((value) => {
        if (isCurrent(key, version)) resources.value = value.items;
      })
      .catch((error: unknown) => {
        if (!isCurrent(key, version) || signal.aborted) return;
        resourcesUnsupported.value = isUnsupported(error);
        resourcesError.value = messageOf(error);
      })
      .finally(() => {
        if (isCurrent(key, version)) resourcesLoading.value = false;
      }),
  ]);
}

// Secrets exist only in this form and the request body; they are never repopulated.
function clearConnectionForm() {
  connectionForm.baseUrl = "";
  connectionForm.credential = "";
  connectionForm.accessKey = "";
  connectionForm.secretKey = "";
}
function resetDialogs() {
  connectionOpen.value = false;
  createOpen.value = false;
  confirmOpen.value = false;
  confirmation.value = null;
  formError.value = "";
  clearConnectionForm();
  siteForm.name = "";
  siteForm.domains = "";
}
watch(contextKey, () => {
  ++mutationVersion;
  writeController?.abort();
  busy.value = "";
  resetDialogs();
});
watch(
  () =>
    `${contextKey.value}/${installation.value?.id ?? ""}/${installation.value?.status ?? ""}`,
  () => void loadDetails(),
  { immediate: true },
);
watch(connectionOpen, (open) => {
  if (!open) clearConnectionForm();
});
onBeforeUnmount(() => {
  ++requestVersion;
  ++mutationVersion;
  readController?.abort();
  writeController?.abort();
  clearConnectionForm();
});

async function mutate(
  name: string,
  work: (signal: AbortSignal) => Promise<unknown>,
  success: string,
) {
  if (busy.value) return;
  const key = contextKey.value;
  const version = ++mutationVersion;
  writeController = new AbortController();
  const signal = writeController.signal;
  busy.value = name;
  formError.value = "";
  try {
    await work(signal);
    if (key !== contextKey.value || version !== mutationVersion) return;
    resetDialogs();
    cloud.notify(success, "success");
    try {
      await cloud.refreshInstallations();
    } catch {
      if (key === contextKey.value && version === mutationVersion)
        cloud.notify(
          "操作已完成，但项目应用暂时无法刷新，请稍后重试。",
          "error",
        );
      return;
    }
    if (key === contextKey.value && version === mutationVersion)
      await loadDetails();
  } catch (error) {
    if (
      key !== contextKey.value ||
      version !== mutationVersion ||
      signal.aborted
    )
      return;
    formError.value = messageOf(error);
    cloud.notify(messageOf(error), "error");
  } finally {
    if (key === contextKey.value && version === mutationVersion)
      busy.value = "";
  }
}
function enableApplication() {
  if (!application.value || !canManageConnection.value) return;
  const item = installation.value;
  const path = item ? pathFor(item) : cloud.projectPath("/installations");
  const body = item
    ? { status: "enabled" }
    : { applicationId: applicationId.value };
  void mutate(
    "enable",
    (signal) => api(path, { method: item ? "PATCH" : "POST", body, signal }),
    "应用已在当前项目启用",
  );
}
function openConnection() {
  if (!canManageConnection.value || !enabled.value || busy.value) return;
  clearConnectionForm();
  connectionForm.baseUrl = installation.value?.connection?.baseUrl ?? "";
  formError.value = "";
  connectionOpen.value = true;
}
function saveConnection() {
  const item = installation.value;
  if (!item || !enabled.value || !canManageConnection.value) return;
  const url = safeUrl(connectionForm.baseUrl.trim());
  if (!url) {
    formError.value = "请输入有效的 HTTP 或 HTTPS 服务地址。";
    return;
  }
  const body: { baseUrl: string; credential?: string } = { baseUrl: url };
  if (bearerConnection.value && connectionForm.credential)
    body.credential = connectionForm.credential;
  if (
    keyPairConnection.value &&
    (connectionForm.accessKey || connectionForm.secretKey)
  ) {
    if (!connectionForm.accessKey || !connectionForm.secretKey) {
      formError.value =
        "请同时填写 Access Key 与 Secret Key，或全部留空以保留原凭据。";
      return;
    }
    body.credential = JSON.stringify({
      kind: "access_key_pair",
      accessKey: connectionForm.accessKey,
      secretKey: connectionForm.secretKey,
    });
  }
  const path = pathFor(item, "/connection");
  void mutate(
    "connection",
    (signal) => api(path, { method: "PUT", body, signal }),
    "连接已保存",
  );
  connectionForm.credential = "";
  connectionForm.accessKey = "";
  connectionForm.secretKey = "";
}
function openCreateSite() {
  if (
    !canWriteResources.value ||
    !canCreateSite.value ||
    !enabled.value ||
    busy.value
  )
    return;
  formError.value = "";
  siteForm.name = "";
  siteForm.domains = "";
  createOpen.value = true;
}
function createSite() {
  const item = installation.value;
  if (
    !item ||
    !enabled.value ||
    !canWriteResources.value ||
    !canCreateSite.value
  )
    return;
  const domains = [
    ...new Set(
      siteForm.domains
        .split(/[\s,，;；]+/)
        .map((value) => value.trim())
        .filter(Boolean),
    ),
  ];
  const name = siteForm.name.trim();
  if (!name || !domains.length) {
    formError.value = "请填写站点名称及至少一个域名。";
    return;
  }
  if (domains.some((value) => /[\/:?#]/.test(value))) {
    formError.value =
      "域名仅填写主机名，例如 example.com，请勿包含协议或路径。";
    return;
  }
  const path = pathFor(item, "/resources");
  void mutate(
    "create-site",
    (signal) => api(path, { method: "POST", body: { name, domains }, signal }),
    "站点已创建",
  );
}
function askRemoveConnection() {
  if (!canManageConnection.value || !enabled.value || busy.value) return;
  formError.value = "";
  confirmation.value = { kind: "connection" };
  confirmOpen.value = true;
}
function askDeleteResource(resource: Resource) {
  if (
    !canWriteResources.value ||
    !enabled.value ||
    !canDeleteResource.value ||
    busy.value
  )
    return;
  formError.value = "";
  confirmation.value = { kind: "resource", resource };
  confirmOpen.value = true;
}
function confirmDelete() {
  const item = installation.value;
  const target = confirmation.value;
  if (!item || !target || !enabled.value) return;
  if (
    target.kind === "connection"
      ? !canManageConnection.value
      : !canWriteResources.value
  )
    return;
  if (
    target.kind === "resource" &&
    (!canDeleteResource.value || !target.resource)
  )
    return;
  const suffix =
    target.kind === "connection"
      ? "/connection"
      : `/resources/${encodeURIComponent(target.resource!.id)}`;
  const path = pathFor(item, suffix);
  void mutate(
    "delete",
    (signal) => api(path, { method: "DELETE", signal }),
    target.kind === "connection"
      ? "连接已移除，远端资源未被删除"
      : "资源已删除",
  );
}
</script>

<template>
  <div class="application-detail stack">
    <RouterLink to="/catalog" class="back-link muted"
      ><CloudIcon name="arrow" class="back-arrow" :size="16" />
      应用目录</RouterLink
    >
    <CloudState
      v-if="!application"
      kind="empty"
      title="未找到此应用"
      description="请返回应用目录，选择当前可用的应用。"
    />
    <template v-else>
      <header class="page-heading application-heading">
        <div class="application-title">
          <span class="app-mark detail-mark" :data-app="application.id"
            ><CloudIcon :name="appIcon" :size="28"
          /></span>
          <div>
            <p class="eyebrow">{{ application.category }}</p>
            <h1>{{ application.name }}</h1>
            <p class="muted app-description">{{ application.description }}</p>
          </div>
        </div>
        <div class="row heading-actions">
          <span
            v-if="installation"
            class="badge"
            :class="enabled ? 'success' : 'neutral'"
            >{{ enabled ? "已启用" : "已停用" }}</span
          >
          <button
            v-if="!enabled && cloud.state.projectId && !contextLoading"
            class="button button-primary"
            :disabled="!canManageConnection || Boolean(busy)"
            @click="enableApplication"
          >
            {{
              busy === "enable"
                ? "正在启用…"
                : installation
                  ? "重新启用"
                  : "在项目中启用"
            }}
          </button>
          <button
            v-if="enabled"
            class="button"
            :disabled="summaryLoading || resourcesLoading || Boolean(busy)"
            @click="loadDetails"
          >
            <CloudIcon name="refresh" :size="16" /> 刷新
          </button>
        </div>
      </header>

      <div v-if="!cloud.state.projectId" class="info-note">
        请先选择团队与项目，再启用和管理应用。
      </div>
      <div v-else-if="!cloud.canManageProject.value" class="info-note">
        {{
          canWriteResources
            ? "你可以管理当前项目的应用资源。启用应用和修改连接需要项目管理员权限。"
            : "你可以查看当前项目的应用与资源。写入操作需要相应的项目权限。"
        }}
      </div>

      <CloudState
        v-if="contextLoading"
        kind="loading"
        title="正在读取项目应用"
      />
      <CloudState
        v-else-if="cloud.state.installationError"
        kind="error"
        title="暂时无法读取项目应用"
        :description="cloud.state.installationError"
        @retry="cloud.refreshInstallations().catch(() => undefined)"
      />

      <div v-else class="detail-grid application-columns">
        <div class="stack detail-main">
          <section v-if="!enabled" class="panel activation-panel">
            <p class="eyebrow">
              {{ installation ? "应用已停用" : "开始使用" }}
            </p>
            <h2>
              {{ installation ? "重新启用后继续使用" : "将应用加入当前项目" }}
            </h2>
            <p class="muted">
              启用会为当前项目建立独立的应用配置。连接完成后，这里将显示实际摘要和可管理的资源。
            </p>
            <p class="info-note">启用应用不会自动创建远端业务资源。</p>
          </section>

          <template v-if="enabled">
            <section class="panel">
              <div class="panel-heading">
                <h2>应用概况</h2>
                <span v-if="summary" class="badge" :class="summaryTone">{{
                  summaryLabels[summary.state] ?? summary.state
                }}</span>
              </div>
              <CloudState
                v-if="summaryLoading"
                kind="loading"
                title="正在读取应用概况"
              />
              <CloudState
                v-else-if="summaryError"
                kind="error"
                title="暂时无法获取概况"
                :description="summaryError"
                @retry="loadDetails"
              />
              <div v-else-if="summary" class="summary-content">
                <p class="muted summary-message">{{ summary.message }}</p>
                <div v-if="summary.metrics?.length" class="metric-grid">
                  <div
                    v-for="(metric, index) in summary.metrics"
                    :key="`${metric.label}-${index}`"
                    class="metric"
                  >
                    <span class="muted">{{ metric.label }}</span
                    ><strong>{{ metric.value }}</strong>
                  </div>
                </div>
                <div v-if="summary.verification" class="verification-row row">
                  <span class="muted">身份验证</span
                  ><strong>{{
                    verificationLabels[summary.verification.status] ??
                    summary.verification.status
                  }}</strong
                  ><span
                    v-if="summary.verification.identityType"
                    class="muted"
                    >{{
                      verificationLabels[summary.verification.identityType] ??
                      summary.verification.identityType
                    }}</span
                  >
                </div>
                <div class="summary-footer row">
                  <span v-if="summary.checkedAt" class="muted"
                    >检查于 {{ formatDate(summary.checkedAt) }}</span
                  ><a
                    v-if="safeUrl(summary.consoleUrl)"
                    class="button button-quiet"
                    :href="safeUrl(summary.consoleUrl)"
                    target="_blank"
                    rel="noopener noreferrer"
                    >打开应用控制台 <CloudIcon name="external" :size="15"
                  /></a>
                </div>
              </div>
            </section>

            <section class="panel resources-panel">
              <div class="panel-heading resource-heading">
                <div>
                  <h2>
                    {{ application.id === "weauth" ? "站点" : "项目资源" }}
                  </h2>
                  <p class="muted section-caption">
                    只显示当前项目绑定的资源。
                  </p>
                </div>
                <button
                  v-if="canCreateSite"
                  data-testid="resource-create"
                  class="button button-primary"
                  :disabled="
                    !canWriteResources ||
                    Boolean(busy) ||
                    summary?.state !== 'ready'
                  "
                  @click="openCreateSite"
                >
                  <CloudIcon name="plus" :size="16" /> 创建站点
                </button>
              </div>
              <CloudState
                v-if="resourcesLoading"
                kind="loading"
                title="正在读取资源"
              />
              <CloudState
                v-else-if="resourcesUnsupported"
                kind="empty"
                title="暂不支持在此管理资源"
                :description="
                  resourcesError || '此应用尚未提供可用的资源管理能力。'
                "
              />
              <CloudState
                v-else-if="resourcesError"
                kind="error"
                title="资源暂时无法加载"
                :description="resourcesError"
                @retry="loadDetails"
              />
              <div v-else-if="resources.length" class="table-wrap">
                <table class="data-table">
                  <thead>
                    <tr>
                      <th scope="col">名称</th>
                      <th scope="col">类型</th>
                      <th scope="col">状态</th>
                      <th scope="col" class="action-cell">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr v-for="resource in resources" :key="resource.id">
                      <td>
                        <strong class="resource-name">{{
                          resource.name || resource.id
                        }}</strong
                        ><span class="muted resource-id">{{
                          resource.id
                        }}</span>
                      </td>
                      <td>{{ resource.type }}</td>
                      <td>{{ resource.status || "未提供" }}</td>
                      <td>
                        <div class="resource-actions">
                          <a
                            v-if="safeUrl(resource.url)"
                            class="button button-quiet"
                            :href="safeUrl(resource.url)"
                            target="_blank"
                            rel="noopener noreferrer"
                            :aria-label="`打开 ${resource.name || resource.id}`"
                            ><CloudIcon name="external" :size="15" /><span
                              >打开</span
                            ></a
                          ><button
                            v-if="canDeleteResource"
                            class="button button-quiet resource-delete"
                            :disabled="!canWriteResources || Boolean(busy)"
                            @click="askDeleteResource(resource)"
                          >
                            删除</button
                          ><span
                            v-if="!safeUrl(resource.url) && !canDeleteResource"
                            class="muted"
                            >仅查看</span
                          >
                        </div>
                      </td>
                    </tr>
                  </tbody>
                </table>
              </div>
              <CloudState
                v-else
                kind="empty"
                :title="
                  application.id === 'weauth'
                    ? '当前项目暂无站点'
                    : '当前项目暂无资源'
                "
                :description="
                  canCreateSite
                    ? '连接就绪后，可以创建第一个站点。'
                    : '当前连接尚未返回此项目的资源。'
                "
              />
            </section>
          </template>
        </div>

        <aside class="stack detail-aside">
          <section v-if="enabled" class="panel connection-panel">
            <div class="panel-heading">
              <h2>连接</h2>
              <span
                class="badge"
                :class="
                  installation?.connection?.configured ? 'success' : 'neutral'
                "
                >{{
                  identityConnection
                    ? "当前身份"
                    : installation?.connection?.configured
                      ? "已配置"
                      : "未配置"
                }}</span
              >
            </div>
            <p class="muted">{{ connectionModeLabel }}</p>
            <p v-if="identityConnection" class="info-note">
              身份由当前登录会话验证，查询范围由服务端限制。
            </p>
            <template v-else>
              <dl class="connection-meta">
                <dt>服务地址</dt>
                <dd>{{ installation?.connection?.baseUrl || "尚未设置" }}</dd>
                <template v-if="installation?.connection?.updatedAt"
                  ><dt>更新时间</dt>
                  <dd>
                    {{ formatDate(installation.connection.updatedAt) }}
                  </dd></template
                >
              </dl>
              <div class="row connection-actions">
                <button
                  data-testid="installation-connect"
                  class="button"
                  :disabled="!canManageConnection || Boolean(busy)"
                  @click="openConnection"
                >
                  {{
                    installation?.connection?.configured
                      ? "编辑连接"
                      : "配置连接"
                  }}</button
                ><button
                  v-if="installation?.connection?.configured"
                  class="button button-quiet resource-delete"
                  :disabled="!canManageConnection || Boolean(busy)"
                  @click="askRemoveConnection"
                >
                  移除连接
                </button>
              </div>
            </template>
          </section>
          <section class="panel">
            <div class="panel-heading"><h2>使用说明</h2></div>
            <ul
              v-if="application.limitations?.length"
              class="limitations muted"
            >
              <li
                v-for="limitation in application.limitations"
                :key="limitation"
              >
                {{ limitation }}
              </li>
            </ul>
            <p v-else class="muted">可用操作以当前应用提供的能力为准。</p>
            <div class="stack related-links">
              <a
                v-if="safeUrl(application.homepage)"
                :href="safeUrl(application.homepage)"
                target="_blank"
                rel="noopener noreferrer"
                >应用主页 <CloudIcon name="external" :size="14" /></a
              ><a
                v-if="safeUrl(application.repository)"
                :href="safeUrl(application.repository)"
                target="_blank"
                rel="noopener noreferrer"
                >开源仓库 <CloudIcon name="external" :size="14"
              /></a>
            </div>
          </section>
        </aside>
      </div>
    </template>

    <CloudModal
      v-model="connectionOpen"
      :title="
        installation?.connection?.configured ? '编辑应用连接' : '配置应用连接'
      "
      description="连接只用于当前项目。服务地址需要符合平台允许的来源。"
    >
      <form class="stack" @submit.prevent="saveConnection">
        <label class="field"
          >服务地址<input
            v-model="connectionForm.baseUrl"
            type="url"
            inputmode="url"
            placeholder="https://service.example.com"
            autocomplete="url"
            required
            :disabled="Boolean(busy)"
        /></label>
        <label v-if="bearerConnection" class="field"
          >访问令牌<input
            v-model="connectionForm.credential"
            type="password"
            placeholder="输入应用访问令牌"
            autocomplete="new-password"
            :disabled="Boolean(busy)"
          /><span class="muted field-help">{{
            installation?.connection?.configured
              ? "留空保留已保存的凭据。现有令牌不会回填。"
              : "填写当前应用独立账号的访问令牌。"
          }}</span></label
        >
        <template v-if="keyPairConnection"
          ><label class="field"
            >Access Key<input
              v-model="connectionForm.accessKey"
              type="password"
              autocomplete="new-password"
              :disabled="Boolean(busy)" /></label
          ><label class="field"
            >Secret Key<input
              v-model="connectionForm.secretKey"
              type="password"
              autocomplete="new-password"
              :disabled="Boolean(busy)"
            /><span class="muted field-help">{{
              installation?.connection?.configured
                ? "两项全部留空可保留原凭据。现有密钥不会回填。"
                : "填写独立存储账号的访问密钥对。"
            }}</span></label
          ></template
        >
        <p v-if="formError" class="form-error" role="alert">{{ formError }}</p>
        <div class="row modal-actions">
          <button
            type="button"
            class="button"
            :disabled="Boolean(busy)"
            @click="connectionOpen = false"
          >
            取消</button
          ><button
            type="submit"
            class="button button-primary"
            :disabled="!canManageConnection || Boolean(busy)"
          >
            {{ busy === "connection" ? "正在保存…" : "保存连接" }}
          </button>
        </div>
      </form>
    </CloudModal>
    <CloudModal
      v-model="createOpen"
      title="创建 WeAuth 站点"
      description="站点将在已连接的 WeAuth 服务中创建，并绑定当前项目。"
    >
      <form class="stack" @submit.prevent="createSite">
        <label class="field"
          >站点名称<input
            v-model="siteForm.name"
            aria-label="站点名称"
            placeholder="例如：产品官网"
            maxlength="120"
            required
            :disabled="Boolean(busy)" /></label
        ><label class="field"
          >域名<textarea
            v-model="siteForm.domains"
            aria-label="域名"
            rows="4"
            placeholder="example.com&#10;www.example.com"
            required
            :disabled="Boolean(busy)"
          /><span class="muted field-help"
            >每行一个域名，不含协议和路径。</span
          ></label
        >
        <p v-if="formError" class="form-error" role="alert">{{ formError }}</p>
        <div class="row modal-actions">
          <button
            type="button"
            class="button"
            :disabled="Boolean(busy)"
            @click="createOpen = false"
          >
            取消</button
          ><button
            type="submit"
            class="button button-primary"
            :disabled="!canWriteResources || !canCreateSite || Boolean(busy)"
          >
            {{ busy === "create-site" ? "正在创建…" : "创建站点" }}
          </button>
        </div>
      </form>
    </CloudModal>
    <CloudModal
      v-model="confirmOpen"
      :title="
        confirmation?.kind === 'connection' ? '移除应用连接？' : '删除此资源？'
      "
      :description="
        confirmation?.kind === 'connection'
          ? '将移除当前项目的连接和凭据，不会删除远端业务资源。'
          : '将调用应用服务删除此资源。删除后无法通过本页面恢复。'
      "
    >
      <div class="stack">
        <p v-if="confirmation?.resource" class="delete-target">
          {{ confirmation.resource.name || confirmation.resource.id }}
        </p>
        <p v-if="formError" class="form-error" role="alert">{{ formError }}</p>
        <div class="row modal-actions">
          <button
            class="button"
            :disabled="Boolean(busy)"
            @click="confirmOpen = false"
          >
            取消</button
          ><button
            class="button button-danger"
            :disabled="
              (confirmation?.kind === 'connection'
                ? !canManageConnection
                : !canWriteResources) || Boolean(busy)
            "
            @click="confirmDelete"
          >
            {{
              busy === "delete"
                ? "正在处理…"
                : confirmation?.kind === "connection"
                  ? "移除连接"
                  : "确认删除"
            }}
          </button>
        </div>
      </div>
    </CloudModal>
  </div>
</template>

<style scoped>
.application-detail {
  gap: 24px;
}
.back-link {
  display: inline-flex;
  gap: 7px;
  align-items: center;
  text-decoration: none;
  width: fit-content;
  font-size: 13px;
}
.back-arrow {
  transform: rotate(180deg);
}
.application-heading {
  align-items: flex-start;
  gap: 24px;
}
.application-title {
  display: flex;
  align-items: flex-start;
  gap: 18px;
  min-width: 0;
}
.application-title h1 {
  margin: 3px 0 8px;
  font-size: clamp(25px, 3vw, 32px);
  line-height: 1.2;
}
.detail-mark {
  width: 58px;
  height: 58px;
  flex-shrink: 0;
  border-radius: 16px;
}
.app-description {
  max-width: 680px;
  line-height: 1.7;
  margin: 0;
}
.heading-actions {
  flex-shrink: 0;
  flex-wrap: wrap;
}
.application-columns {
  display: grid;
  grid-template-columns: minmax(0, 1fr) 300px;
  gap: 24px;
  align-items: start;
}
.detail-main,
.detail-aside {
  min-width: 0;
  gap: 22px;
}
.panel {
  min-width: 0;
}
.panel h2 {
  font-size: 16px;
  line-height: 1.5;
  margin: 0;
}
.activation-panel {
  display: grid;
  gap: 14px;
  padding: 24px;
}
.activation-panel p {
  margin: 0;
  line-height: 1.7;
}
.activation-panel h2 {
  font-size: 21px;
}
.summary-content {
  padding: 0 24px 24px;
  min-width: 0;
}
.summary-message {
  line-height: 1.8;
  margin: 0 0 18px;
  font-size: 12px;
  overflow-wrap: anywhere;
}
.metric-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
  gap: 16px;
}
.metric {
  display: flex;
  flex-direction: column;
  gap: 7px;
  padding: 16px;
  border: 1px solid var(--cloud-border);
  background: var(--cloud-panel-soft);
  border-radius: 10px;
  overflow-wrap: anywhere;
  min-width: 0;
}
.metric span {
  font-size: 11px;
}
.metric strong {
  font-size: 25px;
  font-variant-numeric: tabular-nums;
}
.verification-row {
  flex-wrap: wrap;
  margin-top: 18px;
}
.summary-footer {
  margin-top: 18px;
  justify-content: space-between;
  flex-wrap: wrap;
  font-size: 12px;
  gap: 12px;
}
.section-caption {
  margin: 5px 0 0;
  padding: 0;
  font-size: 11px;
}
.resource-heading {
  gap: 12px;
  align-items: center;
  flex-wrap: wrap;
}
.table-wrap {
  max-width: 100%;
  overflow-x: auto;
}
.data-table {
  width: 100%;
  min-width: 480px;
}
.resource-name {
  display: block;
  font-weight: 500;
  overflow-wrap: anywhere;
}
.resource-id {
  display: block;
  font-size: 11px;
  margin-top: 5px;
  overflow-wrap: anywhere;
}
.action-cell {
  text-align: right;
}
.resource-actions {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 4px;
  white-space: nowrap;
}
.resource-delete {
  color: var(--eu-color-danger, #b42318);
}
.connection-panel > p,
.detail-aside .panel > p {
  margin: 0 24px 18px;
  font-size: 12px;
  line-height: 1.8;
  overflow-wrap: anywhere;
}
.connection-panel > .info-note {
  margin-bottom: 24px;
  padding: 14px;
}
.connection-meta {
  display: grid;
  gap: 8px;
  font-size: 12px;
  margin: 0 24px;
}
.connection-meta dt {
  color: var(--eu-text-secondary, #667085);
  margin-top: 8px;
}
.connection-meta dd {
  margin: 0;
  overflow-wrap: anywhere;
  line-height: 1.6;
}
.connection-actions {
  margin: 18px 24px 24px;
  gap: 8px;
  flex-wrap: wrap;
}
.limitations {
  margin: 0 24px;
  padding-left: 16px;
  font-size: 12px;
  line-height: 1.9;
  overflow-wrap: anywhere;
}
.limitations li + li {
  margin-top: 10px;
}
.related-links {
  margin-top: 20px;
  padding: 18px 24px 24px;
  border-top: 1px solid var(--cloud-border);
  gap: 12px;
  font-size: 12px;
}
.related-links a {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  width: fit-content;
  max-width: 100%;
  color: var(--cloud-blue);
  text-decoration: none;
  overflow-wrap: anywhere;
}
.field-help {
  display: block;
  font-size: 12px;
  font-weight: 400;
  line-height: 1.6;
}
.form-error {
  margin: 0;
  color: var(--eu-color-danger, #b42318);
  font-size: 13px;
  line-height: 1.6;
}
.modal-actions {
  justify-content: flex-end;
  flex-wrap: wrap;
  margin-top: 8px;
}
.delete-target {
  padding: 12px 14px;
  border: 1px solid var(--eu-border, #e4e7ec);
  border-radius: 8px;
  overflow-wrap: anywhere;
  margin: 0;
}
.field input,
.field textarea {
  box-sizing: border-box;
  width: 100%;
}
.field textarea {
  resize: vertical;
  min-height: 96px;
  font: inherit;
}
@media (max-width: 1080px) {
  .application-columns {
    grid-template-columns: minmax(0, 1fr);
  }
  .detail-aside {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    align-items: start;
  }
  .application-heading {
    flex-direction: column;
  }
}
@media (max-width: 640px) {
  .application-title {
    gap: 12px;
  }
  .detail-mark {
    width: 44px;
    height: 44px;
    border-radius: 12px;
  }
  .detail-aside {
    grid-template-columns: minmax(0, 1fr);
  }
  .application-detail,
  .application-columns {
    gap: 18px;
  }
  .heading-actions {
    width: 100%;
  }
  .resource-actions .button {
    padding-left: 6px;
    padding-right: 6px;
  }
  .summary-footer {
    align-items: flex-start;
  }
  .summary-content {
    padding: 0 18px 18px;
  }
  .activation-panel {
    padding: 18px;
  }
  .connection-panel > p,
  .detail-aside .panel > p,
  .connection-meta,
  .connection-actions,
  .limitations {
    margin-left: 18px;
    margin-right: 18px;
  }
  .related-links {
    padding: 16px 18px 18px;
  }
}
</style>
