import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e/app-cloud",
  timeout: 60_000,
  expect: { timeout: 10_000 },
  workers: 1,
  retries: 0,
  reporter: [["list"]],
  outputDir: "test-results/app-cloud",
  use: {
    baseURL: "http://127.0.0.1:19390",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    browserName: "chromium",
    launchOptions: process.env.PLAYWRIGHT_EXECUTABLE_PATH
      ? { executablePath: process.env.PLAYWRIGHT_EXECUTABLE_PATH }
      : {},
  },
  webServer: {
    command: "node e2e/app-cloud/support/run-stack.mjs",
    url: "http://127.0.0.1:19390",
    timeout: 180_000,
    reuseExistingServer: false,
  },
});
