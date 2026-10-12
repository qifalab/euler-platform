/**
 * VPC CIDR rules — the client mirror of svc-orchestrator's euvpc `cidrBlock`
 * validation (fulfil params): an RFC1918 private IPv4 block with a /16–/28
 * prefix that does not overlap any of the account's VPCs in the same region.
 * The orchestrator stays authoritative (400 Resource.InvalidAttribute / 409 on
 * overlap); checking here keeps a paid order from failing at fulfil.
 */

export interface ParsedCidr {
  /** Network address as an unsigned 32-bit integer (host bits cleared). */
  network: number;
  /** The address as written (host bits possibly set). */
  address: number;
  prefix: number;
}

export const VPC_PREFIX_MIN = 16;
export const VPC_PREFIX_MAX = 28;
/** RFC1918 private ranges. */
export const PRIVATE_CIDRS = ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"] as const;

function maskOf(prefix: number): number {
  return prefix === 0 ? 0 : (0xffffffff << (32 - prefix)) >>> 0;
}

function parseIPv4(s: string): number | null {
  const parts = s.split(".");
  if (parts.length !== 4) return null;
  let n = 0;
  for (const p of parts) {
    // Decimal octets only, no leading zeros ("010" reads as octal elsewhere).
    if (!/^(0|[1-9][0-9]{0,2})$/.test(p)) return null;
    const v = Number(p);
    if (v > 255) return null;
    n = n * 256 + v;
  }
  return n >>> 0;
}

/** Render an unsigned 32-bit address as dotted quad. */
export function formatIPv4(n: number): string {
  return [24, 16, 8, 0].map((s) => (n >>> s) & 255).join(".");
}

/** Parse "a.b.c.d/n"; null when it is not an IPv4 CIDR. */
export function parseCidr(text: string): ParsedCidr | null {
  const m = /^([0-9.]+)\/([0-9]{1,2})$/.exec(text.trim());
  if (!m) return null;
  const address = parseIPv4(m[1]);
  const prefix = Number(m[2]);
  if (address === null || prefix > 32 || (m[2].length > 1 && m[2].startsWith("0"))) return null;
  return { address, network: (address & maskOf(prefix)) >>> 0, prefix };
}

/** Canonical "network/prefix" text of a parsed block. */
export function formatCidr(c: Pick<ParsedCidr, "network" | "prefix">): string {
  return `${formatIPv4(c.network)}/${c.prefix}`;
}

/** Whether `inner` lies entirely within `outer`. */
export function cidrContains(outer: ParsedCidr, inner: ParsedCidr): boolean {
  return inner.prefix >= outer.prefix && ((inner.network & maskOf(outer.prefix)) >>> 0) === outer.network;
}

/** Whether two blocks share at least one address. */
export function cidrsOverlap(a: ParsedCidr, b: ParsedCidr): boolean {
  const m = maskOf(Math.min(a.prefix, b.prefix));
  return ((a.network & m) >>> 0) === ((b.network & m) >>> 0);
}

/** Whether a block lies inside one of the RFC1918 private ranges. */
export function isPrivateCidr(c: ParsedCidr): boolean {
  return PRIVATE_CIDRS.some((p) => cidrContains(parseCidr(p)!, c));
}

/**
 * The first rule `text` breaks as a user-facing message, or null when the
 * orchestrator would accept it. `existing` are the CIDR blocks of the
 * account's VPCs in the target region (unparsable entries are ignored).
 */
export function validateVpcCidr(text: string, existing: readonly string[] = []): string | null {
  const raw = text.trim();
  if (!raw) return "请输入 IPv4 网段,如 10.0.0.0/16";
  const c = parseCidr(raw);
  if (!c) return "网段格式不正确,应为 IPv4 CIDR,如 10.0.0.0/16";
  if (c.prefix < VPC_PREFIX_MIN || c.prefix > VPC_PREFIX_MAX) {
    return `掩码长度需在 /${VPC_PREFIX_MIN} ~ /${VPC_PREFIX_MAX} 之间`;
  }
  if (!isPrivateCidr(c)) return `仅支持私网网段:${PRIVATE_CIDRS.join("、")}`;
  if (c.address !== c.network) return `网段地址需与掩码对齐,应为 ${formatCidr(c)}`;
  for (const other of existing) {
    const o = parseCidr(other);
    if (o && cidrsOverlap(c, o)) return `与该地域已有专有网络网段 ${other.trim()} 重叠`;
  }
  return null;
}
