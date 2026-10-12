/**
 * Cross-app navigation from a sub-app to a shell (first-level) route (02§2.2).
 *
 * Product sub-apps run in history mode inside the Wujie sandbox, so a hash like
 * `location.hash = "#/billing"` only edits the sub-app's own URL fragment — the
 * shell never sees it and nothing happens. The base injects a `navigate` prop
 * (SharedProps.navigate) for this; without one, a shell route is reached by
 * navigating the shell window itself (same origin as
 * the sandbox iframe): the base boots on that path and its router hands it to
 * the owning sub-app (e.g. /billing → web-billing).
 */
import { inWujieSandbox, readSharedProps } from "@eu/wujie-bridge";

type ShellWindow = Pick<Window, "location">;

/**
 * Navigate the console shell to `path` (e.g. "/billing"). Prefers a
 * shell-injected `navigate` prop when the base provides one, else navigates the
 * parent (shell) window. Returns false when there is no shell to navigate —
 * the sub-app was opened standalone, or the parent is not reachable — so the
 * caller can tell the user where to go instead of silently doing nothing.
 */
export function navigateShell(path: string): boolean {
  const props = readSharedProps();
  if (typeof props.navigate === "function") {
    props.navigate(path);
    return true;
  }
  if (!inWujieSandbox()) return false;
  try {
    // Wujie runs sub-app scripts in a same-origin iframe: window.parent is the
    // shell. Fall back to top for nested hosts.
    const shell = (window.parent && window.parent !== window ? window.parent : window.top) as ShellWindow | null;
    if (!shell || (shell as unknown) === window) return false;
    shell.location.assign(path);
    return true;
  } catch {
    // Cross-origin parent (misconfigured deploy): nothing we can navigate.
    return false;
  }
}

type LinkClick = Pick<MouseEvent, "button" | "metaKey" | "ctrlKey" | "shiftKey" | "altKey" | "defaultPrevented">;

/**
 * Whether a click on an in-app list link (e.g. "/instances/x") should go
 * through the sub-app's history-mode router instead of the browser. A plain
 * <a href> click there made the browser load the URL — a full reload of the
 * sandbox iframe. Only plain primary-button clicks on app paths are routed:
 * modified / middle clicks keep the browser default (new tab via the href),
 * and hash links ("#/euvpc/vpcs/x", the hash-routed sub-apps) and absolute
 * URLs never reload the app or are not ours.
 */
export function routesInApp(to: string, e: LinkClick): boolean {
  if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return false;
  return to.startsWith("/") && !to.startsWith("//");
}
