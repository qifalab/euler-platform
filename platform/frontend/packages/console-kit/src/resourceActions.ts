/**
 * Lifecycle actions on a provisioned resource (02§7.3): the rules
 * svc-orchestrator enforces, mirrored so a detail page only offers what the
 * backend accepts.
 *
 * POST /api/v1/orchestrator/resources/{id}/actions and /release run every
 * action through the pkg-go/resource state machine, for ANY product code:
 *  - start: STOPPED only; stop / restart: RUNNING only (409 otherwise);
 *  - upgrade: a RUNNING 按量 (POSTPAID) instance, to an on-sale SKU of the same
 *    product and charge type — a PREPAID instance needs an upgrade order that
 *    collects the price difference (409 Resource.UpgradeRequiresOrder);
 *  - release: RUNNING / STOPPED, plus the dead-end states a customer must be
 *    able to clean up — CREATE_FAILED (provisioning failed), LOCKED (arrears)
 *    and EXPIRED (prepaid term over); transitional states are driven by the
 *    system;
 *  - 续费 shortcut: RUNNING / STOPPED, like the ECS console.
 */

/** Actions the orchestrator executes. */
export type ResourceAction = "stop" | "start" | "restart" | "upgrade" | "release";
/** Buttons an InstanceActions bar can show; 续费 is a shell navigation. */
export type InstanceActionButton = ResourceAction | "renew";

/** The fields of a resource record (GET /resources/{id}) the rules read. */
export interface ActionTarget {
  productCode: string;
  state: string;
  chargeType: string;
  specCode: string;
}

/** States POST /resources/{id}/release accepts. */
export const RELEASABLE_STATES: readonly string[] = ["RUNNING", "STOPPED", "CREATE_FAILED", "LOCKED", "EXPIRED"];

/** Whether `action` is currently allowed for a resource in `state`. */
export function canRunAction(action: InstanceActionButton, state: string, chargeType?: string): boolean {
  switch (action) {
    case "start":
      return state === "STOPPED";
    case "stop":
    case "restart":
      return state === "RUNNING";
    case "upgrade":
      return state === "RUNNING" && chargeType === "POSTPAID";
    case "renew":
      return state === "RUNNING" || state === "STOPPED";
    case "release":
      return RELEASABLE_STATES.includes(state);
    default:
      return false;
  }
}

/** A catalogue SKU (GET /api/v1/catalog/skus?productCode=…). */
export interface CatalogSku {
  skuCode: string;
  productCode: string;
  chargeType: string; // PREPAID | POSTPAID
  specJson: string;
  status: string; // "1" = on sale
}

/** Display label for a catalogue spec (parsed specJson). */
export type SpecLabel = (spec: Record<string, unknown>, sku: CatalogSku) => string;

export interface UpgradeOption {
  skuCode: string;
  label: string;
}

/** Parse a SKU's specJson; {} when it is missing or malformed. */
export function parseSpec(specJson: string): Record<string, unknown> {
  try {
    const v: unknown = JSON.parse(specJson);
    return v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
  } catch {
    return {};
  }
}

/** Fallback label: the spec's key/value pairs, else the SKU code. */
export const defaultSpecLabel: SpecLabel = (spec, sku) => {
  const parts = Object.entries(spec).map(([k, v]) => `${k}=${String(v)}`);
  return parts.length ? parts.join(" · ") : sku.skuCode;
};

/**
 * Upgrade targets the orchestrator would accept: on sale, same product (its
 * SKU code prefix is checked server-side), same charge type as the instance,
 * and not the current spec.
 */
export function upgradeOptions(
  skus: CatalogSku[],
  current: Pick<ActionTarget, "productCode" | "chargeType" | "specCode">,
  label: SpecLabel = defaultSpecLabel,
): UpgradeOption[] {
  return skus
    .filter(
      (s) =>
        s.status === "1" &&
        s.skuCode.startsWith(`${current.productCode}.`) &&
        s.chargeType === current.chargeType &&
        s.skuCode !== current.specCode,
    )
    .map((s) => ({ skuCode: s.skuCode, label: label(parseSpec(s.specJson), s) }));
}
