/**
 * Golden path 2: SSO login → real-name gate (02§9.4, §5.1).
 *
 * Requires: console-base (:5173), web-account (:5175) and svc-iam web-auth
 * (:9101) running, with web-auth started with EULER_DEV_SEED=1 (outside a
 * production EULER_ENV) — that seeds the dev account admin@euler.emoera.com /
 * euler123 used below. Without the seed the first test fails at login (401).
 */
import { test, expect } from "@playwright/test";

const ACCOUNT = "http://localhost:5175";
const CONSOLE = "http://localhost:5173";

test("login with the dev-seed account reaches the console", async ({ page }) => {
  const resps: number[] = [];
  page.on("response", (r) => {
    if (r.url().includes("/api/auth/login")) resps.push(r.status());
  });

  await page.goto(`${ACCOUNT}/login`);
  await page.fill('input[type=email], input[placeholder*="邮箱"]', "admin@euler.emoera.com");
  await page.locator("input[type=password]").first().fill("euler123");
  await page.click('button:has-text("登录")');

  // Login succeeded (200) and the browser was redirected to the console —
  // a missing redirect must fail the test, not be swallowed.
  await page.waitForURL(`${CONSOLE}/**`, { timeout: 10_000 });
  expect(resps).toEqual([200]);
  await expect(page).toHaveURL(new RegExp(`^${CONSOLE}/`));
});

test("wrong password is rejected", async ({ page }) => {
  // A throwaway address: five failures lock an email for 15 minutes, so
  // failing against the dev-seed account would lock out the test above (and
  // anyone using the seed) on repeated runs. Unknown emails get the same
  // generic answer, so the UI path under test is identical.
  const email = `e2e-wrong-password-${Date.now()}@example.com`;
  await page.goto(`${ACCOUNT}/login`);
  await page.fill('input[type=email], input[placeholder*="邮箱"]', email);
  await page.locator("input[type=password]").first().fill("wrongpassword");
  await page.click('button:has-text("登录")');
  await expect(page.getByText("邮箱或密码错误")).toBeVisible({ timeout: 5_000 });
  await expect(page).toHaveURL(`${ACCOUNT}/login`);
});
