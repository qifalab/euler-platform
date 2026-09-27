import { mount, flushPromises } from "@vue/test-utils";
import { describe, it, expect, vi } from "vitest";
import AnalyticsPanel from "./AnalyticsPanel.vue";
import { appContextKey, type AppContext, type AppPermission } from "../shared";

function setup(permissions: AppPermission[] = ["read", "manage"]) {
  const settings = { retentionDays: 365, eventNames: ["signup", "completed"] };
  const request = vi.fn(
    async (path: string, options?: { method?: string; body?: unknown }) => {
      if (path.endsWith("/settings"))
        return options?.method === "PUT" ? options.body : settings;
      if (path.includes("/analytics?"))
        return {
          period: {
            from: "2026-09-01",
            to: "2026-09-02",
            timezone: "Asia/Shanghai",
            startInclusive: "2026-08-31T16:00:00Z",
            endExclusive: "2026-09-02T16:00:00Z",
          },
          trend: [
            { date: "2026-09-01", pageViews: 4, uniqueVisitors: 2, events: 2 },
          ],
          sources: [{ name: "search.example", count: 4, uniqueVisitors: 2 }],
          devices: [],
          browsers: [],
          pages: [
            { name: "https://docs.example/a", count: 4, uniqueVisitors: 2 },
          ],
          events: [{ name: "signup", count: 2, uniqueVisitors: 2 }],
          pageViews: 4,
          uniqueVisitors: 2,
          eventCount: 2,
        };
      if (path.includes("/funnel?"))
        return {
          steps: [
            { name: "signup", visitors: 2, conversion: 1 },
            { name: "completed", visitors: 1, conversion: 0.5 },
          ],
          definition: "同访客按顺序转化",
        };
      if (path.endsWith("/cleanup")) return { deleted: 5 };
      throw Error(`Unexpected request ${path}`);
    },
  );
  const download = vi.fn<(path: string, filename: string) => Promise<void>>(
    async () => {},
  );
  const context = {
    can: (p: AppPermission) => permissions.includes(p),
    request,
    download,
    notify: vi.fn(),
  } as unknown as AppContext;
  return {
    wrapper: mount(AnalyticsPanel, {
      props: { siteId: "site-a" },
      global: { provide: { [appContextKey as symbol]: context } },
    }),
    request,
    download,
  };
}
function button(wrapper: ReturnType<typeof mount>, text: string) {
  const value = wrapper.findAll("button").find((b) => b.text() === text);
  if (!value) throw Error(`Missing ${text}`);
  return value;
}

describe("statistics analytics controls", () => {
  it("shows chart data, clears stale figures after a date change, and exports the selected range", async () => {
    const { wrapper, request, download } = setup();
    await flushPromises();
    expect(wrapper.find('svg[role="img"]').exists()).toBe(true);
    expect(wrapper.text()).toContain("search.example");
    await wrapper.find('input[type="date"]').setValue("2026-09-01");
    expect(wrapper.find('svg[role="img"]').exists()).toBe(false);
    expect(button(wrapper, "导出 CSV").attributes("disabled")).toBeDefined();
    await wrapper.find("form.filters").trigger("submit");
    await flushPromises();
    expect(request.mock.calls.at(-1)?.[0]).toContain("from=2026-09-01");
    await button(wrapper, "导出 CSV").trigger("click");
    await flushPromises();
    expect(download.mock.calls[0]?.[0]).toContain(
      "/sites/site-a/export.csv?from=2026-09-01",
    );
    expect(wrapper.text()).toContain("CSV 下载已开始");
    wrapper.unmount();
  });
  it("calculates a selected ordered funnel and hides all retention mutation controls from viewers", async () => {
    const { wrapper, request } = setup(["read"]);
    await flushPromises();
    expect(wrapper.text()).not.toContain("保存并应用留存");
    expect(wrapper.text()).not.toContain("清理过期数据");
    await wrapper.findAll("form")[1]!.trigger("submit");
    await flushPromises();
    expect(
      request.mock.calls.find(([path]) => path.includes("/funnel?"))?.[1]?.body,
    ).toMatchObject({ steps: ["signup", "completed"], windowMinutes: 60 });
    expect(wrapper.text()).toContain("50.0%");
    await wrapper.find('input[type="number"]').setValue(120);
    expect(wrapper.text()).not.toContain("50.0%");
    wrapper.unmount();
  });
  it("requires confirmation before shortening retention and sends only registered event names", async () => {
    const { wrapper, request } = setup();
    await flushPromises();
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    const retention = wrapper.findAll('input[type="number"]').at(-1)!;
    await retention.setValue(30);
    const form = wrapper.findAll("form").at(-1)!;
    await form.trigger("submit");
    await flushPromises();
    expect(
      request.mock.calls.some(([, options]) => options?.method === "PUT"),
    ).toBe(false);
    confirm.mockReturnValue(true);
    await wrapper.find("textarea").setValue("signup\ncompleted\ncheckout");
    await form.trigger("submit");
    await flushPromises();
    expect(
      request.mock.calls.find(([, options]) => options?.method === "PUT")?.[1]
        ?.body,
    ).toEqual({
      retentionDays: 30,
      eventNames: ["signup", "completed", "checkout"],
    });
    confirm.mockRestore();
    wrapper.unmount();
  });
});
