/**
 * @vitest-environment jsdom
 *
 * Project overview "最近动态": the server lets only project administrators
 * (team owner/admin, or a project admin) read project audit. Other members
 * must get a neutral note instead of a permanent 403 error panel.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import { createMemoryHistory, createRouter } from "vue-router";
import OverviewPage from "./OverviewPage.vue";
import { useCloud } from "./context";

const { state } = useCloud();
let wrapper: VueWrapper | undefined;
afterEach(() => {
  wrapper?.unmount();
  wrapper = undefined;
  vi.unstubAllGlobals();
});

async function mountAs(projectRole: "member" | "admin") {
  const fetchMock = vi.fn(async (input: unknown) => {
    const path = String(input);
    if (path.startsWith("/api/v1/tenants/ten_1/audit?"))
      return projectRole === "admin"
        ? new Response(
            JSON.stringify({
              items: [
                {
                  id: "evt_1",
                  actorId: "u_me",
                  action: "project.renamed",
                  targetId: "prj_1",
                  createdAt: "2026-10-30T00:00:00Z",
                  summary: "Updated project name",
                },
              ],
            }),
            { status: 200, headers: { "Content-Type": "application/json" } },
          )
        : new Response(
            JSON.stringify({
              error: {
                code: "forbidden",
                message: "You do not have permission for this operation",
              },
            }),
            { status: 403, headers: { "Content-Type": "application/json" } },
          );
    return new Response("{}", { status: 404 });
  });
  vi.stubGlobal("fetch", fetchMock);
  Object.assign(state, {
    session: { authenticated: true, loginAvailable: true },
    catalog: [],
    tenants: [{ id: "ten_1", name: "Team", role: "member", createdAt: "" }],
    tenantId: "ten_1",
    projects: [
      {
        id: "prj_1",
        tenantId: "ten_1",
        name: "Project",
        role: projectRole,
        status: "active",
        createdAt: "",
      },
    ],
    projectId: "prj_1",
    installations: [],
    installationsLoading: false,
    installationError: "",
  });
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: "/:rest(.*)*", component: OverviewPage }],
  });
  await router.push("/");
  wrapper = mount(OverviewPage, { global: { plugins: [router] } });
  await flushPromises();
  const auditCalls = fetchMock.mock.calls.filter(([input]) =>
    String(input).includes("/audit?"),
  ).length;
  return { wrapper, auditCalls };
}

describe("OverviewPage recent activity", () => {
  it("does not request audit or show an error for a plain project member", async () => {
    const { wrapper, auditCalls } = await mountAs("member");
    expect(auditCalls).toBe(0);
    expect(wrapper.text()).not.toContain("动态暂不可用");
    expect(wrapper.text()).toContain("仅项目管理员可查看");
  });

  it("still shows recent activity to a project administrator", async () => {
    const { wrapper, auditCalls } = await mountAs("admin");
    expect(auditCalls).toBe(1);
    expect(wrapper.text()).toContain("Updated project name");
  });
});
