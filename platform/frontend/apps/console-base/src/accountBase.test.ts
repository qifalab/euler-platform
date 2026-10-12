/**
 * @vitest-environment node
 */
import { describe, it, expect } from "vitest";
import { resolveAccountBase, PROD_ACCOUNT_BASE, DEV_ACCOUNT_BASE } from "./accountBase";

describe("resolveAccountBase", () => {
  it("defaults to the production account center outside dev", () => {
    expect(resolveAccountBase(undefined, { DEV: false })).toBe(PROD_ACCOUNT_BASE);
    expect(resolveAccountBase({}, {})).toBe(PROD_ACCOUNT_BASE);
  });

  it("uses the local web-account only in dev", () => {
    expect(resolveAccountBase(undefined, { DEV: true })).toBe(DEV_ACCOUNT_BASE);
  });

  it("prefers the runtime __SC_ENV__ over the build env", () => {
    expect(resolveAccountBase({ accountBase: "https://acct.staging.example/" }, { VITE_ACCOUNT_BASE: "https://b", DEV: true }))
      .toBe("https://acct.staging.example");
    expect(resolveAccountBase({ ACCOUNT_BASE: "https://a" }, { DEV: false })).toBe("https://a");
  });

  it("falls back to VITE_ACCOUNT_BASE, ignoring blank values", () => {
    expect(resolveAccountBase({ accountBase: "  " }, { VITE_ACCOUNT_BASE: "https://b/", DEV: true })).toBe("https://b");
  });
});
