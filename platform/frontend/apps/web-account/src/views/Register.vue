<script setup lang="ts">
/**
 * Register page (02§5.1). account.euler.emoera.com public signup route.
 * Collects email/phone + password, validates confirmation match and the
 * service-agreement consent, then on success shows a success toast and
 * redirects to /realname to complete identity verification.
 */
import { reactive, ref } from "vue";
import { useRouter } from "vue-router";
import { useAccountAuth } from "../stores/auth";
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
  password: string;
  confirm: string;
  agreed: boolean;
}

const form = reactive<RegisterForm>({
  account: "",
  password: "",
  confirm: "",
  agreed: false,
});

/** Email or China mobile phone pattern (02§5.1 multi-identifier login). */
const accountRe =
  /^(?:[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}|1[3-9]\d{9})$/;

const validateAccount = (_r: unknown, value: string, cb: (e?: Error) => void) => {
  if (!value) {
    cb(new Error("请输入邮箱或手机号"));
  } else if (!accountRe.test(value)) {
    cb(new Error("邮箱格式不正确，或输入 11 位手机号"));
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

const rules = reactive<FormRules>({
  account: [{ validator: validateAccount, trigger: "blur" }],
  password: [
    { required: true, message: "请输入密码", trigger: "blur" },
    { min: 8, max: 32, message: "密码长度为 8-32 位", trigger: "blur" },
  ],
  confirm: [{ validator: validateConfirm, trigger: "blur" }],
  agreed: [{ validator: validateAgreed, trigger: "change" }],
});

const loading = ref(false);

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
    await auth.register(form.account, form.password);
    await auth.login(form.account, form.password);
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
        <el-form-item label="邮箱/手机号" prop="account">
          <el-input
            v-model="form.account"
            placeholder="请输入邮箱或手机号"
            size="large"
            clearable
          />
        </el-form-item>

        <el-form-item label="密码" prop="password">
          <el-input
            v-model="form.password"
            type="password"
            placeholder="8-32 位密码"
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
