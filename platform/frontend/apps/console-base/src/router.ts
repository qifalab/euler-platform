/**
 * Router (02§5.3 route guard, §2.2 URL convention).
 * First path segment = productCode (e.g. /scecs/instances); the shell owns
 * first-level routing, sub-apps own level 2+ via Wujie URL sync.
 */
import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";
import { useRegistry } from "./registry";
import { useAuthStore } from "./stores/auth";

const Overview = () => import("./views/Overview.vue");
const NotFound = () => import("./views/NotFound.vue");

const routes: RouteRecordRaw[] = [
  { path: "/", name: "overview", component: Overview, meta: { title: "总览" } },
  // Catch-all: any /productCode/* is handed to a Wujie sub-app via the container.
  { path: "/:productCode(.*)?", name: "sub-app", component: () => import("./views/SubAppHost.vue") },
  { path: "/404", name: "not-found", component: NotFound },
];

export const router = createRouter({
  history: createWebHistory(),
  routes,
});

// Three-level guard (02§5.3): whitelist → silent refresh → permission check.
router.beforeEach(async (to) => {
  document.title = (to.meta.title as string | undefined) ?? "辰云控制台";

  const auth = useAuthStore();
  const registry = useRegistry();

  // 2) No token in memory → try silent refresh once (cookie may still be valid).
  if (!auth.accessToken) {
    const refreshed = await auth.silentRefresh();
    if (!refreshed) {
      // In the scaffold we don't bounce to account.starcloud.cn; real deploy
      // redirects with `?redirect=<target>` (02§5.3).
      console.warn("[console-base] not authenticated — redirecting to account in real deploy");
    }
  }

  // 3) Real-name gate (01§4.5/07§2.3): opening/operating resources requires
  // verified real name. Unverified accounts hitting a product route are bounced
  // to the account center for 实名. The overview page stays accessible.
  if (to.path !== "/" && !auth.isRealNameVerified && auth.isAuthenticated) {
    const app = registry.resolve(to.path);
    if (app && app.productCodes.length > 0) {
      console.warn("[console-base] real-name verification required for", to.path, "— open http://localhost:5175/realname");
      return false;
    }
  }

  // 4) Permission check: first path segment → productCode → RAM action prefix.
  if (to.path !== "/") {
    const app = registry.resolve(to.path);
    if (!app) return { name: "not-found" };
    // UI gate only; resource-level authz stays backend-decided (02§5.3).
    const productCode = registry.productCodeOf(to.path);
    if (productCode && !auth.can(`${productCode}:Read`)) {
      // Scaffold: no real IAM snapshot, so we don't hard-block. Real deploy
      // returns { name: "no-permission", query: { app: productCode } }.
    }
  }
  return true;
});
