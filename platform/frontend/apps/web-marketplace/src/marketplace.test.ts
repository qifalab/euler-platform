import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

function envelope(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

// marketplace.ts caches the operator probe at module level: load a fresh copy
// per test.
async function load() {
  vi.resetModules();
  return import("./marketplace");
}

beforeEach(() => {
  vi.unstubAllGlobals();
});
afterEach(() => {
  vi.unstubAllGlobals();
});

describe("isOperator", () => {
  it("is true when the operator-only review queue loads", async () => {
    const fetchMock = vi.fn().mockResolvedValue(envelope(200, { Code: "OK", Data: { listings: [] } }));
    vi.stubGlobal("fetch", fetchMock);
    const m = await load();
    await expect(m.isOperator()).resolves.toBe(true);
    expect(String(fetchMock.mock.calls[0][0])).toContain("status=PENDING_APPROVAL");
    // Decided once — cached.
    await m.isOperator();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("is false (and cached) on 403 Marketplace.NotOperator", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(envelope(403, { Code: "Marketplace.NotOperator", Message: "platform operator role required" }));
    vi.stubGlobal("fetch", fetchMock);
    const m = await load();
    await expect(m.isOperator()).resolves.toBe(false);
    await expect(m.isOperator()).resolves.toBe(false);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("re-probes after a transient failure instead of hiding operator tools for good", async () => {
    const fetchMock = vi
      .fn()
      .mockRejectedValueOnce(new TypeError("Failed to fetch"))
      .mockResolvedValue(envelope(200, { Code: "OK", Data: { listings: [] } }));
    vi.stubGlobal("fetch", fetchMock);
    const m = await load();
    await expect(m.isOperator()).resolves.toBe(false);
    await expect(m.isOperator()).resolves.toBe(true);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
});

describe("formatDate", () => {
  it("uses the viewer's time zone rather than the UTC day of the string", async () => {
    const { formatDate } = await load();
    expect(formatDate("2026-10-01T20:00:00Z", { locale: "en-CA", timeZone: "Asia/Shanghai" })).toBe("2026-10-02");
    expect(formatDate("")).toBe("");
    expect(formatDate("garbage")).toBe("garbage");
  });
});
