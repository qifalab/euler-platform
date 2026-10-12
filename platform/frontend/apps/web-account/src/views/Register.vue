<script setup lang="ts">
/**
 * Register page (02§5.1). account.euler.emoera.com public signup route.
 * Collects email + mailed verification code + password, validates
 * confirmation match and the service-agreement consent, then on success shows
 * a success toast and redirects to /realname to complete identity verification.
 * The code comes from POST /api/auth/register/send-code (60s resend cooldown)
 * and is checked server-side only (see ../register.ts for the error mapping).
 */
import { reactive, ref } from "vue";
import { useRouter } from "vue-router";
import { useAccountAuth } from "../stores/auth";
import { isValidLoginEmail, PASSWORD_MAX, PASSWORD_MIN, validatePassword } from "../validation";
import {
  isRateLimited,
  isRegisterCodeInvalid,
  REGISTER_CODE_PATTERN,
  REGISTER_CODE_SENT_MESSAGE,
  registerErrorMessage,
  sendRegisterCode,
  useCodeCooldown,
} from "../register";
import {
  ElForm,
  ElFormItem,
  ElInput,
  ElButton,
  ElCheckbox,
  ElMessage,
  type FormInstance,
  type FormRules,
} from "element-plus";

const router = useRouter();
const auth = useAccountAuth();

const formRef = ref<FormInstance>();

interface RegisterForm {
  account: string;
  code: string;
  password: string;
  confirm: string;
  agreed: boolean;
}

const form = reactive<RegisterForm>({
  account: "",
  code: "",
  password: "",
  confirm: "",
  agreed: false,
});

// svc-iam registers by email only (validLoginEmail); phone sign-up does not
// exist yet, so a phone number here only failed later with a 400.
const validateAccount = (_r: unknown, value: string, cb: (e?: Error) => void) => {
  if (!value || !value.trim()) {
    cb(new Error("请输入邮箱"));
  } else if (!isValidLoginEmail(value)) {
    cb(new Error("邮箱格式不正确"));
  } else {
    cb();
  }
};

const validateConfirm = (_r: unknown, value: string, cb: (e?: Error) => void) => {
  if (!value) {
    cb(new Error("请再次输入密码"));
  } else if (value !== form.password) {
    cb(new Error("两次输入的密码不一致"));
  } else {
    cb();
  }
};

const validateAgreed = (_r: unknown, value: boolean, cb: (e?: Error) => void) => {
  if (!value) {
    cb(new Error("请阅读并同意服务协议"));
  } else {
    cb();
  }
};

const validateCode = (_r: unknown, value: string, cb: (e?: Error) => void) => {
  if (!value || !value.trim()) {
    cb(new Error("请输入邮箱验证码"));
  } else if (!REGISTER_CODE_PATTERN.test(value.trim())) {
    cb(new Error("验证码为 6 位数字"));
  } else {
    cb();
  }
};

const rules = reactive<FormRules>({
  account: [{ validator: validateAccount, trigger: "blur" }],
  code: [{ validator: validateCode, trigger: "blur" }],
  // Same bounds as the backend (8-128, measured in UTF-8 bytes).
  password: [{ required: true, validator: validatePassword, trigger: "blur" }],
  confirm: [{ validator: validateConfirm, trigger: "blur" }],
  agreed: [{ validator: validateAgreed, trigger: "change" }],
});

const loading = ref(false);

// 获取验证码: validate the email alone, then mail a code (60s cooldown; a 429
// cools the button down too, so it cannot hammer the limiter).
const sending = ref(false);
const cooldown = useCodeCooldown();
const { canSend, label: sendLabel } = cooldown;

async function onSendCode() {
  if (!canSend.value || sending.value || !formRef.value) return;
  try {
    await formRef.value.validateField("account");
  } catch {
    return;
  }
  sending.value = true;
  try {
    await sendRegisterCode(form.account);
    cooldown.start();
    ElMessage.success(REGISTER_CODE_SENT_MESSAGE);
  } catch (e) {
    if (isRateLimited(e)) cooldown.start();
    ElMessage.error(registerErrorMessage(e, "send"));
  } finally {
    sending.value = false;
  }
}

async function onSubmit() {
  if (!formRef.value) return;
  try {
    await formRef.value.validate();
  } catch {
    return; // validation messages already shown by ElForm
  }
  loading.value = true;
  try {
    // Real backend: POST /api/auth/register (svc-iam web-auth). Then auto-login
    // so the user can proceed to real-name verification in an authenticated state.
    const email = form.account.trim();
    try {
      await auth.register(email, form.password, form.code);
    } catch (e) {
      // Wrong / expired code: clear it so a fresh one is entered.
      if (isRegisterCodeInvalid(e)) form.code = "";
      ElMessage.error(registerErrorMessage(e));
      return;
    }
    await auth.login(email, form.password);
    ElMessage.success("注册成功,请完成实名认证");
    router.push("/realname");
  } catch (e) {
    ElMessage.error((e as Error).message || "注册失败");
  } finally {
    loading.value = false;
  }
}
</script>

<template>
  <div class="register-page">
    <div class="register-card">
      <h1 class="register-title">注册辰云账号</h1>
      <p class="register-sub">注册后即可登录控制台、管理资源与费用</p>

      <el-form
        ref="formRef"
        :model="form"
        :rules="rules"
        label-position="top"
        class="register-form"
        @submit.prevent="onSubmit"
      >
        <el-form-item label="邮箱" prop="account">
          <el-input
            v-model="form.account"
            type="email"
            placeholder="请输入邮箱"
            size="large"
            clearable
          />
        </el-form-item>

        <el-form-item label="邮箱验证码" prop="code">
          <div class="code-row">
            <el-input
              v-model="form.code"
              placeholder="请输入 6 位验证码"
              maxlength="6"
              inputmode="numeric"
              size="large"
            />
            <el-button size="large" :loading="sending" :disabled="!canSend" @click="onSendCode">
              {{ sendLabel }}
            </el-button>
          </div>
        </el-form-item>

        <el-form-item label="密码" prop="password">
          <el-input
            v-model="form.password"
            type="password"
            :placeholder="`${PASSWORD_MIN}-${PASSWORD_MAX} 位密码`"
            size="large"
            show-password
          />
        </el-form-item>

        <el-form-item label="确认密码" prop="confirm">
          <el-input
            v-model="form.confirm"
            type="password"
            placeholder="请再次输入密码"
            size="large"
            show-password
          />
        </el-form-item>

        <el-form-item prop="agreed" class="register-agree">
          <el-checkbox v-model="form.agreed">
            阅读并同意
            <a class="register-link-inline" href="#" @click.prevent>《辰云服务协议》</a>
          </el-checkbox>
        </el-form-item>

        <el-button
          type="primary"
          size="large"
          class="register-btn"
          :loading="loading"
          @click="onSubmit"
        >
          {{ loading ? "注册中…" : "注册" }}
        </el-button>
      </el-form>

      <div class="register-links">
        <router-link to="/login">已有账号?返回登录</router-link>
      </div>
    </div>
  </div>
</template>

<style scoped>
.register-page {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: calc(100vh - var(--eu-topbar-height));
  padding: var(--eu-spacing-6);
}
.register-card {
  background: var(--eu-glass-bg);
  -webkit-backdrop-filter: var(--eu-glass-blur);
  backdrop-filter: var(--eu-glass-blur);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-xl);
  box-shadow: var(--eu-glass-shadow);
  padding: 40px;
  width: 100%;
  max-width: 440px;
}
.register-title {
  font-size: 24px;
  margin: 0 0 8px;
  text-align: center;
  color: var(--eu-text-primary);
}
.register-sub {
  font-size: var(--eu-font-size-sm);
  color: var(--eu-text-secondary);
  text-align: center;
  margin: 0 0 var(--eu-spacing-7);
}
.register-form :deep(.el-form-item) {
  margin-bottom: var(--eu-spacing-4);
}
.register-form :deep(.el-form-item__label) {
  font-size: var(--eu-font-size-sm);
  color: var(--eu-text-secondary);
  padding-bottom: var(--eu-spacing-1);
}
.register-form :deep(.el-input__wrapper) {
  border-radius: var(--eu-radius-sm);
}
.code-row {
  display: flex;
  gap: var(--eu-spacing-3);
  width: 100%;
}
.code-row :deep(.el-input) {
  flex: 1;
}
.register-agree :deep(.el-form-item__content) {
  line-height: 1.6;
}
.register-link-inline {
  color: var(--eu-color-brand);
  text-decoration: none;
}
.register-link-inline:hover {
  color: var(--eu-color-brand-hover);
}
.register-btn {
  width: 100%;
  margin-top: var(--eu-spacing-2);
}
.register-links {
  margin-top: var(--eu-spacing-6);
  text-align: center;
}
.register-links a {
  color: var(--eu-color-brand);
  text-decoration: none;
  font-size: var(--eu-font-size-sm);
}
.register-links a:hover {
  color: var(--eu-color-brand-hover);
}
</style>
