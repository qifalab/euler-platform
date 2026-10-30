/**
 * @vitest-environment jsdom
 *
 * Audit page: team audit is readable by team owners/admins, project audit by
 * project administrators. Others get a neutral note, never a request the
 * server refuses with 403.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import AuditPage from "./AuditPage.vue";
import { useCloud } from "./context";

const { state } = useCloud();
let wrapper: VueWrapper | undefined;
afterEach(() => {
  wrapper?.unmount();
  wrapper = undefined;
  vi.unstubAllGlobals();
});

async function mountAs(teamRole: string, projectRole: string) {
  const fetchMock = vi.fn(
    async () =>
      new Response(JSON.stringify({ items: [] }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
  );
  vi.stubGlobal("fetch", fetchMock);
  Object.assign(state, {
    session: { authenticated: true, loginAvailable: true },
    tenants: [{ id: "ten_1", name: "Team", role: teamRole, createdAt: "" }],
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
  });
  wrapper = mount(AuditPage);
  await flushPromises();
  return {
    wrapper,
    paths: fetchMock.mock.calls.map((call) => String((call as unknown[])[0])),
  };
}

describe("AuditPage", () => {
  it("does not request audit for a plain project member", async () => {
    const { wrapper, paths } = await mountAs("member", "member");
    expect(paths).toEqual([]);
    expect(wrapper.text()).toContain("操作审计仅管理员可查看");
    expect(wrapper.text()).not.toContain("审计记录加载失败");
  });

  it("reads the project audit for a project administrator", async () => {
    const { paths } = await mountAs("member", "admin");
    expect(paths).toEqual([
      "/api/v1/tenants/ten_1/audit?limit=100&projectId=prj_1",
    ]);
  });
});
