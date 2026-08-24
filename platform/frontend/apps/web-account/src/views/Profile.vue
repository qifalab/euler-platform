<script setup lang="ts">
/**
 * Profile page (02§5.2). Authenticated account info view: shows the principal
 * identity bound to the auth store plus a 安全设置 card with modify links.
 */
import { computed, onMounted, ref } from "vue";
import { ElButton, ElEmpty, ElTag } from "element-plus";
import { useAccountAuth } from "../stores/auth";

const auth = useAccountAuth();

// Profile attributes fetched from svc-iam GET /api/account/profile (02§5.2).
// The envelope {RequestId,Code,Message,Data} is unwrapped here; Data holds the
// account profile (realNameStatus, mfaEnabled, phoneBound, etc.).
interface AccountProfile {
  createdAt?: string;
  phone?: string;
  phoneBound?: boolean;
  email?: string;
  emailBound?: boolean;
  mfaEnabled?: boolean;
  passwordSet?: boolean;
  realNameStatus?: "verified" | "unverified";
}
const profile = ref<AccountProfile>({});

async function fetchProfile() {
  if (!auth.isAuthenticated) return;
  try {
    const res = await fetch("/api/account/profile", {
      headers: { Authorization: `Bearer ${auth.accessToken}` },
    });
    const body = await res.json();
    if (!res.ok || body.Code !== "OK") {
      throw new Error(body.Message ?? "profile fetch failed");
    }
    profile.value = (body.Data ?? {}) as AccountProfile;
  } catch {
    /* leave profile empty — rows fall back to "—" */
  }
}

onMounted(fetchProfile);

const realNameStatus = computed(() =>
  profile.value.realNameStatus === "verified" || auth.user?.realName
    ? "已认证"
    : "未认证",
);

type TagType = "primary" | "success" | "warning" | "info" | "danger";
interface InfoRow { label: string; value: string; tag?: TagType; status?: string }
interface SecurityItem { key: string; label: string; desc: string; tag: TagType; status: string }

const infoRows = computed<InfoRow[]>(() => [
  { label: "账号ID", value: String(auth.user?.id ?? "—") },
  { label: "账号名", value: auth.user?.name ?? "—" },
  {
    label: "实名状态",
    value: realNameStatus.value,
    tag: realNameStatus.value === "已认证" ? "success" : "warning",
    status: realNameStatus.value,
  },
  { label: "注册时间", value: profile.value.createdAt ?? "—" },
  {
    label: "手机绑定",
    value: profile.value.phoneBound ? (profile.value.phone ?? "已绑定") : "未绑定",
    tag: profile.value.phoneBound ? "success" : "info",
  },
  {
    label: "邮箱绑定",
    value: profile.value.emailBound ? (profile.value.email ?? "已绑定") : "未绑定",
    tag: profile.value.emailBound ? "success" : "info",
  },
]);

const securityItems = computed<SecurityItem[]>(() => [
  {
    key: "password",
    label: "登录密码",
    desc: "建议每 90 天更换一次",
    status: profile.value.passwordSet ? "已设置" : "未设置",
    tag: profile.value.passwordSet ? "success" : "danger",
  },
  {
    key: "phone",
    label: "手机号",
    desc: profile.value.phone ?? "—",
    status: profile.value.phoneBound ? "已绑定" : "未绑定",
    tag: profile.value.phoneBound ? "success" : "warning",
  },
  {
    key: "email",
    label: "邮箱",
    desc: profile.value.email ?? "—",
    status: profile.value.emailBound ? "已绑定" : "未绑定",
    tag: profile.value.emailBound ? "success" : "warning",
  },
  {
    key: "mfa",
    label: "MFA 双因素认证",
    desc: profile.value.mfaEnabled ? "虚拟 MFA 设备" : "未启用，建议开启以提升账号安全",
    status: profile.value.mfaEnabled ? "已启用" : "未启用",
    tag: profile.value.mfaEnabled ? "success" : "warning",
  },
]);
</script>

<template>
  <div class="profile-page">
    <ElEmpty v-if="!auth.isAuthenticated" description="请先登录" />

    <template v-else>
      <h1 class="profile-title">账号信息</h1>

      <div class="profile-grid">
        <!-- 账号信息 -->
        <section class="profile-card">
          <h2 class="card-heading">基本信息</h2>
          <dl class="info-rows">
            <div v-for="row in infoRows" :key="row.label" class="info-row">
              <dt class="info-label">{{ row.label }}</dt>
              <dd class="info-value">
                <span>{{ row.value }}</span>
                <ElTag
                  v-if="row.tag"
                  :type="row.tag"
                  size="small"
                  class="info-tag"
                  effect="light"
                >
                  {{ row.status || row.value }}
                </ElTag>
              </dd>
            </div>
          </dl>
        </section>

        <!-- 安全设置 -->
        <section class="profile-card security-card">
          <h2 class="card-heading">安全设置</h2>
          <ul class="security-list">
            <li
              v-for="item in securityItems"
              :key="item.key"
              class="security-item"
            >
              <div class="security-main">
                <span class="security-label">{{ item.label }}</span>
                <span class="security-desc">{{ item.desc }}</span>
              </div>
              <div class="security-actions">
                <ElTag :type="item.tag" size="small" effect="light">
                  {{ item.status }}
                </ElTag>
                <ElButton link type="primary" size="small">修改</ElButton>
              </div>
            </li>
          </ul>
        </section>
      </div>
    </template>
  </div>
</template>

<style scoped>
.profile-page {
  padding: var(--sc-spacing-6);
  min-height: calc(100vh - var(--sc-topbar-height));
}
.profile-title {
  margin: 0 0 var(--sc-spacing-6);
  font-size: 22px;
  font-weight: 600;
  color: var(--sc-text-primary);
}
.profile-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--sc-spacing-6);
  align-items: start;
}
.profile-card {
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  box-shadow: var(--sc-shadow-sm);
  padding: var(--sc-spacing-6);
}
.card-heading {
  margin: 0 0 20px;
  padding-bottom: 12px;
  border-bottom: 1px solid var(--sc-border);
  font-size: 16px;
  font-weight: 600;
  color: var(--sc-text-primary);
}
.info-rows {
  margin: 0;
  display: flex;
  flex-direction: column;
}
.info-row {
  display: flex;
  align-items: center;
  padding: 14px 0;
  border-bottom: 1px solid var(--sc-border);
}
.info-row:last-child {
  border-bottom: none;
}
.info-label {
  flex: 0 0 120px;
  margin: 0;
  font-size: 14px;
  color: var(--sc-text-secondary);
}
.info-value {
  flex: 1;
  margin: 0;
  display: flex;
  align-items: center;
  gap: 12px;
  font-size: 14px;
  color: var(--sc-text-primary);
}
.info-tag {
  margin-left: 4px;
}
.security-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
}
.security-item {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 16px 0;
  border-bottom: 1px solid var(--sc-border);
  gap: 16px;
}
.security-item:last-child {
  border-bottom: none;
}
.security-main {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}
.security-label {
  font-size: 14px;
  font-weight: 500;
  color: var(--sc-text-primary);
}
.security-desc {
  font-size: 12px;
  color: var(--sc-text-secondary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.security-actions {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-shrink: 0;
}
</style>
