/**
 * @sc/ui — design-system entry (02§10.2).
 * Thin-wrap Element Plus (do not change default semantics) + pure display
 * business components. Components that depend on the Wujie bridge live in
 * @sc/console-kit (they are console-framework concerns, not pure UI).
 */
import StatusBadge from "./StatusBadge.vue";
import EmptyGuide from "./EmptyGuide.vue";
import PriceText from "./PriceText.vue";
import PageHeader from "./PageHeader.vue";
import SiteTopbar from "./SiteTopbar.vue";

export { StatusBadge, EmptyGuide, PriceText, PageHeader, SiteTopbar };

import type { App } from "vue";
const components = { StatusBadge, EmptyGuide, PriceText, PageHeader, SiteTopbar };
export const ScUI = {
  install(app: App) {
    for (const [name, comp] of Object.entries(components)) {
      app.component(name, comp);
    }
  },
};
export default ScUI;
