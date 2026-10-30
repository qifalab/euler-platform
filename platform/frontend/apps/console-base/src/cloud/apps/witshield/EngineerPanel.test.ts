import { mount, flushPromises } from "@vue/test-utils";
import { describe, it, expect, vi } from "vitest";
import { appContextKey, type AppContext, type AppPermission } from "../shared";
import EngineerPanel from "./EngineerPanel.vue";

const device = {
  id: "device-one",
  name: "Linux host",
  hostname: "host",
  os: "linux",
  arch: "amd64",
  agentVersion: "euler",
  observerOnly: false,
  status: "online",
  enrolledAt: "2026-09-26T00:00:00Z",
};
const incident = {
  id: "incident-one",
  deviceId: device.id,
  correlationKey: "ssh",
  category: "network",
  severity: "high",
  status: "awaiting_approval",
  title: "SSH brute force",
  summary: "many failures",
  signalCount: 3,
  firstSeenAt: "2026-09-26T00:00:00Z",
  lastSeenAt: "2026-09-26T00:00:00Z",
  createdAt: "2026-09-26T00:00:00Z",
  updatedAt: "2026-09-26T00:00:00Z",
};
function detail(actionId?: string) {
  return {
    incident,
    signals: [],
    investigations: [],
    timeline: [],
    responsePlans: [
      {
        id: "plan-one",
        incidentId: incident.id,
        title: "Contain source",
        rationale: "brute force",
        risk: "low",
        status: actionId ? "approved" : "proposed",
        requiresApproval: true,
        steps: [
          {
            id: "step-one",
            actionType: "temporary_ip_ban",
            title: "Ban source",
            rationale: "brute force",
            parameters: { address: "198.51.100.7" },
            risk: "low",
            requiresApproval: true,
            ...(actionId ? { actionId } : {}),
          },
        ],
      },
    ],
  };
}
const prepared = {
  action: {
    id: "action-one",
    deviceId: device.id,
    type: "temporary_ip_ban",
    parameters: { address: "198.51.100.7" },
    preview: {},
    status: "draft",
    createdAt: "2026-09-26T00:00:00Z",
    updatedAt: "2026-09-26T00:00:00Z",
  },
  approvalNonce: "nonce",
  approvalExpiresAt: "2099-01-01T00:00:00Z",
};

describe("WitShield response plan preparation", () => {
  it("refreshes the incident after preparing a step so it cannot be prepared twice", async () => {
    let preparedOnServer = false;
    const permissions: AppPermission[] = ["read", "write", "manage"];
    const request = vi.fn(
      async (path: string, options?: { method?: string; body?: unknown }) => {
        if (path.startsWith("/incidents?")) return { items: [incident] };
        if (path === "/ai/investigation-policy")
          return { policy: { profile: "balanced" }, usage: {} };
        if (path === `/incidents/${incident.id}`)
          return detail(preparedOnServer ? "action-one" : undefined);
        if (path.endsWith("/prepare") && options?.method === "POST") {
          preparedOnServer = true;
          return prepared;
        }
        return {};
      },
    );
    const value = {
      scope: { permissions },
      can: (permission: AppPermission) => permissions.includes(permission),
      request,
      notify: vi.fn(),
    } as unknown as AppContext;
    const wrapper = mount(EngineerPanel, {
      props: { devices: [device] },
      global: { provide: { [appContextKey as symbol]: value } },
    });
    await flushPromises();
    await wrapper.get("button.ws-link").trigger("click");
    await flushPromises();
    const prepare = () =>
      wrapper.findAll("button").find((b) => b.text() === "准备此步骤并查看审批预览");
    expect(prepare()).toBeDefined();
    await prepare()!.trigger("click");
    await flushPromises();
    expect(wrapper.emitted("prepared")?.[0]).toEqual([prepared]);
    // The engine linked the new draft action to the step in the same
    // transaction; the open detail must show it instead of a stale button.
    expect(prepare()).toBeUndefined();
    expect(wrapper.text()).toContain("已经关联动作");
    // The workspace action list must learn about the new draft as well.
    expect(wrapper.emitted("changed")).toBeDefined();
    wrapper.unmount();
  });
  it("keeps an incident note within the engine's 1000-byte limit", async () => {
    const permissions: AppPermission[] = ["read", "write"];
    const request = vi.fn(
      async (path: string, _options?: { method?: string; body?: unknown }) => {
        if (path.startsWith("/incidents?")) return { items: [incident] };
        if (path === "/ai/investigation-policy")
          return { policy: { profile: "balanced" }, usage: {} };
        if (path === `/incidents/${incident.id}`) return detail();
        return {};
      },
    );
    const value = {
      scope: { permissions },
      can: (permission: AppPermission) => permissions.includes(permission),
      request,
      notify: vi.fn(),
    } as unknown as AppContext;
    const wrapper = mount(EngineerPanel, {
      props: { devices: [device] },
      global: { provide: { [appContextKey as symbol]: value } },
    });
    await flushPromises();
    await wrapper.get("button.ws-link").trigger("click");
    await flushPromises();
    const note = wrapper.get(".ws-dialog textarea");
    expect(Number(note.attributes("maxlength"))).toBeLessThanOrEqual(1000);
    // 400 Chinese characters fit any character counter but are 1200 bytes,
    // which PATCH /incidents/{id} rejects as "summary is too long".
    await note.setValue("确".repeat(400));
    await wrapper
      .findAll("button")
      .find((b) => b.text() === "重新打开")!
      .trigger("click");
    await flushPromises();
    expect(
      request.mock.calls.some(([, options]) => options?.method === "PATCH"),
    ).toBe(false);
    expect(wrapper.text()).toContain("1000 字节");
    wrapper.unmount();
  });
});
