import { test, expect } from "@playwright/test";
import { workspace } from "./helpers";

test("service accounts and project archive use real native API boundaries", async ({
  page,
  request,
}) => {
  const w = await workspace(page, "访问权限与归档验收", ["weauth"]);
  const contextQuery = `?tenantId=${w.tenant.id}&projectId=${w.project.id}`;
  await page.goto(`/service-accounts${contextQuery}`);
  await page.getByLabel("名称", { exact: true }).fill("服务端自动化验收");
  await page.getByLabel("应用", { exact: true }).selectOption("weauth");
  await page.getByLabel("创建与更新", { exact: true }).check();
  await page
    .getByRole("button", { name: "创建并显示密钥", exact: true })
    .click();
  let dialog = page.getByRole("dialog", { name: "保存服务账号密钥" });
  await expect(dialog).toBeVisible();
  const secret = await dialog.getByLabel("服务账号密钥").inputValue();
  expect(secret).toMatch(/^euler_sa_/);
  await dialog.getByRole("button", { name: "已保存，关闭" }).click();
  await expect(dialog).not.toBeVisible();
  const machine = `/api/v1/machine/tenants/${w.tenant.id}/projects/${w.project.id}/apps/weauth`;
  const headers = { Authorization: `Bearer ${secret}` };
  const created = await request.post(`${machine}/sites`, {
    headers,
    data: { name: "机器账号创建", domains: ["example.test"] },
  });
  expect(created.status()).toBe(201);
  const site = await created.json();
  expect(
    (
      await request.get(`${machine}/sites/${site.id}/secret`, { headers })
    ).status(),
  ).toBe(403);
  expect(
    (
      await request.get(`${machine}/sites`, {
        headers: { ...headers, Cookie: "euler_session=browser" },
      })
    ).status(),
  ).toBe(403);
  await page.getByRole("button", { name: "轮换密钥", exact: true }).click();
  dialog = page.getByRole("dialog", { name: "保存服务账号密钥" });
  await expect(dialog).toBeVisible();
  const rotated = await dialog.getByLabel("服务账号密钥").inputValue();
  expect(rotated).not.toBe(secret);
  await dialog.getByRole("button", { name: "已保存，关闭" }).click();
  expect((await request.get(`${machine}/sites`, { headers })).status()).toBe(
    401,
  );
  const newHeaders = { Authorization: `Bearer ${rotated}` };
  expect(
    (await request.get(`${machine}/sites`, { headers: newHeaders })).status(),
  ).toBe(200);
  await page.goto(`/project-settings${contextQuery}`);
  await page.getByRole("button", { name: "归档项目", exact: true }).click();
  dialog = page.getByRole("dialog", { name: "归档项目" });
  await dialog.getByLabel("项目名称", { exact: true }).fill(w.project.name);
  await dialog.getByRole("button", { name: "确认归档", exact: true }).click();
  await expect(dialog).not.toBeVisible();
  await expect(
    page.getByRole("button", { name: "恢复项目", exact: true }),
  ).toBeVisible();
  expect(
    (await request.get(`${machine}/sites`, { headers: newHeaders })).status(),
  ).toBe(409);
  expect((await page.request.get(`${w.base}/apps/weauth/sites`)).status()).toBe(
    409,
  );
  // A populated archived project cannot silently delete its retained resources.
  await page.getByRole("button", { name: "删除空项目", exact: true }).click();
  dialog = page.getByRole("dialog", { name: "删除空项目" });
  await dialog.getByLabel("项目名称", { exact: true }).fill(w.project.name);
  await dialog.getByRole("button", { name: "确认删除", exact: true }).click();
  await expect(
    page.getByText(/project retains applications or service accounts/),
  ).toBeVisible();
  await dialog.getByRole("button", { name: "取消", exact: true }).click();
  await page.getByRole("button", { name: "恢复项目", exact: true }).click();
  dialog = page.getByRole("dialog", { name: "恢复项目" });
  await dialog.getByLabel("项目名称", { exact: true }).fill(w.project.name);
  await dialog.getByRole("button", { name: "确认恢复", exact: true }).click();
  await expect(dialog).not.toBeVisible();
  expect(
    (await request.get(`${machine}/sites`, { headers: newHeaders })).status(),
  ).toBe(200);
  await page.goto(`/service-accounts${contextQuery}`);
  await page.getByRole("button", { name: "撤销", exact: true }).click();
  await page
    .getByRole("dialog", { name: "撤销服务账号" })
    .getByRole("button", { name: "确认撤销" })
    .click();
  await expect(page.getByText("已撤销", { exact: true })).toBeVisible();
  expect(
    (await request.get(`${machine}/sites`, { headers: newHeaders })).status(),
  ).toBe(401);
});

test("empty project and team deletion require typed confirmation", async ({
  page,
}) => {
  const w = await workspace(page, "空团队退役验收", []);
  const contextQuery = `?tenantId=${w.tenant.id}&projectId=${w.project.id}`;
  await page.goto(`/members${contextQuery}`);
  await expect(
    page.getByRole("button", { name: "删除空团队", exact: true }),
  ).toBeDisabled();
  await page.goto(`/project-settings${contextQuery}`);
  await page.getByRole("button", { name: "归档项目", exact: true }).click();
  let dialog = page.getByRole("dialog", { name: "归档项目" });
  await expect(dialog.getByRole("button", { name: "确认归档" })).toBeDisabled();
  await dialog.getByLabel("项目名称", { exact: true }).fill(w.project.name);
  await dialog.getByRole("button", { name: "确认归档" }).click();
  await expect(dialog).not.toBeVisible();
  await page.getByRole("button", { name: "删除空项目", exact: true }).click();
  dialog = page.getByRole("dialog", { name: "删除空项目" });
  await dialog.getByLabel("项目名称", { exact: true }).fill(w.project.name);
  await dialog.getByRole("button", { name: "确认删除", exact: true }).click();
  await expect(dialog).not.toBeVisible();
  await page.goto("/members");
  await page.getByRole("button", { name: "删除空团队", exact: true }).click();
  dialog = page.getByRole("dialog", { name: "删除空团队" });
  await expect(
    dialog.getByRole("button", { name: "确认删除团队" }),
  ).toBeDisabled();
  await dialog.getByLabel("团队名称", { exact: true }).fill(w.tenant.name);
  await dialog.getByRole("button", { name: "确认删除团队" }).click();
  await expect(dialog).not.toBeVisible();
  const tenants = await (await page.request.get("/api/v1/tenants")).json();
  expect(tenants.items.some((t: { id: string }) => t.id === w.tenant.id)).toBe(
    false,
  );
});
