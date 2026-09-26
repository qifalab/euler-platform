import { mount, flushPromises } from "@vue/test-utils";
import { describe, it, expect, vi } from "vitest";
import App from "./App.vue";
import { appContextKey, type AppContext, type AppPermission } from "../shared";

function setup(permissions: AppPermission[]) {
  const site = {
    id: "site-1",
    name: "Portal",
    enabled: true,
    sitekey: "wa_public",
    domains: ["example.com"],
    baseDifficulty: 1,
    maxDifficulty: 3,
    batchModeEnabled: false,
    batchDifficulty: 1,
    minBatchCount: 1,
    maxBatchCount: 4,
    challengeTimeout: 300,
    tokenTimeout: 300,
    policy: {
      enabled: true,
      timeWindowSeconds: 300,
      requestThreshold: 10,
      difficultyIncrement: 1,
      batchIncrement: 1,
      failureWeight: 2,
    },
    createdAt: "2026-09-26",
  };
  const request = vi.fn(
    async (path: string, options?: { method?: string; body?: unknown }) => {
      if (path === "/sites") return { items: [site] };
      if (path.endsWith("/stats"))
        return {
          totalChallenges: 10,
          solvedChallenges: 8,
          totalVerifications: 10,
          successVerifications: 8,
          last24hChallenges: 4,
          last24hSolved: 3,
        };
      if (path.endsWith("/ip-records")) return { items: [] };
      if (path.endsWith("/secret")) return { secret: "private-site-secret" };
      if (options?.method === "PUT") Object.assign(site, options.body);
      return { ...site };
    },
  );
  const context = {
    scope: { permissions },
    publicBase: "https://euler.example/public/weauth",
    can: (permission: AppPermission) => permissions.includes(permission),
    request,
    notify: vi.fn(),
  } as unknown as AppContext;
  return {
    wrapper: mount(App, {
      global: { provide: { [appContextKey as symbol]: context } },
    }),
    request,
  };
}
function button(wrapper: ReturnType<typeof mount>, text: string) {
  const found = wrapper.findAll("button").find((item) => item.text() === text);
  if (!found) throw new Error(`Missing button ${text}`);
  return found;
}

describe("native WeAuth", () => {
  it("keeps secrets out of overview and retrieves them only after an explicit privileged click", async () => {
    const { wrapper, request } = setup(["read", "write", "manage", "secrets"]);
    await flushPromises();
    expect(wrapper.text()).toContain("80.0%");
    expect(request.mock.calls.some(([path]) => path.endsWith("/secret"))).toBe(
      false,
    );
    await button(wrapper, "域名与密钥").trigger("click");
    await button(wrapper, "查看服务端密钥").trigger("click");
    await flushPromises();
    expect(wrapper.text()).toContain("private-site-secret");
    await button(wrapper, "隐藏").trigger("click");
    expect(wrapper.text()).not.toContain("private-site-secret");
    wrapper.unmount();
  });
  it("renders read-only settings and hides privileged actions from viewers", async () => {
    const { wrapper, request } = setup(["read"]);
    await flushPromises();
    expect(wrapper.text()).not.toContain("新建站点");
    await button(wrapper, "验证配置").trigger("click");
    expect(
      wrapper
        .findAll("input")
        .every((input) => input.attributes("disabled") !== undefined),
    ).toBe(true);
    expect(wrapper.text()).not.toContain("删除站点");
    await button(wrapper, "域名与密钥").trigger("click");
    expect(wrapper.text()).not.toContain("查看服务端密钥");
    expect(
      request.mock.calls.some(([path]) => path.endsWith("/ip-records")),
    ).toBe(false);
    wrapper.unmount();
  });
  it("saves typed settings and exposes a real public widget with server verification instructions", async () => {
    const { wrapper, request } = setup(["read", "write"]);
    await flushPromises();
    await button(wrapper, "验证配置").trigger("click");
    await wrapper.find('input[maxlength="100"]').setValue("Changed portal");
    await wrapper.find("form.tab-body").trigger("submit");
    await flushPromises();
    const update = request.mock.calls.find(
      ([, options]) => options?.method === "PUT",
    );
    expect(update?.[0]).toBe("/sites/site-1");
    expect(update?.[1]?.body).toMatchObject({
      name: "Changed portal",
      baseDifficulty: 1,
      tokenTimeout: 300,
    });
    expect(update?.[1]?.body).not.toHaveProperty("sitekey");
    await button(wrapper, "集成与预览").trigger("click");
    expect(wrapper.text()).toContain("/public/weauth/weauth.js");
    expect(wrapper.text()).toContain("verification.hostname");
    expect(wrapper.text()).toContain("process.env.WEAUTH_SECRET");
    wrapper.unmount();
  });
});
