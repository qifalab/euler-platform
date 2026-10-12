// Reproducible local/CI stack: real Go service + fresh SQLite + Vue + test IdP.
import { spawn, execFileSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { randomBytes } from "node:crypto";

const here = dirname(fileURLToPath(import.meta.url));
const frontend = resolve(here, "../../..");
const repo = resolve(frontend, "../..");
const work = mkdtempSync(join(tmpdir(), "euler-app-cloud-e2e-"));
const children = [];
let stopping = false;
function start(command, args, options = {}) {
  const child = spawn(command, args, { stdio: "inherit", ...options });
  children.push(child);
  child.on("error", (error) => {
    console.error(error.message);
    stop(1);
  });
  child.on("exit", (code) => {
    if (!stopping) stop(code || 1);
  });
  return child;
}
function stop(code = 0) {
  if (stopping) return;
  stopping = true;
  for (const child of children) child.kill("SIGTERM");
  setTimeout(() => {
    for (const child of children) child.kill("SIGKILL");
    rmSync(work, { recursive: true, force: true });
    process.exit(code);
  }, 1000).unref();
}
process.on("SIGTERM", () => stop());
process.on("SIGINT", () => stop());
async function ready(url) {
  const end = Date.now() + 45000;
  while (Date.now() < end && !stopping) {
    try {
      if ((await fetch(url)).ok) return;
    } catch {}
    await new Promise((r) => setTimeout(r, 150));
  }
  throw new Error(`Service did not become ready: ${url}`);
}

try {
  const binary = join(work, "app-cloud");
  execFileSync(
    process.env.GO_BINARY || "go",
    ["build", "-o", binary, "./cmd/server"],
    { cwd: join(repo, "services/app-cloud"), stdio: "inherit" },
  );
  start(process.execPath, [join(here, "upstream-fixture.mjs")]);
  await ready("http://127.0.0.1:19391/healthz");
  start(binary, [], {
    env: {
      ...process.env,
      EULER_HTTP_ADDR: "127.0.0.1:19392",
      EULER_DATABASE_PATH: join(work, "cloud.sqlite"),
      EULER_ENCRYPTION_KEY: randomBytes(32).toString("base64"),
      EULER_PUBLIC_URL: "http://127.0.0.1:19390",
      EULER_PLATFORM_ADMIN_IDENTITIES: JSON.stringify([
        { provider: "http://127.0.0.1:19391", subject: "101" },
      ]),
      EULER_OIDC_ISSUER: "http://127.0.0.1:19391",
      EULER_OIDC_CLIENT_ID: "euler-browser-test",
      EULER_OIDC_CLIENT_SECRET: "test-only-client-secret",
      EULER_OIDC_REDIRECT_URL: "http://127.0.0.1:19390/auth/callback",
      EULER_ALLOW_INSECURE_LOOPBACK: "true",
      EULER_ALLOWED_ORIGINS: "http://127.0.0.1:19391",
      EULER_ALLOW_INSECURE_HTTP: "true",
      EULER_ALLOW_PRIVATE_NETWORK: "true",
      EULER_VERIFICATION_PROVIDER: "http://127.0.0.1:19391",
      EULER_EID_BASE_URL: "http://127.0.0.1:19391",
      EULER_TRUST_BASE_URL: "http://127.0.0.1:19391",
      EULER_TRUST_SCHEME_ID: "3",
    },
  });
  await ready("http://127.0.0.1:19392/healthz");
  start(
    process.execPath,
    [
      join(frontend, "node_modules/vite/bin/vite.js"),
      "--host",
      "127.0.0.1",
      "--port",
      "19390",
      "--strictPort",
    ],
    {
      cwd: join(frontend, "apps/console-base"),
      env: { ...process.env, EULER_API_ORIGIN: "http://127.0.0.1:19392" },
    },
  );
} catch (error) {
  console.error(error.message);
  stop(1);
}
