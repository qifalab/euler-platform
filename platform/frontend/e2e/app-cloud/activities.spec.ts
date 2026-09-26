import { expect, test } from "@playwright/test";
import { created, origin, workspace, write } from "./helpers";

test("statistics: site setup, real browser collection, page widget and isolated report", async ({
  page,
}) => {
  const work = await workspace(page, "访问统计验收", ["statistics"]);
  await page.goto(work.url("statistics"));
  await page.getByRole("button", { name: "＋ 添加站点", exact: true }).click();
  await page.getByLabel("站点名称", { exact: true }).fill("真实浏览器站点");
  await page.getByLabel("允许采集的域名").fill("127.0.0.1:19390");
  await page.getByRole("button", { name: "保存站点", exact: true }).click();
  await expect(page.getByLabel("当前站点")).toContainText("真实浏览器站点");
  const base = `${work.base}/apps/statistics`;
  const sites = await (await page.request.get(`${base}/sites`)).json();
  expect(sites.sites).toHaveLength(1);
  const site = sites.sites[0];
  const integration = await (
    await page.request.get(`${base}/sites/${site.id}/integration`)
  ).json();
  await page.getByRole("button", { name: "接入与计数器", exact: true }).click();
  await expect(
    page.getByText("安装轻量采集脚本", { exact: true }),
  ).toBeVisible();
  await expect(page.locator("pre").first()).toContainText(
    integration.trackerURL,
  );
  // Load the actual public script in a real browser: no API interception or mock.
  const collected = page.waitForResponse(
    (r) => r.url() === integration.collectURL && r.status() === 202,
  );
  await page.addScriptTag({ url: integration.trackerURL });
  await collected;
  await page.addScriptTag({ url: integration.widgetURL });
  await expect(page.locator("#page-view-counter")).toHaveText("浏览 1 次");
  await page.addScriptTag({ url: integration.helperURL });
  expect(
    await page.evaluate(async () =>
      (window as unknown as { getPageViews(): Promise<number> }).getPageViews(),
    ),
  ).toBe(1);
  await page.getByRole("button", { name: "访问概览", exact: true }).click();
  await page.getByRole("button", { name: "立即刷新", exact: true }).click();
  await expect(page.locator(".statistics-app tbody tr")).toHaveCount(1);
  await expect(page.locator(".statistics-app tbody")).toContainText(
    "/apps/statistics",
  );
  expect(
    (await (await page.request.get(`${base}/sites/${site.id}/report`)).json())
      .pageViews,
  ).toBe(1);
  const forbidden = await page.request.post(integration.collectURL, {
    headers: { Origin: "https://unrelated.example" },
    data: {
      visitor_id: "untrusted-visitor",
      url: "https://unrelated.example/",
    },
  });
  expect(forbidden.status()).toBe(403);
  await page.getByRole("button", { name: "站点设置", exact: true }).click();
  await page
    .getByRole("combobox", { name: "采集状态", exact: true })
    .selectOption({ label: "暂停采集" });
  await page.getByRole("button", { name: "保存站点", exact: true }).click();
  await expect(page.getByText("已暂停", { exact: true })).toBeVisible();
  expect((await page.request.get(integration.trackerURL)).status()).toBe(404);
});

test("lottery: mobile signup, manual and batch participants, draw history, retry and room reset", async ({
  page,
  browser,
}) => {
  const work = await workspace(page, "现场抽奖验收", ["lottery"]);
  await page.goto(work.url("lottery"));
  await page.getByRole("button", { name: "＋ 新建活动", exact: true }).click();
  await page.getByLabel("活动名称", { exact: true }).fill("九月幸运现场");
  await page.getByLabel("活动说明", { exact: true }).fill("真实报名与抽奖验收");
  await page.getByRole("button", { name: "创建活动", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: /九月幸运现场/ }),
  ).toBeVisible();
  await page.getByRole("button", { name: "报名二维码", exact: true }).click();
  const signupURL = await page
    .getByLabel("报名链接", { exact: true })
    .inputValue();
  await expect(page.getByRole("img", { name: "活动报名二维码" })).toBeVisible();
  const mobile = await browser.newContext({
    baseURL: origin,
    viewport: { width: 390, height: 844 },
    isMobile: true,
  });
  try {
    const signup = await mobile.newPage();
    await signup.goto(signupURL);
    await expect(
      signup.getByRole("heading", { name: "九月幸运现场", exact: true }),
    ).toBeVisible();
    await signup.getByLabel("姓名", { exact: true }).fill("扫码参与者");
    await signup.getByLabel(/部门/).fill("现场来宾");
    await signup.getByRole("button", { name: "确认报名", exact: true }).click();
    await expect(signup.getByRole("status")).toContainText("报名成功");
    expect(
      (await mobile.request.get(`${work.base}/apps/lottery/rooms`)).status(),
    ).toBe(401);
  } finally {
    await mobile.close();
  }
  await page.getByRole("button", { name: "参与者", exact: true }).click();
  await page.getByLabel("姓名", { exact: true }).fill("手工参与者");
  await page.getByLabel("部门（可选）", { exact: true }).fill("产品团队");
  await page.getByRole("button", { name: "添加", exact: true }).click();
  await expect(page.locator(".lottery-app tbody")).toContainText("手工参与者");
  await page.getByLabel("生成数量", { exact: true }).fill("3");
  await page.getByLabel("起始号码", { exact: true }).fill("1");
  await page.getByRole("button", { name: "生成序号", exact: true }).click();
  await expect(page.locator(".lottery-app tbody tr")).toHaveCount(5);
  const removable = page.locator("tbody tr").filter({ hasText: "用户3" });
  page.once("dialog", (dialog) => dialog.accept());
  await removable.getByRole("button", { name: "移除", exact: true }).click();
  await expect(page.locator(".lottery-app tbody tr")).toHaveCount(4);
  await page.getByRole("button", { name: "现场抽奖", exact: true }).click();
  await page.getByLabel("奖品名称（可选）", { exact: true }).fill("幸运礼盒");
  await page.getByLabel("本次中奖人数", { exact: true }).fill("2");
  await page.getByRole("button", { name: "✦ 开始抽奖", exact: true }).click();
  await page.getByRole("button", { name: "确认抽奖", exact: true }).click();
  await expect(page.getByRole("dialog")).toBeVisible();
  await expect(page.getByRole("dialog").locator(".winner-pill")).toHaveCount(2);
  await expect(page.getByRole("dialog")).toContainText("幸运礼盒");
  await page.getByRole("button", { name: "收下这份幸运", exact: true }).click();
  await page.getByRole("button", { name: "本场历史", exact: true }).click();
  await expect(page.locator(".lottery-app tbody tr")).toHaveCount(1);
  await expect(page.locator(".lottery-app tbody")).toContainText("幸运礼盒");
  // A reload uses server history, not localStorage ownership or winners.
  await page.reload();
  await page.getByRole("button", { name: "中奖历史", exact: true }).click();
  await expect(page.locator(".lottery-app tbody")).toContainText("幸运礼盒");
  const base = `${work.base}/apps/lottery`;
  const room = (await (await page.request.get(`${base}/rooms`)).json())
    .rooms[0];
  const body = {
    requestId: "browser-retry-request",
    count: 1,
    prizeName: "重试验证",
    preventDuplicates: true,
  };
  const first = await write(
    work.context,
    work.session,
    "POST",
    `${base}/rooms/${room.id}/draws`,
    body,
  );
  expect(first.ok()).toBe(true);
  const retry = await write(
    work.context,
    work.session,
    "POST",
    `${base}/rooms/${room.id}/draws`,
    body,
  );
  expect((await retry.json()).id).toBe((await first.json()).id);
  await page.getByRole("button", { name: "活动房间", exact: true }).click();
  await page.getByRole("button", { name: "进入活动 →", exact: true }).click();
  await page.getByRole("button", { name: "活动设置", exact: true }).click();
  page.once("dialog", (dialog) => dialog.accept());
  await page
    .getByRole("button", { name: "确认并重置本场活动", exact: true })
    .click();
  await expect(
    page.getByText("第 2 个抽奖周期", { exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "本场历史", exact: true }).click();
  await expect(
    page.getByText("还没有符合条件的中奖记录。", { exact: true }),
  ).toBeVisible();
  const participants = await (
    await page.request.get(`${base}/rooms/${room.id}/participants`)
  ).json();
  expect(participants.total).toBe(4);
  expect(participants.available).toBe(4);
});

test("lottery: a real committed draw with a dropped response survives switching rooms and reload", async ({
  page,
}) => {
  const work = await workspace(page, "抽奖重试验收", ["lottery"]);
  const base = `${work.base}/apps/lottery`;
  const firstRoom = await created(work.context, work.session, `${base}/rooms`, {
    name: "响应丢失活动",
  });
  const secondRoom = await created(
    work.context,
    work.session,
    `${base}/rooms`,
    { name: "另一个活动" },
  );
  await created(
    work.context,
    work.session,
    `${base}/rooms/${firstRoom.id}/participants/batch`,
    { count: 4, startFrom: 1 },
  );
  await page.goto(work.url("lottery"));
  await page
    .locator(".room-card")
    .filter({ hasText: "响应丢失活动" })
    .getByRole("button", { name: "进入活动 →", exact: true })
    .click();
  await page.getByLabel("本次中奖人数", { exact: true }).fill("2");
  // Forward the request to the real Go/SQLite service and let it commit, then
  // discard only the browser's response. This is network fault injection, not
  // a mocked API or fabricated draw result.
  let committedID = "";
  await page.route(
    `**${base}/rooms/${firstRoom.id}/draws`,
    async (route) => {
      const response = await route.fetch();
      expect(response.status()).toBe(200);
      committedID = (await response.json()).id;
      await route.abort("failed");
    },
    { times: 1 },
  );
  await page.getByRole("button", { name: "✦ 开始抽奖", exact: true }).click();
  await page.getByRole("button", { name: "确认抽奖", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "确认上次抽奖结果", exact: true }),
  ).toBeEnabled();
  await expect(
    page.getByText(
      "上次请求结果尚未确认。重试会取回同一次抽奖，不会额外抽取。",
      { exact: true },
    ),
  ).toBeVisible();
  expect(committedID).toBeTruthy();
  await page.getByRole("button", { name: "活动房间", exact: true }).click();
  await page
    .locator(".room-card")
    .filter({ hasText: "另一个活动" })
    .getByRole("button", { name: "进入活动 →", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "确认上次抽奖结果", exact: true }),
  ).toHaveCount(0);
  expect(
    (
      await (
        await page.request.get(`${base}/rooms/${secondRoom.id}/draws`)
      ).json()
    ).total,
  ).toBe(0);
  await page.reload();
  await page
    .locator(".room-card")
    .filter({ hasText: "响应丢失活动" })
    .getByRole("button", { name: "进入活动 →", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "确认上次抽奖结果", exact: true }),
  ).toBeEnabled();
  await page
    .getByRole("button", { name: "确认上次抽奖结果", exact: true })
    .click();
  await expect(page.getByRole("dialog").locator(".winner-pill")).toHaveCount(2);
  const history = await (
    await page.request.get(`${base}/rooms/${firstRoom.id}/draws`)
  ).json();
  expect(history.total).toBe(1);
  expect(history.draws[0].id).toBe(committedID);
  await page.getByRole("button", { name: "收下这份幸运", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "确认上次抽奖结果", exact: true }),
  ).toHaveCount(0);
});
