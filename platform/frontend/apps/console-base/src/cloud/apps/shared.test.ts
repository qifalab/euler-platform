/**
 * @vitest-environment jsdom
 *
 * The application context keeps every module request under its own
 * /apps/<id> prefix. The URL parser that fetch uses also resolves
 * percent-encoded dot segments (%2e%2e), so those must be refused like "..".
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { createAppContext, type AppScope } from "./shared";

const scope = {
  tenantId: "ten_1",
  projectId: "prj_1",
  applicationId: "storage",
  permissions: ["read"],
} as unknown as AppScope;
afterEach(() => vi.unstubAllGlobals());

describe("createAppContext path guard", () => {
  it("refuses encoded parent segments and keeps dots inside names", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response("{}", {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const app = createAppContext(scope, vi.fn(), new AbortController().signal);
    for (const path of [
      "/../service-accounts",
      "/%2e%2e/%2e%2e/service-accounts",
      "/buckets/.%2E/x",
      "/a/%2E./b",
    ])
      expect(() => app.request(path), path).toThrow("Invalid application path");
    await expect(app.download("/%2e%2e/export", "x.json")).rejects.toThrow(
      "Invalid application path",
    );
    expect(fetchMock).not.toHaveBeenCalled();

    await app.request("/buckets/release.v1/objects?prefix=a.b");
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(String((fetchMock.mock.calls[0] as unknown[])[0])).toBe(
      "/api/v1/tenants/ten_1/projects/prj_1/apps/storage/buckets/release.v1/objects?prefix=a.b",
    );
  });
});
