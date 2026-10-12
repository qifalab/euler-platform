// Playwright config — e2e for the three golden paths (02§9.4):
//  1. console overview renders real BFF data
//  2. login → real-name gate (SSO closed loop)
//  3. sub-app loads via Wujie cross-origin
import { defineConfig } from "@playwright/test";

// Playwright's own downloaded Chromium is used by default. To run against a
// specific browser binary, set PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH (never a
// machine-specific path in the repo).
const chromiumExecutable = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH;

export default defineConfig({
  testDir: "./e2e",
  timeout: 30_000,
  retries: 0,
  use: {
    baseURL: "http://localhost:5173",
    headless: true,
    trace: "on-first-retry",
  },
  projects: [
    {
      name: "chromium",
      use: {
        browserName: "chromium",
        launchOptions: chromiumExecutable ? { executablePath: chromiumExecutable } : {},
      },
    },
  ],
  webServer: {
    command: "pnpm dev",
    port: 5173,
    reuseExistingServer: true,
    timeout: 60_000,
  },
});
