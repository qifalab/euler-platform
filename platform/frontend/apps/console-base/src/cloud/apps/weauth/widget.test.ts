/**
 * @vitest-environment jsdom
 */
import { afterEach, describe, expect, it, vi } from "vitest";

// app-cloud serves the widget from a Go-embedded directory (/public/weauth/).
// Read the shipped files, not copies, so this suite runs what embedding pages
// receive. Vite refuses ?raw imports outside platform/frontend, and
// console-base has no Node typings, so node:fs is loaded untyped here.
const fsModule = "node:fs";
const { readFileSync } = (await import(/* @vite-ignore */ fsModule)) as {
  readFileSync(path: URL, encoding: "utf8"): string;
};
// A variable base keeps Vite from rewriting new URL(..., import.meta.url)
// into a URL on the jsdom origin.
const testFile = import.meta.url;
const assets = new URL(
  "../../../../../../../../services/app-cloud/internal/apps/weauth/assets/",
  testFile,
);
const widgetHtml = readFileSync(new URL("widget.html", assets), "utf8");
const widgetSource = readFileSync(new URL("widget.js", assets), "utf8");

// The default site ChallengeTimeout (300 s) that /pow/challenge applies.
const WINDOW_MS = 300_000;
const DEVICE_NOW = Date.UTC(2026, 9, 30, 12, 0, 0);

type Job = { seeds: string[]; difficulty: number; expiresAt: number };
type Emitted = { event: string; message?: string };

class FakeWorker {
  static created: FakeWorker[] = [];
  onmessage: ((event: MessageEvent) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  jobs: Job[] = [];
  terminated = false;
  constructor() {
    FakeWorker.created.push(this);
  }
  postMessage(job: Job) {
    this.jobs.push(job);
  }
  terminate() {
    this.terminated = true;
  }
}

// public.go writes expires_at with time.RFC3339 (whole seconds, "Z"); Go's
// HTTP server writes the Date header as an IMF-fixdate.
const rfc3339 = (ms: number) =>
  new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");
const httpDate = (ms: number) => new Date(ms).toUTCString();

function mountWidget(serverNow: number, dateHeader: string | null) {
  const main = /<main>[\s\S]*<\/main>/.exec(widgetHtml);
  if (!main) throw new Error("widget.html has no <main> element");
  document.body.innerHTML = main[0];
  history.replaceState(
    null,
    "",
    "/public/weauth/widget.html?sitekey=wa_public&origin=https%3A%2F%2Fshop.example&action=login",
  );
  const fetchMock = vi.fn(async (_url: URL, _init: RequestInit) => ({
    ok: true,
    status: 200,
    headers: new Headers(dateHeader === null ? {} : { Date: dateHeader }),
    json: async () => ({
      challenge_id: "wac_test",
      difficulty: 4,
      batch_mode: false,
      batch_total: 1,
      expires_at: rfc3339(serverNow + WINDOW_MS),
      challenge: "seed",
    }),
  }));
  const postMessage = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
  vi.stubGlobal("Worker", FakeWorker);
  // A parent other than itself makes the widget post its events, as it does
  // to an embedding page.
  vi.stubGlobal("parent", { postMessage });
  new Function(widgetSource)();
  return {
    fetchMock,
    run: document.getElementById("run") as HTMLButtonElement,
    events: () => postMessage.mock.calls.map(([data]) => data as Emitted),
  };
}

describe("WeAuth widget challenge window", () => {
  afterEach(() => {
    vi.clearAllTimers();
    vi.useRealTimers();
    vi.unstubAllGlobals();
    FakeWorker.created = [];
    document.body.innerHTML = "";
  });

  it.each([
    {
      case: "device clock 10 minutes ahead of the server",
      skew: 600_000,
      header: httpDate,
    },
    {
      case: "device clock 10 minutes behind the server",
      skew: -600_000,
      header: httpDate,
    },
    {
      case: "no Date header, device clock used",
      skew: 0,
      header: () => null,
    },
    {
      case: "unparsable Date header, device clock used",
      skew: 0,
      header: () => "soon",
    },
  ])(
    "gives the worker the server's 5-minute window: $case",
    async ({ skew, header }) => {
      vi.useFakeTimers({
        now: DEVICE_NOW,
        toFake: ["setTimeout", "clearTimeout", "Date"],
      });
      const serverNow = DEVICE_NOW - skew;
      const { fetchMock, run, events } = mountWidget(
        serverNow,
        header(serverNow),
      );
      run.click();
      await vi.advanceTimersByTimeAsync(0);

      // Same-origin request, so the response's Date header is readable.
      expect(String(fetchMock.mock.calls[0]?.[0])).toBe(
        new URL("/public/weauth/pow/challenge", location.href).href,
      );
      const worker = FakeWorker.created[0];
      expect(worker?.jobs).toHaveLength(1);
      // The deadline is on the device clock, one window from now.
      const left = worker.jobs[0].expiresAt - Date.now();
      expect.soft(left).toBeGreaterThan(WINDOW_MS - 2_000);
      expect.soft(left).toBeLessThanOrEqual(WINDOW_MS);

      // No early timeout, and the widget still gives up when the window ends.
      await vi.advanceTimersByTimeAsync(WINDOW_MS - 1_000);
      expect.soft(events().map((e) => e.event)).not.toContain("error");
      expect.soft(worker.terminated).toBe(false);
      await vi.advanceTimersByTimeAsync(2_000);
      expect(events()).toContainEqual(
        expect.objectContaining({ event: "error", message: "验证超时，请重试" }),
      );
      expect(worker.terminated).toBe(true);
    },
  );
});
