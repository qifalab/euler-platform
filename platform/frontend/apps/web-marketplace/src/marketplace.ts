/** Shared marketplace client + DTO types (svc-marketplace :9212). */
import { createSDK } from "@eu/sdk";

export const sdk = createSDK({ baseURL: "" });

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

export function fetchListings(status?: string, category?: string): Promise<ListingDTO[]> {
  const q = new URLSearchParams();
  if (status) q.set("status", status);
  if (category) q.set("category", category);
  const suffix = q.toString() ? `?${q.toString()}` : "";
  return sdk
    .get<{ listings: ListingDTO[] }>(`/api/v1/marketplace/listings${suffix}`)
    .then((res) => res.data?.listings ?? []);
}
