/**
 * @eu/console-kit — console framework entry (02§7).
 * Fill-in-the-blank building blocks: every product console sub-app is built on this.
 * Components that depend on the Wujie bridge live here (console-framework concerns).
 */
import ResourceTable from "./ResourceTable.vue";
import RegionSelector from "./RegionSelector.vue";
import AppErrorBoundary from "./AppErrorBoundary.vue";
import InstanceListView from "./InstanceListView.vue";
import InstanceActions from "./InstanceActions.vue";
import AttributeEditDialog from "./AttributeEditDialog.vue";
import CouponPicker from "./CouponPicker.vue";
import OrderAmountSummary from "./OrderAmountSummary.vue";
export { useResourceTable } from "./useResourceTable";
export { useCatalogMeta, fetchRegions, fetchImages, fetchPlacement, setCatalogClient } from "./useCatalogMeta";
export { formatTime, formatDate, EMPTY_TIME } from "./format";
export { paginate, isTransitionalState, TRANSITIONAL_STATES, resourceZone, resourceDisplayName } from "./listing";
export { navigateShell, routesInApp } from "./navigation";
export { canRunAction, upgradeOptions, parseSpec, defaultSpecLabel, RELEASABLE_STATES } from "./resourceActions";
export {
  attributesOf,
  numberAttr,
  toParams,
  validateResourceName,
  validateIntRange,
  attributeErrorMessage,
  patchResourceAttributes,
  NAME_MAX,
} from "./resourceAttributes";
export type { ResourceAttributes } from "./resourceAttributes";
export {
  parseCidr,
  formatCidr,
  cidrsOverlap,
  isPrivateCidr,
  validateVpcCidr,
  PRIVATE_CIDRS,
  VPC_PREFIX_MIN,
  VPC_PREFIX_MAX,
} from "./cidr";
export type { ParsedCidr } from "./cidr";
export type {
  ResourceAction,
  InstanceActionButton,
  ActionTarget,
  CatalogSku,
  SpecLabel,
  UpgradeOption,
} from "./resourceActions";
export {
  useOrderCheckout,
  fulfilledMessage,
  orderAmountText,
  payIsFinal,
  fulfilIsFinal,
  fulfilParamsRejected,
  QUOTA_EXCEEDED_MESSAGE,
} from "./useOrderCheckout";
export type { CheckoutOrder, FulfilResult, PendingCheckout, OrderCheckoutOptions, OrderExtras } from "./useOrderCheckout";
export {
  fetchCoupons,
  couponQueryString,
  chargeTypeTakesCoupons,
  couponKindLabel,
  couponStatusLabel,
  couponSourceLabel,
  couponValueText,
  couponScopeText,
  couponExpiresSoon,
  couponRejectReason,
  toggleCoupon,
  pruneSelection,
  formatYuan,
  formatDiscountRate,
  isCouponInvalidError,
  isCouponExpiredError,
  MAX_COUPONS_PER_ORDER,
  COUPON_ID_PATTERN,
} from "./coupons";
export type {
  Coupon,
  CouponKind,
  CouponStatus,
  CouponStatusFilter,
  CouponContext,
  CouponQuery,
  CouponListData,
} from "./coupons";
export { useCoupons } from "./useCoupons";
export type { UseCouponsOptions } from "./useCoupons";
export {
  ResourceTable,
  RegionSelector,
  AppErrorBoundary,
  InstanceListView,
  InstanceActions,
  AttributeEditDialog,
  CouponPicker,
  OrderAmountSummary,
};
export type { AttributeField } from "./AttributeEditDialog.vue";
export type { Column, ResourceTableOptions } from "./useResourceTable";
export type { InstanceListConfig } from "./InstanceListView.vue";
export type {
  CatalogRegion,
  CatalogZone,
  CatalogImage,
  CatalogPlacement,
  UseCatalogMetaOptions,
  CatalogClient,
} from "./useCatalogMeta";

import type { App } from "vue";
const components = {
  ResourceTable, RegionSelector, AppErrorBoundary, InstanceListView, InstanceActions, AttributeEditDialog,
  CouponPicker, OrderAmountSummary,
};
export const ConsoleKit = {
  install(app: App) {
    for (const [name, comp] of Object.entries(components)) {
      app.component(name, comp);
    }
  },
};
export default ConsoleKit;
