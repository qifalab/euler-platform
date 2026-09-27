<script setup lang="ts">
import { computed, onMounted, onBeforeUnmount, ref } from "vue";
import { useApp } from "../shared";
const props = defineProps<{ roomId: string }>();
const app = useApp();
const room = ref<{ name: string; description: string; status: string }>();
const policy = ref<{
  requireLogin: boolean;
  requireEid: boolean;
  trustSchemeId: string;
  sitekey?: string;
}>();
const name = ref(app.scope.actorName),
  department = ref(""),
  token = ref(""),
  frame = ref<HTMLIFrameElement>();
const busy = ref(false),
  error = ref(""),
  done = ref(false);
const widget = computed(
  () =>
    `/public/weauth/widget.html?${new URLSearchParams({ sitekey: policy.value?.sitekey ?? "", action: "lottery:" + props.roomId, origin: location.origin })}`,
);
function message(event: MessageEvent) {
  if (
    event.origin !== location.origin ||
    event.source !== frame.value?.contentWindow ||
    event.data?.type !== "euler-weauth"
  )
    return;
  token.value = event.data.event === "success" ? event.data.token : "";
}
async function submit() {
  busy.value = true;
  error.value = "";
  try {
    await app.request(`/rooms/${encodeURIComponent(props.roomId)}/signup`, {
      method: "POST",
      body: {
        name: name.value,
        department: department.value,
        weauthToken: token.value,
      },
    });
    done.value = true;
  } catch (e) {
    error.value = e instanceof Error ? e.message : "报名未成功，请重试";
  } finally {
    busy.value = false;
    token.value = "";
    frame.value?.contentWindow?.postMessage(
      { type: "euler-weauth-control", action: "reset" },
      location.origin,
    );
  }
}
onMounted(async () => {
  window.addEventListener("message", message);
  try {
    [room.value, policy.value] = await Promise.all([
      app.request<{ name: string; description: string; status: string }>(
        `/rooms/${encodeURIComponent(props.roomId)}`,
      ),
      app.request<{
        requireLogin: boolean;
        requireEid: boolean;
        trustSchemeId: string;
        sitekey?: string;
      }>(`/rooms/${encodeURIComponent(props.roomId)}/signup-policy`),
    ]);
  } catch (e) {
    error.value = e instanceof Error ? e.message : "无法载入活动";
  }
});
onBeforeUnmount(() => window.removeEventListener("message", message));
</script>
<template>
  <section class="native-app panel signup-card">
    <p class="eyebrow">EULER · 活动报名</p>
    <h2>{{ room?.name ?? "正在载入活动" }}</h2>
    <p class="muted">{{ room?.description }}</p>
    <p v-if="error" role="alert" class="inline-error">{{ error }}</p>
    <p v-if="done" class="success-note" role="status">
      报名成功。中奖结果可在“应用联动 · 我的通知”查看（需主办方配置通知规则）。
    </p>
    <form
      v-else-if="room?.status === 'open' && policy"
      @submit.prevent="submit"
    >
      <p v-if="policy.trustSchemeId || policy.requireEid" class="muted">
        本次报名将检查{{ policy.trustSchemeId ? " Trust 认证" : ""
        }}{{ policy.requireEid ? " EID 成员资格" : "" }}。
      </p>
      <label class="field"
        >姓名<input
          aria-label="姓名"
          v-model="name"
          required
          minlength="2"
          maxlength="20"
          autocomplete="name" /></label
      ><label class="field"
        >部门<input
          aria-label="部门"
          v-model="department"
          maxlength="50"
          autocomplete="organization" /></label
      ><iframe
        v-if="policy.sitekey"
        ref="frame"
        :src="widget"
        title="WeAuth 人机验证"
        width="340"
        height="114"
      /><button
        class="button button-primary"
        :disabled="busy || Boolean(policy.sitekey && !token)"
      >
        确认本人报名
      </button>
      <p class="muted">报名绑定当前欧拉账号，每个账号仅报名一次。</p>
    </form>
    <p v-else-if="room">活动已暂停报名。</p>
  </section>
</template>
<style scoped>
.signup-card {
  max-width: 600px;
  margin: 24px auto;
  padding: 28px;
  background: var(--cloud-panel);
}
.signup-card form {
  display: grid;
  gap: 20px;
}
.signup-card iframe {
  border: 0;
  max-width: 100%;
}
.eyebrow {
  letter-spacing: 0.1em;
  color: var(--cloud-blue);
  font-size: 12px;
}
.success-note {
  background: var(--cloud-panel-soft);
  color: var(--cloud-success);
  padding: 20px;
  border-radius: 12px;
}
.field {
  display: grid;
  gap: 8px;
}
.field input {
  padding: 12px;
  border: 1px solid var(--cloud-border);
  border-radius: 9px;
  background: var(--cloud-panel);
  color: var(--cloud-text);
  font: inherit;
}
</style>
