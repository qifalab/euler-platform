/**
 * @vitest-environment jsdom
 *
 * Application-cloud router (the one main.ts boots). It has no registry guard:
 * CloudShell.vue gates session, team and project in its template, and an
 * application deep link (/apps/<id>?tenantId=&projectId=) is resolved by the
 * shell, not by a sub-app registry. These tests pin the route-table contract
 * the shell relies on. They replace tests written for the retired IaaS/Wujie
 * guard (named "not-found"/"sub-app" routes, /euecs deep links), which this
 * router intentionally no longer has.
 */
import { describe, it, expect, vi, beforeEach } from "vitest";
import { router } from "./router";
import NotFoundPage from "./cloud/NotFoundPage.vue";
import AppWorkspace from "./cloud/AppWorkspace.vue";
import ServiceAccountsPage from "./cloud/ServiceAccountsPage.vue";

describe("console-base application-cloud router", () => {
  beforeEach(async () => {
    // vue-router's scrollBehavior calls window.scrollTo, which jsdom lacks.
    vi.stubGlobal("scrollTo", vi.fn());
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new Error("offline");
      }),
    );
    await router.replace("/");
  });

  it("routes an unknown path to the 404 page in a single navigation", async () => {
    const guard = vi.fn(() => true);
    const off = router.beforeEach(guard);
    try {
      await router.push("/no-such-product/x");
    } finally {
      off();
    }
    const route = router.currentRoute.value;
    // No redirect: the URL is kept and the catch-all renders the 404 page.
    expect(route.fullPath).toBe("/no-such-product/x");
    expect(route.matched).toHaveLength(1);
    expect(route.matched[0].path).toBe("/:pathMatch(.*)*");
    expect(route.matched[0].components?.default).toBe(NotFoundPage);
    expect(guard).toHaveBeenCalledTimes(1);
    expect(document.title).toBe("页面不存在 · 欧拉应用云");
  });

  it("resolves an application deep link directly and keeps its context query", async () => {
    await router.push("/apps/storage?tenantId=ten_1&projectId=prj_1");
    const route = router.currentRoute.value;
    expect(route.matched[0].path).toBe("/apps/:applicationId");
    expect(route.matched[0].components?.default).toBe(AppWorkspace);
    expect(route.params.applicationId).toBe("storage");
    // CloudShell.selectLinkContext reads these to switch team and project.
    expect(route.query).toEqual({ tenantId: "ten_1", projectId: "prj_1" });
    expect(route.meta.project).toBe(true);
    expect(document.title).toBe("应用详情 · 欧拉应用云");
  });

  it("resolves every shell page instead of falling through to 404", async () => {
    await router.push("/service-accounts");
    expect(router.currentRoute.value.matched[0].components?.default).toBe(
      ServiceAccountsPage,
    );
    for (const path of [
      "/",
      "/catalog",
      "/members",
      "/identity",
      "/permissions",
      "/automation",
      "/services",
      "/project-settings",
      "/audit",
      "/join",
    ]) {
      const resolved = router.resolve(path);
      expect(resolved.matched, path).toHaveLength(1);
      expect(resolved.matched[0].path, path).toBe(path);
    }
  });

  it("marks the pages that need a selected project with meta.project", () => {
    // CloudShell renders the "create a project" state instead of these pages
    // when no project is selected, so their projectPath() calls cannot throw.
    // Team-level pages (catalog, members, audit, join) must stay reachable.
    const scoped = router
      .getRoutes()
      .filter((route) => route.meta.project)
      .map((route) => route.path)
      .sort();
    expect(scoped).toEqual(
      [
        "/",
        "/apps/:applicationId",
        "/automation",
        "/identity",
        "/permissions",
        "/project-settings",
        "/service-accounts",
        "/services",
      ].sort(),
    );
  });
});
