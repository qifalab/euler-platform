/**
 * console-ecs — Wujie sub-app entry.
 *
 * Exports the Wujie lifecycle hooks (mount/unmount/beforeLoad/afterLoad...).
 * In the real architecture these are consumed by the Wujie bridge when the
 * base mounts this sub-app; when run standalone (independent run, degradation
 * path), the module mounts itself.
 *
 * Reference: docs/architecture/02-frontend-architecture.md §3.4.
 */
import { App } from "./App.js";

const MOUNT_ID = "app";

export const mount = (el, props) => {
  const host = typeof el === "string" ? document.querySelector(el) : el;
  const root = host || document.getElementById(MOUNT_ID);
  if (!root) {
    throw new Error("[console-ecs] mount target not found");
  }
  root.innerHTML = "";
  root.appendChild(App({ props }));
};

export const unmount = (el) => {
  const host = typeof el === "string" ? document.querySelector(el) : el;
  const root = host || document.getElementById(MOUNT_ID);
  if (root) root.innerHTML = "";
};

export const beforeLoad = (props) => {
  console.debug("[console-ecs] beforeLoad", props?.name);
  return props;
};

export const afterLoad = (props) => {
  console.debug("[console-ecs] afterLoad", props?.name);
};

// Standalone mode: if not loaded inside the Wujie sandbox, self-mount so the
// sub-app can run independently (independent-run / degradation path).
const inWujieSandbox = typeof window !== "undefined" && window.__POWERED_BY_WUJIE__;
if (!inWujieSandbox && typeof document !== "undefined") {
  document.addEventListener("DOMContentLoaded", () => {
    const el = document.getElementById(MOUNT_ID);
    if (el) mount(el, { regionId: "cn-north-1" });
  });
}
