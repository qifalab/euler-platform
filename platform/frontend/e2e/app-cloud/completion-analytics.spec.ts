import { readFile } from "node:fs/promises";
import { expect, test } from "@playwright/test";
import { origin, workspace } from "./helpers";

test("statistics completion: registered browser events, dated trends, ordered funnel and downloaded CSV", async ({
  page,
  browser,
}, testInfo) => {
  const work = await workspace(page, "日期分析与转化验收", ["statistics"]);
  await page.goto(work.url("statistics"));
  await page.getByRole("button", { name: "＋ 添加站点", exact: true }).click();
  await page.getByLabel("站点名称", { exact: true }).fill("真实分析站点");
  await page.getByLabel("允许采集的域名").fill("127.0.0.1:19390");
  await page.getByRole("button", { name: "保存站点", exact: true }).click();
  await expect(page.getByLabel("当前站点")).toContainText("真实分析站点");
  const base = `${work.base}/apps/statistics`;
  const site = (await (await page.request.get(`${base}/sites`)).json())
    .sites[0];
  const integration = await (
    await page.request.get(`${base}/sites/${site.id}/integration`)
  ).json();

  // Register names through the real management form. No route or API mocking.
  await page
    .getByRole("button", { name: "趋势、事件与漏斗", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "每日访问趋势", exact: true }),
  ).toBeVisible();
  await page
    .getByLabel("允许的事件名")
    .fill("signup_started\nsignup_completed");
  const settingsSaved = page.waitForResponse(
    (response) =>
      response.url().endsWith(`/sites/${site.id}/settings`) &&
      response.request().method() === "PUT",
  );
  await page
    .getByRole("button", { name: "保存并应用留存", exact: true })
    .click();
  expect((await settingsSaved).status()).toBe(200);
  await expect(
    page.getByRole("button", { name: "保存并应用留存", exact: true }),
  ).toBeEnabled();

  // The first actual browser visitor views twice and completes the journey.
  // Repeated completion events count as events but only once in the funnel.
  const firstCollected = page.waitForResponse(
    (response) =>
      response.url() === integration.collectURL && response.status() === 202,
  );
  await page.addScriptTag({ url: integration.trackerURL });
  await firstCollected;
  await page.evaluate(async () => {
    const tracker = window as unknown as {
      eulerTrackPage(): Promise<void>;
      eulerTrackEvent(name: string): Promise<boolean>;
    };
    await tracker.eulerTrackPage();
    await tracker.eulerTrackEvent("signup_started");
    await tracker.eulerTrackEvent("signup_completed");
    await tracker.eulerTrackEvent("signup_completed");
  });

  // A separate anonymous browser receives its own genuine tracker-generated
  // localStorage visitor ID, reaches only the first step, and cannot read reports.
  const visitorContext = await browser.newContext({ baseURL: origin });
  try {
    const visitor = await visitorContext.newPage();
    await visitor.goto(origin);
    const secondCollected = visitor.waitForResponse(
      (response) =>
        response.url() === integration.collectURL && response.status() === 202,
    );
    await visitor.addScriptTag({ url: integration.trackerURL });
    await secondCollected;
    await visitor.evaluate(async () => {
      await (
        window as unknown as { eulerTrackEvent(name: string): Promise<boolean> }
      ).eulerTrackEvent("signup_started");
    });
    expect(
      (
        await visitorContext.request.get(`${base}/sites/${site.id}/analytics`)
      ).status(),
    ).toBe(401);
  } finally {
    await visitorContext.close();
  }

  const timezone = "Asia/Shanghai";
  const formatter = new Intl.DateTimeFormat("en-CA", {
    timeZone: timezone,
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
  });
  const to = formatter.format(new Date());
  const from = formatter.format(new Date(Date.now() - 86_400_000));
  await page.getByLabel("开始日期", { exact: true }).fill(from);
  await page.getByLabel("结束日期", { exact: true }).fill(to);
  await page.getByLabel("统计时区", { exact: true }).fill(timezone);
  const analysisLoaded = page.waitForResponse(
    (response) =>
      response.url().includes(`/sites/${site.id}/analytics?`) &&
      response.request().method() === "GET",
  );
  await page.getByRole("button", { name: "查询分析", exact: true }).click();
  const analysisResponse = await analysisLoaded;
  expect(analysisResponse.status()).toBe(200);
  const analysis = await analysisResponse.json();
  expect(analysis).toMatchObject({
    pageViews: 3,
    uniqueVisitors: 2,
    eventCount: 4,
  });
  expect(analysis.period).toMatchObject({ from, to, timezone });
  await expect(
    page.locator(".analytics .summary .panel").nth(0).locator("strong"),
  ).toHaveText("3");
  await expect(
    page.locator(".analytics .summary .panel").nth(1).locator("strong"),
  ).toHaveText("2");
  await expect(
    page.locator(".analytics .summary .panel").nth(2).locator("strong"),
  ).toHaveText("4");
  await expect(
    page.getByRole("img", {
      name: "每日 PV 与 UV 趋势，下方提供完整数据表",
      exact: true,
    }),
  ).toBeVisible();
  await page.getByText("查看每日数据", { exact: true }).click();
  await expect(page.locator(".analytics details tbody tr")).toHaveCount(2);
  const eventPanel = page.locator(".analytics article").filter({
    has: page.getByRole("heading", { name: "自定义事件", exact: true }),
  });
  await expect(
    eventPanel
      .locator("tbody tr")
      .filter({ hasText: "signup_started" })
      .locator("td"),
  ).toHaveText(["signup_started", "2", "2"]);
  await expect(
    eventPanel
      .locator("tbody tr")
      .filter({ hasText: "signup_completed" })
      .locator("td"),
  ).toHaveText(["signup_completed", "2", "1"]);

  await page
    .getByLabel("第 1 步", { exact: true })
    .selectOption("signup_started");
  await page
    .getByLabel("第 2 步", { exact: true })
    .selectOption("signup_completed");
  await page.getByLabel("转化窗口（分钟）", { exact: true }).fill("60");
  await page.getByRole("button", { name: "计算转化", exact: true }).click();
  await expect(page.locator(".funnel-result .bar-row").nth(0)).toContainText(
    "2 人 · 100.0%",
  );
  await expect(page.locator(".funnel-result .bar-row").nth(1)).toContainText(
    "1 人 · 50.0%",
  );

  const downloading = page.waitForEvent("download");
  await page.getByRole("button", { name: "导出 CSV", exact: true }).click();
  const download = await downloading;
  expect(download.suggestedFilename()).toBe(`statistics-${from}-${to}.csv`);
  const csvPath = testInfo.outputPath("statistics-analytics.csv");
  await download.saveAs(csvPath);
  const csv = await readFile(csvPath, "utf8");
  expect(csv).toContain(
    "kind,site,timezone,from,to,name,count,unique_visitors",
  );
  expect(csv).toContain(
    `event,真实分析站点,${timezone},${from},${to},signup_started,2,2`,
  );
  expect(csv).toContain(
    `event,真实分析站点,${timezone},${from},${to},signup_completed,2,1`,
  );
  expect(csv).toContain(`page,真实分析站点,${timezone},${from},${to},`);
  expect(csv).not.toContain("visitor_hash");
  await expect(
    page.getByText("CSV 下载已开始。", { exact: true }),
  ).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("statistics-analytics.png"),
    fullPage: true,
  });
});
