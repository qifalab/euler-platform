/**
 * Golden path 1: console overview renders real BFF data (02§9.4).
 * Requires console-base (:5173) + console-bff (:9200) running.
 */
import { test, expect } from "@playwright/test";

test("overview shows real balance/resources/orders from BFF", async ({ page }) => {
  await page.goto("/");
  // Wait for the BFF data to render (not "加载中…").
  await expect(page.getByText("账户余额")).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText("¥500.00")).toBeVisible();
  await expect(page.getByText("待支付订单")).toBeVisible();
  // Real resources from the BFF seed.
  await expect(page.getByText("euecs-cn-north-1-01-a1b2c3d4")).toBeVisible();
});

test("product menu lists the registry sub-apps", async ({ page }) => {
  await page.goto("/");
  await page.getByText("产品 ▾").hover();
  await expect(page.getByText("云服务器 ECS").first()).toBeVisible();
  await expect(page.getByText("对象存储 OSS").first()).toBeVisible();
});
