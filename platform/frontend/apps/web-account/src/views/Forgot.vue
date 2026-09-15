<script setup lang="ts">
/**
 * 找回密码页（Forgot）
 *
 * 实现《02-frontend-architecture.md》「账号中心视图 — 找回密码流程」一节：
 * 邮箱校验 → 验证码校验 → 重置密码 → 回到登录。
 * 验证码由后端下发并在后端比对（前端绝不比对验证码）；后端接口未上线时,
 * 提交会得到「暂未开放」提示。
 */
import { reactive, ref, computed, onBeforeUnmount } from "vue";
import { useRouter } from "vue-router";
import {
  ElSteps,
  ElStep,
  ElForm,
  ElFormItem,
  ElInput,
  ElButton,
  ElMessage,
  type FormInstance,
  type FormRules,
} from "element-plus";

const router = useRouter();

// 步骤：0 验证邮箱 / 1 重置密码 / 2 完成
const active = ref(0);

// 倒计时（秒）
const countdown = ref(0);
let timer: ReturnType<typeof setInterval> | null = null;

interface EmailForm {
  email: string;
}
interface ResetForm {
  code: string;
  password: string;
  confirm: string;
}

const emailFormRef = ref<FormInstance>();
const emailForm = reactive<EmailForm>({ email: "" });

const resetFormRef = ref<FormInstance>();
const resetForm = reactive<ResetForm>({ code: "", password: "", confirm: "" });

const sending = ref(false);
const resetting = ref(false);

const canResend = computed(() => countdown.value === 0);

const emailRules: FormRules = {
  email: [
    { required: true, message: "请输入邮箱地址", trigger: "blur" },
    { type: "email", message: "邮箱格式不正确", trigger: ["blur", "change"] },
  ],
};

const resetRules: FormRules = {
  code: [
    { required: true, message: "请输入验证码", trigger: "blur" },
    { len: 6, message: "验证码为 6 位数字", trigger: "blur" },
  ],
  password: [
    { required: true, message: "请输入新密码", trigger: "blur" },
    { min: 8, max: 20, message: "密码长度 8-20 位", trigger: "blur" },
  ],
  confirm: [
    { required: true, message: "请再次输入密码", trigger: "blur" },
    {
      validator: (_rule: unknown, value: string, callback: (error?: Error) => void) => {
        if (value !== resetForm.password) {
          callback(new Error("两次输入的密码不一致"));
        } else {
          callback();
        }
      },
      trigger: ["blur", "change"],
    },
  ],
};

function startCountdown() {
  countdown.value = 60;
  timer = setInterval(() => {
    countdown.value -= 1;
    if (countdown.value <= 0) {
      countdown.value = 0;
      if (timer) {
        clearInterval(timer);
        timer = null;
      }
    }
  }, 1000);
}

// Leaving the page mid-countdown must stop the ticker: a component-unmounted
// interval keeps writing to a dead ref (leak) and would resume on re-entry.
onBeforeUnmount(() => {
  if (timer) {
    clearInterval(timer);
    timer = null;
  }
});

/** 统一调用找回密码后端接口；接口尚未实现时抛出「暂未开放」。 */
async function forgotApi(path: string, body: Record<string, unknown>): Promise<void> {
  let res: Response;
  try {
    res = await fetch(path, {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json", "X-Requested-With": "XMLHttpRequest" },
      body: JSON.stringify(body),
    });
  } catch {
    throw new Error("网络异常，请稍后再试");
  }
  if (res.status === 404 || res.status === 405 || res.status === 501) {
    throw new Error("找回密码功能暂未开放，请联系管理员重置密码");
  }
  const payload = await res.json().catch(() => ({} as { Code?: string; Message?: string }));
  if (!res.ok || (payload.Code && payload.Code !== "OK")) {
    throw new Error(payload.Message ?? "请求失败，请稍后再试");
  }
}

async function handleSendCode() {
  const valid = await emailFormRef.value?.validate().catch(() => false);
  if (!valid) return;
  sending.value = true;
  try {
    // 验证码由 svc-iam 下发到注册邮箱；是否已注册也由后端判断。
    await forgotApi("/api/auth/forgot/send-code", { email: emailForm.email });
    startCountdown();
    active.value = 1;
    ElMessage.success("验证码已发送至邮箱");
  } catch (e) {
    ElMessage.error((e as Error).message);
  } finally {
    sending.value = false;
  }
}

async function handleResend() {
  if (!canResend.value) return;
  try {
    await forgotApi("/api/auth/forgot/send-code", { email: emailForm.email });
    startCountdown();
    ElMessage.success("验证码已发送至邮箱");
  } catch (e) {
    ElMessage.error((e as Error).message);
  }
}

async function handleReset() {
  const valid = await resetFormRef.value?.validate().catch(() => false);
  if (!valid) return;
  resetting.value = true;
  try {
    // 验证码校验在后端完成；前端只透传。
    await forgotApi("/api/auth/forgot/reset", {
      email: emailForm.email,
      code: resetForm.code,
      password: resetForm.password,
    });
    active.value = 2;
    ElMessage.success("密码已重置");
    setTimeout(() => {
      router.push("/login");
    }, 800);
  } catch (e) {
    ElMessage.error((e as Error).message);
  } finally {
    resetting.value = false;
  }
}

function goLogin() {
  router.push("/login");
}
</script>

<template>
  <div class="forgot-page">
    <div class="forgot-card">
      <div class="forgot-header">
        <h2 class="forgot-title">找回密码</h2>
        <p class="forgot-subtitle">通过注册邮箱重置您的 Euler 账户密码</p>
      </div>

      <ElSteps :active="active" align-center class="forgot-steps">
        <ElStep title="验证邮箱" />
        <ElStep title="重置密码" />
        <ElStep title="完成" />
      </ElSteps>

      <!-- 步骤 1：邮箱校验 -->
      <ElForm
        v-if="active === 0"
        ref="emailFormRef"
        :model="emailForm"
        :rules="emailRules"
        label-position="top"
        class="forgot-form"
        @submit.prevent
      >
        <ElFormItem label="注册邮箱" prop="email">
          <ElInput
            v-model="emailForm.email"
            type="email"
            placeholder="请输入注册邮箱"
            clearable
            size="large"
          />
        </ElFormItem>
        <ElButton
          type="primary"
          size="large"
          class="forgot-submit"
          :loading="sending"
          @click="handleSendCode"
        >
          发送验证码
        </ElButton>
      </ElForm>

      <!-- 步骤 2：重置密码 -->
      <ElForm
        v-else-if="active === 1"
        ref="resetFormRef"
        :model="resetForm"
        :rules="resetRules"
        label-position="top"
        class="forgot-form"
        @submit.prevent
      >
        <ElFormItem label="验证码" prop="code">
          <div class="code-row">
            <ElInput
              v-model="resetForm.code"
              placeholder="请输入 6 位验证码"
              maxlength="6"
              size="large"
            />
            <ElButton
              size="large"
              :disabled="!canResend"
              @click="handleResend"
            >
              {{ canResend ? "重新发送" : `${countdown}s 后重发` }}
            </ElButton>
          </div>
        </ElFormItem>
        <ElFormItem label="新密码" prop="password">
          <ElInput
            v-model="resetForm.password"
            type="password"
            placeholder="8-20 位新密码"
            show-password
            size="large"
          />
        </ElFormItem>
        <ElFormItem label="确认密码" prop="confirm">
          <ElInput
            v-model="resetForm.confirm"
            type="password"
            placeholder="请再次输入新密码"
            show-password
            size="large"
          />
        </ElFormItem>
        <div class="action-row">
          <ElButton size="large" @click="active = 0">上一步</ElButton>
          <ElButton
            type="primary"
            size="large"
            class="forgot-submit"
            :loading="resetting"
            @click="handleReset"
          >
            重置密码
          </ElButton>
        </div>
      </ElForm>

      <!-- 步骤 3：完成 -->
      <div v-else class="forgot-done">
        <div class="done-icon">
          <svg viewBox="0 0 24 24" fill="none" xmlns="http://www.w3.org/2000/svg">
            <path
              d="M12 21C16.9706 21 21 16.9706 21 12C21 7.02944 16.9706 3 12 3C7.02944 3 3 7.02944 3 12C3 16.9706 7.02944 21 12 21Z"
              fill="currentColor"
              fill-opacity="0.15"
            />
            <path
              d="M8.5 12.5L11 15L15.5 9.5"
              stroke="currentColor"
              stroke-width="2"
              stroke-linecap="round"
              stroke-linejoin="round"
            />
          </svg>
        </div>
        <p class="done-text">密码已重置，正在跳转登录…</p>
        <ElButton text type="primary" @click="goLogin">立即登录</ElButton>
      </div>

      <div class="forgot-footer">
        <ElButton text type="primary" @click="goLogin">返回登录</ElButton>
      </div>
    </div>
  </div>
</template>

<style scoped>
.forgot-page {
  min-height: calc(100vh - var(--eu-topbar-height));
  display: flex;
  align-items: center;
  justify-content: center;
  padding: var(--eu-spacing-6);
  box-sizing: border-box;
}

.forgot-card {
  width: 100%;
  max-width: 440px;
  background: var(--eu-glass-bg);
  -webkit-backdrop-filter: var(--eu-glass-blur);
  backdrop-filter: var(--eu-glass-blur);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-xl);
  box-shadow: var(--eu-glass-shadow);
  padding: var(--eu-spacing-7) var(--eu-spacing-6);
}

.forgot-header {
  text-align: center;
  margin-bottom: var(--eu-spacing-6);
}

.forgot-title {
  margin: 0 0 var(--eu-spacing-2);
  font-size: 22px;
  font-weight: 600;
  color: var(--eu-text-primary);
}

.forgot-subtitle {
  margin: 0;
  font-size: 13px;
  line-height: 1.6;
  color: var(--eu-text-secondary);
}

.forgot-steps {
  margin-bottom: var(--eu-spacing-7);
}

.forgot-form {
  margin-top: var(--eu-spacing-2);
}

.forgot-submit {
  width: 100%;
}

.action-row {
  display: flex;
  gap: var(--eu-spacing-3);
}

.action-row .forgot-submit {
  flex: 1;
}

.code-row {
  display: flex;
  gap: var(--eu-spacing-3);
  width: 100%;
}

.code-row :deep(.el-input) {
  flex: 1;
}

.forgot-done {
  text-align: center;
  padding: var(--eu-spacing-4) 0;
}

.done-icon {
  width: 64px;
  height: 64px;
  margin: 0 auto var(--eu-spacing-4);
  border-radius: 50%;
  background: var(--eu-color-success-soft);
  color: var(--eu-color-success);
  display: flex;
  align-items: center;
  justify-content: center;
}

.done-icon svg {
  width: 34px;
  height: 34px;
}

.done-text {
  margin: 0 0 var(--eu-spacing-4);
  font-size: 15px;
  color: var(--eu-text-primary);
}

.forgot-footer {
  margin-top: var(--eu-spacing-5);
  text-align: center;
}
</style>