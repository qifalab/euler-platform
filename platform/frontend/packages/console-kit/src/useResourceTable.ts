/**
 * useResourceTable — declarative list-page model (02§7.2).
 * List pages are 80% of console work; this composable owns fetch, filters,
 * polling (with visibility-based backoff), and selection state.
 */
import { ref, computed, onMounted, onUnmounted, watch, type Ref } from "vue";

export interface Column<T = Record<string, unknown>> {
  key: string;
  title: string;
  /** Render fn receives the row; return string or a render-hint. */
  render?: (row: T) => unknown;
  copyable?: boolean;
  link?: (row: T) => string;
}

export interface ResourceTableOptions<T> {
  /** Fetcher — returns the page of rows. `signal` aborts superseded requests. */
  api: (params: Record<string, unknown>, signal?: AbortSignal) => Promise<{ items: T[]; total?: number }>;
  columns: Column<T>[];
  filters?: Record<string, unknown>;
  /** Polling interval, ms. 0 = off. */
  polling?: { interval: number; when?: (rows: T[]) => boolean };
  pageSize?: number;
}

export function useResourceTable<T = Record<string, unknown>>(opts: ResourceTableOptions<T>) {
  const rows = ref<T[]>([]) as unknown as Ref<T[]>;
  const loading = ref(false);
  /** Last fetch failure — list pages MUST render this instead of "暂无资源". */
  const error = ref<unknown>(null);
  const total = ref(0);
  const page = ref(1);
  const pageSize = ref(opts.pageSize ?? 20);
  const selectedKeys = ref<Array<string | number>>([]);

  let pollTimer: ReturnType<typeof setInterval> | null = null;
  // Stale-response guard: each fetch bumps the seq and aborts the previous
  // in-flight request, so a slow old response can never overwrite fresh rows.
  let fetchSeq = 0;
  let abortCtrl: AbortController | null = null;

  async function fetchPage() {
    const seq = ++fetchSeq;
    abortCtrl?.abort();
    abortCtrl = new AbortController();
    const signal = abortCtrl.signal;
    loading.value = true;
    error.value = null;
    try {
      const params = {
        ...(opts.filters ?? {}),
        page: page.value,
        pageSize: pageSize.value,
      };
      const result = await opts.api(params, signal);
      if (seq !== fetchSeq) return; // superseded — drop the stale response
      rows.value = result.items;
      total.value = result.total ?? result.items.length;
    } catch (e) {
      if (seq !== fetchSeq || signal.aborted) return; // aborted/stale — ignore
      error.value = e;
    } finally {
      if (seq === fetchSeq) loading.value = false;
    }
  }

  function startPolling() {
    stopPolling();
    const polling = opts.polling;
    if (!polling?.interval) return;
    pollTimer = setInterval(async () => {
      // Stop when tab hidden (02§7.2).
      if (document.hidden) return;
      // Only poll while transitional states exist (02§7.2).
      if (polling.when && !polling.when(rows.value)) return;
      await fetchPage();
    }, polling.interval);
  }

  function stopPolling() {
    if (pollTimer) {
      clearInterval(pollTimer);
      pollTimer = null;
    }
  }

  onMounted(() => {
    fetchPage();
    startPolling();
  });

  onUnmounted(() => {
    stopPolling();
    abortCtrl?.abort();
  });

  watch(() => opts.filters, () => {
    page.value = 1;
    fetchPage();
  }, { deep: true });

  return {
    rows,
    loading,
    error,
    total,
    page,
    pageSize,
    selectedKeys,
    columns: computed(() => opts.columns),
    refresh: fetchPage,
    setPage: (p: number) => {
      page.value = p;
      fetchPage();
    },
  };
}
