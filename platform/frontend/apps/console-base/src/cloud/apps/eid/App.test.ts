import { beforeAll, describe, expect, it, vi } from "vitest";
import { flushPromises, mount } from "@vue/test-utils";
import { createMemoryHistory, createRouter } from "vue-router";
import App from "./App.vue";
import { appContextKey, type AppContext, type AppPermission } from "../shared";
import { CloudError } from "../../api";
import type { Club } from "./types";

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

type Responder = (path: string, options?: any) => unknown;
async function mounted(permissions: AppPermission[], responder: Responder) {
  const request = vi.fn(async (path: string, options?: any) => {
    const out = await responder(path, options);
    return out === undefined ? { items: [], total: 0, page: 1 } : out;
  });
  const app: AppContext = {
    scope: {
      actorId: "reviewer",
      actorName: "Reviewer",
      email: "reviewer@example.test",
      emailVerified: true,
      provider: "issuer",
      subject: "reviewer",
      tenantId: "t",
      projectId: "p",
      installationId: "i",
      applicationId: "eid",
      permissions,
    },
    basePath: "/api/eid",
    publicBase: "https://euler.test/public/eid",
    can: (permission) => permissions.includes(permission),
    request: request as AppContext["request"],
    upload: vi.fn() as AppContext["upload"],
    download: vi.fn(),
    notify: vi.fn(),
  };
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: "/:path(.*)*", component: { render: () => null } }],
  });
  await router.push("/apps/eid");
  await router.isReady();
  const wrapper = mount(App, {
    global: {
      plugins: [router],
      provide: { [appContextKey as symbol]: app },
    },
  });
  await flushPromises();
  return { wrapper, app, request };
}
async function click(wrapper: ReturnType<typeof mount>, text: string) {
  const found = wrapper.findAll("button").find((b) => b.text() === text);
  if (!found) throw new Error(`Missing button ${text}`);
  await found.trigger("click");
  await flushPromises();
}
const pending: Club = {
  id: "eca_1",
  actorId: "alice",
  actorName: "Alice",
  realName: "Alice",
  email: "alice@example.test",
  status: "pending",
  trustSchemeId: "",
  trustVerified: false,
  version: 1,
  createdAt: "2026-10-30T08:00:00Z",
  updatedAt: "2026-10-30T08:00:00Z",
  interviewSentAt: "",
  offerSentAt: "",
  confirmedAt: "",
};

describe("EID recruitment notifications", () => {
  it("replays the same idempotency key when a send is retried after a lost response", async () => {
    let club = pending;
    let attempts = 0;
    const { wrapper, request } = await mounted(
      ["read", "write", "review"],
      (path, options) => {
        if (path.startsWith("/review/club?"))
          return {
            items: [club],
            total: 1,
            page: 1,
            counts: { [club.status]: 1 },
          };
        if (path === `/review/club/${club.id}/notifications`) {
          attempts++;
          // The server is still delivering when the browser gives up.
          if (attempts === 1)
            throw new CloudError("请求超时，请检查网络后重试。");
          club = {
            ...club,
            status: options.body.resend ? club.status : "interview_sent",
            version: club.version + 1,
          };
          return {
            id: `ent_${attempts}`,
            targetId: club.id,
            kind: "interview",
            state: "sent",
            error: "",
          };
        }
        return undefined;
      },
    );
    await click(wrapper, "招募审核");
    await click(wrapper, "发送笔试通知");
    await click(wrapper, "确认操作");
    expect(attempts).toBe(1);
    await click(wrapper, "确认操作");
    const sends = request.mock.calls.filter(([path]) =>
      String(path).endsWith("/notifications"),
    );
    expect(sends).toHaveLength(2);
    const [first, second] = sends.map(([, options]) => options);
    expect(second.body.idempotencyKey).toBe(first.body.idempotencyKey);
    // SMTP delivery can outlast the 20 s default request timeout.
    expect(first.timeoutMs).toBeGreaterThanOrEqual(60_000);
    // A new confirmation is a new operation and gets its own key.
    await click(wrapper, "重发笔试通知");
    await click(wrapper, "确认操作");
    const resend = request.mock.calls
      .filter(([path]) => String(path).endsWith("/notifications"))
      .at(-1)![1];
    expect(resend.body.resend).toBe(true);
    expect(resend.body.idempotencyKey).not.toBe(first.body.idempotencyKey);
    wrapper.unmount();
  });
});

describe("EID console loading", () => {
  it("ignores a late response from a tab the user already left", async () => {
    let release!: (value: unknown) => void;
    const slowMembers = new Promise((resolve) => (release = resolve));
    const { wrapper } = await mounted(
      ["read", "write", "review", "admin"],
      (path) => {
        if (path.startsWith("/admin/members?")) return slowMembers;
        if (path.startsWith("/notifications?"))
          return {
            items: [
              {
                id: "ent_1",
                targetId: "eca_1",
                actorId: "reviewer",
                kind: "interview",
                state: "sent",
                error: "",
                createdAt: "2026-10-30T08:00:00Z",
                completedAt: "2026-10-30T08:00:01Z",
              },
            ],
            total: 1,
            page: 1,
          };
        return undefined;
      },
    );
    await click(wrapper, "会员管理");
    await click(wrapper, "通知记录");
    expect(wrapper.text()).toContain("通知发送记录");
    release({ items: [], total: 500, page: 1 });
    await flushPromises();
    // The members total belongs to the abandoned tab; the notifications
    // view has one record and no pager.
    expect(wrapper.text()).not.toContain("共 500 条");
    expect(wrapper.find(".pagination").exists()).toBe(false);
    expect(wrapper.text()).toContain("eca_1");
    wrapper.unmount();
  });
});
