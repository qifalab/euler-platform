import { computed, reactive } from "vue";
import { api, errorMessage, setCSRFToken } from "./api";
import type {
  Application,
  Installation,
  List,
  Project,
  Session,
  Tenant,
} from "./types";

const state = reactive({
  session: null as Session | null,
  catalog: [] as Application[],
  tenants: [] as Tenant[],
  projects: [] as Project[],
  installations: [] as Installation[],
  tenantId: "",
  projectId: "",
  loading: true,
  contextLoading: false,
  installationsLoading: false,
  error: "",
  contextError: "",
  installationError: "",
  toast: null as { message: string; tone: "success" | "error" } | null,
});
const tenant = computed(() =>
  state.tenants.find((item) => item.id === state.tenantId),
);
const project = computed(() =>
  state.projects.find((item) => item.id === state.projectId),
);
const canManageTeam = computed(() =>
  ["owner", "admin"].includes(tenant.value?.role ?? ""),
);
const canManageProject = computed(() =>
  Boolean(
    tenant.value &&
      tenant.value.role !== "viewer" &&
      (canManageTeam.value ||
        ["owner", "admin"].includes(project.value?.role ?? "")),
  ),
);
let contextSequence = 0;
let installationSequence = 0;
let toastTimer: ReturnType<typeof setTimeout>;
function saved(key: string) {
  try {
    return localStorage.getItem(`euler:cloud:${key}`) ?? "";
  } catch {
    return "";
  }
}
function persist(key: string, value: string) {
  try {
    localStorage.setItem(`euler:cloud:${key}`, value);
  } catch {
    /* Selection still works when storage is unavailable. */
  }
}
function projectPath(suffix = "") {
  if (!state.tenantId || !state.projectId)
    throw new Error("请先选择团队与项目。");
  return `/api/v1/tenants/${encodeURIComponent(state.tenantId)}/projects/${encodeURIComponent(state.projectId)}${suffix}`;
}
function tenantPath(suffix = "") {
  if (!state.tenantId) throw new Error("请先选择团队。");
  return `/api/v1/tenants/${encodeURIComponent(state.tenantId)}${suffix}`;
}
function notify(message: string, tone: "success" | "error" = "success") {
  clearTimeout(toastTimer);
  state.toast = { message, tone };
  toastTimer = setTimeout(() => (state.toast = null), 6500);
}
async function refreshInstallations() {
  const sequence = ++installationSequence;
  if (!state.projectId) {
    state.installations = [];
    state.installationsLoading = false;
    return;
  }
  const path = projectPath("/installations");
  state.installationsLoading = true;
  state.installationError = "";
  try {
    const result = await api<List<Installation>>(path);
    if (sequence === installationSequence) state.installations = result.items;
  } catch (error) {
    if (sequence === installationSequence) {
      state.installations = [];
      state.installationError = errorMessage(error);
    }
    throw error;
  } finally {
    if (sequence === installationSequence) state.installationsLoading = false;
  }
}
async function selectProject(id: string) {
  state.projectId = state.projects.some((item) => item.id === id) ? id : "";
  persist(`project:${state.tenantId}`, state.projectId);
  state.installations = [];
  try {
    await refreshInstallations();
  } catch {
    /* Visible project-level error, never an empty-success state. */
  }
}
async function selectTenant(id: string) {
  const sequence = ++contextSequence;
  ++installationSequence;
  state.tenantId = state.tenants.some((item) => item.id === id) ? id : "";
  state.projectId = "";
  state.projects = [];
  state.installations = [];
  state.contextError = "";
  state.installationError = "";
  state.installationsLoading = false;
  persist("tenant", state.tenantId);
  if (!state.tenantId) {
    state.contextLoading = false;
    return;
  }
  state.contextLoading = true;
  try {
    const result = await api<List<Project>>(tenantPath("/projects"));
    if (sequence !== contextSequence) return;
    state.projects = result.items;
    const preferred = saved(`project:${state.tenantId}`);
    await selectProject(
      result.items.find((item) => item.id === preferred)?.id ??
        result.items[0]?.id ??
        "",
    );
  } catch (error) {
    if (sequence === contextSequence) state.contextError = errorMessage(error);
  } finally {
    if (sequence === contextSequence) state.contextLoading = false;
  }
}
async function refreshTenants(preferred = state.tenantId) {
  const result = await api<List<Tenant>>("/api/v1/tenants");
  state.tenants = result.items;
  await selectTenant(
    result.items.find((item) => item.id === preferred)?.id ??
      result.items[0]?.id ??
      "",
  );
}
async function bootstrap() {
  state.loading = true;
  state.error = "";
  try {
    state.session = await api<Session>("/api/v1/session");
    setCSRFToken(state.session.csrfToken);
    if (!state.session.authenticated) return;
    const [catalog] = await Promise.all([
      api<List<Application>>("/api/v1/catalog"),
      refreshTenants(saved("tenant")),
    ]);
    state.catalog = catalog.items;
  } catch (error) {
    state.error = errorMessage(error);
  } finally {
    state.loading = false;
  }
}
function expireSession() {
  ++contextSequence;
  ++installationSequence;
  setCSRFToken();
  if (state.session)
    state.session = {
      authenticated: false,
      loginAvailable: state.session.loginAvailable,
    };
  state.tenants = [];
  state.projects = [];
  state.installations = [];
  state.tenantId = "";
  state.projectId = "";
}
window.addEventListener("cloud:unauthorized", expireSession);
export function useCloud() {
  return {
    state,
    tenant,
    project,
    canManageTeam,
    canManageProject,
    projectPath,
    tenantPath,
    notify,
    bootstrap,
    refreshTenants,
    selectTenant,
    selectProject,
    refreshInstallations,
    expireSession,
  };
}
