/**
 * @vitest-environment node
 *
 * VPC CIDR rules: RFC1918, /16–/28, aligned, no overlap with the region's VPCs
 * (the orchestrator's euvpc cidrBlock contract).
 */
import { describe, it, expect } from "vitest";
import { cidrsOverlap, formatCidr, isPrivateCidr, parseCidr, validateVpcCidr } from "./cidr";

describe("parseCidr", () => {
  it("parses dotted-quad/prefix and clears host bits in network", () => {
    const c = parseCidr(" 10.1.2.3/16 ")!;
    expect(c.prefix).toBe(16);
    expect(formatCidr(c)).toBe("10.1.0.0/16");
    expect(c.address).not.toBe(c.network);
  });

  it("rejects malformed input", () => {
    for (const s of ["", "10.0.0.0", "10.0.0/16", "10.0.0.256/16", "10.0.0.0/33", "10.00.0.0/16", "a.b.c.d/8", "10.0.0.0/016", "10.0.0.0/-1"]) {
      expect(parseCidr(s)).toBeNull();
    }
  });
});

describe("cidr relations", () => {
  it("overlap is symmetric and prefix-aware", () => {
    const a = parseCidr("10.0.0.0/16")!;
    expect(cidrsOverlap(a, parseCidr("10.0.128.0/24")!)).toBe(true);
    expect(cidrsOverlap(parseCidr("10.0.128.0/24")!, a)).toBe(true);
    expect(cidrsOverlap(a, parseCidr("10.1.0.0/16")!)).toBe(false);
    expect(cidrsOverlap(parseCidr("10.0.0.0/8")!, parseCidr("10.200.0.0/16")!)).toBe(true);
  });

  it("knows the RFC1918 ranges", () => {
    expect(isPrivateCidr(parseCidr("172.31.0.0/16")!)).toBe(true);
    expect(isPrivateCidr(parseCidr("172.32.0.0/16")!)).toBe(false);
    expect(isPrivateCidr(parseCidr("192.168.10.0/24")!)).toBe(true);
    expect(isPrivateCidr(parseCidr("11.0.0.0/16")!)).toBe(false);
  });
});

describe("validateVpcCidr", () => {
  it("accepts private /16–/28 blocks", () => {
    expect(validateVpcCidr("10.0.0.0/16")).toBeNull();
    expect(validateVpcCidr("172.16.0.0/20")).toBeNull();
    expect(validateVpcCidr("192.168.1.0/28")).toBeNull();
  });

  it("enforces the prefix range", () => {
    expect(validateVpcCidr("10.0.0.0/8")).toContain("/16 ~ /28");
    expect(validateVpcCidr("10.0.0.0/15")).toContain("/16 ~ /28");
    expect(validateVpcCidr("10.0.0.0/29")).toContain("/16 ~ /28");
  });

  it("rejects public, malformed, empty and unaligned blocks", () => {
    expect(validateVpcCidr("8.8.0.0/16")).toContain("私网");
    expect(validateVpcCidr("172.15.0.0/16")).toContain("私网");
    expect(validateVpcCidr("10.0.0/16")).toContain("格式");
    expect(validateVpcCidr("  ")).toContain("请输入");
    expect(validateVpcCidr("10.0.1.0/16")).toContain("10.0.0.0/16");
  });

  it("rejects overlap with the region's existing VPCs, ignoring junk entries", () => {
    const existing = ["10.0.0.0/16", "not-a-cidr", "192.168.0.0/24"];
    expect(validateVpcCidr("10.0.4.0/24", existing)).toContain("10.0.0.0/16");
    expect(validateVpcCidr("192.168.0.0/16", existing)).toContain("192.168.0.0/24");
    expect(validateVpcCidr("10.1.0.0/16", existing)).toBeNull();
  });
});
