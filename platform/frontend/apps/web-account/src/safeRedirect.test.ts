import { describe, expect, it } from "vitest";
import { resolveSafeRedirect, safeRedirect } from "./safeRedirect";

const account = { origin: "https://account.euler.emoera.com", hostname: "account.euler.emoera.com" };
const dev = { origin: "http://localhost:5175", hostname: "localhost" };

describe("resolveSafeRedirect", () => {
  it("accepts same-origin paths (kept relative)", () => {
    expect(resolveSafeRedirect("/profile?tab=1#x", account)).toBe("/profile?tab=1#x");
    expect(resolveSafeRedirect("https://account.euler.emoera.com/ak", account)).toBe("/ak");
  });

  it("accepts https targets on the SSO site (console → realname → console)", () => {
    expect(resolveSafeRedirect("https://console.euler.emoera.com/euecs/instances", account)).toBe(
      "https://console.euler.emoera.com/euecs/instances",
    );
  });

  it("refuses other sites, look-alikes, downgrades and script URLs", () => {
    expect(resolveSafeRedirect("https://evil.example/", account)).toBeNull();
    expect(resolveSafeRedirect("https://euler.emoera.com.evil.example/", account)).toBeNull();
    expect(resolveSafeRedirect("https://evileuler.emoera.com/", account)).toBeNull();
    expect(resolveSafeRedirect("http://console.euler.emoera.com/", account)).toBeNull();
    expect(resolveSafeRedirect("//evil.example/x", account)).toBeNull();
    expect(resolveSafeRedirect("/\\evil.example/x", account)).toBeNull();
    expect(resolveSafeRedirect("javascript:alert(1)", account)).toBeNull();
  });

  it("allows loopback hosts only in local development", () => {
    expect(resolveSafeRedirect("http://localhost:5173/euecs", dev)).toBe("http://localhost:5173/euecs");
    expect(resolveSafeRedirect("http://localhost:5173/euecs", account)).toBeNull();
  });

  it("returns null for absent values", () => {
    expect(resolveSafeRedirect(undefined, account)).toBeNull();
    expect(resolveSafeRedirect("", account)).toBeNull();
    expect(resolveSafeRedirect(["/a"], account)).toBeNull();
  });
});

describe("safeRedirect", () => {
  it("falls back to the console home", () => {
    expect(safeRedirect("https://evil.example/", account)).toBe("https://console.euler.emoera.com/");
    expect(safeRedirect(undefined, dev)).toBe("http://localhost:5173/");
  });
});
