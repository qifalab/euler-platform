/**
 * Client-side mirrors of svc-iam web-auth's input rules, so the form rejects
 * exactly what the backend would (cmd/web-auth: validLoginEmail, password
 * length checks in register / change-password / reset).
 */

/** RFC 5321 forward-path limit (maxEmailLen). */
export const MAX_EMAIL_LEN = 254;
/** Password bounds; the backend measures the UTF-8 byte length. */
export const PASSWORD_MIN = 8;
export const PASSWORD_MAX = 128;

// dot-atom: atext runs separated by single dots (no leading/trailing/double
// dots). The backend parses a bare addr-spec (net/mail) and additionally
// requires a dot in the domain.
const ATEXT = "[A-Za-z0-9!#$%&'*+/=?^_`{|}~\\u0080-\\uFFFF-]+";
const LOGIN_EMAIL_RE = new RegExp(`^${ATEXT}(?:\\.${ATEXT})*@${ATEXT}(?:\\.${ATEXT})+$`);

/** Whether svc-iam would accept `raw` as a login email (after trim + lowercase). */
export function isValidLoginEmail(raw: string): boolean {
  const s = raw.trim().toLowerCase();
  if (s === "" || s.length > MAX_EMAIL_LEN) return false;
  return LOGIN_EMAIL_RE.test(s);
}

/** UTF-8 byte length — what Go's len() measures on the backend. */
export function utf8Length(s: string): number {
  return new TextEncoder().encode(s).length;
}

/** Error message for an out-of-range password, or null when acceptable. */
export function passwordError(value: string): string | null {
  if (!value) return "请输入密码";
  const n = utf8Length(value);
  if (n < PASSWORD_MIN || n > PASSWORD_MAX) return `密码长度为 ${PASSWORD_MIN}-${PASSWORD_MAX} 位`;
  return null;
}

type Callback = (e?: Error) => void;

/** Element Plus form-rule validator for passwords. */
export function validatePassword(_rule: unknown, value: string, cb: Callback): void {
  const msg = passwordError(value ?? "");
  if (msg) cb(new Error(msg));
  else cb();
}
