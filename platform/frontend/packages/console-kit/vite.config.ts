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
      // Vite 6 names the extracted stylesheet after the package
      // (console-kit.css); package.json exports ./dist/style.css, so pin it.
      cssFileName: "style",
    },
    rollupOptions: {
      // Shared layer externals (02§6.5). @eu/* resolve via workspace at build,
      // Vue/Element Plus are single-instance injected by the base at runtime.
      // vue-router MUST stay external too: a bundled copy carries its own
      // injection-key Symbol, so useRouter()/inject(routerKey) in the kit
      // never found the app's router (InstanceListView crashed on
      // router.resolve).
      external: [
        "vue", "vue-router", "element-plus",
        "@eu/tokens", "@eu/sdk", "@eu/wujie-bridge", "@eu/ui",
      ],
    },
  },
});
