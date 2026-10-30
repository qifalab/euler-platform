/**
 * @vitest-environment jsdom
 *
 * Shell deep-link context switch (?tenantId=&projectId=). If the user
 * navigates elsewhere while the linked team is still loading, the newer
 * navigation supersedes the switch; the shell must not stay on the
 * "正在切换项目空间" loading state forever.
 */
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import { createMemoryHistory, createRouter } from "vue-router";
import { h } from "vue";
import CloudShell from "./CloudShell.vue";

const page = (name: string) => ({
  render: () => h("div", { "data-testid": "page" }, name),
});
const json = (body: unknown) =>
  new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
const project = (id: string, tenantId: string) => ({
  id,
  tenantId,
  name: id,
  role: "admin",
  status: "active",
  createdAt: "",
});

let wrapper: VueWrapper | undefined;
beforeAll(() => {
  // jsdom has no modal dialog support; CloudModal only needs open/close.
  Object.defineProperty(HTMLDialogElement.prototype, "showModal", {
    configurable: true,
    value(this: HTMLDialogElement) {
      this.setAttribute("open", "");
    },
  });
  Object.defineProperty(HTMLDialogElement.prototype, "close", {
    configurable: true,
    value(this: HTMLDialogElement) {
      this.removeAttribute("open");
    },
  });
});
afterEach(() => {
  wrapper?.unmount();
  wrapper = undefined;
  vi.unstubAllGlobals();
});

describe("CloudShell link context", () => {
  it("does not stay loading when a deep-link switch is superseded", async () => {
    localStorage.clear();
    let releaseTeamB!: () => void;
    const teamBProjects = new Promise<Response>((resolve) => {
      releaseTeamB = () => resolve(json({ items: [project("prj_b", "ten_b")] }));
    });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: unknown) => {
        const path = String(input);
        if (path === "/api/v1/session")
          return json({
            authenticated: true,
            loginAvailable: true,
            csrfToken: "csrf",
            user: { id: "u_me", displayName: "Me", provider: "t", subject: "s" },
          });
        if (path === "/api/v1/catalog") return json({ items: [] });
        if (path === "/api/v1/tenants")
          return json({
            items: [
              { id: "ten_a", name: "A", role: "admin", createdAt: "" },
              { id: "ten_b", name: "B", role: "admin", createdAt: "" },
            ],
          });
        if (path === "/api/v1/tenants/ten_a/projects")
          return json({ items: [project("prj_a", "ten_a")] });
        if (path === "/api/v1/tenants/ten_b/projects") return teamBProjects;
        if (path.endsWith("/installations")) return json({ items: [] });
        return new Response("{}", { status: 404 });
      }),
    );
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [
        { path: "/", component: page("overview"), meta: { project: true } },
        { path: "/catalog", component: page("catalog") },
        {
          path: "/apps/:applicationId",
          component: page("app"),
          meta: { project: true },
        },
        { path: "/:rest(.*)*", component: page("other") },
      ],
    });
    await router.push("/");
    wrapper = mount(CloudShell, { global: { plugins: [router] } });
    await flushPromises();
    expect(wrapper.find('[data-testid="page"]').text()).toBe("overview");

    // Open a link into team B; its project list is still loading...
    await router.push("/apps/eid?tenantId=ten_b&projectId=prj_b");
    await flushPromises();
    expect(wrapper.text()).toContain("正在切换项目空间");
    // ...and the user moves on before it arrives.
    await router.push("/catalog");
    await flushPromises();
    releaseTeamB();
    await flushPromises();

    expect(wrapper.text()).not.toContain("正在切换项目空间");
    expect(wrapper.find('[data-testid="page"]').text()).toBe("catalog");
  });
});
