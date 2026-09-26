import { mount, flushPromises } from "@vue/test-utils";
import { describe, it, expect, vi } from "vitest";
import { appContextKey, type AppContext, type AppPermission } from "../shared";
import ActionReview from "./ActionReview.vue";
import DevicePanel from "./DevicePanel.vue";
import SettingsPanel from "./SettingsPanel.vue";
import type { Prepared } from "./model";

const prepared: Prepared = {
  action: {
    id: "action-one",
    deviceId: "device-one",
    type: "ssh_password_hardening",
    parameters: { rollbackAfterSeconds: 300 },
    preview: {
      impact: "SSH access changes",
      rollback: "Restore configuration",
    },
    status: "draft",
    createdAt: "2026-09-26",
    updatedAt: "2026-09-26",
  },
  approvalNonce: "one-use-nonce",
  approvalExpiresAt: "2099-01-01T00:00:00Z",
};
function context(
  permissions: AppPermission[],
  handler?: (
    path: string,
    options?: { method?: string; body?: unknown },
  ) => unknown,
) {
  const request = vi.fn(
    async (path: string, options?: { method?: string; body?: unknown }) =>
      handler?.(path, options) ?? {},
  );
  const value = {
    scope: { permissions },
    can: (permission: AppPermission) => permissions.includes(permission),
    request,
    notify: vi.fn(),
  } as unknown as AppContext;
  return { request, global: { provide: { [appContextKey as symbol]: value } } };
}
describe("WitShield explicit approval and credential boundaries", () => {
  it("requires a separate checked approval and sends only the one-time nonce", async () => {
    const setup = context(["read", "manage"]);
    const wrapper = mount(ActionReview, {
      props: { prepared },
      global: setup.global,
    });
    expect(setup.request).not.toHaveBeenCalled();
    const approve = wrapper
      .findAll("button")
      .find((b) => b.text() === "批准本次操作")!;
    expect(approve.attributes("disabled")).toBeDefined();
    await wrapper.get("input[type=checkbox]").setValue(true);
    await approve.trigger("click");
    await flushPromises();
    expect(setup.request).toHaveBeenCalledExactlyOnceWith(
      "/actions/action-one/approve",
      { method: "POST", body: { approvalNonce: "one-use-nonce" } },
    );
    expect(wrapper.emitted("approved")).toHaveLength(1);
    expect(wrapper.emitted("close")).toHaveLength(1);
    wrapper.unmount();
  });
  it("does not permit expired or read-only approvals", async () => {
    const setup = context(["read", "manage"]);
    const wrapper = mount(ActionReview, {
      props: {
        prepared: { ...prepared, approvalExpiresAt: "2000-01-01T00:00:00Z" },
      },
      global: setup.global,
    });
    await wrapper.get("input[type=checkbox]").setValue(true);
    await wrapper
      .findAll("button")
      .find((b) => b.text() === "批准本次操作")!
      .trigger("click");
    await flushPromises();
    expect(setup.request).not.toHaveBeenCalled();
    expect(wrapper.text()).toContain("审批已过期");
    wrapper.unmount();
    const viewer = context(["read"]);
    const readonly = mount(ActionReview, {
      props: { prepared },
      global: viewer.global,
    });
    await readonly.get("input[type=checkbox]").setValue(true);
    expect(
      readonly
        .findAll("button")
        .find((b) => b.text() === "批准本次操作")!
        .attributes("disabled"),
    ).toBeDefined();
    readonly.unmount();
  });
  it("prepares an action without approving and blocks observer-only action forms", async () => {
    const setup = context(["read", "write", "manage"], (path) =>
      path === "/instance"
        ? {
            publicAgentURL:
              "https://euler.example/public/witshield/team/project/install",
          }
        : path === "/actions"
          ? prepared
          : path.endsWith("/defense-policy")
            ? {
                enabled: false,
                emergencyStop: false,
                autoBan: false,
                failureThreshold: 30,
                window: "5m",
                banDuration: "15m",
                maxBansPerHour: 20,
                allowlist: [],
              }
            : { items: [] },
    );
    const device = {
      id: "device-one",
      name: "Linux host",
      hostname: "host",
      os: "linux",
      arch: "amd64",
      agentVersion: "euler",
      observerOnly: false,
      status: "online",
      enrolledAt: "2026-09-26",
    };
    const wrapper = mount(DevicePanel, {
      props: { devices: [device] },
      global: setup.global,
    });
    await flushPromises();
    const form = wrapper
      .findAll("form")
      .find((f) => f.text().includes("生成预览，下一步审批"))!;
    await form.trigger("submit");
    await flushPromises();
    expect(
      setup.request.mock.calls.filter(([path]) => path === "/actions"),
    ).toHaveLength(1);
    expect(
      setup.request.mock.calls.some(([path]) => path.endsWith("/approve")),
    ).toBe(false);
    expect(wrapper.emitted("prepared")?.[0]).toEqual([prepared]);
    await wrapper.setProps({ devices: [{ ...device, observerOnly: true }] });
    expect(form.get("fieldset").attributes("disabled")).toBeDefined();
    wrapper.unmount();
  });
  it("preserves stored AI secrets without submitting redaction masks", async () => {
    const ai = {
      protocol: "openai_responses",
      baseUrl: "https://ai.example/v1",
      model: "model-one",
      keyConfigured: true,
      apiKeyHint: "••••1234",
      customHeaders: { "X-Secret": "••••••" },
    };
    const setup = context(["read", "manage"], (path) =>
      path === "/ai/settings"
        ? ai
        : {
            webhookEnabled: false,
            webhookUrl: "",
            smtpEnabled: false,
            smtpHost: "",
            smtpPort: 587,
            smtpUsername: "",
            smtpFrom: "",
            smtpTo: [],
          },
    );
    const wrapper = mount(SettingsPanel, { global: setup.global });
    await flushPromises();
    const form = wrapper.findAll("form")[0];
    await form.trigger("submit");
    await flushPromises();
    const update = setup.request.mock.calls.find(
      ([path, options]) => path === "/ai/settings" && options?.method === "PUT",
    );
    expect(update?.[1]?.body).toEqual({
      protocol: "openai_responses",
      baseUrl: "https://ai.example/v1",
      model: "model-one",
    });
    expect(JSON.stringify(update)).not.toContain("••");
    wrapper.unmount();
  });
  it("requires explicit secret handling when the AI origin changes", async () => {
    const ai = {
      protocol: "openai_responses",
      baseUrl: "https://ai.example/v1",
      model: "model-one",
      keyConfigured: true,
      customHeaders: {},
    };
    const setup = context(["read", "manage"], (path) =>
      path === "/ai/settings"
        ? ai
        : { webhookEnabled: false, smtpEnabled: false },
    );
    const wrapper = mount(SettingsPanel, { global: setup.global });
    await flushPromises();
    await wrapper
      .findAll("form")[0]
      .get("input[type=url]")
      .setValue("https://new-provider.example/v1");
    await wrapper.findAll("form")[0].trigger("submit");
    await flushPromises();
    expect(wrapper.text()).toContain(
      "更换 AI 服务来源时必须重新输入或清除旧密钥",
    );
    expect(
      setup.request.mock.calls.some(([, options]) => options?.method === "PUT"),
    ).toBe(false);
    wrapper.unmount();
  });
});
