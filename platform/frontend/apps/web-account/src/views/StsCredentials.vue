<script setup lang="ts">
/**
 * STS 临时凭证 (phase-3 D-3, 07§10 M1 角色/STS; deferred from phase 2).
 * 以当前账号身份 Assume 一个 RAM 角色,换取限时 AK/SK/SecurityToken 三元组;
 * SecretKey 仅显示一次。临时凭证随请求携带 x-cps-security-token,由网关
 * 在 CPS1 签名之上校验 (03§9.2)。
 */
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { ElButton, ElMessage, ElSelect, ElOption } from "element-plus";
import { useAccountAuth } from "../stores/auth";

const auth = useAccountAuth();

interface StsCredential {
  accessKey: string;
  secretKey: string;
  securityToken: string;
  expiresAt: string;
}

interface RoleDto { name: string; description?: string; policies?: string[] }

const roles = ref<RoleDto[]>([]);
const role = ref("");
const cred = ref<StsCredential | null>(null);
const loadingRoles = ref(true);
const assuming = ref(false);
const error = ref<string | null>(null);

// 剩余有效期倒计时 (秒)。
const nowMs = ref(Date.now());
let timer: ReturnType<typeof setInterval> | null = null;

async function readEnvelope<T>(res: Response): Promise<T> {
  const body = await res.json();
  if (!res.ok || body.Code !== "OK") {
    throw new Error(body.Message ?? `request failed (HTTP ${res.status})`);
  }
  return body.Data as T;
}
function authHeaders(): HeadersInit {
  return auth.accessToken ? { Authorization: `Bearer ${auth.accessToken}` } : {};
}

async function fetchRoles() {
  if (!auth.isAuthenticated) { loadingRoles.value = false; return; }
  try {
    const data = await readEnvelope<{ roles: RoleDto[] }>(
      await fetch("/api/ram/roles", { headers: authHeaders() }),
    );
    roles.value = data?.roles ?? [];
    if (roles.value.length && !role.value) role.value = roles.value[0].name;
  } catch (e) {
    error.value = `加载 RAM 角色失败：${(e as Error).message}`;
  } finally {
    loadingRoles.value = false;
  }
}

async function assumeRole() {
  if (!role.value) { ElMessage.warning("请先选择要扮演的 RAM 角色"); return; }
  assuming.value = true;
  cred.value = null;
  try {
    const data = await readEnvelope<StsCredential>(
      await fetch("/api/sts/assume-role", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...authHeaders() },
        body: JSON.stringify({ role: role.value }),
      }),
    );
    cred.value = data;
    ElMessage.success("临时凭证已签发,SecretKey 仅本次显示");
  } catch (e) {
    ElMessage.error(`签发失败：${(e as Error).message}`);
  } finally {
    assuming.value = false;
  }
}

const remainMs = computed(() => {
  if (!cred.value) return 0;
  return new Date(cred.value.expiresAt).getTime() - nowMs.value;
});
const remainLabel = computed(() => {
  const s = Math.max(0, Math.floor(remainMs.value / 1000));
  const m = Math.floor(s / 60);
  return `${String(m).padStart(2, "0")}:${String(s % 60).padStart(2, "0")}`;
});
const expired = computed(() => cred.value !== null && remainMs.value <= 0);

async function copyText(text: string, label: string) {
  try {
    await navigator.clipboard.writeText(text);
    ElMessage.success(`${label}已复制到剪贴板`);
  } catch {
    ElMessage.error("复制失败,请手动选择文本复制");
  }
}

onMounted(() => {
  void fetchRoles();
  timer = setInterval(() => { nowMs.value = Date.now(); }, 1000);
});
onBeforeUnmount(() => { if (timer) clearInterval(timer); });
</script>

<template>
  <div class="sts-page">
    <div class="sts-header">
      <div class="sts-heading">
        <h1 class="sts-title">STS 临时凭证</h1>
        <p class="sts-sub">
          以当前账号身份扮演 RAM 角色,签发限时访问凭证(AK/SK/SecurityToken)。临时凭证默认 1 小时有效,
          适合临时授权与跨账号协作场景;SecretKey 仅在签发时显示一次。
        </p>
        <p v-if="auth.user" class="sts-owner">当前账号:{{ auth.user.realName ?? auth.user.name }} (ID: {{ auth.user.id }})</p>
      </div>
    </div>

    <p v-if="error" class="sts-error">{{ error }}</p>

    <section class="sts-card">
      <div class="sts-form">
        <div class="sts-field">
          <label class="sts-label" for="sts-role">RAM 角色</label>
          <ElSelect id="sts-role" v-model="role" placeholder="选择要扮演的角色" :loading="loadingRoles" class="sts-select">
            <ElOption v-for="r in roles" :key="r.name" :value="r.name" :label="r.name" />
          </ElSelect>
        </div>
        <ElButton type="primary" :loading="assuming" :disabled="!roles.length" @click="assumeRole">获取临时凭证</ElButton>
      </div>
      <p v-if="!loadingRoles && !roles.length" class="sts-empty">
        暂无 RAM 角色——请先在「RAM 角色」页创建角色并绑定策略,再回到此处签发临时凭证。
      </p>
    </section>

    <section v-if="cred" class="sts-card" :class="{ expired }">
      <div class="sts-cred-head">
        <h2 class="sts-cred-title">已签发凭证</h2>
        <span class="sts-countdown" :class="{ expired }">{{ expired ? "已过期" : `剩余 ${remainLabel}` }}</span>
      </div>
      <div class="sts-alert">
        请妥善保存,SecretKey 与 SecurityToken 仅显示一次,关闭页面后无法再次查看。过期后请重新签发。
      </div>
      <div class="sts-field">
        <div class="sts-field-label">AccessKey</div>
        <div class="sts-field-value">
          <code class="sts-code">{{ cred.accessKey }}</code>
          <ElButton link type="primary" size="small" @click="copyText(cred.accessKey, 'AccessKey ')">复制</ElButton>
        </div>
      </div>
      <div class="sts-field">
        <div class="sts-field-label">SecretKey</div>
        <div class="sts-field-value">
          <code class="sts-code">{{ cred.secretKey }}</code>
          <ElButton link type="primary" size="small" @click="copyText(cred.secretKey, 'SecretKey ')">复制</ElButton>
        </div>
      </div>
      <div class="sts-field">
        <div class="sts-field-label">SecurityToken</div>
        <div class="sts-field-value">
          <code class="sts-code">{{ cred.securityToken }}</code>
          <ElButton link type="primary" size="small" @click="copyText(cred.securityToken, 'SecurityToken ')">复制</ElButton>
        </div>
      </div>
      <div class="sts-field">
        <div class="sts-field-label">过期时间</div>
        <div class="sts-field-value"><code class="sts-code">{{ cred.expiresAt.replace("T", " ").slice(0, 19) }} UTC</code></div>
      </div>
    </section>
  </div>
</template>

<style scoped>
.sts-page { padding: var(--eu-spacing-6); max-width: 760px; }
.sts-header { margin-bottom: var(--eu-spacing-5); }
.sts-title { margin: 0; font-size: var(--eu-font-size-xl); color: var(--eu-text-primary); }
.sts-sub { margin: var(--eu-spacing-2) 0 0; font-size: var(--eu-font-size-sm); color: var(--eu-text-secondary); line-height: 1.6; }
.sts-owner { margin: var(--eu-spacing-2) 0 0; font-size: var(--eu-font-size-xs); color: var(--eu-text-secondary); }
.sts-error { padding: var(--eu-spacing-3); color: var(--eu-color-danger); font-size: var(--eu-font-size-sm); }
.sts-card {
  display: flex; flex-direction: column; gap: var(--eu-spacing-4);
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  padding: var(--eu-spacing-6);
  margin-bottom: var(--eu-spacing-5);
}
.sts-card.expired { border-color: var(--eu-color-danger); }
.sts-form { display: flex; gap: var(--eu-spacing-4); align-items: flex-end; flex-wrap: wrap; }
.sts-field { display: flex; flex-direction: column; gap: var(--eu-spacing-1); }
.sts-label { font-size: var(--eu-font-size-sm); color: var(--eu-text-secondary); }
.sts-select { min-width: 260px; }
.sts-empty { margin: 0; font-size: var(--eu-font-size-sm); color: var(--eu-text-secondary); line-height: 1.6; }
.sts-cred-head { display: flex; align-items: center; justify-content: space-between; }
.sts-cred-title { margin: 0; font-size: var(--eu-font-size-md); color: var(--eu-text-primary); }
.sts-countdown {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: var(--eu-font-size-sm); color: var(--eu-color-brand);
  padding: 2px 10px; border-radius: var(--eu-radius-sm); background: var(--eu-color-brand-soft);
}
.sts-countdown.expired { color: var(--eu-color-danger); background: var(--eu-color-danger-soft); }
.sts-alert {
  padding: var(--eu-spacing-3) var(--eu-spacing-4);
  background: var(--eu-color-warning-soft);
  border: 1px solid var(--eu-color-warning);
  border-radius: var(--eu-radius-md);
  color: var(--eu-color-warning-text);
  font-size: var(--eu-font-size-sm);
  line-height: 1.6;
}
.sts-field-label { font-size: var(--eu-font-size-sm); color: var(--eu-text-secondary); }
.sts-field-value {
  display: flex; align-items: center; gap: var(--eu-spacing-2);
  padding: var(--eu-spacing-2) var(--eu-spacing-3);
  background: var(--eu-bg-page);
  border: 1px solid var(--eu-border);
  border-radius: var(--eu-radius-md);
}
.sts-code {
  flex: 1;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: var(--eu-font-size-sm);
  color: var(--eu-text-primary);
  word-break: break-all;
}
</style>
