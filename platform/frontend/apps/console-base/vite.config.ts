import { defineConfig, loadEnv } from "vite";
import vue from "@vitejs/plugin-vue";
import { resolve } from "node:path";
export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), "");
  const target =
    process.env.EULER_API_ORIGIN ||
    env.EULER_API_ORIGIN ||
    "http://localhost:8080";
  return {
    plugins: [vue()],
    resolve: { alias: { "@": resolve(__dirname, "src") } },
    server: {
      port: 5173,
      proxy: {
        "/api": { target, changeOrigin: true },
        "/auth": { target, changeOrigin: true },
        "/public": { target, changeOrigin: true },
      },
    },
  };
});
