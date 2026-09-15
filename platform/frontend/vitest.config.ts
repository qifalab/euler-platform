import { configDefaults, defineConfig } from "vitest/config";
import vue from "@vitejs/plugin-vue";

/**
 * Root Vitest config for the Euler frontend monorepo (02§10.2).
 *
 * Per-package vite.config.ts files register @vitejs/plugin-vue for builds,
 * but `vitest run` (the repo `test` script) resolves this root config first,
 * so SFC `.vue` imports in co-located *.test.ts files compile here too.
 * jsdom is the default environment; individual suites may override via the
 * `@vitest-environment` docblock (packages/sdk uses node for pure-logic tests).
 */
export default defineConfig({
  plugins: [vue()],
  test: {
    environment: "jsdom",
    // e2e/*.spec.ts are Playwright tests (run via `pnpm test:e2e`), not vitest.
    exclude: [...configDefaults.exclude, "e2e/**", "**/dist/**", "apps/site/**", "apps/docs-site/**"],
  },
});
