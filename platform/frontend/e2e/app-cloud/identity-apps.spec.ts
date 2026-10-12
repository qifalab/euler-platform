import { test, expect } from "@playwright/test";
import { workspace, created, write, login } from "./helpers";

test("native Trust materials and approval feed EID identity and recruitment", async ({
  page,
  browser,
}) => {
  test.setTimeout(120_000);
  const w = await workspace(page, "身份与认证闭环", ["eid", "trust"]);
  const trustAPI = `${w.base}/apps/trust`,
    eidAPI = `${w.base}/apps/eid`;
  await page.goto(w.url("trust"));
  await expect(
    page.getByRole("button", { name: "审核工作台", exact: true }),
  ).toHaveCount(0);
  expect(
    (await page.request.get(`${trustAPI}/review/submissions`)).status(),
  ).toBe(403);
  expect(
    (await page.request.get(`${eidAPI}/review/verifications`)).status(),
  ).toBe(403);
  await w.grant("trust", "admin");
  await w.grant("trust", "review");
  await w.grant("eid", "review");
  await page.reload();
  await page
    .getByRole("navigation", { name: "Trust 功能" })
    .getByRole("button", { name: "认证方案", exact: true })
    .click();
  await page.getByRole("button", { name: "创建方案", exact: true }).click();
  const editor = page.getByRole("dialog", { name: "编辑认证方案" });
  await editor
    .getByLabel("方案名称", { exact: true })
    .fill("团队实名与入团资格");
  const first = editor.locator(".field-editor").nth(0);
  await first.getByLabel("字段标题", { exact: true }).fill("姓名");
  await first.getByLabel("字段标识", { exact: true }).fill("full_name");
  await first.getByLabel("必须填写", { exact: true }).check();
  await editor.getByRole("button", { name: "添加字段", exact: true }).click();
  const second = editor.locator(".field-editor").nth(1);
  await second.getByLabel("字段标题", { exact: true }).fill("资格证明");
  await second.getByLabel("字段标识", { exact: true }).fill("proof");
  await second.getByLabel("字段类型", { exact: true }).selectOption("file");
  await second.getByLabel("必须填写", { exact: true }).check();
  const schemeResponse = page.waitForResponse(
    (response) =>
      response.url().endsWith(`${trustAPI}/schemes`) &&
      response.request().method() === "POST",
  );
  await editor.getByRole("button", { name: "保存方案", exact: true }).click();
  const schemeHTTP = await schemeResponse;
  expect(schemeHTTP.status()).toBe(201);
  const scheme = await schemeHTTP.json();
  await expect(editor).toHaveCount(0);
  await page.getByRole("button", { name: "填写申请", exact: true }).click();
  const form = page.getByRole("dialog", { name: "提交认证材料" });
  await form.locator("#trust-full_name").fill("Alice 实名资料");
  const materialResponse = page.waitForResponse(
    (response) =>
      response.url().endsWith(`${trustAPI}/materials`) &&
      response.request().method() === "POST",
  );
  await form
    .locator("#trust-proof")
    .setInputFiles({
      name: "private-proof.txt",
      mimeType: "text/plain",
      buffer: Buffer.from("Private proof belonging to Alice only."),
    });
  const materialHTTP = await materialResponse;
  expect(materialHTTP.status()).toBe(201);
  const material = await materialHTTP.json();
  await expect(form.getByText(/private-proof.txt/)).toBeVisible();
  const submitResponse = page.waitForResponse(
    (response) =>
      response.url().endsWith(`${trustAPI}/submissions`) &&
      response.request().method() === "POST",
  );
  await form.getByRole("button", { name: "确认提交材料", exact: true }).click();
  const submittedHTTP = await submitResponse;
  expect(submittedHTTP.status()).toBe(201);
  const submission = await submittedHTTP.json();
  const detail = page.getByRole("dialog", { name: "认证申请详情" });
  await expect(
    detail.getByText("Alice 实名资料", { exact: true }),
  ).toBeVisible();
  await detail.getByRole("button", { name: "关闭", exact: true }).click();
  await page
    .getByRole("navigation", { name: "Trust 功能" })
    .getByRole("button", { name: "审核工作台", exact: true })
    .click();
  await page.getByRole("button", { name: "查看与审核", exact: true }).click();
  await detail.getByRole("button", { name: "通过认证", exact: true }).click();
  await page
    .getByRole("alertdialog")
    .getByRole("button", { name: "确认", exact: true })
    .click();
  await expect(detail.locator(".detail-meta .badge")).toHaveText("已通过");
  const ownMaterial = await page.request.get(
    `${trustAPI}/materials/${material.id}`,
  );
  expect(ownMaterial.status()).toBe(200);
  expect(await ownMaterial.text()).toContain(
    "Private proof belonging to Alice only.",
  );

  await page.goto(w.url("eid"));
  await page.getByRole("button", { name: "流程设置", exact: true }).click();
  await page.getByLabel("招募名称", { exact: true }).fill("研发团队新成员招募");
  await page
    .getByLabel("前置认证方案", { exact: true })
    .selectOption(scheme.id);
  await page.getByRole("button", { name: "保存流程设置", exact: true }).click();
  await expect(
    page.getByText("流程与通知设置已保存", { exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "我的身份", exact: true }).click();
  await page.getByRole("button", { name: "申请身份", exact: true }).click();
  const identityForm = page.getByRole("dialog", { name: "申请成员身份" });
  await identityForm.getByLabel("真实姓名", { exact: true }).fill("Alice 成员");
  await identityForm.getByLabel("学号", { exact: true }).fill("20260001");
  await identityForm
    .getByLabel("身份名称", { exact: true })
    .fill("研发团队核心成员");
  await identityForm
    .getByLabel("身份类型", { exact: true })
    .selectOption("core");
  await identityForm
    .getByRole("button", { name: "提交申请", exact: true })
    .click();
  await expect(identityForm).toHaveCount(0);
  await page.getByRole("button", { name: "身份审核", exact: true }).click();
  await page.getByRole("button", { name: "通过", exact: true }).click();
  await page
    .getByRole("dialog", { name: "通过身份审核" })
    .getByRole("button", { name: "确认操作", exact: true })
    .click();
  await expect(page.getByRole("dialog", { name: "通过身份审核" })).toHaveCount(
    0,
  );
  await page.getByRole("button", { name: "我的身份", exact: true }).click();
  await expect(page.locator(".credential")).toContainText("研发团队核心成员");
  await page.getByRole("button", { name: "社团报名", exact: true }).click();
  await expect(
    page.getByText("✓ Trust 认证已通过", { exact: true }),
  ).toBeVisible();
  await page
    .locator(".club-form")
    .getByLabel("真实姓名", { exact: true })
    .fill("Alice 成员");
  await page.getByRole("button", { name: "提交报名", exact: true }).click();
  await expect(page.locator(".record-card .badge")).toHaveText("待审核");
  const club = (await (await page.request.get(`${eidAPI}/club`)).json())
    .items[0];
  expect(club.trustVerified).toBe(true);
  await page.getByRole("button", { name: "招募审核", exact: true }).click();
  await page.getByRole("button", { name: "发送笔试通知", exact: true }).click();
  const mailResponse = page.waitForResponse(
    (response) =>
      response.url().endsWith(`/review/club/${club.id}/notifications`) &&
      response.request().method() === "POST",
  );
  await page
    .getByRole("dialog", { name: "发送笔试通知" })
    .getByRole("button", { name: "确认操作", exact: true })
    .click();
  expect((await mailResponse).status()).toBe(503);
  await expect(page.getByRole("alert")).toContainText("尚未配置邮件服务");
  expect(
    (await (await page.request.get(`${eidAPI}/club`)).json()).items[0].status,
  ).toBe("pending");

  // A real second user's session and membership do not grant access to Alice's records.
  const bob = await browser.newContext(),
    bobPage = await bob.newPage();
  const bobSession = await login(bobPage, "Bob");
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
  expect(
    (
      await bob.request.get(`${trustAPI}/submissions/${submission.id}`)
    ).status(),
  ).toBe(404);
  expect(
    (await bob.request.get(`${trustAPI}/materials/${material.id}`)).status(),
  ).toBe(404);
  expect(
    (
      await write(
        bob,
        bobSession,
        "POST",
        `${eidAPI}/club/applications/${club.id}/confirm`,
        { version: club.version },
      )
    ).status(),
  ).toBe(403);
  expect(
    (await (await bob.request.get(`${eidAPI}/verifications`)).json()).items,
  ).toEqual([]);
  await bob.close();

  const other = await created(
    w.context,
    w.session,
    `/api/v1/tenants/${w.tenant.id}/projects`,
    { name: "隔离认证项目" },
  );
  const otherBase = `/api/v1/tenants/${w.tenant.id}/projects/${other.id}`;
  await created(w.context, w.session, `${otherBase}/installations`, {
    applicationId: "trust",
  });
  expect(
    (
      await page.request.get(
        `${otherBase}/apps/trust/submissions/${submission.id}`,
      )
    ).status(),
  ).toBe(404);
  expect(
    (
      await page.request.get("/public/eid/verification/status?oauth_id=101")
    ).status(),
  ).toBe(401);
});
