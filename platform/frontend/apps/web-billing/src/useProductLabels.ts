/** 产品编码 → 展示名 composable。
 *  数据源 svc-catalog GET /api/v1/catalog/products;模块级缓存,多视图共享一次请求。 */
import { ref, type Ref } from "vue";
import { createSDK } from "@eu/sdk";

const sdk = createSDK({ baseURL: "" });

/** svc-catalog 商品条目(仅声明映射所需字段,其余字段透传忽略)。 */
export interface CatalogProduct {
  productCode: string;
  productName: string;
  category?: string;
  description?: string;
}

// 模块级缓存:结果 + 进行中的请求(并发去重)。
let cachedLabels: Record<string, string> | null = null;
let pending: Promise<Record<string, string>> | null = null;

// 全局共享的 labels,所有调用方引用同一份,加载完成后视图响应式更新。
const sharedLabels = ref<Record<string, string>>({});

function loadLabels(): Promise<Record<string, string>> {
  if (cachedLabels) return Promise.resolve(cachedLabels);
  if (!pending) {
    pending = sdk
      .get<CatalogProduct[]>("/api/v1/catalog/products")
      .then((res) => {
        const map: Record<string, string> = {};
        for (const p of res.data ?? []) map[p.productCode] = p.productName;
        cachedLabels = map;
        sharedLabels.value = map;
        return map;
      })
      .finally(() => {
        pending = null;
      });
  }
  return pending;
}

export function useProductLabels() {
  const loading = ref(!cachedLabels);
  const error = ref<string | null>(null);
  const labels: Ref<Record<string, string>> = sharedLabels;

  /** 缺失时返回 code 原样,保证表格永不出现空白。 */
  function label(code: string): string {
    return labels.value[code] ?? code;
  }

  if (!cachedLabels) {
    loadLabels()
      .catch((e) => {
        error.value = (e as Error).message;
      })
      .finally(() => {
        loading.value = false;
      });
  }

  return { labels, label, loading, error };
}
