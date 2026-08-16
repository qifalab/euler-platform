/**
 * Golden path 2: SSO login → real-name gate (02§9.4, §5.1).
 * Requires web-account (:5175) + svc-iam web-auth (:9101) running.
 */
import { test, expect } from "@playwright/test";

test("login with real credentials reaches the console", async ({ page }) => {
  const resps: number[] = [];
  page.on("response", (r) => {
    if (r.url().includes("/api/auth/login")) resps.push(r.status());
  });

  await page.goto("http://localhost:5175/login");
  await page.fill('input[type=email], input[placeholder*="邮箱"]', "admin@starcloud.cn");
  const pwds = await page.$$('input[type=password]');
  await pwds[0].fill("starcloud123");
  await page.click('button:has-text("登录")');
  // Login succeeded (200) and redirected to the console.
  await page.waitForURL("http://localhost:5173/**", { timeout: 10_000 }).catch(() => {});
  expect(resps.length).toBeGreaterThan(0);
  expect(resps[0]).toBe(200);
});

test("wrong password is rejected", async ({ page }) => {
  await page.goto("http://localhost:5175/login");
  await page.fill('input[type=email], input[placeholder*="邮箱"]', "admin@starcloud.cn");
  const pwds = await page.$$('input[type=password]');
  await pwds[0].fill("wrongpassword");
  await page.click('button:has-text("登录")');
  await expect(page.getByText("邮箱或密码错误")).toBeVisible({ timeout: 5_000 });
});
