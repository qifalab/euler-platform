// Playwright config — e2e for the three golden paths (02§9.4):
//  1. console overview renders real BFF data
//  2. login → real-name gate (SSO closed loop)
//  3. sub-app loads via Wujie cross-origin
import { defineConfig } from "@playwright/test";

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
        launchOptions: {
          executablePath: "C:/Users/wjy13/AppData/Local/ms-playwright/chromium-1223/chrome-win64/chrome.exe",
        },
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
