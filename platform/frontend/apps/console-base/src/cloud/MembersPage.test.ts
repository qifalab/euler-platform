/**
 * @vitest-environment jsdom
 *
 * Members page: a change to your OWN membership (leaving the team, demoting
 * yourself, editing your own project role) changes what this session may see
 * and do, so the team/project context must be reloaded, not only the lists.
 */
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import MembersPage from "./MembersPage.vue";
import { useCloud } from "./context";

beforeAll(() => {
  // jsdom has no modal dialog support; CloudModal only needs open/close.
  Object.defineProperty(HTMLDialogElement.prototype, "showModal", {
    configurable: true,
    value(this: HTMLDialogElement) {
      this.setAttribute("open", "");
    },
  });
  Object.defineProperty(HTMLDialogElement.prototype, "close", {
    configurable: true,
    value(this: HTMLDialogElement) {
      this.removeAttribute("open");
    },
  });
});

type Reply = { status: number; body?: unknown } | undefined;
function serve(route: (method: string, path: string) => Reply) {
  const fetchMock = vi.fn(async (input: unknown, init?: RequestInit) => {
    const out = route(init?.method ?? "GET", String(input)) ?? {
      status: 404,
      body: { error: { code: "not_found", message: "Resource not found" } },
    };
    return out.status === 204
      ? new Response(null, { status: 204 })
      : new Response(JSON.stringify(out.body ?? {}), {
          status: out.status,
          headers: { "Content-Type": "application/json" },
        });
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}
const ok = (items: unknown[]): Reply => ({ status: 200, body: { items } });

const { state, tenant, canManageTeam, canManageProject } = useCloud();
const me = { userId: "u_me", displayName: "Me", role: "admin", joinedAt: "" };
const owner = {
  userId: "u_owner",
  displayName: "Owner",
  role: "owner",
  joinedAt: "",
};
const project = {
  id: "prj_1",
  tenantId: "ten_1",
  name: "Project",
  role: "admin",
  status: "active",
  createdAt: "",
};
function signInAsTeamAdmin() {
  Object.assign(state, {
    session: {
      authenticated: true,
      loginAvailable: true,
      user: { id: "u_me", displayName: "Me", provider: "test", subject: "me" },
    },
    tenants: [{ id: "ten_1", name: "Team", role: "admin", createdAt: "" }],
    tenantId: "ten_1",
    projects: [project],
    projectId: "prj_1",
    installations: [],
    loading: false,
    contextLoading: false,
    error: "",
    contextError: "",
  });
}
function listCalls(fetchMock: ReturnType<typeof serve>, path: string) {
  return fetchMock.mock.calls.filter(
    ([input, init]) =>
      String(input) === path && (init?.method ?? "GET") === "GET",
  ).length;
}
function button(wrapper: VueWrapper, text: string) {
  const found = wrapper.findAll("button").find((b) => b.text().includes(text));
  if (!found) throw new Error(`no button ${text}`);
  return found;
}
function ownRow(wrapper: VueWrapper) {
  const row = wrapper
    .findAll("tbody tr")
    .find((r) => r.text().includes("（你）"));
  if (!row) throw new Error("own row not rendered");
  return row;
}

let wrapper: VueWrapper | undefined;
afterEach(() => {
  wrapper?.unmount();
  wrapper = undefined;
  vi.unstubAllGlobals();
});

describe("MembersPage own-membership changes", () => {
  it("reloads the team context after you remove yourself from the team", async () => {
    signInAsTeamAdmin();
    let removed = false;
    const fetchMock = serve((method, path) => {
      if (method === "DELETE" && path === "/api/v1/tenants/ten_1/members/u_me") {
        removed = true;
        return { status: 204 };
      }
      if (path === "/api/v1/tenants")
        return ok(removed ? [] : [{ id: "ten_1", name: "Team", role: "admin" }]);
      if (removed) return undefined; // no longer a member: 404 everywhere
      if (path === "/api/v1/tenants/ten_1/members") return ok([me, owner]);
      if (path === "/api/v1/tenants/ten_1/projects/prj_1/members") return ok([]);
      if (path === "/api/v1/tenants/ten_1/invitations") return ok([]);
      return undefined;
    });
    wrapper = mount(MembersPage, { attachTo: document.body });
    await flushPromises();
    await ownRow(wrapper).find("button.danger-text").trigger("click");
    await flushPromises();
    await button(wrapper, "确认变更").trigger("click");
    await flushPromises();
    expect(listCalls(fetchMock, "/api/v1/tenants")).toBe(1);
    expect(state.tenants).toEqual([]);
    expect(state.tenantId).toBe("");
  });

  it("drops team-admin controls after you demote yourself", async () => {
    signInAsTeamAdmin();
    let demoted = false;
    const fetchMock = serve((method, path) => {
      if (method === "PATCH" && path === "/api/v1/tenants/ten_1/members/u_me") {
        demoted = true;
        return { status: 204 };
      }
      if (path === "/api/v1/tenants")
        return ok([
          { id: "ten_1", name: "Team", role: demoted ? "member" : "admin" },
        ]);
      // A plain member only sees projects granted to them: none here.
      if (path === "/api/v1/tenants/ten_1/projects")
        return ok(demoted ? [] : [project]);
      if (path === "/api/v1/tenants/ten_1/members")
        return ok([{ ...me, role: demoted ? "member" : "admin" }, owner]);
      if (path === "/api/v1/tenants/ten_1/projects/prj_1/members") return ok([]);
      if (path === "/api/v1/tenants/ten_1/invitations")
        return demoted
          ? { status: 403, body: { error: { code: "forbidden", message: "no" } } }
          : ok([]);
      return undefined;
    });
    wrapper = mount(MembersPage, { attachTo: document.body });
    await flushPromises();
    expect(canManageTeam.value).toBe(true);
    await ownRow(wrapper).find("select").setValue("member");
    await flushPromises();
    await button(wrapper, "确认变更").trigger("click");
    await flushPromises();
    expect(listCalls(fetchMock, "/api/v1/tenants")).toBe(1);
    expect(tenant.value?.role).toBe("member");
    expect(canManageTeam.value).toBe(false);
    expect(state.projectId).toBe("");
  });

  it("keeps the context when you change another member's role", async () => {
    signInAsTeamAdmin();
    const other = { ...me, userId: "u_other", displayName: "Other", role: "member" };
    const fetchMock = serve((method, path) => {
      if (method === "PATCH" && path === "/api/v1/tenants/ten_1/members/u_other")
        return { status: 204 };
      if (path === "/api/v1/tenants/ten_1/members") return ok([me, owner, other]);
      if (path === "/api/v1/tenants/ten_1/projects/prj_1/members") return ok([]);
      if (path === "/api/v1/tenants/ten_1/invitations") return ok([]);
      return undefined;
    });
    wrapper = mount(MembersPage, { attachTo: document.body });
    await flushPromises();
    const row = wrapper
      .findAll("tbody tr")
      .find((r) => r.text().includes("Other"));
    await row!.find("select").setValue("viewer");
    await flushPromises();
    await button(wrapper, "确认变更").trigger("click");
    await flushPromises();
    expect(listCalls(fetchMock, "/api/v1/tenants")).toBe(0);
    expect(listCalls(fetchMock, "/api/v1/tenants/ten_1/members")).toBe(2);
    expect(state.tenantId).toBe("ten_1");
    expect(state.projectId).toBe("prj_1");
  });

  it("reloads the project context after you remove your own project access", async () => {
    signInAsTeamAdmin();
    // A plain team member who administers this one project.
    state.tenants = [{ id: "ten_1", name: "Team", role: "member", createdAt: "" }];
    let removed = false;
    const fetchMock = serve((method, path) => {
      if (
        method === "DELETE" &&
        path === "/api/v1/tenants/ten_1/projects/prj_1/members/u_me"
      ) {
        removed = true;
        return { status: 204 };
      }
      if (path === "/api/v1/tenants")
        return ok([{ id: "ten_1", name: "Team", role: "member" }]);
      if (path === "/api/v1/tenants/ten_1/projects")
        return ok(removed ? [] : [project]);
      if (path === "/api/v1/tenants/ten_1/members")
        return ok([{ ...me, role: "member" }, owner]);
      if (path === "/api/v1/tenants/ten_1/projects/prj_1/members")
        return removed ? undefined : ok([{ ...me, role: "admin" }]);
      return undefined;
    });
    wrapper = mount(MembersPage, { attachTo: document.body });
    await flushPromises();
    expect(canManageProject.value).toBe(true);
    await button(wrapper, "项目授权").trigger("click");
    const row = wrapper
      .findAll("tbody tr")
      .find((r) => r.text().includes("u_me"));
    await row!.find("button.danger-text").trigger("click");
    await flushPromises();
    await button(wrapper, "确认变更").trigger("click");
    await flushPromises();
    expect(listCalls(fetchMock, "/api/v1/tenants")).toBe(1);
    expect(state.projects).toEqual([]);
    expect(state.projectId).toBe("");
    expect(canManageProject.value).toBe(false);
  });
});
