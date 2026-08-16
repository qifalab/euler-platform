/**
 * i18n setup (02§9.1 — vue-i18n, zh-CN first, en reserved).
 * Loaded in console-base main.ts via app.use(i18n). The locale is read from
 * localStorage so the user's choice persists; defaults to zh-CN.
 */
import { createI18n } from "vue-i18n";
import zhCN from "./locales/zh-CN.json";
import en from "./locales/en.json";

const saved = typeof localStorage !== "undefined" ? localStorage.getItem("sc:locale") : null;

export const i18n = createI18n({
  legacy: false,
  locale: (saved || "zh-CN") as "zh-CN" | "en",
  fallbackLocale: "zh-CN",
  messages: { "zh-CN": zhCN, en },
});

export function setLocale(locale: string) {
  i18n.global.locale.value = locale as "zh-CN" | "en";
  if (typeof localStorage !== "undefined") localStorage.setItem("sc:locale", locale);
}
