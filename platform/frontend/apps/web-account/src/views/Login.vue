<script setup lang="ts">
/**
 * Login page (02§5.1, §5.2). account.euler.emoera.com is the SSO-only auth domain.
 * On success: writes token to the account store + broadcasts to the parent
 * shell via the bridge, then redirects to the console (or ?redirect target).
 */
import { ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import { useAccountAuth } from "../stores/auth";

const route = useRoute();
const router = useRouter();
const auth = useAccountAuth();

const email = ref("");
const password = ref("");
const loading = ref(false);
const error = ref<string | null>(null);

// Dev hint: never render real credentials — the seeded dev account lives in
// the project docs (02§2.4 / 部署文档), not in the page source.
const DEV_SEED_HINT = "开发环境测试账号请查阅项目部署文档";

function isLoopbackHost(host: string): boolean {
  return host === "localhost" || host === "127.0.0.1" || host === "[::1]";
}

/**
 * Resolve the post-login target, refusing anything off-origin.
 *
 * `?redirect=` is attacker-controllable, so using it raw is an open redirect:
 * a phishing link to the real login page that lands on the attacker's site
 * inherits the page's trust. Only same-origin paths/URLs are accepted; dev
 * loopback hosts are additionally allowed so the console shell on its own dev
 * port keeps working.
 */
function safeRedirect(raw: unknown): string {
  const here = window.location;
  const loopbackDev = isLoopbackHost(here.hostname);
  const fallback = loopbackDev ? "http://localhost:5173/" : "/";
  if (typeof raw !== "string" || raw === "") return fallback;
  if (raw.startsWith("/") && !raw.startsWith("//")) return raw;
  try {
    const url = new URL(raw, here.origin);
    if (url.origin === here.origin) return url.toString();
    if (loopbackDev && isLoopbackHost(url.hostname)) return url.toString();
  } catch {
    /* malformed URL: fall through to the safe default */
  }
  return fallback;
}

async function onSubmit() {
  error.value = null;
  loading.value = true;
  try {
    await auth.login(email.value, password.value);
    // Only a same-origin (or dev-loopback) target may be used; see safeRedirect.
    window.location.href = safeRedirect(route.query.redirect);
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}
</script>

<template>
  <div class="login-page">
    <div class="login-card">
      <h1 class="login-title">登录辰云</h1>
      <p class="login-sub">登录后可访问控制台、费用中心与工单支持</p>

      <form class="login-form" @submit.prevent="onSubmit">
        <label class="login-field">
          <span>邮箱</span>
          <input v-model="email" type="email" placeholder="请输入邮箱" required />
        </label>
        <label class="login-field">
          <span>密码</span>
          <input v-model="password" type="password" placeholder="请输入密码" required />
        </label>

        <p v-if="error" class="login-error">{{ error }}</p>

        <button class="login-btn" type="submit" :disabled="loading">
          {{ loading ? "登录中…" : "登录" }}
        </button>
      </form>

      <div class="login-links">
        <router-link to="/register">注册账号</router-link>
        <router-link to="/forgot">忘记密码</router-link>
      </div>

      <p class="login-hint">{{ DEV_SEED_HINT }}</p>
    </div>
  </div>
</template>

<style scoped>
.login-page { display: flex; align-items: center; justify-content: center; min-height: calc(100vh - var(--eu-topbar-height)); padding: var(--eu-spacing-6); }
.login-card {
  background: var(--eu-glass-bg);
  -webkit-backdrop-filter: var(--eu-glass-blur);
  backdrop-filter: var(--eu-glass-blur);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-xl);
  box-shadow: var(--eu-glass-shadow);
  padding: 40px;
  width: 100%;
  max-width: 400px;
}
.login-title { font-size: 24px; margin: 0 0 var(--eu-spacing-2); text-align: center; color: var(--eu-text-primary); }
.login-sub { font-size: 13px; color: var(--eu-text-secondary); text-align: center; margin: 0 0 var(--eu-spacing-7); }
.login-form { display: flex; flex-direction: column; gap: var(--eu-spacing-4); }
.login-field { display: flex; flex-direction: column; gap: 6px; font-size: 13px; color: var(--eu-text-secondary); }
.login-field input {
  padding: 10px 12px;
  background: var(--eu-bg-container);
  border: 1px solid var(--eu-border);
  border-radius: var(--eu-radius-md);
  font-size: 14px;
  color: var(--eu-text-primary);
  outline: none;
  transition: border-color var(--eu-transition), box-shadow var(--eu-transition);
}
.login-field input:focus { border-color: var(--eu-color-brand); box-shadow: 0 0 0 3px var(--eu-color-brand-soft); }
.login-error { color: var(--eu-color-danger); font-size: 13px; margin: 0; }
.login-btn {
  padding: 11px; border: none; border-radius: var(--eu-radius-md);
  background: var(--eu-color-brand); color: var(--eu-text-on-brand); font-size: 14px;
  cursor: pointer; margin-top: var(--eu-spacing-2);
  transition: background var(--eu-transition);
}
.login-btn:hover { background: var(--eu-color-brand-hover); }
.login-btn:disabled { opacity: 0.6; cursor: not-allowed; }
.login-links { display: flex; justify-content: space-between; margin-top: var(--eu-spacing-6); }
.login-links a { color: var(--eu-color-brand); text-decoration: none; font-size: 13px; }
.login-hint { margin: var(--eu-spacing-6) 0 0; padding: var(--eu-spacing-2) var(--eu-spacing-3); background: var(--eu-color-brand-soft); border-radius: var(--eu-radius-sm); font-size: 12px; color: var(--eu-text-secondary); text-align: center; }
</style>
