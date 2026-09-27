import { test, expect } from "@playwright/test";
import { workspace } from "./helpers";

test("unified overview and usage retain explicit context across desktop and mobile", async ({
  page,
}, testInfo) => {
  const w = await workspace(page, "平台资源验收", [
    "database",
    "storage",
    "trust",
  ]);
  const query = `?tenantId=${w.tenant.id}&projectId=${w.project.id}`;
  await page.goto(`/${query}`);
  await expect(
    page.getByRole("heading", { name: "让项目开始运转" }),
  ).toBeVisible();
  await expect(page.getByText("等待配置连接", { exact: true })).toHaveCount(0);
  await expect(
    page.getByText("已启用 · 进入应用工作台", { exact: true }),
  ).toHaveCount(3);
  await page.goto(`/services${query}`);
  await expect(
    page.getByRole("heading", { name: "资源与用量." }),
  ).toBeVisible();
  await expect(
    page.getByText("存储服务尚未配置", { exact: true }),
  ).toBeVisible();
  await expect(page.getByText("引擎尚未配置", { exact: true })).toHaveCount(2);
  await expect(page.getByText("v0.2.0", { exact: true })).toHaveCount(8);
  await page.screenshot({
    path: testInfo.outputPath("project-usage-desktop.png"),
    fullPage: true,
    animations: "disabled",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect
    .poll(() =>
      page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth + 1,
      ),
    )
    .toBe(true);
  await page.screenshot({
    path: testInfo.outputPath("project-usage-mobile.png"),
    fullPage: true,
    animations: "disabled",
  });
  await page.evaluate(() => (document.documentElement.dataset.theme = "dark"));
  await page.screenshot({
    path: testInfo.outputPath("project-usage-dark.png"),
    fullPage: true,
    animations: "disabled",
  });
});
