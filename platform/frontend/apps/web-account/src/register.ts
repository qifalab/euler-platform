/**
 * 注册 — svc-iam web-auth's sign-up endpoints:
 *   POST /api/auth/register/send-code {email}       — 200 for any valid email
 *        (whether it is already registered is never revealed); 400
 *        Common.InvalidParameter (malformed email), 429 Auth.TooManyAttempts
 *   POST /api/auth/register {email, password, code} — 201 {id, email,
 *        realNameStatus}; 400 Auth.InvalidCode (code missing / wrong / expired,
 *        or the address is already registered), 429 Auth.TooManyAttempts,
 *        409 Auth.EmailExists (legacy no-code mode only)
 * The verification code is compared server-side only. The calls share
 * forgot.ts's transport (empty-200 tolerant, envelope-checked).
 */
import { computed, getCurrentScope, onScopeDispose, ref } from "vue";
import { postAuth } from "./forgot";

export const REGISTER_SEND_CODE_PATH = "/api/auth/register/send-code";

/** Seconds before another code may be requested. */
export const CODE_COOLDOWN_SECONDS = 60;

/** A mailed verification code: 6 digits. */
export const REGISTER_CODE_PATTERN = /^\d{6}$/;

/** Shown after send-code: the same text whether or not the address can register. */
export const REGISTER_CODE_SENT_MESSAGE = "验证码已发送,请查收邮件(如未收到,请检查垃圾邮件或稍后重新获取)";

/** Ask svc-iam to mail a sign-up code. */
export function sendRegisterCode(email: string): Promise<void> {
  return postAuth(REGISTER_SEND_CODE_PATH, { email: email.trim() });
}

interface ErrLike { message?: string; code?: string; status?: number }

/** Whether the code was rejected (wrong / expired / missing, or the address is taken). */
export function isRegisterCodeInvalid(e: unknown): boolean {
  return ((e ?? {}) as ErrLike).code === "Auth.InvalidCode";
}

/** Whether a failure was a rate limit (429) — the send button should cool down too. */
export function isRateLimited(e: unknown): boolean {
  const err = (e ?? {}) as ErrLike;
  return err.status === 429 || err.code === "Auth.TooManyAttempts" || err.code === "Common.TooManyRequests";
}

/** User-facing message for a failed send-code / register call. */
export function registerErrorMessage(e: unknown, step: "send" | "register" = "register"): string {
  const err = (e ?? {}) as ErrLike;
  // svc-iam answers InvalidCode also for an address that is already registered,
  // so the copy points at login / password reset without confirming either.
  if (isRegisterCodeInvalid(e)) return "验证码错误或已过期,请检查或重新获取;如该邮箱已注册,请直接登录或找回密码";
  if (err.code === "Auth.EmailExists") return "该邮箱已注册,请直接登录或找回密码";
  if (isRateLimited(e)) {
    return step === "send" ? "验证码发送过于频繁,请稍后再试" : "验证码尝试次数过多,请重新获取验证码后再试";
  }
  if (err.code === "Common.InvalidParameter" && step === "send") return "邮箱格式不正确";
  return err.message || (step === "send" ? "验证码发送失败,请稍后重试" : "注册失败,请稍后重试");
}

/** The resend countdown of a "获取验证码" button. */
export function useCodeCooldown(seconds = CODE_COOLDOWN_SECONDS) {
  const remaining = ref(0);
  const sentOnce = ref(false);
  let timer: ReturnType<typeof setInterval> | null = null;

  function stop() {
    if (timer) {
      clearInterval(timer);
      timer = null;
    }
    remaining.value = 0;
  }

  function start() {
    stop();
    sentOnce.value = true;
    remaining.value = seconds;
    timer = setInterval(() => {
      remaining.value -= 1;
      if (remaining.value <= 0) stop();
    }, 1000);
  }

  // Leaving the page mid-countdown must stop the ticker.
  if (getCurrentScope()) onScopeDispose(stop);

  const canSend = computed(() => remaining.value === 0);
  const label = computed(() => {
    if (remaining.value > 0) return `${remaining.value}s 后重新获取`;
    return sentOnce.value ? "重新获取" : "获取验证码";
  });
  return { remaining, canSend, label, start, stop };
}
