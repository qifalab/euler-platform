/**
 * @vitest-environment jsdom
 *
 * Shell router guard: unknown paths land on the 404 page exactly once (the
 * redirect used to re-enter the guard forever), and a deep link navigated
 * before the registry finished loading waits for it instead of 404-ing.
 */
import { describe, it, expect, vi, beforeEach } from "vitest";
import { createPinia, setActivePinia } from "pinia";

vi.mock("element-plus", () => ({ ElMessage: { warning: vi.fn() } }));
vi.mock("./views/Overview.vue", () => ({ default: { render: () => null } }));
vi.mock("./views/NotFound.vue", () => ({ default: { render: () => null } }));
vi.mock("./views/SubAppHost.vue", () => ({ default: { render: () => null } }));

const auth = vi.hoisted(() => ({
  accessToken: "tok",
  isRealNameVerified: true,
  isAuthenticated: true,
  silentRefresh: async () => "tok",
  can: () => true,
}));
vi.mock("./stores/auth", () => ({ useAuthStore: () => auth }));

import { router } from "./router";
import { useRegistry } from "./registry";

describe("console-base router guard", () => {
  beforeEach(async () => {
    setActivePinia(createPinia());
    sessionStorage.clear();
    // No svc-api-meta: the registry falls back to its built-in list.
    vi.stubGlobal("fetch", vi.fn(async () => { throw new Error("offline"); }));
    vi.spyOn(console, "warn").mockImplementation(() => {});
    vi.spyOn(console, "error").mockImplementation(() => {}); // the shell SDK logs the offline error
    await router.replace("/");
  });

  it("routes an unknown product path to not-found without looping", async () => {
    const guard = vi.fn();
    const off = router.beforeEach(() => { guard(); return true; });
    try {
      await useRegistry().load();
      await router.push("/no-such-product/x");
      expect(router.currentRoute.value.name).toBe("not-found");
      // original navigation + the single redirect to /404
      expect(guard.mock.calls.length).toBeLessThanOrEqual(2);
    } finally {
      off();
    }
  });

  it("waits for the registry instead of 404-ing a deep link", async () => {
    // Registry NOT loaded yet when the navigation starts (as during boot).
    expect(useRegistry().loaded).toBe(false);
    await router.push("/euecs/instances");
    expect(router.currentRoute.value.name).toBe("sub-app");
    expect(router.currentRoute.value.path).toBe("/euecs/instances");
  });
});
