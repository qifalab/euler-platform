import { describe, expect, it, vi } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import App from "./App.vue";
import { appContextKey, type AppContext, type AppPermission } from "../shared";

const scheme = {
  id: "tsc_one",
  name: "成员认证",
  description: "提供所需资料",
  status: "active",
  version: 1,
  fields: [
    {
      id: "name",
      name: "full_name",
      label: "姓名",
      type: "text",
      required: true,
      description: "",
      validations: {},
    },
    {
      id: "proof",
      name: "proof",
      label: "证明",
      type: "file",
      required: false,
      description: "",
      validations: {},
    },
  ],
};
function context(
  permissions: AppPermission[],
  responder?: (path: string, options?: any) => unknown,
) {
  const request = vi.fn(async (path: string, options?: any) => {
    if (responder) {
      const result = responder(path, options);
      if (result !== undefined) return result;
    }
    if (path === "/bootstrap") return { schemes: [scheme], submissions: [] };
    if (path === "/schemes/tsc_one") return structuredClone(scheme);
    if (path.startsWith("/review/submissions?"))
      return { items: [], total: 0, page: 0, limit: 20 };
    return { items: [] };
  });
  const app: AppContext = {
    scope: {
      actorId: "alice",
      actorName: "Alice",
      email: "alice@example.test",
      emailVerified: true,
      provider: "issuer",
      subject: "alice",
      tenantId: "t",
      projectId: "p",
      installationId: "i",
      applicationId: "trust",
      permissions,
    },
    basePath: "/api/trust",
    publicBase: "https://euler.test/public/trust",
    can: (permission) => permissions.includes(permission),
    request: request as AppContext["request"],
    upload: vi.fn() as AppContext["upload"],
    download: vi.fn(),
    notify: vi.fn(),
  };
  return { app, request };
}
async function mounted(
  permissions: AppPermission[],
  responder?: (path: string, options?: any) => unknown,
) {
  const { app, request } = context(permissions, responder);
  const wrapper = mount(App, {
    global: { provide: { [appContextKey as symbol]: app } },
  });
  await flushPromises();
  return { wrapper, app, request };
}
function button(wrapper: ReturnType<typeof mount>, text: string) {
  const found = wrapper.findAll("button").find((b) => b.text() === text);
  if (!found) throw new Error(`Missing button ${text}`);
  return found;
}

describe("Trust native workflows", () => {
  it("shows business controls according to independent product permissions", async () => {
    const { wrapper } = await mounted(["read", "write", "manage", "secrets"]);
    expect(wrapper.text()).toContain("成员认证");
    expect(wrapper.text()).not.toContain("审核工作台");
    expect(wrapper.text()).not.toContain("查询密钥");
    await button(wrapper, "认证方案").trigger("click");
    await flushPromises();
    expect(wrapper.text()).not.toContain("创建方案");
    expect(wrapper.text()).toContain("姓名");
    wrapper.unmount();
  });
  it("renders dynamic fields and submits the current scheme version without client identities", async () => {
    const { wrapper, request } = await mounted(
      ["read", "write"],
      (path, options) =>
        path === "/submissions" && options?.method === "POST"
          ? {
              id: "sub",
              schemeId: scheme.id,
              schemeName: scheme.name,
              actorId: "alice",
              status: "pending",
              version: 1,
              fields: scheme.fields,
              data: options.body.data,
              materials: [],
              history: [],
              updatedAt: new Date().toISOString(),
            }
          : undefined,
    );
    await button(wrapper, "开始认证").trigger("click");
    await flushPromises();
    expect(
      wrapper.find("[role=dialog][aria-label='提交认证材料']").exists(),
    ).toBe(true);
    await wrapper.find("#trust-full_name").setValue("张三");
    expect(wrapper.find("#trust-proof[type=file]").exists()).toBe(true);
    await wrapper.find("[aria-label='提交认证材料'] form").trigger("submit");
    await flushPromises();
    expect(request).toHaveBeenCalledWith("/submissions", {
      method: "POST",
      body: {
        schemeId: "tsc_one",
        schemeVersion: 1,
        version: 0,
        data: { full_name: "张三" },
      },
    });
    expect(wrapper.text()).toContain("完整处理历史");
    wrapper.unmount();
  });
  it("provides real field editing controls for operators", async () => {
    const { wrapper } = await mounted(["read", "admin"]);
    await button(wrapper, "认证方案").trigger("click");
    await flushPromises();
    await button(wrapper, "创建方案").trigger("click");
    const dialog = wrapper.find("[aria-label='编辑认证方案']");
    expect(dialog.exists()).toBe(true);
    expect(dialog.text()).toContain("字段标题");
    expect(dialog.text()).toContain("字段类型");
    await button(wrapper, "添加字段").trigger("click");
    expect(dialog.findAll(".field-editor")).toHaveLength(2);
    expect(wrapper.text()).not.toContain("审核工作台");
    wrapper.unmount();
  });
  it("requires secret management permission before showing key issuance or revocation", async () => {
    const responder = (path: string) =>
      path === "/keys"
        ? {
            items: [
              {
                id: "key_one",
                name: "状态查询",
                schemeIds: [scheme.id],
                actorIds: ["alice"],
                details: false,
                expiresAt: "2026-10-01T00:00:00Z",
                revokedAt: "",
              },
            ],
          }
        : undefined;
    const operator = await mounted(["read", "admin"], responder);
    await button(operator.wrapper, "查询密钥").trigger("click");
    await flushPromises();
    expect(operator.wrapper.text()).toContain("状态查询");
    expect(
      operator.wrapper
        .findAll("button")
        .some((b) => ["签发密钥", "撤销"].includes(b.text())),
    ).toBe(false);
    operator.wrapper.unmount();
    const authorized = await mounted(["read", "admin", "secrets"], responder);
    await button(authorized.wrapper, "查询密钥").trigger("click");
    await flushPromises();
    expect(button(authorized.wrapper, "签发密钥").exists()).toBe(true);
    expect(button(authorized.wrapper, "撤销").exists()).toBe(true);
    authorized.wrapper.unmount();
  });
  it("keeps line breaks while an operator types select options one per line", async () => {
    const { wrapper, request } = await mounted(["read", "admin"], (path, options) =>
      path === "/schemes" && options?.method === "POST"
        ? { ...structuredClone(scheme), id: "tsc_new" }
        : undefined,
    );
    await button(wrapper, "认证方案").trigger("click");
    await flushPromises();
    await button(wrapper, "创建方案").trigger("click");
    const dialog = wrapper.find("[aria-label='编辑认证方案']");
    await dialog.find(".form-grid input").setValue("学籍认证");
    await dialog.find(".field-editor input").setValue("年级");
    await dialog.find("select[aria-label='字段类型']").setValue("select");
    const options = dialog.find(".field-editor textarea");
    await options.setValue("大一\n");
    expect((options.element as HTMLTextAreaElement).value).toBe("大一\n");
    await options.setValue("大一\n大二\n");
    expect((options.element as HTMLTextAreaElement).value).toBe("大一\n大二\n");
    await dialog.find("form").trigger("submit");
    await flushPromises();
    const saved = request.mock.calls.find(
      ([path, init]) => path === "/schemes" && init?.method === "POST",
    );
    expect(saved?.[1]?.body.fields[0].options).toEqual(["大一", "大二"]);
    wrapper.unmount();
  });
  it("clears an earlier API test result when a later status query fails", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          success: true,
          data: { verified: true, status: "approved" },
        }),
      })
      .mockResolvedValueOnce({
        ok: false,
        json: async () => ({
          error: { code: "forbidden", message: "查询超出密钥范围" },
        }),
      });
    vi.stubGlobal("fetch", fetchMock);
    try {
      const { wrapper } = await mounted(["read", "admin"]);
      await button(wrapper, "API 文档").trigger("click");
      await flushPromises();
      const form = wrapper.find(".docs form");
      await form.find("input[type=password]").setValue("trust_token");
      await form.find("select").setValue("tsc_one");
      await form.trigger("submit");
      await flushPromises();
      expect(form.find("p.alert").text()).toContain("已验证");
      await form.find("input:not([type=password])").setValue("bob");
      await form.trigger("submit");
      await flushPromises();
      expect(fetchMock).toHaveBeenCalledTimes(2);
      expect(wrapper.text()).toContain("查询超出密钥范围");
      expect(form.find("p.alert").exists()).toBe(false);
      wrapper.unmount();
    } finally {
      vi.unstubAllGlobals();
    }
  });
  it("defaults a new key to expire 30 days from now in the operator's local time", async () => {
    // console-base has no Node typings; vitest still runs in Node, where
    // assigning TZ switches the process time zone for later Date calls.
    const env = (
      globalThis as unknown as {
        process: { env: Record<string, string | undefined> };
      }
    ).process.env;
    const previous = env.TZ;
    env.TZ = "Asia/Shanghai";
    try {
      const { wrapper, request } = await mounted(["read", "admin", "secrets"]);
      await button(wrapper, "查询密钥").trigger("click");
      await flushPromises();
      await button(wrapper, "签发密钥").trigger("click");
      await flushPromises();
      const input = wrapper.find("input[type=datetime-local]");
      const expected = Date.now() + 30 * 86400000;
      const shown = new Date((input.element as HTMLInputElement).value);
      expect(Math.abs(shown.getTime() - expected)).toBeLessThan(120000);
      await wrapper.find("[aria-label='签发查询密钥'] input").setValue("状态");
      await wrapper
        .find("[aria-label='签发查询密钥'] input[type=checkbox][value='tsc_one']")
        .setValue(true);
      await wrapper.find("[aria-label='签发查询密钥'] form").trigger("submit");
      await flushPromises();
      const issued = request.mock.calls.find(
        ([path, init]) => path === "/keys" && init?.method === "POST",
      );
      const expiresAt = Date.parse(issued?.[1]?.body.expiresAt);
      expect(Math.abs(expiresAt - expected)).toBeLessThan(120000);
      wrapper.unmount();
    } finally {
      if (previous === undefined) delete env.TZ;
      else env.TZ = previous;
    }
  });
  it("labels lifecycle notifications with delivery states, not review states", async () => {
    const { wrapper } = await mounted(["read", "admin"], (path) =>
      path === "/notifications"
        ? {
            items: [
              {
                id: "tn_one",
                submissionId: "tsu_one",
                channel: "wecom",
                status: "pending",
                attempts: 0,
                lastError: "",
                createdAt: new Date().toISOString(),
              },
            ],
            channels: { wecom: true, feishu: false },
          }
        : undefined,
    );
    await button(wrapper, "通知记录").trigger("click");
    await flushPromises();
    const row = wrapper.find("tbody tr");
    expect(row.text()).toContain("待发送");
    expect(row.text()).not.toContain("待审核");
    wrapper.unmount();
  });
  function queueResponder(state: { pending: number }) {
    const item = (i: number) => ({
      id: `tsu_${i}`,
      schemeId: scheme.id,
      schemeName: scheme.name,
      actorId: `user${i}`,
      status: "pending",
      version: 1,
      createdAt: "2026-10-01T00:00:00Z",
      updatedAt: "2026-10-01T00:00:00Z",
    });
    return (path: string, options?: any) => {
      if (path.startsWith("/review/submissions?")) {
        const page = Number(
          new URLSearchParams(path.slice(path.indexOf("?") + 1)).get("page"),
        );
        const items = Array.from({ length: state.pending }, (_, i) =>
          item(i),
        ).slice(page * 20, page * 20 + 20);
        return { items, total: state.pending, page, limit: 20 };
      }
      if (path === "/review/submissions/tsu_20") {
        const decided = options?.method === "POST";
        if (decided) state.pending = 20;
        return {
          ...item(20),
          status: decided ? "approved" : "pending",
          version: decided ? 2 : 1,
          fields: scheme.fields,
          data: { full_name: "申请人" },
          materials: [],
          history: [],
        };
      }
      return undefined;
    };
  }
  it("returns to the last non-empty queue page after the only item on a page is decided", async () => {
    const state = { pending: 21 };
    const { wrapper } = await mounted(["read", "review"], queueResponder(state));
    await button(wrapper, "审核工作台").trigger("click");
    await flushPromises();
    await button(wrapper, "下一页").trigger("click");
    await flushPromises();
    expect(wrapper.findAll(".table-wrap tbody tr")).toHaveLength(1);
    await button(wrapper, "查看与审核").trigger("click");
    await flushPromises();
    await button(wrapper, "通过认证").trigger("click");
    await button(wrapper, "确认").trigger("click");
    await flushPromises();
    expect(wrapper.text()).toContain("第 1 页");
    expect(wrapper.findAll(".table-wrap tbody tr")).toHaveLength(20);
    wrapper.unmount();
  });
  it("reloads the open review queue when the workspace refresh button is used", async () => {
    const state = { pending: 1 };
    const { wrapper, request } = await mounted(
      ["read", "review"],
      queueResponder(state),
    );
    await button(wrapper, "审核工作台").trigger("click");
    await flushPromises();
    expect(wrapper.findAll(".table-wrap tbody tr")).toHaveLength(1);
    state.pending = 2;
    const before = request.mock.calls.length;
    await button(wrapper, "刷新").trigger("click");
    await flushPromises();
    expect(
      request.mock.calls
        .slice(before)
        .some(([path]) => String(path).startsWith("/review/submissions?")),
    ).toBe(true);
    expect(wrapper.findAll(".table-wrap tbody tr")).toHaveLength(2);
    wrapper.unmount();
  });
});
