<script setup lang="ts">
import { ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import { api, errorMessage } from "./api";
import { useCloud } from "./context";
import type { Tenant } from "./types";
import CloudIcon from "./CloudIcon.vue";
const route = useRoute();
const router = useRouter();
const { refreshTenants, notify } = useCloud();
const token = ref(
  typeof route.query.token === "string" ? route.query.token : "",
);
const busy = ref(false);
const error = ref("");
async function accept() {
  busy.value = true;
  error.value = "";
  try {
    const result = await api<Tenant>("/api/v1/invitations/accept", {
      method: "POST",
      body: { token: token.value.trim() },
    });
    token.value = "";
    await refreshTenants(result.id);
    notify(`已加入 ${result.name}。`);
    await router.replace("/");
  } catch (err) {
    error.value = errorMessage(err);
  } finally {
    busy.value = false;
  }
}
</script>
<template>
  <section class="page-heading">
    <div>
      <span class="eyebrow">TEAM INVITATION</span>
      <h1>一起完成下一件事</h1>
      <p>接受邀请，加入协作者的团队。</p>
    </div>
  </section>
  <form class="panel join-panel stack" @submit.prevent="accept">
    <span class="app-mark"><CloudIcon name="users" :size="28" /></span>
    <h2>加入团队</h2>
    <p class="muted">
      请确认邀请来自你信任的团队。接受后将获得邀请中指定的团队角色，项目仍需单独授权。
    </p>
    <label class="field"
      >邀请令牌<input
        v-model="token"
        autocomplete="off"
        type="password"
        required
        placeholder="从邀请链接自动填入，或粘贴令牌"
    /></label>
    <p v-if="error" class="form-error" role="alert">{{ error }}</p>
    <button class="button button-primary" :disabled="busy || !token.trim()">
      {{ busy ? "正在加入…" : "接受邀请" }}<CloudIcon name="arrow" :size="17" />
    </button>
  </form>
</template>
