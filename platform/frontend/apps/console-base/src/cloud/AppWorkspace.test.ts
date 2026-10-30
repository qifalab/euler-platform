/**
 * @vitest-environment jsdom
 *
 * Application workspace error state: when the project's installation list
 * failed to load, "重试" must reload that list. load() alone has no
 * installation to open and returns early, so the error could never clear.
 */
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import { createMemoryHistory, createRouter } from "vue-router";
import AppWorkspace from "./AppWorkspace.vue";
import { useCloud } from "./context";

const { state } = useCloud();
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

describe("AppWorkspace", () => {
  it("retries the installation list when that is what failed", async () => {
    const installations = "/api/v1/tenants/ten_1/projects/prj_1/installations";
    const fetchMock = vi.fn(async (input: unknown) => {
      if (String(input) === installations)
        return new Response(
          JSON.stringify({
            items: [
              {
                id: "ins_1",
                tenantId: "ten_1",
                projectId: "prj_1",
                applicationId: "storage",
                status: "disabled",
                createdAt: "",
              },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        );
      return new Response("{}", { status: 404 });
    });
    vi.stubGlobal("fetch", fetchMock);
    Object.assign(state, {
      session: { authenticated: true, loginAvailable: true },
      catalog: [
        {
          id: "storage",
          name: "Storage",
          category: "数据",
          description: "",
          color: "",
          homepage: "",
          repository: "",
          capabilities: [],
          connectionMode: "native",
          limitations: [],
        },
      ],
      tenants: [{ id: "ten_1", name: "Team", role: "admin", createdAt: "" }],
      tenantId: "ten_1",
      projects: [
        {
          id: "prj_1",
          tenantId: "ten_1",
          name: "Project",
          role: "admin",
          status: "active",
          createdAt: "",
        },
      ],
      projectId: "prj_1",
      installations: [],
      installationsLoading: false,
      installationError: "暂时无法连接服务，请检查网络后重试。",
    });
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [
        { path: "/apps/:applicationId", component: AppWorkspace },
        { path: "/:rest(.*)*", component: { render: () => null } },
      ],
    });
    await router.push("/apps/storage");
    wrapper = mount(AppWorkspace, { global: { plugins: [router] } });
    await flushPromises();
    expect(wrapper.text()).toContain("应用暂时无法打开");

    const retry = wrapper.findAll("button").find((b) => b.text() === "重试");
    await retry!.trigger("click");
    await flushPromises();

    expect(fetchMock.mock.calls.map(([input]) => String(input))).toContain(
      installations,
    );
    expect(state.installationError).toBe("");
    expect(wrapper.text()).not.toContain("应用暂时无法打开");
    expect(wrapper.text()).toContain("此应用已暂停使用");
  });
});
