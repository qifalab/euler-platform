/** 工单枚举 meta composable。
 *  数据源 svc-ticket GET /api/v1/tickets/meta;模块级缓存,多视图共享一次请求。 */
import { ref, type Ref } from "vue";
import { createSDK } from "@sc/sdk";

const sdk = createSDK({ baseURL: "" });

export interface TicketOption { value: string; label: string; }

interface TicketMeta {
  categories?: TicketOption[];
  priorities?: TicketOption[];
  statuses?: TicketOption[];
}

// 模块级缓存:结果 + 进行中的请求(并发去重)。
let cachedMeta: Required<TicketMeta> | null = null;
let pending: Promise<Required<TicketMeta>> | null = null;

// 全局共享的三组枚举,所有调用方引用同一份。
const sharedCategories = ref<TicketOption[]>([]);
const sharedPriorities = ref<TicketOption[]>([]);
const sharedStatuses = ref<TicketOption[]>([]);

function loadMeta(): Promise<Required<TicketMeta>> {
  if (cachedMeta) return Promise.resolve(cachedMeta);
  if (!pending) {
    pending = sdk
      .get<TicketMeta>("/api/v1/tickets/meta")
      .then((res) => {
        const m = res.data ?? {};
        cachedMeta = {
          categories: m.categories ?? [],
          priorities: m.priorities ?? [],
          statuses: m.statuses ?? [],
        };
        sharedCategories.value = cachedMeta.categories;
        sharedPriorities.value = cachedMeta.priorities;
        sharedStatuses.value = cachedMeta.statuses;
        return cachedMeta;
      })
      .finally(() => {
        pending = null;
      });
  }
  return pending;
}

export function useTicketMeta() {
  const loading = ref(!cachedMeta);
  const error = ref<string | null>(null);
  const categories: Ref<TicketOption[]> = sharedCategories;
  const priorities: Ref<TicketOption[]> = sharedPriorities;
  const statuses: Ref<TicketOption[]> = sharedStatuses;

  /** 枚举就绪(或失败降级)后 resolve,供视图对齐表单初值/先渲染后拉列表。 */
  const ready = loadMeta()
    .catch((e) => {
      error.value = (e as Error).message;
    })
    .then(() => {
      loading.value = false;
    });

  return { categories, priorities, statuses, loading, error, ready };
}
