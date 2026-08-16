/**
 * @vitest-environment jsdom
 *
 * Tests for the useResourceTable composable (02§7.2).
 *
 * The composable registers onMounted / onUnmounted hooks, so we exercise it
 * through a minimal host component mounted with @vue/test-utils. Real timers
 * are replaced with vi.useFakeTimers() so polling intervals can be advanced
 * deterministically without real wall-clock waits.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { defineComponent, h, nextTick } from "vue";
import { mount, flushPromises } from "@vue/test-utils";
import { useResourceTable, type ResourceTableOptions } from "./useResourceTable";

type Row = { id: string; name: string };

/** Build opts with a vi.fn api that resolves to the given items. */
function makeOpts(
  api: ResourceTableOptions<Row>["api"],
  overrides: Partial<ResourceTableOptions<Row>> = {},
): ResourceTableOptions<Row> {
  return {
    api,
    columns: [
      { key: "id", title: "ID" },
      { key: "name", title: "Name" },
    ],
    pageSize: 20,
    ...overrides,
  };
}

/** A deferrable api: resolves with `payload` only once `release()` is called. */
function deferrableApi(payload: { items: Row[]; total?: number }) {
  let release!: () => void;
  const inFlight = vi.fn(
    () =>
      new Promise<{ items: Row[]; total?: number }>((resolve) => {
        release = () => resolve(payload);
      }),
  );
  return {
    api: inFlight as unknown as ResourceTableOptions<Row>["api"],
    release: () => release(),
  };
}

/** Mount a host component whose setup runs the composable and returns it. */
function mountWith(opts: ResourceTableOptions<Row>) {
  let captured!: ReturnType<typeof useResourceTable<Row>>;
  const Host = defineComponent({
    setup() {
      captured = useResourceTable<Row>(opts);
      // surface reactive state in the DOM so wrappers update the render cycle
      return () =>
        h("div", {
          "data-loading": String(captured.loading.value),
          "data-rows": String(captured.rows.value.length),
          "data-page": String(captured.page.value),
          "data-error": captured.error.value ? "1" : "0",
        });
    },
  });
  const wrapper = mount(Host);
  return { wrapper, get r() { return captured; } };
}

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("useResourceTable", () => {
  it("fetchPage calls api and populates rows on mount", async () => {
    const api = vi.fn(async () => ({
      items: [
        { id: "a", name: "Alpha" },
        { id: "b", name: "Beta" },
      ],
      total: 2,
    }));
    const { wrapper, r } = mountWith(makeOpts(api));

    // onMounted triggers fetchPage synchronously; the await is the api call.
    await flushPromises();

    expect(api).toHaveBeenCalledTimes(1);
    expect(api).toHaveBeenCalledWith(
      expect.objectContaining({ page: 1, pageSize: 20 }),
    );
    expect(r.rows.value).toHaveLength(2);
    expect(r.rows.value[0]).toEqual({ id: "a", name: "Alpha" });
    expect(r.total.value).toBe(2);
    // host component reflects the populated rows
    expect(wrapper.attributes("data-rows")).toBe("2");
  });

  it("loading is true while the api is in flight and false after it settles", async () => {
    const { api, release } = deferrableApi({
      items: [{ id: "x", name: "X" }],
      total: 1,
    });
    const { wrapper, r } = mountWith(makeOpts(api));

    // onMounted fired fetchPage synchronously; the promise is unresolved,
    // so loading.value is already true. Flush the render so the DOM catches up.
    await nextTick();
    expect(r.loading.value).toBe(true);
    expect(wrapper.attributes("data-loading")).toBe("true");

    release();
    await flushPromises();

    expect(r.loading.value).toBe(false);
    expect(wrapper.attributes("data-loading")).toBe("false");
    expect(r.rows.value).toHaveLength(1);
  });

  it("captures the error and clears loading when the api throws", async () => {
    const boom = new Error("boom");
    const api = vi.fn(async () => {
      throw boom;
    });
    const { wrapper, r } = mountWith(makeOpts(api));

    await flushPromises();

    expect(api).toHaveBeenCalledTimes(1);
    expect(r.error.value).toBe(boom);
    expect(r.loading.value).toBe(false);
    // rows never populated
    expect(r.rows.value).toHaveLength(0);
    expect(wrapper.attributes("data-error")).toBe("1");
  });

  it("setPage(p) updates page and re-fetches with the new page index", async () => {
    let lastParams: Record<string, unknown> | undefined;
    const api = vi.fn(async (params: Record<string, unknown>) => {
      lastParams = params;
      return { items: [{ id: String(params.page), name: `page-${params.page}` }] };
    });
    const { r } = mountWith(makeOpts(api));

    await flushPromises();
    expect(api).toHaveBeenCalledTimes(1);
    expect(lastParams).toMatchObject({ page: 1 });

    r.setPage(3);
    expect(r.page.value).toBe(3);
    await flushPromises();

    expect(api).toHaveBeenCalledTimes(2);
    expect(lastParams).toMatchObject({ page: 3, pageSize: 20 });
    expect(r.rows.value[0]).toEqual({ id: "3", name: "page-3" });
  });

  it("does not poll-fetch while polling.when returns false", async () => {
    const api = vi.fn(async () => ({ items: [{ id: "1", name: "one" }] }));
    // when() always false → interval body bails before calling api again
    mountWith(
      makeOpts(api, { polling: { interval: 1000, when: () => false } }),
    );

    await flushPromises(); // initial onMounted fetch
    expect(api).toHaveBeenCalledTimes(1);

    // Advance well past several polling intervals.
    vi.advanceTimersByTime(5000);
    await flushPromises();

    // No additional fetches should have happened because when() gated them.
    expect(api).toHaveBeenCalledTimes(1);
  });

  it("stopPolling (via unmount) clears the interval so no further fetches occur", async () => {
    const api = vi.fn(async () => ({ items: [{ id: "1", name: "one" }] }));
    const { wrapper } = mountWith(
      makeOpts(api, { polling: { interval: 1000, when: () => true } }),
    );

    await flushPromises(); // initial fetch
    expect(api).toHaveBeenCalledTimes(1);

    // Unmount triggers onUnmounted(stopPolling).
    wrapper.unmount();

    // Advancing past multiple intervals must not call api again.
    vi.advanceTimersByTime(5000);
    await flushPromises();
    expect(api).toHaveBeenCalledTimes(1);
  });

  it("polls and re-fetches when the interval elapses and when() is true", async () => {
    const api = vi.fn(async () => ({ items: [{ id: "1", name: "one" }] }));
    mountWith(
      makeOpts(api, { polling: { interval: 1000, when: () => true } }),
    );

    await flushPromises(); // initial onMounted fetch
    expect(api).toHaveBeenCalledTimes(1);

    // One polling tick — when() is true and document is visible.
    vi.advanceTimersByTime(1000);
    await flushPromises();
    expect(api).toHaveBeenCalledTimes(2);

    // A second tick.
    vi.advanceTimersByTime(1000);
    await flushPromises();
    expect(api).toHaveBeenCalledTimes(3);
  });
});
