/**
 * @vitest-environment jsdom
 *
 * Vitest tests for @eu/wujie-bridge:
 *   - bootstrapSubApp: Wujie lifecycle hooks in the sandbox, self-mount when
 *     standalone (immediately or on DOMContentLoaded), unmount waits for an
 *     in-flight mount, lifecycle errors never escape as unhandled rejections.
 *   - subAppAuthOptions: the shell's refreshToken is used inside Wujie; a
 *     standalone app redeems the refresh cookie itself (single-flight).
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { bootstrapSubApp, bridge, subAppAuthOptions } from "./index";

type W = Record<string, unknown>;
const win = window as unknown as W;

function enterSandbox(extra: W = {}) {
  win.__POWERED_BY_WUJIE__ = true;
  Object.assign(win, extra);
}

function setReadyState(state: DocumentReadyState) {
  Object.defineProperty(document, "readyState", { value: state, configurable: true });
}

beforeEach(() => {
  // Clears the module-level standalone token cache between tests.
  bridge.emit("auth:logout");
});

afterEach(() => {
  for (const k of ["__POWERED_BY_WUJIE__", "__WUJIE", "__WUJIE_MOUNT", "__WUJIE_UNMOUNT", "$wujie"]) delete win[k];
  // Drop the own-property override so jsdom's real readyState is back.
  delete (document as unknown as W).readyState;
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("bootstrapSubApp — standalone", () => {
  it("mounts right away when the document is already parsed", () => {
    setReadyState("complete");
    const mount = vi.fn();
    bootstrapSubApp(mount, vi.fn());
    expect(mount).toHaveBeenCalledTimes(1);
    expect(win.__WUJIE_MOUNT).toBeUndefined();
  });

  it("waits for DOMContentLoaded while the document is still loading", () => {
    setReadyState("loading");
    const mount = vi.fn();
    bootstrapSubApp(mount, vi.fn());
    expect(mount).not.toHaveBeenCalled();
    document.dispatchEvent(new Event("DOMContentLoaded"));
    expect(mount).toHaveBeenCalledTimes(1);
    // { once: true } — a second event does not mount twice.
    document.dispatchEvent(new Event("DOMContentLoaded"));
    expect(mount).toHaveBeenCalledTimes(1);
  });

  it("logs (does not throw) when mount fails", async () => {
    setReadyState("complete");
    const err = vi.spyOn(console, "error").mockImplementation(() => {});
    bootstrapSubApp(() => Promise.reject(new Error("boom")), vi.fn());
    await Promise.resolve();
    await Promise.resolve();
    expect(err).toHaveBeenCalled();
  });
});

describe("bootstrapSubApp — inside Wujie", () => {
  it("registers __WUJIE_MOUNT/__WUJIE_UNMOUNT and asks Wujie to mount", async () => {
    const wujieMount = vi.fn(() => (win.__WUJIE_MOUNT as () => void)());
    enterSandbox({ __WUJIE: { mount: wujieMount } });
    const mount = vi.fn();
    const unmount = vi.fn();

    bootstrapSubApp(mount, unmount);

    expect(typeof win.__WUJIE_MOUNT).toBe("function");
    expect(typeof win.__WUJIE_UNMOUNT).toBe("function");
    expect(wujieMount).toHaveBeenCalledTimes(1);
    expect(mount).toHaveBeenCalledTimes(1);

    await (win.__WUJIE_UNMOUNT as () => Promise<void>)();
    expect(unmount).toHaveBeenCalledTimes(1);

    // Wujie re-mounts a destroyed (non-alive) app through the same hook.
    (win.__WUJIE_MOUNT as () => void)();
    expect(mount).toHaveBeenCalledTimes(2);
  });

  it("does not self-mount on DOMContentLoaded inside the sandbox", () => {
    enterSandbox({ __WUJIE: {} });
    setReadyState("loading");
    const mount = vi.fn();
    bootstrapSubApp(mount, vi.fn());
    document.dispatchEvent(new Event("DOMContentLoaded"));
    expect(mount).not.toHaveBeenCalled();
  });

  it("unmount waits for an in-flight async mount", async () => {
    enterSandbox({ __WUJIE: {} });
    const order: string[] = [];
    let finishMount!: () => void;
    const mount = () =>
      new Promise<void>((resolve) => {
        finishMount = () => {
          order.push("mounted");
          resolve();
        };
      });
    const unmount = () => {
      order.push("unmounted");
    };
    bootstrapSubApp(mount, unmount);
    (win.__WUJIE_MOUNT as () => void)();
    const unmounting = (win.__WUJIE_UNMOUNT as () => Promise<void>)();
    await Promise.resolve();
    expect(order).toEqual([]);
    finishMount();
    await unmounting;
    expect(order).toEqual(["mounted", "unmounted"]);
  });

  it("a throwing unmount does not reject the Wujie hook", async () => {
    enterSandbox({ __WUJIE: {} });
    vi.spyOn(console, "error").mockImplementation(() => {});
    bootstrapSubApp(vi.fn(), () => {
      throw new Error("teardown");
    });
    (win.__WUJIE_MOUNT as () => void)();
    await expect((win.__WUJIE_UNMOUNT as () => Promise<void>)()).resolves.toBeUndefined();
  });
});

describe("bridge — sandbox flag without a $wujie bus", () => {
  it("does not throw at import and falls back to a local bus", async () => {
    enterSandbox(); // __POWERED_BY_WUJIE__ only, no window.$wujie
    vi.resetModules();
    const mod = await import("./index");
    const fn = vi.fn();
    const off = mod.bridge.on("region:changed", fn);
    mod.bridge.emit("region:changed", { regionId: "cn-north-1" });
    expect(fn).toHaveBeenCalledWith({ regionId: "cn-north-1" });
    off();
    mod.bridge.emit("region:changed", { regionId: "cn-east-1" });
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it("uses the Wujie bus when $wujie provides one", () => {
    const bus = { $on: vi.fn(), $off: vi.fn(), $emit: vi.fn() };
    enterSandbox({ $wujie: { bus } });
    const fn = vi.fn();
    const off = bridge.on("auth:logout", fn);
    expect(bus.$on).toHaveBeenCalledWith("auth:logout", fn);
    bridge.emit("auth:logout", { x: 1 });
    expect(bus.$emit).toHaveBeenCalledWith("auth:logout", { x: 1 });
    off();
    expect(bus.$off).toHaveBeenCalledWith("auth:logout", fn);
  });
});

describe("subAppAuthOptions", () => {
  it("inside Wujie: uses the shell's live token and refreshToken, never the cookie", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    enterSandbox({
      $wujie: {
        props: { getToken: () => "shell-token", refreshToken: vi.fn().mockResolvedValue("fresh-token") },
        bus: { $on: vi.fn(), $off: vi.fn(), $emit: vi.fn() },
      },
    });
    const opts = subAppAuthOptions();
    expect(opts.getToken()).toBe("shell-token");
    await expect(opts.onUnauthorized()).resolves.toBe("fresh-token");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("standalone: redeems the refresh cookie once for concurrent 401s", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ Code: "OK", Data: { accessToken: "cookie-token" } }), { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const opts = subAppAuthOptions();
    expect(opts.getToken()).toBeUndefined();

    const [a, b] = await Promise.all([opts.onUnauthorized(), opts.onUnauthorized()]);
    expect(a).toBe("cookie-token");
    expect(b).toBe("cookie-token");
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/auth/refresh");
    expect(init.method).toBe("POST");
    expect(init.credentials).toBe("include");
    // Subsequent requests carry the redeemed token.
    expect(opts.getToken()).toBe("cookie-token");
  });

  it("standalone: a rejected refresh yields no token (the 401 surfaces)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(new Response(JSON.stringify({ Code: "Auth.NoSession" }), { status: 401 })),
    );
    const opts = subAppAuthOptions();
    await expect(opts.onUnauthorized()).resolves.toBeUndefined();
  });
});
