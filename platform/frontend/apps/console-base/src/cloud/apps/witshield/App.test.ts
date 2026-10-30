import { mount, flushPromises } from "@vue/test-utils";
import { afterEach, describe, it, expect, vi } from "vitest";
import { appContextKey, type AppContext, type AppPermission } from "../shared";
import App from "./App.vue";
import type { Action } from "./model";

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
function action(status: string, rollbackPayload?: unknown): Action {
  return {
    id: "action-one",
    deviceId: device.id,
    type: "package_security_upgrade",
    parameters: { packages: ["openssl"] },
    preview: { summary: "upgrade" },
    status,
    createdAt: "2026-09-26T00:00:00Z",
    updatedAt: "2026-09-26T00:00:00Z",
    ...(rollbackPayload === undefined ? {} : { rollbackPayload }),
  };
}
function mountApp(
  item: Action,
  permissions: AppPermission[],
  routes: (path: string) => unknown = () => undefined,
) {
  const request = vi.fn(
    async (path: string, options?: { method?: string; body?: unknown }) => {
      const routed = routes(path);
      if (routed !== undefined) return routed;
      if (path === "/devices") return { items: [device] };
      if (path === "/actions?limit=100") return { items: [item] };
      if (path === "/system/health")
        return { status: "ok", database: "ok", workers: [], checkedAt: "" };
      if (path === `/actions/${item.id}` && !options?.method)
        return { action: item, audit: [] };
      if (options?.method === "POST") return {};
      return { items: [] };
    },
  );
  const value = {
    scope: { permissions },
    can: (permission: AppPermission) => permissions.includes(permission),
    request,
    notify: vi.fn(),
  } as unknown as AppContext;
  const wrapper = mount(App, {
    global: { provide: { [appContextKey as symbol]: value } },
  });
  return { wrapper, request };
}
async function openAction(wrapper: ReturnType<typeof mount>) {
  await flushPromises();
  await wrapper
    .findAll("button")
    .find((b) => b.text() === "操作审计")!
    .trigger("click");
  await wrapper
    .findAll("button")
    .find((b) => b.text() === "查看与处理")!
    .trigger("click");
  await flushPromises();
}
const rollbackButton = (wrapper: ReturnType<typeof mount>) =>
  wrapper.findAll("button").find((b) => b.text() === "请求回滚");

afterEach(() => vi.restoreAllMocks());

describe("WitShield action recovery", () => {
  // The engine keeps the recorded recovery state for a failed or
  // indeterminate action precisely so an administrator can request a manual
  // rollback (RequestRollbackAndEnqueue accepts these states).
  for (const status of [
    "failed",
    "indeterminate",
    "succeeded",
    "awaiting_confirmation",
  ])
    it(`offers a rollback request for a ${status} action with recovery state`, async () => {
      vi.spyOn(window, "confirm").mockReturnValue(true);
      const { wrapper, request } = mountApp(
        action(status, { version: 1, sealed: "state" }),
        ["read", "write", "manage"],
      );
      await openAction(wrapper);
      const button = rollbackButton(wrapper);
      expect(button).toBeDefined();
      await button!.trigger("click");
      await flushPromises();
      expect(request).toHaveBeenCalledWith("/actions/action-one/rollback", {
        method: "POST",
        body: {},
      });
      wrapper.unmount();
    });
  it("does not offer rollback without recovery state, for finished rollbacks, or to non-managers", async () => {
    for (const [item, permissions] of [
      [action("indeterminate"), ["read", "write", "manage"]],
      [action("rolled_back", { version: 1 }), ["read", "write", "manage"]],
      [action("cancelled", { version: 1 }), ["read", "write", "manage"]],
      [action("failed", { version: 1 }), ["read", "write"]],
    ] as [Action, AppPermission[]][]) {
      const { wrapper } = mountApp(item, permissions);
      await openAction(wrapper);
      expect(rollbackButton(wrapper)).toBeUndefined();
      wrapper.unmount();
    }
  });
});

async function auditTab(wrapper: ReturnType<typeof mount>) {
  await flushPromises();
  await wrapper
    .findAll("button")
    .find((b) => b.text() === "操作审计")!
    .trigger("click");
  await flushPromises();
}

describe("WitShield audit tab", () => {
  it("attributes action audit entries to the action's device", async () => {
    // GET /audit returns action_audit rows: {id, actionId, actor, event,
    // details, createdAt}. They carry no deviceId or summary of their own.
    const { wrapper } = mountApp(action("succeeded"), ["read"], (path) =>
      path === "/audit?limit=100"
        ? {
            items: [
              {
                id: 7,
                actionId: "action-one",
                actor: "human",
                event: "approved_and_queued",
                details: {},
                createdAt: "2026-09-26T00:00:00Z",
              },
            ],
          }
        : undefined,
    );
    await auditTab(wrapper);
    const row = wrapper
      .findAll(".ws-row")
      .find((r) => r.text().includes("approved_and_queued"))!;
    expect(row.text()).toContain("Linux host");
    expect(row.text()).toContain("安全软件包升级");
    expect(row.text()).not.toContain("项目");
    wrapper.unmount();
  });
  it("does not page raw observations with a cursor from another device filter", async () => {
    const { wrapper, request } = mountApp(action("succeeded"), ["read"], (path) =>
      path.startsWith("/security-events")
        ? {
            items: [
              {
                id: "event-one",
                deviceId: device.id,
                type: "ssh_auth_failure_untrusted",
                occurredAt: "2026-09-26T00:00:00Z",
                payload: {},
              },
            ],
            nextCursor: "cursor-all-devices",
          }
        : undefined,
    );
    await auditTab(wrapper);
    const button = (text: string) =>
      wrapper.findAll("button").find((b) => b.text() === text);
    await button("读取观察")!.trigger("click");
    await flushPromises();
    expect(button("加载更多")).toBeDefined();
    await wrapper.get("select[aria-label=观察设备]").setValue(device.id);
    await flushPromises();
    expect(button("加载更多")).toBeUndefined();
    await button("读取观察")!.trigger("click");
    await flushPromises();
    const reads = request.mock.calls
      .map(([path]) => path)
      .filter((path) => path.startsWith("/security-events"));
    expect(reads.at(-1)).toBe("/security-events?limit=50&deviceId=device-one");
    expect(reads.some((path) => path.includes("cursor="))).toBe(false);
    wrapper.unmount();
  });
});
