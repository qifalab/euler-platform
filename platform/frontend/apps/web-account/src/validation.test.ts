import { describe, expect, it } from "vitest";
import { isValidLoginEmail, passwordError, utf8Length } from "./validation";

describe("isValidLoginEmail (mirrors svc-iam validLoginEmail)", () => {
  it("accepts plain addresses, case-insensitively and trimmed", () => {
    expect(isValidLoginEmail("alice@example.com")).toBe(true);
    expect(isValidLoginEmail("  Alice.Smith+tag@Mail.Example.COM ")).toBe(true);
  });

  it("rejects phone numbers and non-addresses (email is the only login id)", () => {
    expect(isValidLoginEmail("13800138000")).toBe(false);
    expect(isValidLoginEmail("alice")).toBe(false);
    expect(isValidLoginEmail("alice@localhost")).toBe(false); // backend needs a dot in the domain
    expect(isValidLoginEmail("Alice <alice@example.com>")).toBe(false);
    expect(isValidLoginEmail('"a b"@example.com')).toBe(false);
    expect(isValidLoginEmail("a..b@example.com")).toBe(false);
    expect(isValidLoginEmail(".a@example.com")).toBe(false);
    expect(isValidLoginEmail("a@b@example.com")).toBe(false);
    expect(isValidLoginEmail(`${"a".repeat(250)}@x.co`)).toBe(false);
  });
});

describe("passwordError (8-128 bytes, like the backend)", () => {
  it("enforces the byte bounds", () => {
    expect(passwordError("")).not.toBeNull();
    expect(passwordError("1234567")).not.toBeNull();
    expect(passwordError("12345678")).toBeNull();
    expect(passwordError("x".repeat(128))).toBeNull();
    expect(passwordError("x".repeat(129))).not.toBeNull();
    // 43 CJK chars = 129 UTF-8 bytes: within 128 characters but over the limit.
    expect(utf8Length("密".repeat(43))).toBe(129);
    expect(passwordError("密".repeat(43))).not.toBeNull();
    // 21 chars was rejected by the old 8-20 reset form; the backend accepts it.
    expect(passwordError("p".repeat(21))).toBeNull();
  });
});
