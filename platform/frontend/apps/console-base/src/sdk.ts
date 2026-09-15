/**
 * Console SDK instance (02§9.1).
 * Singleton SDK wired to the console-bff. Token comes from the auth store
 * (memory-only). In dev the vite proxy forwards /console/* to the BFF and
 * injects the dev account header; in prod the APISIX gateway does that.
 */
import { createSDK, type SDK } from "@eu/sdk";
import { useAuthStore } from "./stores/auth";

let _sdk: SDK | null = null;

export function useSDK(): SDK {
  if (_sdk) return _sdk;
  const auth = useAuthStore();
  _sdk = createSDK({
    baseURL: "",
    getToken: () => auth.accessToken || undefined,
    onUnauthorized: async () => {
      // Single-flight silent refresh (02§5.1). Returns fresh token or undefined.
      return auth.silentRefresh();
    },
    onError: (err) => {
      console.error("[sdk]", err.code, err.message, err.requestId ?? "");
    },
  });
  return _sdk;
}

/** Types matching the console-bff JSON contract (services/console-bff store.go). */
export interface OverviewData {
  AccountId: number;
  Balance: { Available: string; Frozen: string; Total: string };
  ResourceCount: { Total: number };
  PendingOrders: number;
}

export interface ResourceItem {
  ResourceId: string;
  ProductCode: string;
  Region: string;
  ChargeType: string;
  State: string;
  SpecCode: string;
  BillingStart: string;
  ExpiredAt: string;
  CreatedAt: string;
}

export interface BillItem {
  BillPeriod: string;
  TotalAmount: string;
  PaidAmount: string;
  ChargeCount: number;
  IncompleteCharges: number;
  UnreconciledCount: number;
  Final: boolean;
}
