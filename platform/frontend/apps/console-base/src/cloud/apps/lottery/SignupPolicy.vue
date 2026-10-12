<script setup lang="ts">
import { onMounted, reactive, ref } from "vue";
import { useApp } from "../shared";
import { api } from "../../api";
const props = defineProps<{ roomId: string }>();
const app = useApp();
const form = reactive({
  requireLogin: false,
  trustSchemeId: "",
  requireEid: false,
  weauthSiteId: "",
});
const schemes = ref<{ id: string; name: string }[]>([]),
  sites = ref<{ id: string; name: string }[]>([]),
  busy = ref(false),
  error = ref("");
onMounted(async () => {
  try {
    const p = await app.request<typeof form>(
      `/rooms/${encodeURIComponent(props.roomId)}/signup-policy`,
    );
    Object.assign(form, {
      requireLogin: p.requireLogin,
      trustSchemeId: p.trustSchemeId,
      requireEid: p.requireEid,
      weauthSiteId: p.weauthSiteId,
    });
    const base = app.basePath.replace(/\/lottery$/, "");
    const [s, w] = await Promise.allSettled([
      api<{
        items?: { id: string; name: string }[];
        schemes?: { id: string; name: string }[];
      }>(base + "/trust/schemes"),
      api<{ items: { id: string; name: string }[] }>(base + "/weauth/sites"),
    ]);
    if (s.status === "fulfilled")
      schemes.value = s.value.items ?? s.value.schemes ?? [];
    if (w.status === "fulfilled") sites.value = w.value.items ?? [];
  } catch (e) {
    error.value = e instanceof Error ? e.message : "无法载入报名条件";
  }
});
async function save() {
  busy.value = true;
  error.value = "";
  try {
    const out = await app.request<typeof form>(
      `/rooms/${encodeURIComponent(props.roomId)}/signup-policy`,
      { method: "PUT", body: { ...form } },
    );
    Object.assign(form, out);
    app.notify("报名条件已保存");
  } catch (e) {
    error.value = e instanceof Error ? e.message : "保存失败";
  } finally {
    busy.value = false;
  }
}
</script>
<template>
  <form class="panel" @submit.prevent="save">
    <h3>报名资格与防刷</h3>
    <p class="muted">
      默认允许匿名报名。启用资格条件后，需要登录并具有当前项目访问权限。
    </p>
    <p v-if="error" class="inline-error" role="alert">{{ error }}</p>
    <div class="form-grid">
      <label class="switch-label field full"
        ><input
          v-model="form.requireLogin"
          type="checkbox"
        />绑定欧拉账号报名</label
      ><label class="field"
        >Trust 认证<select aria-label="Trust 认证" v-model="form.trustSchemeId">
          <option value="">不要求认证</option>
          <option v-for="s in schemes" :key="s.id" :value="s.id">
            {{ s.name }}
          </option>
        </select></label
      ><label class="field"
        >WeAuth 人机验证<select
          aria-label="WeAuth 人机验证"
          v-model="form.weauthSiteId"
        >
          <option value="">不启用</option>
          <option v-for="s in sites" :key="s.id" :value="s.id">
            {{ s.name }}
          </option>
        </select></label
      ><label class="switch-label field full"
        ><input v-model="form.requireEid" type="checkbox" />要求 EID
        已确认录取的成员资格</label
      >
    </div>
    <p class="muted">
      WeAuth 站点需允许欧拉当前域名。中奖资源或通知可在“应用联动”配置。
    </p>
    <button class="button primary" :disabled="busy">保存报名条件</button>
  </form>
</template>
