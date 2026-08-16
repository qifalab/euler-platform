/**
 * Region store (02§7.1) — single source of truth for the current regionId.
 * Switching broadcasts `region:changed` on the Wujie bus so every active
 * sub-app can re-fetch; sub-apps read the latest value via shared props
 * (getRegion) or the bridge event payload.
 */
import { defineStore } from "pinia";
import { bus } from "wujie";

export const useRegionStore = defineStore("region", {
  state: () => ({
    regionId: "cn-north-1" as string,
  }),
  actions: {
    setRegion(id: string) {
      if (!id || id === this.regionId) return;
      this.regionId = id;
      // Broadcast on the real Wujie bus (the base is not inside a sandbox,
      // so the bridge facade's local fallback would not reach sub-apps).
      bus.$emit("region:changed", { regionId: id });
    },
  },
});
