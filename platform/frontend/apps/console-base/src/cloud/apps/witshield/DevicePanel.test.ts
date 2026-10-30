import { mount, flushPromises } from "@vue/test-utils";
import { describe, it, expect, vi } from "vitest";
import { appContextKey, type AppContext, type AppPermission } from "../shared";
import DevicePanel from "./DevicePanel.vue";

describe("WitShield enrollment token form", () => {
  it("keeps the token name within the engine's 100-byte limit", async () => {
    const permissions: AppPermission[] = ["read", "write", "manage"];
    const request = vi.fn(
      async (path: string, _options?: { method?: string; body?: unknown }) =>
        path === "/instance"
          ? { publicAgentURL: "https://euler.example/public/witshield/t/p/i" }
          : { items: [] },
    );
    const value = {
      scope: { permissions },
      can: (permission: AppPermission) => permissions.includes(permission),
      request,
      notify: vi.fn(),
    } as unknown as AppContext;
    const wrapper = mount(DevicePanel, {
      props: { devices: [] },
      global: { provide: { [appContextKey as symbol]: value } },
    });
    await flushPromises();
    // 40 Chinese characters pass maxlength="100" but are 120 UTF-8 bytes;
    // POST /enrollment-tokens rejects names over 100 bytes.
    await wrapper.get("input[maxlength]").setValue("生产环境数据库主节点".repeat(4));
    await wrapper
      .findAll("form")
      .find((f) => f.text().includes("生成一次性注册码"))!
      .trigger("submit");
    await flushPromises();
    expect(
      request.mock.calls.some(([, options]) => options?.method === "POST"),
    ).toBe(false);
    expect(wrapper.text()).toContain("100 字节");
    wrapper.unmount();
  });
});
