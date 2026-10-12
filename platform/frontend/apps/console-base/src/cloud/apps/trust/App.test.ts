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
});
