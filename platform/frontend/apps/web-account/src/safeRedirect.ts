/**
 * Post-login / post-realname redirect validation.
 *
 * `?redirect=` is attacker-controllable, so using it raw is an open redirect: a
 * phishing link to the real login page that lands on the attacker's site
 * inherits the page's trust. Accepted targets:
 *  - same-origin paths/URLs;
 *  - https URLs on the SSO site (*.euler.emoera.com — console, billing, …),
 *    when this page itself is served from that site (the console sends users
 *    here with an absolute console URL);
 *  - dev loopback hosts, so the console shell on its own dev port keeps working.
 * Every candidate goes through URL parsing: "/\evil.com" looks like a path but
 * browsers resolve it as "//evil.com".
 */

/** Registrable site whose subdomains share the SSO session (02§5.1). */
export const SSO_SITE = "euler.emoera.com";

type Here = Pick<Location, "origin" | "hostname">;

export function isLoopbackHost(host: string): boolean {
  return host === "localhost" || host === "127.0.0.1" || host === "[::1]";
}

function onSsoSite(host: string): boolean {
  return host === SSO_SITE || host.endsWith(`.${SSO_SITE}`);
}

/** The validated target, or null when absent or not allowed. */
export function resolveSafeRedirect(raw: unknown, here: Here = window.location): string | null {
  if (typeof raw !== "string" || raw.trim() === "") return null;
  let url: URL;
  try {
    url = new URL(raw, here.origin);
  } catch {
    return null;
  }
  if (url.origin === here.origin) {
    // Keep same-origin targets relative (no scheme/host to tamper with).
    return `${url.pathname}${url.search}${url.hash}`;
  }
  if (url.protocol === "https:" && onSsoSite(url.hostname) && onSsoSite(here.hostname)) {
    return url.toString();
  }
  if (
    isLoopbackHost(here.hostname) &&
    isLoopbackHost(url.hostname) &&
    (url.protocol === "http:" || url.protocol === "https:")
  ) {
    return url.toString();
  }
  return null;
}

/** Validated target, or the console home when there is none / it is refused. */
export function safeRedirect(raw: unknown, here: Here = window.location): string {
  const target = resolveSafeRedirect(raw, here);
  if (target) return target;
  if (isLoopbackHost(here.hostname)) return "http://localhost:5173/";
  if (onSsoSite(here.hostname)) return `https://console.${SSO_SITE}/`;
  return "/";
}
