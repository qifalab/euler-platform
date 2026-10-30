import { mount, flushPromises } from "@vue/test-utils";
import { afterEach, describe, it, expect, vi } from "vitest";
import App from "./App.vue";
import { appContextKey, type AppContext, type AppPermission } from "../shared";

type Obj = {
  key: string;
  size: number;
  contentType: string;
  etag: string;
  modifiedAt: string;
  folder: boolean;
};
const file = (key: string): Obj => ({
  key,
  size: 1,
  contentType: "text/plain",
  etag: `etag-${key}`,
  modifiedAt: "2026-10-30T00:00:00Z",
  folder: false,
});

function setup(
  objects: Obj[],
  handle: (
    path: string,
    options?: { method?: string; body?: unknown },
  ) => unknown | undefined,
  stats: { usedBytes: number; objectCount: number } = {
    usedBytes: 2,
    objectCount: 2,
  },
) {
  const permissions: AppPermission[] = ["read", "write", "manage"];
  const bucket = {
    id: "b1",
    name: "eu-b1",
    displayName: "Files",
    description: "",
    access: "private",
    status: "active",
    ...stats,
    origins: [],
  };
  const request = vi.fn(
    async (path: string, options?: { method?: string; body?: unknown }) => {
      const custom = await handle(path, options);
      if (custom !== undefined) return custom;
      if (path === "/")
        return {
          items: [bucket],
          quota: { storageBytes: 1024, buckets: 2 },
          usedBytes: 2,
          objectCount: 2,
          bucketCount: 1,
          reservedBytes: 0,
          configured: true,
          advanced: false,
          corsMode: "external",
        };
      if (path === "/keys") return { items: [], endpoint: "" };
      if (path.startsWith("/stats/logs")) return { items: [] };
      if (path.startsWith("/buckets/b1/objects?"))
        return { items: [...objects], hasMore: false, next: "" };
      throw new Error(`unexpected request ${path}`);
    },
  );
  const context = {
    scope: { permissions },
    can: (permission: AppPermission) => permissions.includes(permission),
    request,
    notify: vi.fn(),
  } as unknown as AppContext;
  const wrapper = mount(App, {
    global: { provide: { [appContextKey as symbol]: context } },
  });
  return { wrapper, request };
}
function button(wrapper: ReturnType<typeof mount>, text: string) {
  const found = wrapper.findAll("button").find((item) => item.text() === text);
  if (!found) throw new Error(`Missing button ${text}`);
  return found;
}
const listings = (request: { mock: { calls: unknown[][] } }) =>
  request.mock.calls.filter(([path]) =>
    String(path).startsWith("/buckets/b1/objects?"),
  ).length;

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("storage console", () => {
  it("refreshes the file list after a partially failed batch delete", async () => {
    const objects = [file("a.txt"), file("b.txt")];
    const { wrapper, request } = setup(objects, (path) => {
      if (path === "/buckets/b1/objects/delete-batch") {
        objects.splice(0, 1);
        return { deleted: ["a.txt"], failed: ["b.txt"] };
      }
      return undefined;
    });
    vi.stubGlobal("confirm", () => true);
    await flushPromises();
    await button(wrapper, "浏览文件").trigger("click");
    await flushPromises();
    expect(wrapper.text()).toContain("a.txt");
    for (const box of wrapper.findAll('input[type="checkbox"]'))
      await box.setValue(true);
    const before = listings(request);
    await button(wrapper, "删除所选").trigger("click");
    await flushPromises();
    const call = request.mock.calls.find(
      ([path]) => path === "/buckets/b1/objects/delete-batch",
    );
    expect(call?.[1]).toMatchObject({
      method: "POST",
      body: { keys: ["a.txt", "b.txt"] },
    });
    // Up to 1,000 keys are deleted one by one; the default 20 s would abort it.
    expect(
      (call?.[1] as { timeoutMs?: number } | undefined)?.timeoutMs,
    ).toBeGreaterThanOrEqual(120000);
    expect(listings(request)).toBeGreaterThan(before);
    const rows = wrapper.findAll("tbody tr").map((row) => row.text());
    expect(rows.some((row) => row.includes("a.txt"))).toBe(false);
    expect(rows.some((row) => row.includes("b.txt"))).toBe(true);
    expect(wrapper.find('[role="alert"]').text()).toContain(
      "1 个未完成",
    );
    wrapper.unmount();
  });

  it("offers force deletion when stats show an empty bucket the server still refuses", async () => {
    const deletes: string[] = [];
    const { wrapper } = setup(
      [],
      (path, options) => {
        if (options?.method !== "DELETE") return undefined;
        deletes.push(path);
        if (path === "/buckets/b1?force=false")
          throw Object.assign(
            new Error("存储桶不为空；清空后删除或明确选择强制删除"),
            { status: 409 },
          );
        return { deleted: true };
      },
      { usedBytes: 0, objectCount: 0 },
    );
    const confirm = vi.fn(() => true);
    vi.stubGlobal("confirm", confirm);
    await flushPromises();
    await button(wrapper, "删除").trigger("click");
    await flushPromises();
    expect(deletes).toEqual(["/buckets/b1?force=false", "/buckets/b1?force=true"]);
    expect(confirm).toHaveBeenCalledTimes(2);
    expect(String(confirm.mock.calls[1])).toContain("永久删除");
    wrapper.unmount();
  });

  it("shows files that were published before a later file in the same upload failed", async () => {
    const objects: Obj[] = [];
    class FakeXHR {
      status = 0;
      timeout = 0;
      upload: Record<string, unknown> = {};
      onload?: () => void;
      onerror?: () => void;
      ontimeout?: () => void;
      onabort?: () => void;
      open() {}
      abort() {}
      send() {
        this.status = 204;
        queueMicrotask(() => this.onload?.());
      }
    }
    vi.stubGlobal("XMLHttpRequest", FakeXHR);
    let presigns = 0;
    const { wrapper } = setup(objects, (path) => {
      if (path.startsWith("/buckets/b1/objects/info"))
        throw Object.assign(new Error("not found"), { status: 404 });
      if (path === "/buckets/b1/objects/presigned-upload") {
        if (++presigns > 1)
          throw Object.assign(new Error("项目存储配额不足"), { status: 409 });
        return {
          uploadId: "up-1",
          signature: { url: "https://s3.example.test/eu-b1", method: "POST" },
        };
      }
      if (path === "/buckets/b1/objects/confirm-upload") {
        objects.push(file("first.txt"));
        return file("first.txt");
      }
      return undefined;
    });
    await flushPromises();
    await button(wrapper, "浏览文件").trigger("click");
    await flushPromises();
    const input = wrapper.find('input[type="file"]');
    Object.defineProperty(input.element, "files", {
      configurable: true,
      value: [
        new File(["1"], "first.txt", { type: "text/plain" }),
        new File(["2"], "second.txt", { type: "text/plain" }),
      ],
    });
    await input.trigger("change");
    await flushPromises();
    expect(wrapper.find('[role="alert"]').text()).toContain("配额不足");
    expect(
      wrapper
        .findAll("tbody tr")
        .some((row) => row.text().includes("first.txt")),
    ).toBe(true);
    wrapper.unmount();
  });
});
