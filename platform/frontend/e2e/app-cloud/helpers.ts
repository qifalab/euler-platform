import { expect, type BrowserContext, type Page } from "@playwright/test";
export const origin = "http://127.0.0.1:19390";
export type Session = {
  user: { id: string; displayName: string };
  csrfToken: string;
  platformAdmin: boolean;
};
export async function login(
  page: Page,
  name: "Alice" | "Bob" = "Alice",
): Promise<Session> {
  await page.goto("/auth/login?returnTo=/");
  await page.getByRole("button", { name, exact: true }).click();
  await page.waitForURL((url) => url.origin === origin);
  const session = await (await page.request.get("/api/v1/session")).json();
  expect(session.authenticated).toBe(true);
  return session;
}
export async function write(
  context: BrowserContext,
  session: Session,
  method: string,
  path: string,
  data?: unknown,
) {
  return context.request.fetch(path, {
    method,
    data,
    headers: { Origin: origin, "X-CSRF-Token": session.csrfToken },
  });
}
export async function created(
  context: BrowserContext,
  session: Session,
  path: string,
  data: unknown,
) {
  const response = await write(context, session, "POST", path, data);
  expect(response.ok(), await response.text()).toBe(true);
  return response.json();
}
export async function workspace(
  page: Page,
  title: string,
  applicationIds: string[] = [],
) {
  const context = page.context(),
    session = await login(page);
  const tenant = await created(context, session, "/api/v1/tenants", {
    name: `${title}团队`,
  });
  const project = await created(
    context,
    session,
    `/api/v1/tenants/${tenant.id}/projects`,
    { name: title },
  );
  const base = `/api/v1/tenants/${tenant.id}/projects/${project.id}`;
  const installations: Record<string, { id: string }> = {};
  for (const applicationId of applicationIds)
    installations[applicationId] = await created(
      context,
      session,
      `${base}/installations`,
      { applicationId },
    );
  const url = (app: string) =>
    `/apps/${app}?tenantId=${encodeURIComponent(tenant.id)}&projectId=${encodeURIComponent(project.id)}`;
  const grant = (
    applicationId: string,
    permission: "review" | "admin",
    userId = session.user.id,
  ) =>
    created(context, session, "/api/v1/platform/grants", {
      tenantId: tenant.id,
      projectId: project.id,
      applicationId,
      userId,
      permission,
    });
  return { context, session, tenant, project, base, installations, url, grant };
}
