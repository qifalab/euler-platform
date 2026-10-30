import { flushPromises, mount } from "@vue/test-utils";
import { afterEach, describe, expect, it, vi } from "vitest";
import App from "./App.vue";
import { CloudError } from "../../api";
import { appContextKey, type AppContext, type AppPermission } from "../shared";

type Status = "active" | "error";

function setup(permissions: AppPermission[] = ["read", "write", "manage", "secrets"]) {
  const state = { status: "active" as Status, mysqlCount: 1 };
  const overview = () => ({
    quota: { mysqlMB: 100, mysqlCount: state.mysqlCount, postgresMB: 100, postgresCount: 1 },
    mysqlUsedBytes: 0,
    mysqlCount: 1,
    postgresUsedBytes: 0,
    postgresCount: 0,
    engines: { mysql: true, postgresql: true },
    items: [
      {
        id: "db1",
        name: "订单库",
        type: "mysql",
        database: "e_1",
        username: "u_1",
        status: state.status,
        sizeBytes: 0,
        createdAt: "2026-10-01T00:00:00Z",
        lastError: state.status === "error" ? "引擎操作未确认完成，请运营人员核验后处理" : "",
      },
    ],
  });
  const request = vi.fn(async (path: string, options?: { method?: string; body?: unknown }) => {
    const method = options?.method ?? "GET";
    if (path === "/" && method === "GET") return overview();
    if (path === "/databases/db1" && method === "DELETE") {
      // The engine outcome is unknown: the backend records error/uncertain.
      state.status = "error";
      throw new CloudError("删除结果未确认，记录和配额预留已保留", 503, "unavailable");
    }
    if (path === "/packages" && method === "GET") return { items: [], quota: {}, base: {} };
    if (path === "/packages/redeem" && method === "POST") {
      state.mysqlCount = 3;
      return { id: "pkg_1", name: "扩容包" };
    }
    throw new Error(`unexpected ${method} ${path}`);
  });
  const context = {
    scope: { actorId: "u", tenantId: "t", projectId: "p", installationId: "i", applicationId: "database", permissions },
    can: (p: AppPermission) => permissions.includes(p),
    request,
    notify: vi.fn(),
  } as unknown as AppContext;
  const wrapper = mount(App, { global: { provide: { [appContextKey as symbol]: context } } });
  return { wrapper, request };
}
function button(wrapper: ReturnType<typeof mount>, text: string) {
  const value = wrapper.findAll("button").find((b) => b.text().startsWith(text));
  if (!value) throw Error(`Missing ${text}`);
  return value;
}

afterEach(() => vi.restoreAllMocks());

describe("database console state after engine operations", () => {
  it("reloads the list after an unconfirmed engine result and keeps the error visible", async () => {
    const { wrapper, request } = setup();
    await flushPromises();
    expect(wrapper.text()).toContain("运行中");
    vi.spyOn(window, "confirm").mockReturnValue(true);
    await button(wrapper, "删除").trigger("click");
    await flushPromises();
    expect(request.mock.calls.filter(([path]) => path === "/").length).toBe(2);
    expect(wrapper.text()).toContain("需要核验");
    expect(wrapper.find('[role="alert"]').text()).toContain("删除结果未确认");
    wrapper.unmount();
  });
  it("refreshes the project quota after a package is redeemed", async () => {
    const { wrapper } = setup();
    await flushPromises();
    expect(wrapper.find(".metrics strong").text()).toContain("1 / 1");
    await button(wrapper, "资源包").trigger("click");
    await flushPromises();
    await wrapper.find('input[placeholder="输入资源包兑换码"]').setValue("CODE-1");
    await wrapper.find("form.inline").trigger("submit");
    await flushPromises();
    expect(wrapper.find(".metrics strong").text()).toContain("1 / 3");
    wrapper.unmount();
  });
});
