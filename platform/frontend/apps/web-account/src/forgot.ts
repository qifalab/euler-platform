/**
 * 找回密码 — svc-iam web-auth's password-reset endpoints:
 *   POST /api/auth/forgot/send-code {email}                 — always 200: whether
 *        the address is registered is never revealed (no account enumeration)
 *   POST /api/auth/forgot/reset     {email, code, password}  — 400 Auth.InvalidCode
 *        (wrong / expired code), 429 (too many attempts / sends)
 * The verification code is compared server-side only.
 */
import { networkError, readEnvelope } from "./http";

export const SEND_CODE_PATH = "/api/auth/forgot/send-code";
export const RESET_PATH = "/api/auth/forgot/reset";

/** Shown after send-code: the same text whether or not the email is registered. */
export const CODE_SENT_MESSAGE = "如该邮箱已注册,验证码已发送至该邮箱,请查收";

/** POST a public svc-iam auth call; a bare 2xx (no body) counts as success. */
export async function postAuth(path: string, body: Record<string, unknown>): Promise<void> {
  let res: Response;
  try {
    res = await fetch(path, {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json", "X-Requested-With": "XMLHttpRequest" },
      body: JSON.stringify(body),
    });
  } catch {
    throw networkError();
  }
  // A bare 2xx with no body is still a success (send-code answers 200 always).
  const text = await res.text().catch(() => "");
  if (res.ok && text.trim() === "") return;
  await readEnvelope<unknown>(new Response(text, { status: res.status, statusText: res.statusText }));
}

/** Ask svc-iam to mail a reset code. */
export function sendResetCode(email: string): Promise<void> {
  return postAuth(SEND_CODE_PATH, { email: email.trim() });
}

/** Reset the password with the mailed code. */
export function resetPassword(email: string, code: string, password: string): Promise<void> {
  return postAuth(RESET_PATH, { email: email.trim(), code: code.trim(), password });
}

/** Whether a reset failure means the code itself was wrong or expired. */
export function isInvalidCode(e: unknown): boolean {
  return (e as { code?: string } | null)?.code === "Auth.InvalidCode";
}

/** User-facing message for a failed send-code / reset call. */
export function forgotErrorMessage(e: unknown): string {
  const err = (e ?? {}) as { message?: string; code?: string; status?: number };
  if (isInvalidCode(e)) return "验证码错误或已过期,请检查或重新获取";
  // svc-iam caps the guesses against one code; a fresh code resets the count.
  if (err.code === "Auth.TooManyAttempts") return "验证码尝试过于频繁,请重新获取验证码后再试";
  if (err.status === 429 || err.code === "Common.TooManyRequests") return "操作过于频繁,请稍后再试";
  return err.message || "请求失败,请稍后再试";
}
