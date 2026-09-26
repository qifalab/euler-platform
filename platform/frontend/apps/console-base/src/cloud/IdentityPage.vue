<script setup lang="ts">
import { computed } from "vue";
import { RouterLink } from "vue-router";
import { useCloud } from "./context";
import CloudIcon from "./CloudIcon.vue";
const { state } = useCloud();
const apps = computed(() =>
  state.catalog.filter((a) => ["eid", "trust"].includes(a.id)),
);
</script>
<template>
  <section class="page-heading">
    <div>
      <span class="eyebrow">IDENTITY & VERIFICATION</span>
      <h1>你的身份与认证</h1>
      <p>使用一个通行证账号，在欧拉完成认证、成员资格申请与社团报名。</p>
    </div>
  </section>
  <section class="identity-account panel">
    <div class="avatar large">
      {{ state.session?.user?.displayName?.slice(0, 1) ?? "我" }}
    </div>
    <div>
      <h2>{{ state.session?.user?.displayName }}</h2>
      <p>当前登录来源 · {{ state.session?.user?.provider }}</p>
    </div>
    <span class="badge success">已登录</span>
  </section>
  <div class="identity-grid">
    <section v-for="app in apps" :key="app.id" class="panel identity-card">
      <div class="panel-heading">
        <div class="row">
          <span class="app-mark" :data-app="app.id"
            ><CloudIcon
              :name="app.id === 'eid' ? 'shield' : 'check'"
              :size="26"
          /></span>
          <h2>{{ app.name }}</h2>
        </div>
      </div>
      <div class="identity-entry">
        <p>
          {{
            app.id === "eid"
              ? "申请成员资格、查看身份卡，跟进社团报名与录取进度。"
              : "查看认证方案、提交认证资料，跟进审核结果与历史记录。"
          }}
        </p>
        <RouterLink :to="`/apps/${app.id}`" class="button button-primary"
          >{{ app.id === "eid" ? "打开成员资格" : "打开信任中心"
          }}<CloudIcon name="arrow" :size="16"
        /></RouterLink>
      </div>
    </section>
  </div>
  <div class="info-note">
    <CloudIcon name="shield" :size="20" />
    <div>
      <strong>账号与业务权限分别管理</strong>
      <p>
        通行证提供登录身份，欧拉保存本平台的申请和认证记录。项目成员权限及审核权由管理员明确授予。
      </p>
      <a
        href="https://account.emoera.com"
        target="_blank"
        rel="noopener noreferrer"
        class="text-link"
        >管理通行证资料与账号安全<CloudIcon name="external" :size="14"
      /></a>
    </div>
  </div>
</template>
<style scoped>
.identity-entry {
  padding: 0 24px 28px;
}
.identity-entry p {
  color: var(--cloud-muted);
  line-height: 1.9;
  margin-bottom: 24px;
}
</style>
