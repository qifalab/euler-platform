import { createHash, generateKeyPairSync, sign } from "node:crypto";
import { expect, test, type APIRequestContext } from "@playwright/test";
import { created, login, origin, workspace, write } from "./helpers";

// Real Ed25519 enrollment against the public controller. This follows the
// engine protocol; no platform, device or permission API is intercepted.
async function enrollDevice(
  request: APIRequestContext,
  controllerURL: string,
  token: string,
) {
  const { publicKey, privateKey } = generateKeyPairSync("ed25519");
  const jwk = publicKey.export({ format: "jwk" });
  const identityPublicKey = Buffer.from(jwk.x!, "base64url")
    .toString("base64")
    .replace(/=+$/, "");
  const challengeResponse = await request.post(
    `${controllerURL}/agent/v1/enroll/challenge`,
    { data: { enrollmentToken: token, identityPublicKey } },
  );
  expect(challengeResponse.status(), await challengeResponse.text()).toBe(201);
  const challenge = await challengeResponse.json();
  const metadata = {
    name: "浏览器签名设备",
    hostname: "browser-node.example.test",
    os: "linux",
    arch: "amd64",
    agentVersion: "euler-browser-acceptance",
    scanInterval: "24h",
    observerOnly: true,
  };
  const fields = [
    Buffer.from("witshield-enrollment-proof-v1"),
    Buffer.from(challenge.id),
    Buffer.from(challenge.challenge),
    createHash("sha256").update(token).digest(),
    ...[
      metadata.name,
      metadata.hostname,
      metadata.os,
      metadata.arch,
      metadata.agentVersion,
      identityPublicKey,
      metadata.scanInterval,
    ].map((value) => Buffer.from(value)),
    Buffer.from([1]),
  ];
  const message = Buffer.concat(
    fields.flatMap((field) => {
      const size = Buffer.alloc(4);
      size.writeUInt32BE(field.length);
      return [size, field];
    }),
  );
  const identitySignature = sign(null, message, privateKey)
    .toString("base64")
    .replace(/=+$/, "");
  const response = await request.post(`${controllerURL}/agent/v1/enroll`, {
    data: {
      enrollmentToken: token,
      identityPublicKey,
      ...metadata,
      challengeId: challenge.id,
      challenge: challenge.challenge,
      identitySignature,
    },
  });
  expect(response.status(), await response.text()).toBe(201);
  return (await response.json()).device as { id: string; name: string };
}

test("WitShield native device enrollment, schedule CRUD, protected settings and viewer boundary", async ({
  page,
  browser,
}, testInfo) => {
  const work = await workspace(page, "主机安全验收", ["witshield"]);
  const api = `${work.base}/apps/witshield`;
  await page.goto(work.url("witshield"));
  await expect(
    page.getByRole("heading", { name: "WitShield 安全工作台", exact: true }),
  ).toBeVisible();
  await expect(page.getByText("已接入设备", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "设备与计划", exact: true }).click();
  const controllerURL = `${origin}/public/witshield/${work.tenant.id}/${work.project.id}/${work.installations.witshield.id}`;
  await expect(
    page.getByLabel("本项目设备 Controller URL", { exact: true }),
  ).toHaveValue(controllerURL);
  await page.getByLabel("注册码用途", { exact: true }).fill("浏览器接入令牌");
  await page
    .getByRole("button", { name: "生成一次性注册码", exact: true })
    .click();
  const issued = page.locator(".ws-secret");
  await expect(issued).toBeVisible();
  const token = (await issued.textContent())!.trim();
  expect(token).toMatch(/^wse_/);
  const tokenList = await (
    await page.request.get(`${api}/enrollment-tokens`)
  ).json();
  const enrollment = tokenList.items.find(
    (item: { name: string }) => item.name === "浏览器接入令牌",
  );
  expect(enrollment).toBeTruthy();
  expect(JSON.stringify(tokenList)).not.toContain(token);
  await page.getByRole("button", { name: "隐藏注册码", exact: true }).click();
  await expect(issued).toHaveCount(0);
  const device = await enrollDevice(page.request, controllerURL, token);
  await page.getByRole("button", { name: "刷新数据", exact: true }).click();
  await expect(
    page.getByRole("combobox", { name: "选择设备", exact: true }),
  ).toContainText(device.name);
  await page
    .getByRole("combobox", { name: "选择设备", exact: true })
    .selectOption(device.id);
  await expect(
    page.getByText("只读设备不能执行修复", { exact: true }),
  ).toBeVisible();
  await page.getByLabel("扫描间隔", { exact: true }).fill("36h");
  await page.getByRole("button", { name: "添加计划", exact: true }).click();
  await expect
    .poll(async () =>
      (await (await page.request.get(`${api}/schedules`)).json()).items.some(
        (item: { every: string }) => item.every === "36h0m0s",
      ),
    )
    .toBe(true);
  const schedules = (await (await page.request.get(`${api}/schedules`)).json())
    .items;
  const schedule = schedules.find(
    (item: { every: string }) => item.every === "36h0m0s",
  );
  const scheduleRow = page.getByTestId(`schedule-${schedule.id}`);
  await expect(scheduleRow).toBeVisible();
  await scheduleRow.getByLabel("间隔", { exact: true }).fill("48h");
  await scheduleRow.getByRole("button", { name: "保存", exact: true }).click();
  await expect(scheduleRow.getByLabel("间隔", { exact: true })).toHaveValue(
    "48h0m0s",
  );
  page.once("dialog", (dialog) => dialog.accept());
  await scheduleRow.getByRole("button", { name: "删除", exact: true }).click();
  await expect(scheduleRow).toHaveCount(0);
  const revokeRow = page.getByTestId(`enrollment-${enrollment.id}`);
  page.once("dialog", (dialog) => dialog.accept());
  await revokeRow.getByRole("button", { name: "撤销", exact: true }).click();
  await expect(
    revokeRow.getByRole("button", { name: "已撤销", exact: true }),
  ).toBeDisabled();

  await page.getByRole("button", { name: "设置", exact: true }).click();
  const apiSecret = "browser-ai-private-credential";
  await page.getByLabel("模型", { exact: true }).fill("browser-test-model");
  await page
    .getByLabel("服务 Endpoint", { exact: true })
    .fill("https://ai.example.test/v1");
  await page.getByLabel("新的 API Key", { exact: true }).fill(apiSecret);
  await Promise.all([
    page.waitForResponse(
      (response) =>
        response.url().endsWith(`${api}/ai/settings`) &&
        response.request().method() === "PUT" &&
        response.status() === 200,
    ),
    page.getByRole("button", { name: "保存 AI 配置", exact: true }).click(),
  ]);
  await expect(page.getByLabel("新的 API Key", { exact: true })).toHaveValue(
    "",
  );
  const savedAI = await (await page.request.get(`${api}/ai/settings`)).json();
  expect(savedAI.keyConfigured).toBe(true);
  expect(JSON.stringify(savedAI)).not.toContain(apiSecret);
  await expect(page.locator(".ws-app")).not.toContainText(apiSecret);
  // Changing the destination cannot silently forward the already-saved key.
  await page
    .getByLabel("服务 Endpoint", { exact: true })
    .fill("https://other.example.test/v1");
  await page.getByRole("button", { name: "保存 AI 配置", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("重新输入或清除旧密钥");
  expect(
    (await (await page.request.get(`${api}/ai/settings`)).json()).baseUrl,
  ).toBe("https://ai.example.test/v1");
  await page
    .getByLabel("服务 Endpoint", { exact: true })
    .fill("https://ai.example.test/v1");
  // Store disabled notification channels to exercise real encrypted persistence
  // without sending any message to an external recipient during acceptance.
  const webhookSecret = "browser-webhook-private-credential",
    smtpSecret = "browser-smtp-private-credential";
  await page
    .getByLabel("接收 URL", { exact: true })
    .fill("https://notifications.example.test/security");
  await page.getByLabel("新的签名密钥", { exact: true }).fill(webhookSecret);
  await page.getByLabel("SMTP 主机", { exact: true }).fill("smtp.example.test");
  await page.getByLabel("账号", { exact: true }).fill("test-account");
  await page.getByLabel("新的 SMTP 密码", { exact: true }).fill(smtpSecret);
  await Promise.all([
    page.waitForResponse(
      (response) =>
        response.url().endsWith(`${api}/notifications/settings`) &&
        response.request().method() === "PUT" &&
        response.status() === 200,
    ),
    page.getByRole("button", { name: "保存通知渠道", exact: true }).click(),
  ]);
  await expect(page.getByLabel("新的签名密钥", { exact: true })).toHaveValue(
    "",
  );
  await expect(page.getByLabel("新的 SMTP 密码", { exact: true })).toHaveValue(
    "",
  );
  const savedNotifications = await (
    await page.request.get(`${api}/notifications/settings`)
  ).json();
  expect(savedNotifications.webhookSecretConfigured).toBe(true);
  expect(savedNotifications.smtpPasswordConfigured).toBe(true);
  expect(JSON.stringify(savedNotifications)).not.toContain(webhookSecret);
  expect(JSON.stringify(savedNotifications)).not.toContain(smtpSecret);
  await page.reload();
  await page.getByRole("button", { name: "设置", exact: true }).click();
  await expect(page.getByLabel("模型", { exact: true })).toHaveValue(
    "browser-test-model",
  );
  await expect(page.getByLabel("新的 API Key", { exact: true })).toHaveValue(
    "",
  );
  await expect(page.getByLabel("新的 SMTP 密码", { exact: true })).toHaveValue(
    "",
  );

  const viewer = await browser.newContext({ baseURL: origin });
  try {
    const viewerPage = await viewer.newPage(),
      viewerSession = await login(viewerPage, "Bob");
    const invitation = await created(
      work.context,
      work.session,
      `/api/v1/tenants/${work.tenant.id}/invitations`,
      { role: "member" },
    );
    expect(
      (
        await write(
          viewer,
          viewerSession,
          "POST",
          "/api/v1/invitations/accept",
          { token: invitation.token },
        )
      ).ok(),
    ).toBe(true);
    expect(
      (
        await write(
          work.context,
          work.session,
          "POST",
          `${work.base}/members`,
          { userId: viewerSession.user.id, role: "viewer" },
        )
      ).status(),
    ).toBe(204);
    await viewerPage.goto(work.url("witshield"));
    await expect(
      viewerPage.getByRole("heading", {
        name: "WitShield 安全工作台",
        exact: true,
      }),
    ).toBeVisible();
    await viewerPage
      .getByRole("button", { name: "设备与计划", exact: true })
      .click();
    await expect(
      viewerPage.getByRole("button", { name: "生成一次性注册码", exact: true }),
    ).toHaveCount(0);
    await expect(
      viewerPage.getByRole("button", { name: "添加计划", exact: true }),
    ).toHaveCount(0);
    await viewerPage.getByRole("button", { name: "设置", exact: true }).click();
    await expect(
      viewerPage.getByLabel("新的 API Key", { exact: true }),
    ).toBeDisabled();
    await expect(
      viewerPage.getByRole("button", { name: "保存 AI 配置", exact: true }),
    ).toHaveCount(0);
    for (const path of [
      "/enrollment-tokens",
      `/devices/${device.id}/scan`,
      "/actions",
    ])
      expect(
        (await write(viewer, viewerSession, "POST", api + path, {})).status(),
      ).toBe(403);
    expect(
      (
        await write(viewer, viewerSession, "PUT", `${api}/ai/settings`, {})
      ).status(),
    ).toBe(403);
  } finally {
    await viewer.close();
  }
  await page.getByRole("button", { name: "安全概览", exact: true }).click();
  await page.screenshot({
    path: testInfo.outputPath("witshield-native-overview.png"),
    fullPage: true,
    animations: "disabled",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect
    .poll(() =>
      page
        .locator(".cloud-sidebar")
        .evaluate((element) => element.getBoundingClientRect().right),
    )
    .toBeLessThanOrEqual(0);
  await expect
    .poll(() =>
      page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth + 1,
      ),
    )
    .toBe(true);
  await page.screenshot({
    path: testInfo.outputPath("witshield-native-mobile.png"),
    fullPage: true,
    animations: "disabled",
  });
});
