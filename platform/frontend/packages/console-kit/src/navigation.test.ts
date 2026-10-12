/**
 * @vitest-environment jsdom
 */
import { describe, it, expect, vi, beforeEach } from "vitest";

const bridge = vi.hoisted(() => ({ sandbox: false, props: {} as Record<string, unknown> }));
vi.mock("@eu/wujie-bridge", () => ({
  inWujieSandbox: () => bridge.sandbox,
  readSharedProps: () => bridge.props,
}));

import { navigateShell, routesInApp } from "./navigation";

describe("navigateShell", () => {
  beforeEach(() => {
    bridge.sandbox = false;
    bridge.props = {};
  });

  it("uses a shell-injected navigate prop when present", () => {
    const navigate = vi.fn();
    bridge.sandbox = true;
    bridge.props = { navigate };
    expect(navigateShell("/billing")).toBe(true);
    expect(navigate).toHaveBeenCalledWith("/billing");
  });

  it("returns false standalone (no shell to navigate)", () => {
    expect(navigateShell("/billing")).toBe(false);
  });

  it("navigates the parent (shell) window inside the sandbox", () => {
    bridge.sandbox = true;
    const assign = vi.fn();
    const parent = { location: { assign } };
    const spy = vi.spyOn(window, "parent", "get").mockReturnValue(parent as unknown as Window);
    try {
      expect(navigateShell("/billing")).toBe(true);
      expect(assign).toHaveBeenCalledWith("/billing");
    } finally {
      spy.mockRestore();
    }
  });

  it("returns false when there is no distinct parent window", () => {
    bridge.sandbox = true;
    // jsdom: window.parent === window.top === window
    expect(navigateShell("/billing")).toBe(false);
  });
});

describe("routesInApp", () => {
  const click = { button: 0, metaKey: false, ctrlKey: false, shiftKey: false, altKey: false, defaultPrevented: false };

  it("routes a plain left click on an app path", () => {
    expect(routesInApp("/instances/euredis-1", click)).toBe(true);
  });

  it("leaves modified, middle and already-handled clicks to the browser", () => {
    expect(routesInApp("/instances/x", { ...click, button: 1 })).toBe(false);
    expect(routesInApp("/instances/x", { ...click, ctrlKey: true })).toBe(false);
    expect(routesInApp("/instances/x", { ...click, metaKey: true })).toBe(false);
    expect(routesInApp("/instances/x", { ...click, shiftKey: true })).toBe(false);
    expect(routesInApp("/instances/x", { ...click, defaultPrevented: true })).toBe(false);
  });

  it("never routes hash links, protocol-relative or absolute URLs", () => {
    expect(routesInApp("#/euvpc/vpcs/vpc-1", click)).toBe(false);
    expect(routesInApp("//evil.example/x", click)).toBe(false);
    expect(routesInApp("https://docs.euler.emoera.com/", click)).toBe(false);
  });
});
