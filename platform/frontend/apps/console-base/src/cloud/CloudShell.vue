<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";
import { RouterLink, RouterView, useRoute } from "vue-router";
import { api, errorMessage, roleLabels } from "./api";
import { useCloud } from "./context";
import type { Project, Tenant } from "./types";
import CloudIcon from "./CloudIcon.vue";
import CloudState from "./CloudState.vue";
import CloudModal from "./CloudModal.vue";
const route = useRoute();
const {
  state,
  tenant,
  project,
  canManageTeam,
  bootstrap,
  selectTenant,
  selectProject,
  refreshTenants,
  tenantPath,
  expireSession,
  notify,
} = useCloud();
const menuOpen = ref(false);
const modal = ref<"tenant" | "project" | null>(null);
const name = ref("");
const busy = ref(false);
const formError = ref("");
const signingOut = ref(false);
const selectingLink = ref(false);
const linkError = ref("");
let linkSequence = 0;
async function selectLinkContext() {
  const seq = ++linkSequence;
  linkError.value = "";
  if (!state.session?.authenticated || state.loading) return;
  const team =
    typeof route.query.tenantId === "string" ? route.query.tenantId : "";
  const target =
    typeof route.query.projectId === "string" ? route.query.projectId : "";
  if (!team && !target) return;
  if (!team || !target || !state.tenants.some((t) => t.id === team)) {
    linkError.value = "链接中的团队或项目不存在，或你没有访问权限。";
    return;
  }
  selectingLink.value = true;
  try {
    if (state.tenantId !== team) await selectTenant(team);
    if (seq !== linkSequence) return;
    if (!state.projects.some((p) => p.id === target)) {
      linkError.value = "链接中的项目不存在，或你没有访问权限。";
      return;
    }
    if (state.projectId !== target) await selectProject(target);
  } finally {
    if (seq === linkSequence) selectingLink.value = false;
  }
}
const modalOpen = computed({
  get: () => modal.value !== null,
  set: (value) => {
    if (!value) modal.value = null;
  },
});
const dark = ref(document.documentElement.dataset.theme === "dark");
const loginURL = computed(
  () =>
    `/auth/login?returnTo=${encodeURIComponent(route.fullPath.startsWith("/") && !route.fullPath.startsWith("//") ? route.fullPath : "/")}`,
);
const navigation = computed(() => [
  { to: "/", name: "项目总览", icon: "home" },
  { to: "/catalog", name: "应用目录", icon: "grid" },
  { to: "/members", name: "成员与邀请", icon: "users" },
  { to: "/identity", name: "身份与认证", icon: "shield" },
  ...(state.session?.platformAdmin
    ? [{ to: "/permissions", name: "应用权限", icon: "key" }]
    : []),
  { to: "/audit", name: "操作审计", icon: "clock" },
]);
function openCreate(type: "tenant" | "project") {
  name.value = "";
  formError.value = "";
  modal.value = type;
  menuOpen.value = false;
}
async function create() {
  if (!name.value.trim() || !modal.value) return;
  busy.value = true;
  formError.value = "";
  const kind = modal.value;
  const currentTenant = state.tenantId;
  try {
    if (kind === "tenant") {
      const item = await api<Tenant>("/api/v1/tenants", {
        method: "POST",
        body: { name: name.value.trim() },
      });
      await refreshTenants(item.id);
    } else {
      const item = await api<Project>(tenantPath("/projects"), {
        method: "POST",
        body: { name: name.value.trim() },
      });
      if (currentTenant === state.tenantId) {
        await selectTenant(currentTenant);
        await selectProject(item.id);
      }
    }
    modal.value = null;
    notify(
      kind === "tenant"
        ? "团队已创建，可以开始创建项目。"
        : "项目已创建，现在可以添加应用。",
    );
  } catch (err) {
    formError.value = errorMessage(err);
  } finally {
    busy.value = false;
  }
}
function toggleTheme() {
  dark.value = !dark.value;
  document.documentElement.dataset.theme = dark.value ? "dark" : "light";
  try {
    localStorage.setItem("eu:theme", dark.value ? "dark" : "light");
  } catch {
    /* Apply this visit even without persistent storage. */
  }
}
async function logout() {
  signingOut.value = true;
  try {
    await api<void>("/auth/logout", { method: "POST" });
    expireSession();
  } catch (err) {
    notify(errorMessage(err), "error");
  } finally {
    signingOut.value = false;
  }
}
watch(
  () => route.fullPath,
  () => (menuOpen.value = false),
);
watch(() => [route.query.tenantId, route.query.projectId], selectLinkContext);
onMounted(async () => {
  await bootstrap();
  await selectLinkContext();
});
</script>
<template>
  <div class="cloud-shell">
    <a class="skip-link" href="#main-content">跳转到主要内容</a>
    <header class="cloud-topbar">
      <div class="brand-group">
        <button
          v-if="state.session?.authenticated"
          class="icon-button mobile-menu"
          :aria-expanded="menuOpen"
          aria-controls="cloud-navigation"
          aria-label="切换导航菜单"
          @click="menuOpen = !menuOpen"
        >
          <CloudIcon :name="menuOpen ? 'close' : 'menu'" /></button
        ><RouterLink to="/" class="cloud-brand" aria-label="欧拉应用云首页"
          ><span class="brand-symbol" aria-hidden="true"
            ><i /><i /><i /><i /></span
          ><span class="brand-word">Euler<span>应用云</span></span></RouterLink
        >
      </div>
      <div v-if="state.session?.authenticated" class="context-switcher">
        <label class="context-select"
          ><span>团队</span
          ><select
            data-testid="tenant-select"
            aria-label="选择团队"
            :value="state.tenantId"
            :disabled="state.loading || !state.tenants.length"
            @change="selectTenant(($event.target as HTMLSelectElement).value)"
          >
            <option v-if="!state.tenants.length" value="">尚未创建团队</option>
            <option
              v-for="item in state.tenants"
              :key="item.id"
              :value="item.id"
            >
              {{ item.name }}
            </option>
          </select></label
        ><span class="context-slash">/</span
        ><label class="context-select"
          ><span>项目</span
          ><select
            data-testid="project-select"
            aria-label="选择项目"
            :value="state.projectId"
            :disabled="state.contextLoading || !state.projects.length"
            @change="selectProject(($event.target as HTMLSelectElement).value)"
          >
            <option v-if="!state.projects.length" value="">
              {{ state.contextLoading ? "正在加载…" : "尚未选择项目" }}
            </option>
            <option
              v-for="item in state.projects"
              :key="item.id"
              :value="item.id"
            >
              {{ item.name }}
            </option>
          </select></label
        >
      </div>
      <div class="topbar-actions">
        <button
          class="icon-button"
          :aria-label="dark ? '切换浅色主题' : '切换深色主题'"
          @click="toggleTheme"
        >
          <CloudIcon name="sun" :size="19" /></button
        ><template v-if="state.session?.authenticated"
          ><span class="account-name">{{
            state.session.user?.displayName
          }}</span
          ><span class="avatar topbar-avatar" aria-hidden="true">{{
            state.session.user?.displayName?.slice(0, 1) ?? "我"
          }}</span
          ><button
            class="icon-button"
            :disabled="signingOut"
            aria-label="退出登录"
            title="退出登录"
            @click="logout"
          >
            <CloudIcon name="logout" :size="18" /></button></template
        ><span v-else class="topbar-wordmark">一个入口，连接你的应用</span>
      </div>
    </header>
    <template v-if="state.session?.authenticated"
      ><button
        v-if="menuOpen"
        class="sidebar-scrim"
        aria-label="关闭导航菜单"
        @click="menuOpen = false"
      />
      <aside
        id="cloud-navigation"
        class="cloud-sidebar"
        :class="{ open: menuOpen }"
      >
        <div class="sidebar-section-label">工作空间</div>
        <nav aria-label="控制台导航">
          <RouterLink
            v-for="item in navigation"
            :key="item.to"
            :to="item.to"
            class="sidebar-link"
            :class="{
              selected:
                item.to === '/'
                  ? route.path === '/'
                  : route.path === item.to ||
                    (item.to === '/catalog' && route.path.startsWith('/apps/')),
            }"
            :aria-current="
              (
                item.to === '/'
                  ? route.path === '/'
                  : route.path === item.to ||
                    (item.to === '/catalog' && route.path.startsWith('/apps/'))
              )
                ? 'page'
                : undefined
            "
            ><CloudIcon :name="item.icon" :size="19" /><span>{{
              item.name
            }}</span
            ><CloudIcon
              v-if="item.to === '/catalog'"
              name="arrow"
              :size="15"
              class="nav-tail"
          /></RouterLink>
        </nav>
        <div class="sidebar-divider" />
        <div class="sidebar-section-label">团队管理</div>
        <button
          data-testid="tenant-create"
          class="sidebar-link"
          @click="openCreate('tenant')"
        >
          <CloudIcon name="plus" :size="18" />创建团队</button
        ><button
          v-if="canManageTeam"
          data-testid="project-create"
          class="sidebar-link"
          @click="openCreate('project')"
        >
          <CloudIcon name="folder" :size="18" />创建项目
        </button>
        <div v-if="tenant" class="sidebar-context">
          <span class="sidebar-team-icon"
            ><CloudIcon name="users" :size="18"
          /></span>
          <div>
            <strong>{{ tenant.name }}</strong
            ><span>{{ roleLabels[tenant.role] }}</span>
          </div>
        </div>
        <div class="sidebar-footer">
          <span class="tiny-dot" />应用独立 · 协作一致<small
            >Euler Application Cloud</small
          >
        </div>
      </aside></template
    >
    <main
      id="main-content"
      tabindex="-1"
      class="cloud-main"
      :class="{ 'without-sidebar': !state.session?.authenticated }"
    >
      <CloudState
        v-if="state.loading"
        kind="loading"
        title="正在打开你的工作空间"
        description="正在确认登录状态与项目权限。"
      />
      <CloudState
        v-else-if="state.error"
        kind="error"
        title="工作空间暂时无法打开"
        :description="state.error"
        @retry="bootstrap"
      />
      <section v-else-if="!state.session?.authenticated" class="login-layout">
        <div class="login-story">
          <span class="eyebrow">YOUR APPS. ONE WORKSPACE.</span>
          <h1>
            让每个应用，<br />成为协作的一部分<span class="title-dot">.</span>
          </h1>
          <p>
            登录欧拉应用云，在同一个项目里管理身份、连接服务，与团队一起推进工作。
          </p>
          <div class="login-product-row" aria-hidden="true">
            <span class="app-mark" data-app="eid"
              ><CloudIcon name="shield" :size="26" /></span
            ><span class="app-mark" data-app="weauth"
              ><CloudIcon name="key" :size="26" /></span
            ><span class="app-mark" data-app="database"
              ><CloudIcon name="database" :size="26" /></span
            ><span class="app-mark" data-app="storage"
              ><CloudIcon name="storage" :size="26"
            /></span>
          </div>
          <div class="login-principles">
            <span><CloudIcon name="check" :size="16" />独立的团队与项目</span
            ><span><CloudIcon name="check" :size="16" />清晰的资源归属</span
            ><span><CloudIcon name="check" :size="16" />可追溯的每次操作</span>
          </div>
        </div>
        <div class="login-card">
          <span class="login-card-icon"
            ><CloudIcon name="layers" :size="28"
          /></span>
          <h2>欢迎来到你的应用云</h2>
          <p>使用已配置的通行证账号登录。<br />你的团队与应用将在这里继续。</p>
          <a
            v-if="state.session?.loginAvailable"
            :href="loginURL"
            class="button button-primary login-button"
            >使用通行证登录<CloudIcon name="arrow" :size="18"
          /></a>
          <div v-else class="login-unavailable" role="status">
            <CloudIcon name="info" :size="19" />
            <div>
              <strong>登录服务暂不可用</strong>
              <p>
                {{
                  state.session?.loginError ||
                  "当前站点尚未配置登录服务，请联系站点管理员。"
                }}
              </p>
            </div>
          </div>
          <button
            v-if="!state.session?.loginAvailable"
            class="text-link"
            @click="bootstrap"
          >
            重新检查<CloudIcon name="refresh" :size="15" /></button
          ><small
            ><CloudIcon
              name="shield"
              :size="14"
            />权限由你所在的团队与项目管理</small
          >
        </div>
      </section>
      <template v-else-if="route.path === '/join'"><RouterView /></template>
      <CloudState
        v-else-if="state.contextLoading || selectingLink"
        kind="loading"
        title="正在切换项目空间"
        description="正在读取当前团队中你可以访问的项目。"
      />
      <CloudState
        v-else-if="linkError"
        kind="error"
        title="无法打开链接中的项目"
        :description="linkError"
        @retry="selectLinkContext"
      />
      <CloudState
        v-else-if="state.contextError"
        kind="error"
        title="项目列表加载失败"
        :description="state.contextError"
        @retry="selectTenant(state.tenantId)"
      />
      <section v-else-if="!state.tenants.length" class="onboarding panel">
        <span class="app-mark onboarding-mark"
          ><CloudIcon name="users" :size="30" /></span
        ><span class="eyebrow">A PLACE TO START</span>
        <h1>你的工作空间，从这里开始</h1>
        <p>
          创建一个团队作为应用与协作者的共同空间，<br />再用项目划分工作与资源。
        </p>
        <button class="button button-primary" @click="openCreate('tenant')">
          <CloudIcon name="plus" :size="18" />创建第一个团队</button
        ><RouterLink class="text-link" to="/join">我有团队邀请</RouterLink>
        <div class="onboarding-steps">
          <span><b>01</b>建立团队</span
          ><CloudIcon name="arrow" :size="17" /><span><b>02</b>创建项目</span
          ><CloudIcon name="arrow" :size="17" /><span><b>03</b>启用应用</span>
        </div>
      </section>
      <CloudState
        v-else-if="route.meta.project && !project"
        kind="empty"
        title="为你的应用创建一个项目"
        :description="
          canManageTeam
            ? '项目是应用、资源与成员授权的边界。先创建项目，再开始使用应用。'
            : '你还没有可访问的项目，请联系团队管理员授予权限。'
        "
        ><button
          v-if="canManageTeam"
          class="button button-primary"
          @click="openCreate('project')"
        >
          <CloudIcon name="plus" :size="18" />创建项目</button
        ><RouterLink v-else class="button" to="/members"
          >查看团队成员</RouterLink
        ></CloudState
      >
      <RouterView
        v-else
        :key="`${state.tenantId}:${state.projectId}:${route.path}`"
      />
      <footer
        v-if="state.session?.authenticated && !state.loading"
        class="content-footer"
      >
        <span>欧拉应用云</span
        ><span
          >当前范围 · {{ tenant?.name ?? "未选择团队" }} /
          {{ project?.name ?? "未选择项目" }}</span
        >
      </footer>
    </main>
    <div
      v-if="state.toast"
      class="cloud-toast"
      :class="state.toast.tone"
      role="status"
    >
      <CloudIcon
        :name="state.toast.tone === 'success' ? 'check' : 'alert'"
        :size="18"
      />{{ state.toast.message
      }}<button
        class="icon-button"
        aria-label="关闭提示"
        @click="state.toast = null"
      >
        <CloudIcon name="close" :size="16" />
      </button>
    </div>
    <CloudModal
      v-model="modalOpen"
      :title="modal === 'tenant' ? '创建团队' : '创建项目'"
      :description="
        modal === 'tenant'
          ? '为共同协作的人和应用建立独立空间。'
          : `在 ${tenant?.name ?? '当前团队'} 下划分应用与资源。`
      "
      ><form class="stack" @submit.prevent="create">
        <label class="field"
          >{{ modal === "tenant" ? "团队名称" : "项目名称"
          }}<input
            v-model="name"
            :aria-label="modal === 'tenant' ? '团队名称' : '项目名称'"
            required
            maxlength="80"
            :placeholder="
              modal === 'tenant' ? '例如：产品研发团队' : '例如：开发者社区'
            "
            autofocus
        /></label>
        <p v-if="formError" class="form-error" role="alert">{{ formError }}</p>
        <div class="modal-actions">
          <button
            type="button"
            class="button"
            :disabled="busy"
            @click="modal = null"
          >
            取消</button
          ><button
            class="button button-primary"
            :disabled="busy || !name.trim()"
          >
            {{ busy ? "正在创建…" : "确认创建" }}
          </button>
        </div>
      </form></CloudModal
    >
  </div>
</template>
