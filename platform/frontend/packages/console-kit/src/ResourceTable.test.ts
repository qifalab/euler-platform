/**
 * @vitest-environment jsdom
 *
 * ResourceTable: in-app links go through the router (no iframe reload) while
 * keeping a real href, hash links stay plain anchors, and a list shows exactly
 * one empty state.
 */
import { describe, it, expect, vi, beforeAll } from "vitest";
import { h } from "vue";
import { mount, flushPromises } from "@vue/test-utils";
import ElementPlus from "element-plus";
import { createRouter, createMemoryHistory } from "vue-router";
import ResourceTable from "./ResourceTable.vue";

beforeAll(() => {
  // el-table observes its size; jsdom has no ResizeObserver.
  if (!("ResizeObserver" in globalThis)) {
    vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  }
});

const rows = [{ id: "euredis-1" }];

function makeRouter() {
  const router = createRouter({
    history: createMemoryHistory("/euredis/"),
    routes: [
      { path: "/instances", component: { render: () => null } },
      { path: "/instances/:id", component: { render: () => null } },
    ],
  });
  return router;
}

describe("ResourceTable links", () => {
  it("routes an app path through vue-router and keeps a real href", async () => {
    const router = makeRouter();
    await router.push("/instances");
    const push = vi.spyOn(router, "push");
    const w = mount(ResourceTable, {
      props: { rows, columns: [{ key: "id", title: "ID", link: (r: Record<string, unknown>) => `/instances/${String(r.id)}` }] },
      global: { plugins: [ElementPlus, router] },
    });
    await flushPromises();
    const a = w.find("a.eu-rt-link");
    expect(a.exists()).toBe(true);
    expect(a.attributes("href")).toBe("/euredis/instances/euredis-1");
    const ev = new MouseEvent("click", { bubbles: true, cancelable: true, button: 0 });
    a.element.dispatchEvent(ev);
    expect(ev.defaultPrevented).toBe(true);
    expect(push).toHaveBeenCalledWith("/instances/euredis-1");
  });

  it("leaves hash links alone in an app without a router", async () => {
    const w = mount(ResourceTable, {
      props: { rows, columns: [{ key: "id", title: "ID", link: (r: Record<string, unknown>) => `#/euvpc/vpcs/${String(r.id)}` }] },
      global: { plugins: [ElementPlus] },
    });
    await flushPromises();
    const a = w.find("a.eu-rt-link");
    expect(a.attributes("href")).toBe("#/euvpc/vpcs/euredis-1");
    const ev = new MouseEvent("click", { bubbles: true, cancelable: true, button: 0 });
    a.element.dispatchEvent(ev);
    expect(ev.defaultPrevented).toBe(false);
  });
});

describe("ResourceTable cells", () => {
  it("renders a VNode returned by a column render (e.g. action buttons)", async () => {
    const onClick = vi.fn();
    const w = mount(ResourceTable, {
      props: {
        rows,
        columns: [
          { key: "id", title: "ID" },
          { key: "op", title: "操作", render: (r: Record<string, unknown>) => h("button", { class: "row-op", onClick: () => onClick(r.id) }, "删除") },
        ],
      },
      global: { plugins: [ElementPlus] },
    });
    await flushPromises();
    const btn = w.find("button.row-op");
    expect(btn.exists()).toBe(true);
    await btn.trigger("click");
    expect(onClick).toHaveBeenCalledWith("euredis-1");
    expect(w.text()).toContain("euredis-1");
  });
});

describe("ResourceTable empty state", () => {
  const columns = [{ key: "id", title: "ID" }];

  it("shows only the page's empty slot when it passes one", async () => {
    const w = mount(ResourceTable, {
      props: { rows: [], columns },
      slots: { empty: () => h("div", { class: "page-guide" }, "暂无存储桶") },
      global: { plugins: [ElementPlus] },
    });
    await flushPromises();
    expect(w.find(".page-guide").exists()).toBe(true);
    expect(w.find(".el-table").exists()).toBe(false);
    expect(w.text()).not.toContain("暂无资源");
  });

  it("falls back to one generic empty state without a slot", async () => {
    const w = mount(ResourceTable, { props: { rows: [], columns }, global: { plugins: [ElementPlus] } });
    await flushPromises();
    expect(w.text().match(/暂无资源/g)).toHaveLength(1);
    expect(w.text()).not.toContain("No Data");
  });

  it("keeps the table (and its spinner) while the first page loads", async () => {
    const w = mount(ResourceTable, {
      props: { rows: [], columns, loading: true },
      slots: { empty: () => h("div", { class: "page-guide" }) },
      global: { plugins: [ElementPlus] },
    });
    await flushPromises();
    expect(w.find(".el-table").exists()).toBe(true);
    expect(w.find(".page-guide").exists()).toBe(false);
  });
});
