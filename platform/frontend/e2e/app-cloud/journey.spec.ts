import { test, expect } from "@playwright/test";
import { workspace, login, created, write, origin } from "./helpers";

test("native workspace, real WeAuth worker, collaboration and permission revocation", async ({
  page,
  browser,
}, testInfo) => {
  const w = await workspace(page, "统一平台验收", []);
  await page.goto(w.url("weauth"));
  await page.getByRole("button", { name: "启用应用", exact: true }).click();
  await page.getByRole("button", { name: "新建站点", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel("站点名称", { exact: true }).fill("真实验证站点");
  await dialog.getByLabel("允许的域名").fill("127.0.0.1");
  await dialog.getByLabel("验证模式").selectOption({ label: "动态难度" });
  await dialog.getByLabel("基础难度", { exact: true }).fill("1");
  await dialog.getByRole("button", { name: "创建站点", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  const api = `${w.base}/apps/weauth`;
  const sites = await (await w.context.request.get(`${api}/sites`)).json();
  const site = sites.items.find(
    (s: { name: string }) => s.name === "真实验证站点",
  );
  expect(site).toBeTruthy();
  expect(JSON.stringify(sites)).not.toContain('"secret"');
  await page.getByRole("button", { name: "集成与预览", exact: true }).click();
  const widget = page.frameLocator('iframe[title="真实 WeAuth 验证预览"]');
  await widget.getByRole("button", { name: "开始验证" }).click();
  await expect(widget.getByText("验证通过", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "服务端验证并消耗令牌" }).click();
  await expect(page.getByText(/验证成功 · 来源/)).toBeVisible();
  await page.evaluate(() => window.scrollTo(0, 0));
  await page.screenshot({
    path: testInfo.outputPath("native-weauth.png"),
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
  await page.evaluate(() => window.scrollTo(0, 0));
  await page.screenshot({
    path: testInfo.outputPath("native-weauth-mobile.png"),
    fullPage: true,
    animations: "disabled",
  });
  await page.setViewportSize({ width: 1280, height: 800 });

  const bob = await browser.newContext({ baseURL: origin }),
    bobPage = await bob.newPage();
  const bobSession = await login(bobPage, "Bob");
  expect((await bob.request.get(`${api}/sites`)).status()).toBe(404);
  expect(
    (
      await page.request.post(`${api}/sites`, { data: { name: "No CSRF" } })
    ).status(),
  ).toBe(403);
  const invitation = await created(
    w.context,
    w.session,
    `/api/v1/tenants/${w.tenant.id}/invitations`,
    { role: "member" },
  );
  expect(
    (
      await write(bob, bobSession, "POST", "/api/v1/invitations/accept", {
        token: invitation.token,
      })
    ).ok(),
  ).toBe(true);
  expect(
    (
      await write(w.context, w.session, "POST", `${w.base}/members`, {
        userId: bobSession.user.id,
        role: "viewer",
      })
    ).status(),
  ).toBe(204);
  expect((await bob.request.get(`${api}/sites`)).status()).toBe(200);
  expect(
    (
      await write(bob, bobSession, "POST", `${api}/sites`, { name: "Denied" })
    ).status(),
  ).toBe(403);
  expect(
    (await bob.request.get(`${api}/sites/${site.id}/secret`)).status(),
  ).toBe(403);
  expect(
    (
      await write(
        w.context,
        w.session,
        "DELETE",
        `${w.base}/members/${bobSession.user.id}`,
      )
    ).status(),
  ).toBe(204);
  expect((await bob.request.get(`${api}/sites`)).status()).toBe(404);

  const inst = (
    await (await w.context.request.get(`${w.base}/installations`)).json()
  ).items[0];
  expect(
    (
      await write(
        w.context,
        w.session,
        "PUT",
        `${w.base}/installations/${inst.id}/connection`,
        { baseUrl: "https://original.example", credential: "old-token" },
      )
    ).status(),
  ).toBe(410);
  await page.getByRole("button", { name: "应用设置", exact: true }).click();
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "停用应用", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "此应用已暂停使用" }),
  ).toBeVisible();
  expect((await w.context.request.get(`${api}/sites`)).status()).toBe(409);
  expect(
    (
      await page.request.post("/public/weauth/pow/challenge", {
        data: { sitekey: site.sitekey, origin, action: "disabled" },
      })
    ).ok(),
  ).toBe(false);
  await page.getByRole("button", { name: "重新启用", exact: true }).click();
  await expect(
    page.getByText("真实验证站点", { exact: true }).first(),
  ).toBeVisible();
  await page.reload();
  await expect(page.getByTestId("project-select")).toHaveValue(w.project.id);
  const logout = await write(w.context, w.session, "POST", "/auth/logout");
  expect(logout.status()).toBe(204);
  expect((await w.context.request.get(`${api}/sites`)).status()).toBe(401);
  await bob.close();
});

test("all eight native workspaces load, explicit reviewer grants and project links", async ({
  page,
}) => {
  const ids = [
    "eid",
    "trust",
    "weauth",
    "database",
    "storage",
    "statistics",
    "lottery",
    "witshield",
  ];
  const w = await workspace(page, "应用工作台", ids);
  for (const id of ids) {
    const scope = await (
      await page.request.get(`${w.base}/apps/${id}/_context`)
    ).json();
    expect(scope.applicationId).toBe(id);
    expect(scope.actorId).toBe(w.session.user.id);
    expect(scope.permissions).not.toContain("review");
    expect(scope.permissions).not.toContain("admin");
    await page.goto(w.url(id));
    await expect(page.getByTestId("project-select")).toHaveValue(w.project.id);
    await expect(page.locator(".app-workspace-heading h1")).toBeVisible();
    await expect(
      page.getByText("应用组件未能加载", { exact: true }),
    ).toHaveCount(0);
  }
  await page.goto(
    `/permissions?tenantId=${w.tenant.id}&projectId=${w.project.id}`,
  );
  await expect(
    page.getByRole("heading", { name: "授予权限", exact: true }),
  ).toBeVisible();
  await page.getByLabel("应用", { exact: true }).selectOption("trust");
  await page.getByLabel("权限", { exact: true }).selectOption("review");
  await page.getByRole("button", { name: "授予权限", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "撤销授权", exact: true }),
  ).toBeVisible();
  expect(
    (await (await page.request.get(`${w.base}/apps/trust/_context`)).json())
      .permissions,
  ).toContain("review");
  await page.getByRole("button", { name: "撤销授权", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "撤销授权", exact: true }),
  ).toHaveCount(0);
  expect(
    (await (await page.request.get(`${w.base}/apps/trust/_context`)).json())
      .permissions,
  ).not.toContain("review");
  await page.goto(
    `/apps/eid?tenantId=${w.tenant.id}&projectId=foreign-project`,
  );
  await expect(
    page.getByRole("heading", { name: "无法打开链接中的项目" }),
  ).toBeVisible();
});

test("unauthenticated callers cannot impersonate a user with legacy headers", async ({
  request,
}) => {
  const response = await request.get("/api/v1/tenants", {
    headers: { "X-Euler-Account-Id": "owner", "X-Euler-User-Id": "owner" },
  });
  expect(response.status()).toBe(401);
  const session = await (await request.get("/api/v1/session")).json();
  expect(session.authenticated).toBe(false);
});
