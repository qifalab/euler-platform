<script setup lang="ts">
/**
 * Login page (02§5.1, §5.2). account.euler.emoera.com is the SSO-only auth domain.
 * On success: writes token to the account store + broadcasts to the parent
 * shell via the bridge, then redirects to the console (or ?redirect target).
 * MFA accounts (07§2.4): the password step only yields a challenge; a second
 * step collects the authenticator code (POST /api/auth/mfa/verify).
 */
import { ref } from "vue";
import { useRoute } from "vue-router";
import { useAccountAuth } from "../stores/auth";
import { safeRedirect } from "../safeRedirect";

const route = useRoute();
const auth = useAccountAuth();

const email = ref("");
const password = ref("");
const code = ref("");
const loading = ref(false);
const error = ref<string | null>(null);

// Dev hint: never render real credentials — the seeded dev account lives in
// the project docs (02§2.4 / 部署文档), not in the page source.
const DEV_SEED_HINT = "开发环境测试账号请查阅项目部署文档";

// Only a same-origin, same-site (*.euler.emoera.com) or dev-loopback target
// may be used; see safeRedirect.
function finish() {
  window.location.href = safeRedirect(route.query.redirect);
}

async function onSubmit() {
  error.value = null;
  loading.value = true;
  try {
    const result = await auth.login(email.value, password.value);
    if (result.mfaRequired) {
      code.value = ""; // the MFA step renders while auth.mfaPending
      return;
    }
    finish();
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}

async function onVerify() {
  error.value = null;
  if (!/^\d{6}$/.test(code.value.trim())) {
    error.value = "请输入 6 位动态验证码";
    return;
  }
  loading.value = true;
  try {
    await auth.verifyMfa(code.value);
    finish();
  } catch (e) {
    // Wrong / reused code or throttling: stay on this step. An expired or
    // revoked challenge clears auth.mfaPending, which returns the user to the
    // password step with this message.
    error.value = (e as Error).message;
    code.value = "";
    if (!auth.mfaPending) password.value = "";
  } finally {
    loading.value = false;
  }
}

function backToPassword() {
  auth.cancelMfa();
  code.value = "";
  password.value = "";
  error.value = null;
}
</script>

<template>
  <div class="login-page">
    <div class="login-card">
      <h1 class="login-title">登录辰云</h1>
      <p class="login-sub">登录后可访问控制台、费用中心与工单支持</p>

      <form v-if="!auth.mfaPending" class="login-form" @submit.prevent="onSubmit">
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

      <form v-else class="login-form" @submit.prevent="onVerify">
        <p class="login-mfa-tip">
          账号 {{ auth.mfa?.user.name }} 已开启 MFA,请输入身份验证器中的 6 位动态验证码
        </p>
        <label class="login-field">
          <span>动态验证码</span>
          <input
            v-model="code"
            inputmode="numeric"
            autocomplete="one-time-code"
            maxlength="6"
            placeholder="6 位数字"
            required
          />
        </label>

        <p v-if="error" class="login-error">{{ error }}</p>

        <button class="login-btn" type="submit" :disabled="loading">
          {{ loading ? "验证中…" : "验证" }}
        </button>
        <button class="login-back" type="button" :disabled="loading" @click="backToPassword">返回重新输入密码</button>
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
.login-mfa-tip { margin: 0; font-size: 13px; color: var(--eu-text-secondary); line-height: 1.5; }
.login-back { padding: 6px; border: none; background: none; color: var(--eu-color-brand); font-size: 13px; cursor: pointer; }
.login-back:disabled { opacity: 0.6; cursor: not-allowed; }
.login-links { display: flex; justify-content: space-between; margin-top: var(--eu-spacing-6); }
.login-links a { color: var(--eu-color-brand); text-decoration: none; font-size: 13px; }
.login-hint { margin: var(--eu-spacing-6) 0 0; padding: var(--eu-spacing-2) var(--eu-spacing-3); background: var(--eu-color-brand-soft); border-radius: var(--eu-radius-sm); font-size: 12px; color: var(--eu-text-secondary); text-align: center; }
</style>
