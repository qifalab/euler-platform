import { test, expect } from "@playwright/test";
import { workspace, created, write } from "./helpers";

test("workflow UI creates explicit approval entitlement and qualified activity notification", async ({
  page,
}) => {
  test.setTimeout(120_000);
  const w = await workspace(page, "应用联动验收", [
    "trust",
    "database",
    "lottery",
  ]);
  await w.grant("trust", "admin");
  await w.grant("trust", "review");
  await w.grant("database", "admin");
  const scheme = await created(
    w.context,
    w.session,
    `${w.base}/apps/trust/schemes`,
    {
      name: "开发者资格",
      status: "active",
      fields: [
        { name: "full_name", label: "姓名", type: "text", required: true },
      ],
    },
  );
  const template = await created(
    w.context,
    w.session,
    `${w.base}/apps/database/admin/templates`,
    {
      name: "认证开发资源",
      days: 30,
      quota: { mysqlMB: 256, mysqlCount: 1 },
    },
  );
  await page.goto(
    `/automation?tenantId=${w.tenant.id}&projectId=${w.project.id}`,
  );
  await expect(
    page.getByRole("heading", { name: "应用联动", exact: true }),
  ).toBeVisible();
  await page
    .getByLabel("规则名称", { exact: true })
    .fill("认证通过赠送开发资源");
  await page.getByLabel("认证方案", { exact: true }).selectOption(scheme.id);
  await page
    .getByLabel("资源包模板", { exact: true })
    .selectOption(template.id);
  const ruleSaved = page.waitForResponse(
    (r) =>
      r.url().endsWith("/automation/rules") && r.request().method() === "POST",
  );
  await page.getByRole("button", { name: "启用规则", exact: true }).click();
  expect((await ruleSaved).status()).toBe(201);
  await expect(
    page
      .locator(".rule-row")
      .getByText("认证通过赠送开发资源", { exact: true }),
  ).toBeVisible();
  const submission = await created(
    w.context,
    w.session,
    `${w.base}/apps/trust/submissions`,
    {
      schemeId: scheme.id,
      schemeVersion: 1,
      data: { full_name: "Alice 开发者" },
    },
  );
  await created(
    w.context,
    w.session,
    `${w.base}/apps/trust/review/submissions/${submission.id}`,
    { status: "approved", version: 1 },
  );
  await expect
    .poll(
      async () =>
        (
          await (
            await page.request.get(`${w.base}/apps/database/packages`)
          ).json()
        ).items.length,
    )
    .toBe(1);
  await page.getByRole("button", { name: "刷新记录", exact: true }).click();
  await expect(
    page.locator("tbody").getByText("已完成", { exact: true }),
  ).toBeVisible();

  const room = await created(
    w.context,
    w.session,
    `${w.base}/apps/lottery/rooms`,
    { name: "已认证开发者抽奖" },
  );
  await created(w.context, w.session, `${w.base}/automation/rules`, {
    name: "开发者活动中奖通知",
    source: "lottery.won",
    filterId: room.id,
    action: "notify",
  });
  const policy = await write(
    w.context,
    w.session,
    "PUT",
    `${w.base}/apps/lottery/rooms/${room.id}/signup-policy`,
    {
      requireLogin: true,
      trustSchemeId: scheme.id,
    },
  );
  expect(policy.ok(), await policy.text()).toBe(true);
  await page.goto(w.url("lottery") + `&signup=${room.id}`);
  await expect(
    page.getByRole("heading", { name: "已认证开发者抽奖", exact: true }),
  ).toBeVisible();
  await page.getByLabel("姓名", { exact: true }).fill("Alice 开发者");
  await page.getByRole("button", { name: "确认本人报名", exact: true }).click();
  await expect(
    page.getByRole("status").filter({ hasText: "报名成功" }),
  ).toBeVisible();
  await created(
    w.context,
    w.session,
    `${w.base}/apps/lottery/rooms/${room.id}/draws`,
    { count: 1, prizeName: "开发资源", requestId: "automation-ui-draw-0001" },
  );
  await expect
    .poll(
      async () =>
        (
          await (
            await page.request.get(`${w.base}/automation/notifications`)
          ).json()
        ).items.length,
    )
    .toBe(1);
  await page.goto(
    `/automation?tenantId=${w.tenant.id}&projectId=${w.project.id}`,
  );
  await expect(
    page
      .locator(".notice-row")
      .getByText("开发者活动中奖通知", { exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "标为已读", exact: true }).click();
  await expect(
    page.locator(".notice-row").getByText("已读", { exact: true }),
  ).toBeVisible();

  // Successful workflows do not add reviewer/operator grants to beneficiaries;
  // the sole review grant remains the one explicitly created by this test.
  const grants = await (
    await page.request.get(
      `/api/v1/platform/grants?tenantId=${w.tenant.id}&projectId=${w.project.id}`,
    )
  ).json();
  expect(
    grants.items.filter(
      (g: { permission: string }) => g.permission === "review",
    ),
  ).toHaveLength(1);
});
