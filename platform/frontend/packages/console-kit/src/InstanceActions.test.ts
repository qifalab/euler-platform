/**
 * @vitest-environment jsdom
 *
 * InstanceActions: buttons follow the orchestrator's state rules, every action
 * is confirmed, and the bar posts to the orchestrator action/release endpoints.
 */
import { describe, it, expect, vi, beforeEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";

const ui = vi.hoisted(() => ({
  confirm: vi.fn(),
  success: vi.fn(),
  error: vi.fn(),
}));
vi.mock("element-plus", async (importOriginal) => {
  const mod = await importOriginal<typeof import("element-plus")>();
  return {
    ...mod,
    ElMessageBox: { confirm: ui.confirm },
    ElMessage: { success: ui.success, error: ui.error, info: vi.fn(), warning: vi.fn() },
  };
});

import ElementPlus from "element-plus";
import InstanceActions from "./InstanceActions.vue";
import type { ActionTarget, InstanceActionButton } from "./resourceActions";

const ALL: InstanceActionButton[] = ["stop", "start", "restart", "upgrade", "renew", "release"];

function mountBar(instance: ActionTarget | null, post = vi.fn().mockResolvedValue({ data: {} })) {
  const sdk = { get: vi.fn().mockResolvedValue({ data: [] }), post };
  const w = mount(InstanceActions, {
    props: { sdk, resourceId: "euredis-cn-north-1-1", instance, actions: ALL },
    global: { plugins: [ElementPlus] },
  });
  return { w, sdk };
}

const running: ActionTarget = { productCode: "euredis", state: "RUNNING", chargeType: "POSTPAID", specCode: "euredis.redis.small.postpaid" };

function button(w: ReturnType<typeof mount>, text: string) {
  return w.findAll("button").find((b) => b.text() === text);
}

describe("InstanceActions", () => {
  beforeEach(() => {
    ui.confirm.mockReset().mockResolvedValue("confirm");
    ui.success.mockReset();
    ui.error.mockReset();
  });

  it("offers stop/restart on RUNNING and start on STOPPED", () => {
    const { w } = mountBar(running);
    expect(button(w, "停止")).toBeTruthy();
    expect(button(w, "重启")).toBeTruthy();
    expect(button(w, "启动")).toBeUndefined();
    expect(button(w, "变配")!.attributes("disabled")).toBeUndefined();

    const stopped = mountBar({ ...running, state: "STOPPED" }).w;
    expect(button(stopped, "启动")).toBeTruthy();
    expect(button(stopped, "停止")).toBeUndefined();
    expect(button(stopped, "重启")).toBeUndefined();
  });

  it("disables upgrade for prepaid and everything mid-transition", () => {
    const prepaid = mountBar({ ...running, chargeType: "PREPAID" }).w;
    expect(button(prepaid, "变配")!.attributes("disabled")).toBeDefined();

    const creating = mountBar({ ...running, state: "CREATING" }).w;
    expect(button(creating, "释放")!.attributes("disabled")).toBeDefined();
    expect(button(creating, "续费")!.attributes("disabled")).toBeDefined();
    expect(button(creating, "停止")).toBeUndefined();
  });

  it("confirms, posts the action and asks the page to refetch", async () => {
    const { w, sdk } = mountBar(running);
    await button(w, "重启")!.trigger("click");
    await flushPromises();
    expect(ui.confirm).toHaveBeenCalledTimes(1);
    expect(sdk.post).toHaveBeenCalledWith("/api/v1/orchestrator/resources/euredis-cn-north-1-1/actions", { action: "restart" });
    expect(w.emitted("changed")).toHaveLength(1);
  });

  it("does nothing when the confirmation is cancelled", async () => {
    ui.confirm.mockRejectedValue("cancel");
    const { w, sdk } = mountBar(running);
    await button(w, "释放")!.trigger("click");
    await flushPromises();
    expect(sdk.post).not.toHaveBeenCalled();
    expect(w.emitted("released")).toBeUndefined();
  });

  it("offers release (with the danger confirmation) for CREATE_FAILED / LOCKED / EXPIRED", async () => {
    for (const state of ["CREATE_FAILED", "LOCKED", "EXPIRED"]) {
      ui.confirm.mockClear();
      const { w, sdk } = mountBar({ ...running, state });
      const rel = button(w, "释放")!;
      expect(rel.attributes("disabled")).toBeUndefined();
      expect(button(w, "续费")!.attributes("disabled")).toBeDefined();
      await rel.trigger("click");
      await flushPromises();
      expect(ui.confirm).toHaveBeenCalledWith(expect.stringContaining("不可恢复"), "危险操作", expect.objectContaining({ type: "warning" }));
      expect(sdk.post).toHaveBeenCalledWith("/api/v1/orchestrator/resources/euredis-cn-north-1-1/release");
      expect(w.emitted("released")).toHaveLength(1);
    }
  });

  it("releases through the release endpoint and surfaces a 409", async () => {
    const { w, sdk } = mountBar(running);
    await button(w, "释放")!.trigger("click");
    await flushPromises();
    expect(sdk.post).toHaveBeenCalledWith("/api/v1/orchestrator/resources/euredis-cn-north-1-1/release");
    expect(w.emitted("released")).toHaveLength(1);

    const conflict = vi.fn().mockRejectedValue(Object.assign(new Error("资源状态 STOPPED 不允许 stop"), { status: 409 }));
    const second = mountBar(running, conflict).w;
    await button(second, "停止")!.trigger("click");
    await flushPromises();
    expect(ui.error).toHaveBeenCalledWith("资源状态 STOPPED 不允许 stop");
    expect(second.emitted("changed")).toBeUndefined();
  });
});
