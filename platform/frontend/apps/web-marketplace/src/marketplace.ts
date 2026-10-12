/** Shared marketplace client + DTO types (svc-marketplace :9212). */
import type { EuError } from "@eu/sdk";
import { bridge } from "@eu/wujie-bridge";
import { sdk } from "./sdk";

// Auth-wired client (see ./sdk); re-exported for the views.
export { sdk };

// Listing DTO from svc-marketplace (camelCase JSON tags in main.go).
export interface ListingDTO {
  listingId: number;
  partnerId: number;
  name: string;
  category: "IMAGE" | "SAAS" | "SERVICE";
  openapiUrl: string;
  status: "DRAFT" | "PENDING_APPROVAL" | "APPROVED" | "REJECTED" | "OFF_SHELF";
  partnerRateBps: number;
  createdAt: string;
  updatedAt: string;
}

export interface SettlementDTO {
  settlementId: number;
  orderId: string;
  listingId: number;
  partnerId: number;
  gross: number;      // micro-units (1 yuan = 1e6)
  partnerRateBps: number;
  partner: number;    // micro-units
  platform: number;   // micro-units
  settledAt: string;
}

export const CATEGORIES: { value: string; label: string }[] = [
  { value: "", label: "全部" },
  { value: "IMAGE", label: "镜像" },
  { value: "SAAS", label: "SaaS" },
  { value: "SERVICE", label: "服务" },
];

export function categoryLabel(c: string): string {
  return CATEGORIES.find((x) => x.value === c)?.label ?? c;
}

/** bps → "30%" (10000 bps = 100%). */
export function bpsPercent(bps: number): string {
  return `${(bps / 100).toFixed(bps % 100 === 0 ? 0 : 2)}%`;
}

/** micro-units → "100.00 元". */
export function yuan(micro: number): string {
  return `${(micro / 1e6).toFixed(2)} 元`;
}

/**
 * Render an RFC3339 timestamp as a calendar date in the viewer's locale and
 * time zone (slicing the string showed the UTC day — wrong near midnight).
 */
export function formatDate(
  iso: string | null | undefined,
  opts: { locale?: string; timeZone?: string } = {},
): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleDateString(opts.locale, {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    timeZone: opts.timeZone,
  });
}

export function fetchListings(status?: string, category?: string): Promise<ListingDTO[]> {
  const q = new URLSearchParams();
  if (status) q.set("status", status);
  if (category) q.set("category", category);
  const suffix = q.toString() ? `?${q.toString()}` : "";
  return sdk
    .get<{ listings: ListingDTO[] }>(`/api/v1/marketplace/listings${suffix}`)
    .then((res) => res.data?.listings ?? []);
}

/**
 * Whether the signed-in account is a platform operator. Review (上架审核) and
 * settlement (分账) are operator-only in svc-marketplace; partners and tenants
 * get 403 Marketplace.NotOperator. There is no "who am I" endpoint, so probe
 * the operator-only review queue once: a 403 settles it as "not an operator";
 * any other failure (network, 401 before login) is not cached and is re-probed
 * on the next call.
 */
let operatorCheck: Promise<boolean> | null = null;

export function isOperator(): Promise<boolean> {
  if (!operatorCheck) {
    const p: Promise<boolean> = fetchListings("PENDING_APPROVAL").then(
      () => true,
      (e: EuError) => {
        if (e?.status !== 403 && operatorCheck === p) operatorCheck = null;
        return false;
      },
    );
    operatorCheck = p;
  }
  return operatorCheck;
}

// A different principal may be an operator (or not): decide again.
bridge.on("auth:logout", () => { operatorCheck = null; });
bridge.on("auth:switch-account", () => { operatorCheck = null; });
