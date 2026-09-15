<script setup lang="ts">
/**
 * Realname authentication page (01§4.5 compliance, 02§5).
 * web-account collects KYC material before a tenant may open resources:
 * two modes — 个人 (姓名/身份证号) and 企业 (企业名称/统一社会信用代码/法人).
 * On submit the form is validated locally (mock backend); a success toast
 * is shown and the user is routed back to /profile.
 */
import { onMounted, reactive, ref } from "vue";
import { useRouter } from "vue-router";
import { ElMessage } from "element-plus";
import {
  ElForm,
  ElFormItem,
  ElInput,
  ElRadioGroup,
  ElRadio,
  ElButton,
  ElTag,
  type FormInstance,
  type FormRules,
} from "element-plus";
import { useAccountAuth } from "../stores/auth";

type AuthMode = "personal" | "enterprise";

interface PersonalForm {
  name: string;
  idCard: string;
}

interface EnterpriseForm {
  companyName: string;
  uscc: string;
  legalPerson: string;
}

const router = useRouter();
const auth = useAccountAuth();

// Real-name status from the backend (svc-iam /api/realname/status).
const status = ref<"unverified" | "pending" | "verified">("unverified");
const statusText: Record<string, string> = {
  unverified: "未认证",
  pending: "审核中",
  verified: "已认证",
};
const statusType: Record<string, "info" | "warning" | "success"> = {
  unverified: "info",
  pending: "warning",
  verified: "success",
};

const mode = ref<AuthMode>("personal");
const submitting = ref(false);

const personalForm = reactive<PersonalForm>({
  name: "",
  idCard: "",
});

const enterpriseForm = reactive<EnterpriseForm>({
  companyName: "",
  uscc: "",
  legalPerson: "",
});

const personalFormRef = ref<FormInstance>();
const enterpriseFormRef = ref<FormInstance>();

// Load the current real-name status from the backend on mount.
onMounted(async () => {
  if (!auth.isAuthenticated) return;
  try {
    const s = await auth.realnameStatus();
    status.value = s.status >= 1 ? "verified" : "unverified";
  } catch { /* show as unverified */ }
});

// CN ID card: 18 digits, last may be X (checksum elided — backend concern).
const idCardPattern = /^\d{17}[\dXx]$/;
// Unified Social Credit Code: 18 chars, alnum.
const usccPattern = /^[0-9A-Za-z]{18}$/;

const personalRules: FormRules = {
  name: [{ required: true, message: "请输入姓名", trigger: "blur" }],
  idCard: [
    { required: true, message: "请输入身份证号", trigger: "blur" },
    { pattern: idCardPattern, message: "身份证号格式不正确", trigger: "blur" },
  ],
};

const enterpriseRules: FormRules = {
  companyName: [{ required: true, message: "请输入企业名称", trigger: "blur" }],
  uscc: [
    { required: true, message: "请输入统一社会信用代码", trigger: "blur" },
    { pattern: usccPattern, message: "统一社会信用代码为18位", trigger: "blur" },
  ],
  legalPerson: [{ required: true, message: "请输入法人姓名", trigger: "blur" }],
};

async function onSubmit() {
  const formRef = mode.value === "personal" ? personalFormRef.value : enterpriseFormRef.value;
  if (!formRef) return;
  try {
    await formRef.validate();
  } catch {
    return;
  }
  submitting.value = true;
  try {
    // Real backend: POST /api/realname/verify (svc-iam). Individual and
    // enterprise submissions are validated server-side and the status updated.
    const payload = mode.value === "personal"
      ? { type: "individual" as const, name: personalForm.name, idCardNo: personalForm.idCard }
      : { type: "enterprise" as const, enterpriseName: enterpriseForm.companyName, creditCode: enterpriseForm.uscc, legalPerson: enterpriseForm.legalPerson };
    const res = await auth.verifyRealname(payload);
    status.value = res.status >= 1 ? "verified" : "pending";
    ElMessage.success("实名认证已通过,可开通资源");
    router.push("/profile");
  } catch (e) {
    ElMessage.error((e as Error).message || "实名认证提交失败");
  } finally {
    submitting.value = false;
  }
}
</script>

<template>
  <div class="realname-page">
    <div class="realname-card">
      <div class="realname-head">
        <h1 class="realname-title">实名认证</h1>
        <ElTag :type="statusType[status]" size="default" effect="light">
          {{ statusText[status] }}
        </ElTag>
      </div>
      <p class="realname-sub">
        根据《网络安全法》及平台合规要求，开通资源前需完成实名认证（01§4.5）。当前账号：
        <span class="realname-user">{{ auth.user?.name ?? "未登录" }}</span>
      </p>

      <ElRadioGroup v-model="mode" class="realname-mode">
        <ElRadio value="personal">个人认证</ElRadio>
        <ElRadio value="enterprise">企业认证</ElRadio>
      </ElRadioGroup>

      <!-- 个人 -->
      <ElForm
        v-if="mode === 'personal'"
        ref="personalFormRef"
        :model="personalForm"
        :rules="personalRules"
        label-position="top"
        class="realname-form"
      >
        <ElFormItem label="姓名" prop="name">
          <ElInput v-model="personalForm.name" placeholder="请输入真实姓名" />
        </ElFormItem>
        <ElFormItem label="身份证号" prop="idCard">
          <ElInput v-model="personalForm.idCard" placeholder="请输入18位身份证号" />
        </ElFormItem>
      </ElForm>

      <!-- 企业 -->
      <ElForm
        v-else
        ref="enterpriseFormRef"
        :model="enterpriseForm"
        :rules="enterpriseRules"
        label-position="top"
        class="realname-form"
      >
        <ElFormItem label="企业名称" prop="companyName">
          <ElInput v-model="enterpriseForm.companyName" placeholder="请输入企业全称" />
        </ElFormItem>
        <ElFormItem label="统一社会信用代码" prop="uscc">
          <ElInput v-model="enterpriseForm.uscc" placeholder="请输入18位代码" />
        </ElFormItem>
        <ElFormItem label="法定代表人" prop="legalPerson">
          <ElInput v-model="enterpriseForm.legalPerson" placeholder="请输入法人姓名" />
        </ElFormItem>
      </ElForm>

      <ElButton
        type="primary"
        :loading="submitting"
        class="realname-submit"
        @click="onSubmit"
      >
        {{ submitting ? "提交中…" : "提交认证" }}
      </ElButton>

      <p class="realname-note">
        提交后将在 1-3 个工作日内审核，审核通过后可开通云资源；认证信息仅用于合规核验，平台不会向第三方披露。
      </p>
    </div>
  </div>
</template>

<style scoped>
.realname-page {
  display: flex;
  align-items: flex-start;
  justify-content: center;
  min-height: calc(100vh - var(--eu-topbar-height));
  padding: var(--eu-spacing-6);
}
.realname-card {
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  padding: var(--eu-spacing-8);
  width: 100%;
  max-width: 480px;
}
.realname-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: var(--eu-spacing-1);
}
.realname-title {
  font-size: var(--eu-font-size-xl);
  margin: 0;
  color: var(--eu-text-primary);
}
.realname-sub {
  font-size: var(--eu-font-size-sm);
  color: var(--eu-text-secondary);
  margin: 0 0 var(--eu-spacing-6);
}
.realname-user {
  color: var(--eu-text-primary);
  font-weight: 500;
}
.realname-mode {
  margin-bottom: var(--eu-spacing-5);
}
.realname-form {
  margin-bottom: var(--eu-spacing-5);
}
.realname-submit {
  width: 100%;
}
.realname-note {
  margin: var(--eu-spacing-4) 0 0;
  font-size: var(--eu-font-size-xs);
  color: var(--eu-text-secondary);
  line-height: 1.6;
}
</style>
