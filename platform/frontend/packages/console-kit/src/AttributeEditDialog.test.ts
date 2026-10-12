/**
 * @vitest-environment jsdom
 *
 * AttributeEditDialog: opens on the current attributes, validates before the
 * PATCH, sends every field as a string, and keeps the dialog open on 400/409.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";

const ui = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn() }));
vi.mock("element-plus", async (importOriginal) => {
  const mod = await importOriginal<typeof import("element-plus")>();
  return { ...mod, ElMessage: { success: ui.success, error: ui.error, info: vi.fn(), warning: vi.fn() } };
});

import ElementPlus from "element-plus";
import AttributeEditDialog, { type AttributeField } from "./AttributeEditDialog.vue";

const fields: AttributeField[] = [
  { key: "minSize", label: "最小副本数", min: 0, max: 1000 },
  { key: "maxSize", label: "最大副本数", min: 0, max: 1000 },
];

function mountDialog(patch: ReturnType<typeof vi.fn>, attributes: Record<string, string> = { minSize: "1", maxSize: "5" }) {
  return mount(AttributeEditDialog, {
    props: {
      modelValue: true,
      sdk: { patch: patch as never },
      resourceId: "euas-1",
      title: "修改伸缩策略",
      fields,
      attributes,
      validate: (v: Record<string, number | string | undefined>) =>
        Number(v.maxSize) < Number(v.minSize) ? "最大副本数不能小于最小副本数" : null,
    },
    global: { plugins: [ElementPlus] },
    attachTo: document.body,
  });
}

function bodyButton(text: string): HTMLButtonElement | undefined {
  return [...document.body.querySelectorAll("button")].find((b) => b.textContent?.trim() === text) as HTMLButtonElement | undefined;
}

describe("AttributeEditDialog", () => {
  beforeEach(() => {
    ui.success.mockReset();
    ui.error.mockReset();
  });
  afterEach(() => {
    document.body.innerHTML = "";
  });

  it("PATCHes the current values as strings and emits saved + close", async () => {
    const patch = vi.fn().mockResolvedValue({ data: { resourceId: "euas-1", attributes: { minSize: "1", maxSize: "5" } } });
    const w = mountDialog(patch);
    await flushPromises();
    bodyButton("保存")!.click();
    await flushPromises();
    expect(patch).toHaveBeenCalledWith("/api/v1/orchestrator/resources/euas-1/attributes", { attributes: { minSize: "1", maxSize: "5" } });
    expect(w.emitted("saved")?.[0]?.[0]).toMatchObject({ resourceId: "euas-1" });
    expect(w.emitted("update:modelValue")?.[0]).toEqual([false]);
    w.unmount();
  });

  it("runs the cross-field rule before any request", async () => {
    const patch = vi.fn();
    const w = mountDialog(patch, { minSize: "6", maxSize: "5" });
    await flushPromises();
    bodyButton("保存")!.click();
    await flushPromises();
    expect(patch).not.toHaveBeenCalled();
    expect(document.body.textContent).toContain("最大副本数不能小于最小副本数");
    w.unmount();
  });

  it("shows a 409 in place and stays open", async () => {
    const patch = vi.fn().mockRejectedValue(Object.assign(new Error("resource not live"), { status: 409, code: "Resource.StateConflict" }));
    const w = mountDialog(patch);
    await flushPromises();
    bodyButton("保存")!.click();
    await flushPromises();
    expect(document.body.textContent).toContain("状态不允许修改");
    expect(w.emitted("saved")).toBeUndefined();
    expect(w.emitted("update:modelValue")).toBeUndefined();
    w.unmount();
  });
});
