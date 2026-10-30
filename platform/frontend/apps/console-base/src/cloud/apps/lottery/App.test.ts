import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import { afterEach, describe, expect, it, vi } from "vitest";
import App from "./App.vue";
import { appContextKey, type AppContext, type AppPermission } from "../shared";

vi.mock("vue-router", () => ({ useRoute: () => ({ query: {} }) }));

type Person = { id: string; name: string; department: string; createdAt: string; participated: boolean };

function setup(permissions: AppPermission[] = ["read", "write", "manage"]) {
  const people: Record<string, Person[]> = {
    "room-a": Array.from({ length: 21 }, (_, i) => ({
      id: `a${i + 1}`,
      name: `用户${i + 1}`,
      department: "",
      createdAt: "2026-10-01T00:00:00Z",
      participated: false,
    })),
    "room-b": [{ id: "b1", name: "访客", department: "", createdAt: "2026-10-01T00:00:00Z", participated: false }],
  };
  const room = (id: string) => ({
    id,
    name: id === "room-a" ? "年会甲" : "年会乙",
    description: "",
    status: "open",
    preventDuplicates: true,
    generation: 1,
    nextRound: 1,
    createdAt: "2026-10-01T00:00:00Z",
    totalUsers: people[id].length,
    currentWinners: 0,
    totalRounds: 0,
  });
  const request = vi.fn(async (path: string, options?: { method?: string; body?: unknown }) => {
    const url = new URL(path, "https://euler.example");
    const parts = url.pathname.split("/").filter(Boolean);
    if (url.pathname === "/rooms") return { rooms: [room("room-a"), room("room-b")] };
    const id = parts[1];
    if (parts.length === 2) return room(id);
    if (parts[2] === "participants" && options?.method === "DELETE") {
      people[id] = people[id].filter((p) => p.id !== parts[3]);
      return { success: true };
    }
    if (parts[2] === "participants") {
      const page = Number(url.searchParams.get("page")),
        size = Number(url.searchParams.get("pageSize"));
      return {
        participants: people[id].slice((page - 1) * size, page * size),
        total: people[id].length,
        available: people[id].length,
        page,
        pageSize: size,
      };
    }
    if (parts[2] === "invitation") return { signupURL: "https://euler.example/public/lottery/join/x", qrURL: "" };
    if (parts[2] === "draws" && !options?.method) return { draws: [] };
    throw new Error(`unexpected ${options?.method ?? "GET"} ${path}`);
  });
  const context = {
    scope: { actorId: "u", tenantId: "t", projectId: "p", installationId: "i", applicationId: "lottery", permissions },
    basePath: "/api/v1/tenants/t/projects/p/apps/lottery",
    publicBase: "https://euler.example/public/lottery",
    can: (permission: AppPermission) => permissions.includes(permission),
    request,
    notify: vi.fn(),
  } as unknown as AppContext;
  const wrapper = mount(App, { global: { provide: { [appContextKey as symbol]: context } } });
  return { wrapper, request };
}
function buttons(wrapper: VueWrapper, text: string) {
  return wrapper.findAll("button").filter((item) => item.text() === text);
}
function button(wrapper: VueWrapper, text: string) {
  const found = buttons(wrapper, text)[0];
  if (!found) throw new Error(`Missing button ${text}`);
  return found;
}
async function enterRoom(wrapper: VueWrapper, index: number) {
  await buttons(wrapper, "进入活动 →")[index].trigger("click");
  await flushPromises();
}

afterEach(() => vi.restoreAllMocks());

describe("native Lottery console", () => {
  it("does not carry an open draw confirmation into another room", async () => {
    const { wrapper, request } = setup();
    await flushPromises();
    await enterRoom(wrapper, 0);
    await wrapper.find(".draw-panel form").trigger("submit");
    expect(wrapper.text()).toContain("准备开始这一轮？");
    await button(wrapper, "← 全部活动").trigger("click");
    await flushPromises();
    await enterRoom(wrapper, 1);
    expect(wrapper.text()).toContain("年会乙");
    expect(wrapper.text()).not.toContain("准备开始这一轮？");
    expect(request.mock.calls.some(([, options]) => options?.method === "POST")).toBe(false);
    wrapper.unmount();
  });

  it("does not leave the new-activity form open, filled with a room it visited", async () => {
    const { wrapper } = setup();
    await flushPromises();
    await button(wrapper, "＋ 新建活动").trigger("click");
    expect(wrapper.text()).toContain("创建抽奖活动");
    await enterRoom(wrapper, 0);
    await button(wrapper, "← 全部活动").trigger("click");
    await flushPromises();
    const name = wrapper.find('input[placeholder="例如：年度聚会 · 幸运抽奖"]');
    expect(name.exists() ? (name.element as HTMLInputElement).value : "").toBe("");
    wrapper.unmount();
  });

  it("returns to the last page after removing the only participant on a page", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const { wrapper } = setup();
    await flushPromises();
    await enterRoom(wrapper, 0);
    await button(wrapper, "参与者").trigger("click");
    await button(wrapper, "下一页").trigger("click");
    await flushPromises();
    expect(wrapper.text()).toContain("用户21");
    await button(wrapper, "移除").trigger("click");
    await flushPromises();
    expect(wrapper.text()).not.toContain("暂无参与者");
    expect(wrapper.findAll("tbody tr")).toHaveLength(20);
    expect(wrapper.find(".pagination span").text()).toBe("1 / 1");
    wrapper.unmount();
  });
});
