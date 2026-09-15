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
      // Shared layer externals (02§6.5). @eu/* resolve via workspace at build,
      // Vue/Element Plus are single-instance injected by the base at runtime.
      external: [
        "vue", "element-plus",
        "@eu/tokens", "@eu/sdk", "@eu/wujie-bridge", "@eu/ui",
      ],
    },
  },
});
