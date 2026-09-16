/**
 * @eu/console-kit — console framework entry (02§7).
 * Fill-in-the-blank building blocks: every product console sub-app is built on this.
 * Components that depend on the Wujie bridge live here (console-framework concerns).
 */
import ResourceTable from "./ResourceTable.vue";
import RegionSelector from "./RegionSelector.vue";
import AppErrorBoundary from "./AppErrorBoundary.vue";
import InstanceListView from "./InstanceListView.vue";
export { useResourceTable } from "./useResourceTable";
export { useCatalogMeta, fetchRegions, fetchImages, fetchPlacement } from "./useCatalogMeta";
export { ResourceTable, RegionSelector, AppErrorBoundary, InstanceListView };
export type { Column, ResourceTableOptions } from "./useResourceTable";
export type { InstanceListConfig } from "./InstanceListView.vue";
export type {
  CatalogRegion,
  CatalogZone,
  CatalogImage,
  CatalogPlacement,
  UseCatalogMetaOptions,
} from "./useCatalogMeta";

import type { App } from "vue";
const components = { ResourceTable, RegionSelector, AppErrorBoundary, InstanceListView };
export const ConsoleKit = {
  install(app: App) {
    for (const [name, comp] of Object.entries(components)) {
      app.component(name, comp);
    }
  },
};
export default ConsoleKit;
