<script setup lang="ts">
/**
 * Login page (02§5.1, §5.2). account.starcloud.cn is the SSO-only auth domain.
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

async function onSubmit() {
  error.value = null;
  loading.value = true;
  try {
    await auth.login(email.value, password.value);
    // Dev: redirect to the local console shell; prod: console.starcloud.cn.
    const redirect = (route.query.redirect as string) || "http://localhost:5173/";
    window.location.href = redirect;
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
.login-page { display: flex; align-items: center; justify-content: center; min-height: calc(100vh - var(--sc-topbar-height)); padding: var(--sc-spacing-6); }
.login-card {
  background: var(--sc-glass-bg);
  -webkit-backdrop-filter: var(--sc-glass-blur);
  backdrop-filter: var(--sc-glass-blur);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-xl);
  box-shadow: var(--sc-glass-shadow);
  padding: 40px;
  width: 100%;
  max-width: 400px;
}
.login-title { font-size: 24px; margin: 0 0 var(--sc-spacing-2); text-align: center; color: var(--sc-text-primary); }
.login-sub { font-size: 13px; color: var(--sc-text-secondary); text-align: center; margin: 0 0 var(--sc-spacing-7); }
.login-form { display: flex; flex-direction: column; gap: var(--sc-spacing-4); }
.login-field { display: flex; flex-direction: column; gap: 6px; font-size: 13px; color: var(--sc-text-secondary); }
.login-field input {
  padding: 10px 12px;
  background: var(--sc-bg-container);
  border: 1px solid var(--sc-border);
  border-radius: var(--sc-radius-md);
  font-size: 14px;
  color: var(--sc-text-primary);
  outline: none;
  transition: border-color var(--sc-transition), box-shadow var(--sc-transition);
}
.login-field input:focus { border-color: var(--sc-color-brand); box-shadow: 0 0 0 3px var(--sc-color-brand-soft); }
.login-error { color: var(--sc-color-danger); font-size: 13px; margin: 0; }
.login-btn {
  padding: 11px; border: none; border-radius: var(--sc-radius-md);
  background: var(--sc-color-brand); color: var(--sc-text-on-brand); font-size: 14px;
  cursor: pointer; margin-top: var(--sc-spacing-2);
  transition: background var(--sc-transition);
}
.login-btn:hover { background: var(--sc-color-brand-hover); }
.login-btn:disabled { opacity: 0.6; cursor: not-allowed; }
.login-links { display: flex; justify-content: space-between; margin-top: var(--sc-spacing-6); }
.login-links a { color: var(--sc-color-brand); text-decoration: none; font-size: 13px; }
.login-hint { margin: var(--sc-spacing-6) 0 0; padding: var(--sc-spacing-2) var(--sc-spacing-3); background: var(--sc-color-brand-soft); border-radius: var(--sc-radius-sm); font-size: 12px; color: var(--sc-text-secondary); text-align: center; }
</style>
