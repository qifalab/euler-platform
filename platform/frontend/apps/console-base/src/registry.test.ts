/**
 * @vitest-environment jsdom
 *
 * Registry client: /api/v1/meta/console/apps is JWT-protected at the gateway,
 * so the shell sends its access token; any failure (401 after a failed
 * refresh, network) still falls back to the built-in registry.
 */
import { describe, it, expect, vi, beforeEach } from "vitest";
import { createPinia, setActivePinia } from "pinia";

const auth = vi.hoisted(() => ({
  accessToken: "shell-at",
  silentRefresh: vi.fn(async () => undefined as string | undefined),
}));
vi.mock("./stores/auth", () => ({ useAuthStore: () => auth }));

import { useRegistry } from "./registry";

function envelope(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const remote = {
  revision: 7,
  ttl: 300,
  apps: [{ appCode: "console-ecs", appTitle: "ECS", productCodes: ["euecs"], activeRules: ["/euecs"], entryUrl: "/ecs/", version: "1", keepAlive: true, preload: false, status: "online" }],
};

describe("console-base registry", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    sessionStorage.clear();
    auth.silentRefresh.mockClear();
    vi.spyOn(console, "warn").mockImplementation(() => {});
    vi.spyOn(console, "error").mockImplementation(() => {});
  });

  it("sends the shell's bearer token and applies the runtime registry", async () => {
    const fetchMock = vi.fn(async () => envelope(200, { Code: "OK", Data: remote }));
    vi.stubGlobal("fetch", fetchMock);

    const reg = useRegistry();
    await reg.load();

    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("/api/v1/meta/console/apps");
    expect(new Headers(init.headers).get("Authorization")).toBe("Bearer shell-at");
    expect(reg.revision).toBe(7);
    expect(reg.apps.map((a) => a.appCode)).toEqual(["console-ecs"]);
  });

  it("falls back to the built-in registry when the call stays unauthorized", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => envelope(401, { Code: "Auth.Unauthorized", Message: "未认证" })));

    const reg = useRegistry();
    await reg.load();

    expect(auth.silentRefresh).toHaveBeenCalledTimes(1);
    expect(reg.loaded).toBe(true);
    expect(reg.resolve("/billing")?.appCode).toBe("web-billing");
    expect(sessionStorage.getItem("eu:console:registry")).toBeNull();
  });
});
