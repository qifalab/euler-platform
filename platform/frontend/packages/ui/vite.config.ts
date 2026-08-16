import { resolve } from "node:path";
import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";

export default defineConfig({
  plugins: [vue()],
  build: {
    lib: {
      entry: resolve(__dirname, "src/index.ts"),
      formats: ["es", "cjs"],
      fileName: (format) => (format === "es" ? "index.js" : "index.cjs"),
    },
    rollupOptions: {
      // Shared layer — Vue & Element Plus are externals, injected by the base
      // via shared-manifest.json (02§6.5). Never bundle a second Vue.
      external: ["vue", "element-plus", "@sc/tokens", "@sc/wujie-bridge"],
    },
  },
});
