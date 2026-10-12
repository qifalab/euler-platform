/**
 * useCatalogMeta — catalogue-driven purchase metadata (02§7, M-6).
 * Regions / zones / images / placement come from svc-catalog so no client
 * hardcodes a region list: opening a new region is a catalogue row, not a
 * frontend release. The payload carries no tenant data, but the console
 * gateway requires a JWT on /api/v1/catalog/*, so every call goes through an
 * auth-wired @eu/sdk client (a bare fetch sent no Authorization and 401'd).
 */
import { ref } from "vue";
import { createSDK, type SDK } from "@eu/sdk";
import { subAppAuthOptions } from "@eu/wujie-bridge";

export interface CatalogZone {
  zoneId: string;
  zoneName: string;
}
export interface CatalogRegion {
  regionId: string;
  regionName: string;
  zones: CatalogZone[];
}
export interface CatalogImage {
  imageId: string;
  name: string;
  os: string;
  arch: string;
  productCode: string;
  status: string;
}
export interface CatalogPlacement {
  productCode: string;
  regionScope: string;
  zonal: boolean;
  crossAz: boolean;
  zoneRequired: boolean;
}

const CATALOG_BASE = "/api/v1/catalog";

/** The part of an @eu/sdk client the catalogue calls need. */
export type CatalogClient = Pick<SDK, "get">;

let client: CatalogClient | null = null;

// Module-level region cache: the top-bar selector and every BuyWizard in one
// app share a single /regions round-trip; a failure clears it so a retry refetches.
let regionsCache: Promise<CatalogRegion[]> | null = null;

/**
 * Inject the SDK the catalogue calls use. Sub-apps need not call this: the
 * default client reads the shell's live token through the Wujie bridge
 * (subAppAuthOptions). The shell itself is not inside the sandbox, so it must
 * pass its own auth-store-wired SDK here before anything (RegionSelector)
 * loads metadata. Passing null restores the default.
 */
export function setCatalogClient(sdk: CatalogClient | null): void {
  client = sdk;
  regionsCache = null;
}

function catalogClient(): CatalogClient {
  if (!client) client = createSDK({ baseURL: "", ...subAppAuthOptions() });
  return client;
}

async function getJSON<T>(path: string): Promise<T> {
  // The SDK attaches the bearer token, replays once after a 401 refresh,
  // unwraps the {Code, Data} envelope and throws on Code != OK.
  const res = await catalogClient().get<T>(path);
  return res.data;
}

export function fetchRegions(): Promise<CatalogRegion[]> {
  if (!regionsCache) {
    regionsCache = getJSON<CatalogRegion[]>(`${CATALOG_BASE}/regions`).catch((e) => {
      regionsCache = null;
      throw e;
    });
  }
  return regionsCache;
}

export function fetchImages(productCode: string): Promise<CatalogImage[]> {
  return getJSON<CatalogImage[]>(`${CATALOG_BASE}/images?productCode=${encodeURIComponent(productCode)}`);
}

export function fetchPlacement(productCode: string): Promise<CatalogPlacement> {
  return getJSON<CatalogPlacement>(`${CATALOG_BASE}/placement?productCode=${encodeURIComponent(productCode)}`);
}

export interface UseCatalogMetaOptions {
  /** Load the boot image list for this product (compute products only). */
  withImages?: boolean;
}

export function useCatalogMeta(productCode?: string, opts: UseCatalogMetaOptions = {}) {
  const regions = ref<CatalogRegion[]>([]);
  const images = ref<CatalogImage[]>([]);
  const placement = ref<CatalogPlacement | null>(null);
  const loading = ref(false);
  const error = ref<string | null>(null);

  async function load(loadOpts: UseCatalogMetaOptions = opts): Promise<void> {
    loading.value = true;
    error.value = null;
    const jobs: Array<Promise<void>> = [
      fetchRegions()
        .then((rs) => {
          regions.value = rs;
        })
        .catch((e) => {
          throw e;
        }),
    ];
    if (productCode) {
      jobs.push(
        fetchPlacement(productCode)
          .then((p) => {
            placement.value = p;
          })
          .catch(() => undefined),
      );
      if (loadOpts.withImages) {
        jobs.push(
          fetchImages(productCode)
            .then((ims) => {
              images.value = ims;
            })
            .catch(() => undefined),
        );
      }
    }
    try {
      await Promise.all(jobs);
    } catch (e) {
      error.value = (e as Error).message || "地域元数据加载失败";
    } finally {
      loading.value = false;
    }
  }

  function zonesOf(regionId: string): CatalogZone[] {
    return regions.value.find((r) => r.regionId === regionId)?.zones ?? [];
  }

  return { regions, images, placement, loading, error, load, zonesOf };
}
