/**
 * @sc/console-kit — console framework entry (02§7).
 * Fill-in-the-blank building blocks: every product console sub-app is built on this.
 * Components that depend on the Wujie bridge live here (console-framework concerns).
 */
import ResourceTable from "./ResourceTable.vue";
import RegionSelector from "./RegionSelector.vue";
import AppErrorBoundary from "./AppErrorBoundary.vue";
export { useResourceTable } from "./useResourceTable";
export { ResourceTable, RegionSelector, AppErrorBoundary };
export type { Column, ResourceTableOptions } from "./useResourceTable";

import type { App } from "vue";
const components = { ResourceTable, RegionSelector, AppErrorBoundary };
export const ConsoleKit = {
  install(app: App) {
    for (const [name, comp] of Object.entries(components)) {
      app.component(name, comp);
    }
  },
};
export default ConsoleKit;
