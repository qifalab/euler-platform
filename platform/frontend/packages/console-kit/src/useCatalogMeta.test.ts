/**
 * @vitest-environment jsdom
 *
 * useCatalogMeta — /api/v1/catalog/* is JWT-protected at the console gateway,
 * so the metadata calls must carry the bearer token (the old bare fetch sent
 * none and 401'd behind the gateway).
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { fetchPlacement, fetchRegions, setCatalogClient, useCatalogMeta, type CatalogClient } from "./useCatalogMeta";

const W = window as unknown as Record<string, unknown>;

function ok(data: unknown): Response {
  return new Response(JSON.stringify({ Code: "OK", Data: data }), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

const regions = [{ regionId: "cn-north-1", regionName: "华北一", zones: [{ zoneId: "cn-north-1a", zoneName: "可用区 A" }] }];

afterEach(() => {
  setCatalogClient(null);
  delete W.__POWERED_BY_WUJIE__;
  delete W.$wujie;
  vi.unstubAllGlobals();
});

describe("useCatalogMeta auth", () => {
  it("inside the shell: sends the base's live token by default", async () => {
    W.__POWERED_BY_WUJIE__ = true;
    W.$wujie = { props: { getToken: () => "shell-at" }, bus: { $on: vi.fn(), $off: vi.fn(), $emit: vi.fn() } };
    const fetchMock = vi.fn().mockResolvedValue(ok(regions));
    vi.stubGlobal("fetch", fetchMock);

    await expect(fetchRegions()).resolves.toEqual(regions);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/catalog/regions");
    expect(new Headers(init.headers).get("Authorization")).toBe("Bearer shell-at");
  });

  it("uses an injected client (the shell passes its own auth-wired SDK)", async () => {
    const get = vi.fn().mockResolvedValue({
      data: { productCode: "euredis", regionScope: "REGIONAL", zonal: true, crossAz: false, zoneRequired: true },
    });
    setCatalogClient({ get } as unknown as CatalogClient);
    await expect(fetchPlacement("euredis")).resolves.toMatchObject({ zonal: true });
    expect(get).toHaveBeenCalledWith("/api/v1/catalog/placement?productCode=euredis");
  });

  it("surfaces a failed region load and refetches on retry", async () => {
    const get = vi
      .fn()
      .mockRejectedValueOnce(Object.assign(new Error("未认证"), { status: 401 }))
      .mockResolvedValueOnce({ data: regions });
    setCatalogClient({ get } as unknown as CatalogClient);

    const meta = useCatalogMeta();
    await meta.load();
    expect(meta.error.value).toBe("未认证");
    expect(meta.regions.value).toEqual([]);

    await meta.load();
    expect(meta.error.value).toBeNull();
    expect(meta.zonesOf("cn-north-1")).toHaveLength(1);
    expect(get).toHaveBeenCalledTimes(2);
  });
});
