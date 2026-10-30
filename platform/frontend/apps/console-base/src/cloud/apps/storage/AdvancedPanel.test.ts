import { mount, flushPromises } from "@vue/test-utils";
import { describe, it, expect, vi } from "vitest";
import AdvancedPanel from "./AdvancedPanel.vue";
import { appContextKey, type AppContext, type AppPermission } from "../shared";

const version = (key: string, versionId: string) => ({
  key,
  versionId,
  size: 1024,
  modifiedAt: "2026-10-30T00:00:00Z",
  isLatest: false,
  deleteMarker: false,
});

describe("storage version history", () => {
  it("loads the next page for the prefix that produced the cursor, not an unsent edit", async () => {
    const permissions: AppPermission[] = ["read"];
    const request = vi.fn(async (path: string) => {
      if (path === "/buckets/b1/versioning") return { status: "Enabled" };
      const url = new URL(path, "https://euler.example.test");
      if (url.pathname !== "/buckets/b1/versions")
        throw new Error(`unexpected ${path}`);
      const prefix = url.searchParams.get("prefix"),
        cursor = url.searchParams.get("cursor");
      if (prefix === "logs/" && cursor === "")
        return { items: [version("logs/a.txt", "v-a")], next: "cursor-1" };
      if (prefix === "logs/" && cursor === "cursor-1")
        return { items: [version("logs/b.txt", "v-b")], next: "" };
      // A cursor only resumes the listing it came from (see STORAGE-ADVANCED).
      return { items: [], next: "" };
    });
    const context = {
      scope: { permissions },
      can: (permission: AppPermission) => permissions.includes(permission),
      request,
      notify: vi.fn(),
    } as unknown as AppContext;
    const wrapper = mount(AdvancedPanel, {
      props: { bucketId: "b1", prefix: "" },
      global: { provide: { [appContextKey as symbol]: context } },
    });
    await wrapper.find("button.toggle").trigger("click");
    await flushPromises();
    const filter = wrapper.find('input[aria-label="版本路径前缀"]');
    await filter.setValue("logs/");
    await wrapper.find("form").trigger("submit");
    await flushPromises();
    expect(wrapper.text()).toContain("logs/a.txt");
    // The user starts typing another prefix but has not queried it yet.
    await filter.setValue("other/");
    const more = wrapper
      .findAll("button")
      .find((item) => item.text() === "加载更多");
    expect(more).toBeDefined();
    await more!.trigger("click");
    await flushPromises();
    expect(request).toHaveBeenLastCalledWith(
      "/buckets/b1/versions?prefix=logs%2F&cursor=cursor-1",
    );
    expect(wrapper.text()).toContain("logs/a.txt");
    expect(wrapper.text()).toContain("logs/b.txt");
    wrapper.unmount();
  });
});
