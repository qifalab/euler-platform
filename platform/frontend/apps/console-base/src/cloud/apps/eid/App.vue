<script setup lang="ts">
import { computed, reactive, ref, watch } from "vue";
import { RouterLink, useRoute } from "vue-router";
import { useApp } from "../shared";
import CloudModal from "../../CloudModal.vue";
import History from "./History.vue";
import MemberPicker from "./MemberPicker.vue";
import type {
  Verification,
  Club,
  Member,
  Notification,
  Page,
  ClubOverview,
} from "./types";
import { identities, statuses, date } from "./types";
const app = useApp();
const route = useRoute();
const notificationLabels: Record<string, string> = {
  interview: "笔试通知",
  offer: "录取通知",
  resend_interview: "重发笔试通知",
  resend_offer: "重发录取通知",
  webhook_wecom: "企业微信",
  webhook_feishu: "飞书",
};
const tab = ref(route.query.tab === "club" ? "club" : "identity");
const tabs = computed(() => [
  { id: "identity", name: "我的身份" },
  { id: "profile", name: "会员资料" },
  { id: "club", name: "社团报名" },
  ...(app.can("review")
    ? [
        { id: "review-identity", name: "身份审核" },
        { id: "review-club", name: "招募审核" },
        { id: "notifications", name: "通知记录" },
      ]
    : []),
  ...(app.can("admin")
    ? [
        { id: "members", name: "会员管理" },
        { id: "keys", name: "资格查询接口" },
      ]
    : []),
  ...(app.can("manage") ? [{ id: "settings", name: "流程设置" }] : []),
]);
const loading = ref(false),
  busy = ref(false),
  error = ref(""),
  page = ref(1),
  total = ref(0),
  filter = ref("all"),
  search = ref("");
const verifications = ref<Verification[]>([]),
  cards = ref<Verification[]>([]),
  clubs = ref<Club[]>([]),
  members = ref<Member[]>([]),
  notifications = ref<Notification[]>([]);
const profile = ref<Member>();
const profileForm = reactive({ bio: "", phone: "", avatar: "" });
const overview = ref<ClubOverview>();
const counts = ref<Record<string, number>>({});
const application = reactive({
  realName: "",
  studentId: "",
  identityTitle: "",
  identityType: "active",
});
const applying = ref(false);
const clubName = ref("");
const edited = ref<Verification>();
const editOpen = ref(false);
const edit = reactive({
  realName: "",
  studentId: "",
  identityTitle: "",
  identityType: "active",
});
const editedMember = ref<Member>();
const memberOpen = ref(false);
const memberForm = reactive({
  bio: "",
  phone: "",
  avatar: "",
  identityLevel: 0,
  identityTitle: "",
});
const schemes = ref<{ id: string; name: string }[]>([]);
const queryKeys = ref<
  {
    id: string;
    name: string;
    actorIds: string[];
    expiresAt: string;
    createdAt: string;
    revokedAt: string;
  }[]
>([]);
const keyName = ref(""),
  keyActors = ref<string[]>([]),
  keyDays = ref(30),
  rawToken = ref("");
const settings = reactive({
  clubName: "",
  requireTrust: true,
  trustSchemeId: "",
  interviewSubject: "",
  interviewBody: "",
  offerSubject: "",
  offerBody: "",
  smtpAddress: "",
  smtpFrom: "",
  smtpUsername: "",
  smtpPassword: "",
  smtpImplicitTLS: false,
  wecomUrl: "",
  feishuUrl: "",
  clearSMTPPassword: false,
  clearWecom: false,
  clearFeishu: false,
});
const configured = reactive({
  smtpPassword: false,
  wecom: false,
  feishu: false,
});
const confirmOpen = ref(false);
const confirmation = ref<{
  title: string;
  description: string;
  run: () => Promise<void>;
}>();
function confirm(title: string, description: string, run: () => Promise<void>) {
  confirmation.value = { title, description, run };
  confirmOpen.value = true;
}
function fail(e: unknown) {
  const message = e instanceof Error ? e.message : "操作暂时无法完成";
  error.value = message;
  app.notify(message, "error");
}
async function perform(fn: () => Promise<void>) {
  if (busy.value) return;
  busy.value = true;
  error.value = "";
  try {
    await fn();
  } catch (e) {
    fail(e);
  } finally {
    busy.value = false;
  }
}
async function load() {
  loading.value = true;
  error.value = "";
  try {
    if (tab.value === "profile") {
      profile.value = await app.request<Member>("/profile");
      Object.assign(profileForm, {
        bio: profile.value.bio,
        phone: profile.value.phone,
        avatar: profile.value.avatar,
      });
    } else if (tab.value === "identity" || tab.value === "review-identity") {
      const path =
        tab.value === "identity" ? "/verifications" : "/review/verifications";
      const out = await app.request<Page<Verification>>(
        `${path}?page=${page.value}&status=${filter.value}`,
      );
      verifications.value = out.items;
      total.value = out.total;
      if (tab.value === "identity") {
        const out = await app.request<Page<Verification>>("/cards");
        cards.value = out.items;
      }
    } else if (tab.value === "club" || tab.value === "review-club") {
      const path = tab.value === "club" ? "/club" : "/review/club";
      const out = await app.request<ClubOverview>(
        `${path}?page=${page.value}&status=${filter.value}`,
      );
      clubs.value = out.items;
      total.value = out.total;
      if (tab.value === "club") overview.value = out;
      counts.value = out.counts ?? {};
    } else if (tab.value === "members") {
      const out = await app.request<Page<Member>>(
        `/admin/members?page=${page.value}&search=${encodeURIComponent(search.value)}`,
      );
      members.value = out.items;
      total.value = out.total;
    } else if (tab.value === "notifications") {
      const out = await app.request<Page<Notification>>(
        `/notifications?page=${page.value}`,
      );
      notifications.value = out.items;
      total.value = out.total;
    } else if (tab.value === "keys") {
      const keys = await app.request<{ items: typeof queryKeys.value }>(
        "/keys",
      );
      queryKeys.value = keys.items;
    } else if (tab.value === "settings") {
      const out = await app.request<{
        settings: Record<string, unknown>;
        schemes: { id: string; name: string }[];
      }>("/settings");
      for (const key of Object.keys(settings) as (keyof typeof settings)[]) {
        if (key in out.settings)
          (settings as Record<string, unknown>)[key] = out.settings[key];
      }
      settings.smtpPassword = "";
      settings.wecomUrl = "";
      settings.feishuUrl = "";
      settings.clearSMTPPassword = false;
      settings.clearWecom = false;
      settings.clearFeishu = false;
      configured.smtpPassword = Boolean(out.settings.smtpPasswordConfigured);
      configured.wecom = Boolean(out.settings.wecomConfigured);
      configured.feishu = Boolean(out.settings.feishuConfigured);
      schemes.value = out.schemes;
    }
  } catch (e) {
    fail(e);
  } finally {
    loading.value = false;
  }
}
watch(
  tab,
  () => {
    page.value = 1;
    filter.value = "all";
    rawToken.value = "";
    load();
  },
  { immediate: true },
);
async function movePage(next: number) {
  page.value = next;
  await load();
}
async function submitVerification() {
  await perform(async () => {
    await app.request("/verifications", { method: "POST", body: application });
    applying.value = false;
    app.notify("身份申请已提交");
    await load();
  });
}
function editVerification(item: Verification) {
  edited.value = item;
  Object.assign(edit, {
    realName: item.realName,
    studentId: item.studentId,
    identityTitle: item.identityTitle,
    identityType: item.identityType,
  });
  editOpen.value = true;
}
async function saveVerification() {
  if (!edited.value) return;
  await perform(async () => {
    await app.request(`/review/verifications/${edited.value!.id}`, {
      method: "PATCH",
      body: { ...edit, version: edited.value!.version },
    });
    editOpen.value = false;
    app.notify("申请资料已更新");
    await load();
  });
}
function decision(item: Verification, status: string) {
  confirm(
    status === "approved" ? "通过身份审核" : "拒绝身份申请",
    `申请人：${item.realName}。该操作会更新成员资格并保留审核记录。`,
    async () => {
      await app.request(`/review/verifications/${item.id}/decision`, {
        method: "POST",
        body: { status, version: item.version },
      });
      app.notify("审核结果已保存");
      await load();
    },
  );
}
async function submitClub() {
  await perform(async () => {
    await app.request("/club/applications", {
      method: "POST",
      body: { realName: clubName.value },
    });
    clubName.value = "";
    app.notify("报名已提交");
    await load();
  });
}
function acceptOffer(item: Club) {
  confirm(
    "接受录取",
    `确认接受 ${overview.value?.clubName ?? "当前项目"} 的录取？此操作仅能由申请人本人完成。`,
    async () => {
      await app.request(`/club/applications/${item.id}/confirm`, {
        method: "POST",
        body: { version: item.version },
      });
      app.notify("已接受录取");
      await load();
    },
  );
}
function rejectClub(item: Club) {
  confirm(
    "拒绝报名",
    `确认拒绝 ${item.realName} 的报名？对方之后可以重新提交。`,
    async () => {
      await app.request(`/review/club/${item.id}/decision`, {
        method: "POST",
        body: { status: "rejected", version: item.version },
      });
      app.notify("报名已拒绝");
      await load();
    },
  );
}
function send(item: Club, kind: string, resend = false) {
  const label = kind === "offer" ? "录取" : "笔试";
  confirm(
    `${resend ? "重发" : "发送"}${label}通知`,
    `将向 ${item.email} 实际发送邮件。${resend ? "请先核对上一封的投递结果，避免重复通知。" : "仅确认投递成功后推进报名状态。"}`,
    async () => {
      const out = await app.request<Notification>(
        `/review/club/${item.id}/notifications`,
        {
          method: "POST",
          body: {
            kind,
            resend,
            version: item.version,
            idempotencyKey: crypto.randomUUID(),
          },
        },
      );
      app.notify(
        out.state === "sent" ? "通知已发送" : out.error || "投递结果尚未确认",
        out.state === "sent" ? "success" : "error",
      );
      await load();
    },
  );
}
function retryWebhook(item: Notification) {
  confirm(
    "重试机器人通知",
    "请先核对渠道记录，确认重试可能造成重复消息。重试会使用当前项目配置的通知渠道。",
    async () => {
      const out = await app.request<Notification>(
        `/notifications/${item.id}/retry`,
        { method: "POST", body: { confirmed: true } },
      );
      app.notify(
        out.state === "sent" ? "机器人通知已发送" : out.error || "结果尚未确认",
        out.state === "sent" ? "success" : "error",
      );
      await load();
    },
  );
}
function editMember(item: Member) {
  editedMember.value = item;
  Object.assign(memberForm, {
    bio: item.bio,
    phone: item.phone,
    avatar: item.avatar,
    identityLevel: item.identityLevel,
    identityTitle: item.identityTitle,
  });
  memberOpen.value = true;
}
async function saveMember() {
  if (!editedMember.value) return;
  await perform(async () => {
    await app.request(
      `/admin/members/${encodeURIComponent(editedMember.value!.actorId)}`,
      { method: "PATCH", body: memberForm },
    );
    memberOpen.value = false;
    app.notify("会员业务资料已保存");
    await load();
  });
}
function removeMember(item: Member) {
  confirm(
    "删除会员业务记录",
    `将删除 ${item.name} 在当前项目的 EID 资料、身份申请与报名；保留欧拉账号和审计。`,
    async () => {
      await app.request(`/admin/members/${encodeURIComponent(item.actorId)}`, {
        method: "DELETE",
      });
      app.notify("本项目会员业务记录已删除");
      await load();
    },
  );
}
function removeRecord(id: string, type: "verifications" | "club") {
  confirm("删除业务记录", "记录会从当前项目移除，操作审计保留。", async () => {
    await app.request(`/admin/${type}/${id}`, { method: "DELETE" });
    app.notify("记录已删除");
    await load();
  });
}
async function saveSettings() {
  await perform(async () => {
    await app.request("/settings", { method: "PUT", body: settings });
    app.notify("流程与通知设置已保存");
    await load();
  });
}
async function confirmNow() {
  if (!confirmation.value) return;
  await perform(async () => {
    await confirmation.value!.run();
    confirmOpen.value = false;
  });
}
const canApplyClub = computed(
  () =>
    app.can("write") &&
    !clubs.value.some((item) => item.status !== "rejected") &&
    overview.value?.emailVerified &&
    (!overview.value?.requireTrust || overview.value?.trustVerified),
);
async function createQueryKey() {
  await perform(async () => {
    const out = await app.request<{ token: string }>("/keys", {
      method: "POST",
      body: {
        name: keyName.value,
        actorIds: keyActors.value,
        expiresAt: new Date(
          Date.now() + keyDays.value * 86400000,
        ).toISOString(),
      },
    });
    rawToken.value = out.token;
    keyName.value = "";
    app.notify("查询密钥已创建，请保存这次显示的密钥");
    await load();
  });
}
function revokeQueryKey(id: string) {
  confirm("撤销查询密钥", "使用这枚密钥的调用会立即失效。", async () => {
    await app.request(`/keys/${id}`, { method: "DELETE" });
    app.notify("密钥已撤销");
    await load();
  });
}
const showPager = computed(
  () =>
    !["settings", "profile", "keys"].includes(tab.value) && total.value > 25,
);
</script>
<template>
  <div class="native-app eid-app">
    <nav class="app-tabs" aria-label="成员服务功能">
      <button
        v-for="item in tabs"
        :key="item.id"
        :class="{ active: tab === item.id }"
        @click="tab = item.id"
      >
        {{ item.name }}
      </button>
    </nav>
    <div v-if="error" class="status-note error-note" role="alert">
      {{ error }} <button class="button" @click="load">重新加载</button>
    </div>
    <div v-if="loading" class="status-note" role="status">
      正在加载成员服务…
    </div>
    <template v-else>
      <section v-if="tab === 'identity'" class="panel eid-section">
        <div class="toolbar">
          <div>
            <h2>我的身份卡</h2>
            <p class="muted">
              每份身份由独立审核授予，适用于当前项目的成员服务。
            </p>
          </div>
          <button
            v-if="app.can('write')"
            class="button button-primary"
            :disabled="
              busy || verifications.some((v) => v.status === 'pending')
            "
            @click="applying = true"
          >
            申请身份
          </button>
        </div>
        <div v-if="cards.length" class="credential-grid">
          <article v-for="card in cards" :key="card.id" class="credential">
            <div class="credential-brand">
              EID <span>MEMBER CREDENTIAL</span>
            </div>
            <p>{{ identities[card.identityType] }}</p>
            <h3>{{ card.identityTitle }}</h3>
            <div class="credential-bottom">
              <span>{{ card.realName }}</span
              ><span>{{ date(card.updatedAt) }}</span>
            </div>
          </article>
        </div>
        <div v-else class="empty">
          还没有已通过的身份申请。提交申请后，审核结果会显示在这里。
        </div>
      </section>
      <section
        v-if="tab === 'identity' || tab === 'review-identity'"
        class="panel eid-section"
      >
        <div class="toolbar">
          <h2>{{ tab === "identity" ? "申请记录" : "身份审核工作台" }}</h2>
          <label class="inline-field"
            >状态
            <select
              v-model="filter"
              @change="
                page = 1;
                load();
              "
            >
              <option value="all">全部</option>
              <option value="pending">待审核</option>
              <option value="approved">已通过</option>
              <option value="rejected">已拒绝</option>
            </select></label
          >
        </div>
        <div v-if="!verifications.length" class="empty">当前没有申请记录。</div>
        <div v-else class="application-list">
          <article
            v-for="item in verifications"
            :key="item.id"
            class="record-card"
          >
            <div class="toolbar">
              <div>
                <h3>
                  {{ item.identityTitle }}
                  <span class="badge" :data-status="item.status">{{
                    statuses[item.status]
                  }}</span>
                </h3>
                <p class="muted">
                  {{ item.realName }} · {{ item.studentId }} ·
                  {{ identities[item.identityType] }}
                </p>
              </div>
              <span class="muted small">{{ date(item.createdAt) }}</span>
            </div>
            <div v-if="tab === 'review-identity'" class="actions">
              <button
                class="button"
                :disabled="busy"
                @click="editVerification(item)"
              >
                编辑资料</button
              ><button
                v-if="item.status !== 'approved'"
                class="button button-primary"
                :disabled="busy"
                @click="decision(item, 'approved')"
              >
                通过</button
              ><button
                v-if="item.status !== 'rejected'"
                class="button"
                :disabled="busy"
                @click="decision(item, 'rejected')"
              >
                拒绝</button
              ><button
                v-if="app.can('admin')"
                class="button button-danger"
                :disabled="busy"
                @click="removeRecord(item.id, 'verifications')"
              >
                删除
              </button>
            </div>
            <History :items="item.history" />
          </article>
        </div>
      </section>
      <section v-if="tab === 'profile' && profile" class="panel eid-section">
        <div class="profile-heading">
          <img
            v-if="profile.avatar"
            :src="profile.avatar"
            alt="会员头像"
            referrerpolicy="no-referrer"
          />
          <div class="profile-avatar" v-else>
            {{ profile.name.slice(0, 1) }}
          </div>
          <div>
            <h2>{{ profile.name }}</h2>
            <p class="muted">
              {{ profile.email || "未提供邮箱" }} ·
              {{ profile.emailVerified ? "邮箱已验证" : "邮箱未验证" }}
            </p>
            <span class="badge">{{
              profile.identityTitle || "尚未授予成员资格"
            }}</span>
          </div>
        </div>
        <form
          class="form-grid"
          @submit.prevent="
            perform(async () => {
              await app.request('/profile', {
                method: 'PATCH',
                body: profileForm,
              });
              app.notify('资料已保存');
              await load();
            })
          "
        >
          <label class="field"
            >联系电话<input
              v-model="profileForm.phone"
              maxlength="40"
              :disabled="!app.can('write')" /></label
          ><label class="field"
            >头像图片地址<input
              v-model="profileForm.avatar"
              type="url"
              placeholder="https://…"
              :disabled="!app.can('write')" /></label
          ><label class="field full"
            >个人简介<textarea
              v-model="profileForm.bio"
              maxlength="2000"
              :disabled="!app.can('write')"
            />
          </label>
          <div class="actions full">
            <button
              v-if="app.can('write')"
              class="button button-primary"
              :disabled="busy"
            >
              保存资料</button
            ><span class="muted small"
              >姓名和邮箱跟随通行证账号；这里保存本项目会员资料。</span
            >
          </div>
        </form>
      </section>
      <template v-if="tab === 'club'">
        <section class="panel eid-section">
          <div class="toolbar">
            <div>
              <h2>{{ overview?.clubName || "社团报名" }}</h2>
              <p class="muted">
                提交报名，接收笔试和录取通知，在这里确认接受录取。
              </p>
            </div>
            <button class="button" @click="load">刷新进度</button>
          </div>
          <div class="eligibility">
            <span :class="{ ready: overview?.emailVerified }">{{
              overview?.emailVerified ? "✓ 邮箱已验证" : "请先在通行证验证邮箱"
            }}</span
            ><span
              v-if="overview?.requireTrust"
              :class="{ ready: overview?.trustVerified }"
              >{{
                overview.trustVerified
                  ? "✓ Trust 认证已通过"
                  : overview.trustSchemeId
                    ? "等待完成 Trust 认证"
                    : "管理员尚未绑定认证方案"
              }}</span
            ><RouterLink
              v-if="overview?.requireTrust && !overview.trustVerified"
              to="/apps/trust"
              class="text-link"
              >前往本项目 Trust 认证</RouterLink
            >
          </div>
          <form
            v-if="!clubs.some((a) => a.status !== 'rejected')"
            class="club-form"
            @submit.prevent="submitClub"
          >
            <label class="field"
              >真实姓名<input
                v-model="clubName"
                required
                maxlength="50"
                autocomplete="name" /></label
            ><button
              class="button button-primary"
              :disabled="!canApplyClub || busy"
            >
              提交报名
            </button>
          </form>
        </section>
      </template>
      <section
        v-if="tab === 'club' || tab === 'review-club'"
        class="panel eid-section"
      >
        <div class="toolbar">
          <h2>{{ tab === "club" ? "我的报名记录" : "招募审核工作台" }}</h2>
          <label v-if="tab === 'review-club'" class="inline-field"
            >状态<select
              v-model="filter"
              @change="
                page = 1;
                load();
              "
            >
              <option value="all">全部</option>
              <option
                v-for="key in [
                  'pending',
                  'interview_sent',
                  'offer_sent',
                  'offer_confirmed',
                  'rejected',
                ]"
                :key="key"
                :value="key"
              >
                {{ statuses[key] }}
              </option>
            </select></label
          >
        </div>
        <div v-if="tab === 'review-club'" class="count-strip">
          <span
            v-for="key in [
              'pending',
              'interview_sent',
              'offer_sent',
              'offer_confirmed',
              'rejected',
            ]"
            :key="key"
            >{{ statuses[key] }} <strong>{{ counts[key] ?? 0 }}</strong></span
          >
        </div>
        <div v-if="!clubs.length" class="empty">当前没有报名记录。</div>
        <div v-else class="application-list">
          <article v-for="item in clubs" :key="item.id" class="record-card">
            <div class="toolbar">
              <div>
                <h3>
                  {{ item.realName }}
                  <span class="badge" :data-status="item.status">{{
                    statuses[item.status]
                  }}</span>
                </h3>
                <p class="muted small">
                  {{ item.email }} · {{ date(item.createdAt) }} ·
                  {{ item.trustVerified ? "报名时认证已通过" : "无需前置认证" }}
                </p>
              </div>
            </div>
            <div class="progress-steps" v-if="item.status !== 'rejected'">
              <span
                v-for="(stage, index) in [
                  'pending',
                  'interview_sent',
                  'offer_sent',
                  'offer_confirmed',
                ]"
                :key="stage"
                :class="{
                  done:
                    [
                      'pending',
                      'interview_sent',
                      'offer_sent',
                      'offer_confirmed',
                    ].indexOf(item.status) >= index,
                }"
                >{{
                  ["提交报名", "笔试通知", "录取通知", "接受录取"][index]
                }}</span
              >
            </div>
            <div
              v-if="tab === 'club' && item.status === 'offer_sent'"
              class="actions"
            >
              <button
                class="button button-primary"
                :disabled="busy"
                @click="acceptOffer(item)"
              >
                接受录取</button
              ><span class="muted small">请确认后主动接受录取。</span>
            </div>
            <div v-if="tab === 'review-club'" class="actions">
              <button
                v-if="item.status === 'pending'"
                class="button button-primary"
                :disabled="busy"
                @click="send(item, 'interview')"
              >
                发送笔试通知</button
              ><button
                v-if="item.status === 'interview_sent'"
                class="button"
                :disabled="busy"
                @click="send(item, 'interview', true)"
              >
                重发笔试通知</button
              ><button
                v-if="item.status === 'interview_sent'"
                class="button button-primary"
                :disabled="busy"
                @click="send(item, 'offer')"
              >
                发送录取通知</button
              ><button
                v-if="item.status === 'offer_sent'"
                class="button"
                :disabled="busy"
                @click="send(item, 'offer', true)"
              >
                重发录取通知</button
              ><button
                v-if="!['rejected', 'offer_confirmed'].includes(item.status)"
                class="button"
                :disabled="busy"
                @click="rejectClub(item)"
              >
                拒绝报名</button
              ><button
                v-if="app.can('admin')"
                class="button button-danger"
                :disabled="busy"
                @click="removeRecord(item.id, 'club')"
              >
                删除
              </button>
            </div>
            <History :items="item.history" />
          </article>
        </div>
      </section>
      <section v-if="tab === 'notifications'" class="panel eid-section">
        <div class="toolbar">
          <h2>通知发送记录</h2>
          <button class="button" @click="load">刷新</button>
        </div>
        <p class="muted">
          发送结果未知时，请核对邮件服务或收件箱，再决定是否在招募审核中重发。
        </p>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>通知</th>
                <th>状态</th>
                <th>申请记录</th>
                <th>时间</th>
                <th>说明</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="item in notifications" :key="item.id">
                <td>{{ notificationLabels[item.kind] ?? item.kind }}</td>
                <td>
                  <span class="badge">{{
                    statuses[item.state] ?? item.state
                  }}</span>
                </td>
                <td>
                  <code>{{ item.targetId }}</code>
                </td>
                <td>{{ date(item.createdAt) }}</td>
                <td>
                  {{ item.error || "—"
                  }}<button
                    v-if="
                      item.kind.startsWith('webhook_') &&
                      ['queued', 'failed'].includes(item.state)
                    "
                    class="button"
                    :disabled="busy"
                    @click="retryWebhook(item)"
                  >
                    重试机器人通知
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <div v-if="!notifications.length" class="empty">尚无通知发送记录。</div>
      </section>
      <section v-if="tab === 'members'" class="panel eid-section">
        <div class="toolbar">
          <h2>本项目会员</h2>
          <form
            class="actions"
            @submit.prevent="
              page = 1;
              load();
            "
          >
            <input
              v-model="search"
              aria-label="搜索会员"
              placeholder="姓名或欧拉成员编号"
            /><button class="button">搜索</button>
          </form>
        </div>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>会员</th>
                <th>邮箱</th>
                <th>身份</th>
                <th>加入时间</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="item in members" :key="item.actorId">
                <td>{{ item.name }}</td>
                <td>{{ item.email }}</td>
                <td>{{ item.identityTitle || "未认证" }}</td>
                <td>{{ date(item.createdAt) }}</td>
                <td>
                  <div class="actions">
                    <button class="button" @click="editMember(item)">
                      编辑</button
                    ><button
                      class="button button-danger"
                      @click="removeMember(item)"
                    >
                      删除业务记录
                    </button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <div v-if="!members.length" class="empty">
          成员使用本应用后会建立会员档案。欧拉账号在平台统一管理。
        </div>
      </section>
      <section v-if="tab === 'keys'" class="panel eid-section">
        <h2>资格查询接口</h2>
        <p class="muted">
          为业务系统签发只读密钥，明确允许查询的会员及有效期。结果不包含姓名、学号、邮箱和申请资料。
        </p>
        <form
          v-if="app.can('secrets')"
          class="form-grid"
          @submit.prevent="createQueryKey"
        >
          <label class="field"
            >密钥名称<input
              v-model="keyName"
              required
              maxlength="120"
              placeholder="例如：活动报名资格检查" /></label
          ><label class="field"
            >有效天数<input
              v-model.number="keyDays"
              type="number"
              min="1"
              max="365"
              required /></label
          ><MemberPicker v-model="keyActors" /><button
            class="button button-primary"
            :disabled="busy || !keyActors.length"
          >
            创建受限查询密钥
          </button>
        </form>
        <div v-if="rawToken" class="status-note">
          <strong>密钥仅在创建时显示，请安全保存</strong
          ><textarea :value="rawToken" readonly aria-label="新查询密钥" />
        </div>
        <h3>调用方式</h3>
        <p class="muted small">
          GET 请求携带 Authorization: Bearer 密钥。actorId
          必须在授权名单内；密钥放在服务端，勿写入公开网页。
        </p>
        <code>{{ app.publicBase }}/verification/status?actorId=会员编号</code>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>名称</th>
                <th>授权会员数</th>
                <th>有效期</th>
                <th>状态</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="key in queryKeys" :key="key.id">
                <td>{{ key.name }}</td>
                <td>{{ key.actorIds.length }}</td>
                <td>{{ date(key.expiresAt) }}</td>
                <td>
                  {{
                    key.revokedAt
                      ? "已撤销"
                      : new Date(key.expiresAt).getTime() > Date.now()
                        ? "有效"
                        : "已到期"
                  }}
                </td>
                <td>
                  <button
                    v-if="app.can('secrets') && !key.revokedAt"
                    class="button button-danger"
                    :disabled="busy"
                    @click="revokeQueryKey(key.id)"
                  >
                    撤销
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <div v-if="!queryKeys.length" class="empty">还没有签发查询密钥。</div>
      </section>
      <form
        v-if="tab === 'settings'"
        class="panel eid-section"
        @submit.prevent="saveSettings"
      >
        <h2>招募流程</h2>
        <div class="form-grid">
          <label class="field"
            >招募名称<input
              v-model="settings.clubName"
              required
              maxlength="100" /></label
          ><label class="field"
            >前置认证方案<select
              v-model="settings.trustSchemeId"
              aria-label="前置认证方案"
            >
              <option value="">尚未绑定</option>
              <option
                v-for="scheme in schemes"
                :key="scheme.id"
                :value="scheme.id"
              >
                {{ scheme.name }}
              </option>
            </select></label
          ><label class="check-field full"
            ><input v-model="settings.requireTrust" type="checkbox" />
            报名与发放录取时，必须通过所选 Trust 认证</label
          >
          <p v-if="!schemes.length" class="muted full">
            请先启用本项目 Trust，并由获得方案运营权限的成员创建认证方案。
          </p>
        </div>
        <h3>邮件内容</h3>
        <p class="muted small">
          支持占位符：<code v-pre>{{ name }}</code> 姓名、<code v-pre>{{
            club
          }}</code>
          招募名称、<code v-pre>{{ url }}</code> 本项目办理入口。
        </p>
        <div class="form-grid">
          <label class="field"
            >笔试主题<input
              v-model="settings.interviewSubject"
              required
              maxlength="150" /></label
          ><label class="field"
            >录取主题<input
              v-model="settings.offerSubject"
              required
              maxlength="150" /></label
          ><label class="field"
            >笔试正文<textarea
              v-model="settings.interviewBody"
              required
              maxlength="12000"
            /></label
          ><label class="field"
            >录取正文<textarea
              v-model="settings.offerBody"
              required
              maxlength="12000"
            />
          </label>
        </div>
        <h3>邮件服务</h3>
        <p class="muted small">
          邮件使用 TLS 发送。更改服务器、用户名或 TLS
          方式时，需重新输入密码或明确清除。通知按钮会实际投递，失败时保留记录并保持报名状态。
        </p>
        <div class="form-grid">
          <label class="field"
            >SMTP 地址<input
              v-model="settings.smtpAddress"
              placeholder="smtp.example.com:587"
              :disabled="!app.can('secrets')" /></label
          ><label class="field"
            >发件人邮箱<input
              v-model="settings.smtpFrom"
              type="email"
              :disabled="!app.can('secrets')" /></label
          ><label class="field"
            >SMTP 用户名<input
              v-model="settings.smtpUsername"
              autocomplete="off"
              :disabled="!app.can('secrets')" /></label
          ><label class="field"
            >SMTP 密码<input
              v-model="settings.smtpPassword"
              type="password"
              autocomplete="new-password"
              :placeholder="
                configured.smtpPassword ? '已配置，留空保持不变' : '尚未配置'
              "
              :disabled="!app.can('secrets')" /></label
          ><label class="check-field"
            ><input
              v-model="settings.smtpImplicitTLS"
              type="checkbox"
              :disabled="!app.can('secrets')"
            />
            使用隐式 TLS（通常为 465 端口）</label
          ><label class="check-field"
            ><input
              v-model="settings.clearSMTPPassword"
              type="checkbox"
              :disabled="!app.can('secrets')"
            />
            清除已保存的 SMTP 密码</label
          >
        </div>
        <h3>流程通知</h3>
        <p class="muted small">
          事件消息只包含事件与记录编号，不包含姓名、学号和邮箱。
        </p>
        <div class="form-grid">
          <label class="field"
            >企业微信机器人<input
              v-model="settings.wecomUrl"
              type="password"
              autocomplete="new-password"
              :placeholder="
                configured.wecom
                  ? '已配置，留空保持不变'
                  : '官方 HTTPS Webhook 地址'
              "
              :disabled="!app.can('secrets')" /></label
          ><label class="field"
            >飞书机器人<input
              v-model="settings.feishuUrl"
              type="password"
              autocomplete="new-password"
              :placeholder="
                configured.feishu
                  ? '已配置，留空保持不变'
                  : '官方 HTTPS Webhook 地址'
              "
              :disabled="!app.can('secrets')" /></label
          ><label class="check-field"
            ><input
              v-model="settings.clearWecom"
              type="checkbox"
              :disabled="!app.can('secrets')"
            />
            清除企业微信机器人</label
          ><label class="check-field"
            ><input
              v-model="settings.clearFeishu"
              type="checkbox"
              :disabled="!app.can('secrets')"
            />
            清除飞书机器人</label
          >
        </div>
        <div class="actions">
          <button class="button button-primary" :disabled="busy">
            保存流程设置
          </button>
        </div>
      </form>
      <div v-if="showPager" class="pagination">
        <button
          class="button"
          :disabled="page <= 1"
          @click="movePage(page - 1)"
        >
          上一页</button
        ><span>第 {{ page }} 页 · 共 {{ total }} 条</span
        ><button
          class="button"
          :disabled="page * 25 >= total"
          @click="movePage(page + 1)"
        >
          下一页
        </button>
      </div>
    </template>
    <CloudModal
      v-model="applying"
      title="申请成员身份"
      description="请填写真实资料。当前项目的身份审核员可以查看这份申请。"
      ><form class="form-grid" @submit.prevent="submitVerification">
        <label class="field"
          >真实姓名<input
            v-model="application.realName"
            required
            maxlength="50" /></label
        ><label class="field"
          >学号<input
            v-model="application.studentId"
            required
            maxlength="40" /></label
        ><label class="field"
          >身份名称<input
            v-model="application.identityTitle"
            required
            maxlength="100"
            placeholder="例如：研发中心核心成员" /></label
        ><label class="field"
          >身份类型<select
            v-model="application.identityType"
            aria-label="身份类型"
          >
            <option v-for="(name, key) in identities" :key="key" :value="key">
              {{ name }}
            </option>
          </select></label
        >
        <div class="actions full">
          <button class="button button-primary" :disabled="busy">
            提交申请</button
          ><button type="button" class="button" @click="applying = false">
            取消
          </button>
        </div>
      </form></CloudModal
    >
    <CloudModal
      v-model="editOpen"
      title="编辑身份申请"
      description="编辑会保留处理记录。已通过申请的身份卡也会同步更新。"
      ><form class="form-grid" @submit.prevent="saveVerification">
        <label class="field"
          >真实姓名<input
            v-model="edit.realName"
            required
            maxlength="50" /></label
        ><label class="field"
          >学号<input v-model="edit.studentId" required maxlength="40" /></label
        ><label class="field"
          >身份名称<input
            v-model="edit.identityTitle"
            required
            maxlength="100" /></label
        ><label class="field"
          >身份类型<select v-model="edit.identityType">
            <option v-for="(name, key) in identities" :key="key" :value="key">
              {{ name }}
            </option>
          </select></label
        ><button class="button button-primary" :disabled="busy">
          保存更改
        </button>
      </form></CloudModal
    >
    <CloudModal
      v-model="memberOpen"
      title="编辑会员业务资料"
      description="这里不修改通行证账号、邮箱或欧拉角色。"
      ><form class="form-grid" @submit.prevent="saveMember">
        <label class="field"
          >身份类别<select
            v-model.number="memberForm.identityLevel"
            :disabled="!app.can('review')"
          >
            <option :value="0">未认证</option>
            <option
              v-for="(name, key, index) in identities"
              :key="key"
              :value="index + 1"
            >
              {{ name }}
            </option>
          </select></label
        ><label class="field"
          >身份名称<input
            v-model="memberForm.identityTitle"
            maxlength="100"
            :disabled="!app.can('review')" /></label
        ><label class="field"
          >联系电话<input v-model="memberForm.phone" maxlength="40" /></label
        ><label class="field"
          >头像地址<input v-model="memberForm.avatar" type="url" /></label
        ><label class="field full"
          >简介<textarea v-model="memberForm.bio" maxlength="2000" /></label
        ><button class="button button-primary" :disabled="busy">
          保存会员资料
        </button>
      </form></CloudModal
    >
    <CloudModal
      v-model="confirmOpen"
      :title="confirmation?.title ?? '确认操作'"
      :description="confirmation?.description"
      ><div class="actions">
        <button
          class="button button-primary"
          :disabled="busy"
          @click="confirmNow"
        >
          {{ busy ? "正在处理…" : "确认操作" }}</button
        ><button class="button" :disabled="busy" @click="confirmOpen = false">
          取消
        </button>
      </div></CloudModal
    >
  </div>
</template>
<style scoped>
.eid-section {
  padding: 24px;
}
.eid-section h2 {
  font-size: 18px;
  margin: 0 0 8px;
}
.eid-section h3 {
  font-size: 15px;
  margin: 18px 0 10px;
}
.eid-section .toolbar h3 {
  margin: 0 0 8px;
}
.eid-section p {
  line-height: 1.7;
}
.small {
  font-size: 12px;
}
.muted {
  color: var(--muted);
}
.error-note {
  border-color: #dc7980;
  color: #9d2433;
}
.inline-field {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 13px;
}
.inline-field select {
  min-width: 120px;
}
.credential-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(250px, 1fr));
  gap: 16px;
  margin-top: 24px;
}
.credential {
  padding: 25px;
  border: 1px solid #d7d5ee;
  border-radius: 15px;
  background: linear-gradient(120deg, #efedfb, #fbf9f3);
  color: #3e3565;
  min-height: 175px;
}
.credential-brand {
  font-weight: 800;
  letter-spacing: 0.1em;
  font-size: 22px;
}
.credential-brand span {
  font-size: 8px;
  font-weight: 500;
  margin-left: 12px;
}
.credential p {
  font-size: 12px;
  margin-top: 25px;
  margin-bottom: 0;
}
.credential h3 {
  font-size: 19px;
  margin-top: 4px;
}
.credential-bottom {
  display: flex;
  justify-content: space-between;
  gap: 10px;
  margin-top: 24px;
  font-size: 11px;
}
.application-list {
  display: grid;
  gap: 14px;
  margin-top: 20px;
}
.record-card {
  border: 1px solid var(--border);
  border-radius: 12px;
  padding: 18px;
}
.record-card .actions {
  margin-top: 14px;
}
.badge {
  font-size: 11px;
  margin-left: 8px;
  vertical-align: middle;
}
.badge[data-status="approved"],
.badge[data-status="offer_confirmed"] {
  background: #edf7f1;
  color: #24734e;
}
.badge[data-status="rejected"] {
  background: #fbeff0;
  color: #a83f4f;
}
.profile-heading {
  display: flex;
  gap: 20px;
  align-items: center;
  margin-bottom: 30px;
}
.profile-heading img,
.profile-avatar {
  width: 70px;
  height: 70px;
  border-radius: 18px;
  object-fit: cover;
}
.profile-avatar {
  display: grid;
  place-items: center;
  background: #eeeafa;
  color: #6552a8;
  font-size: 27px;
}
.eligibility {
  display: flex;
  flex-wrap: wrap;
  gap: 14px;
  padding: 16px 0;
  color: #9b6b28;
  font-size: 13px;
}
.eligibility .ready {
  color: #277958;
}
.text-link {
  color: var(--accent);
}
.club-form {
  display: flex;
  gap: 14px;
  align-items: end;
  max-width: 550px;
  margin-top: 12px;
}
.club-form .field {
  flex: 1;
}
.progress-steps {
  display: flex;
  gap: 10px;
  margin: 20px 0;
}
.progress-steps span {
  flex: 1;
  padding-top: 10px;
  border-top: 3px solid var(--border);
  font-size: 11px;
  color: var(--muted);
}
.progress-steps .done {
  border-color: #8471c6;
  color: #69529f;
}
.count-strip {
  display: flex;
  flex-wrap: wrap;
  gap: 15px;
  margin: 18px 0;
  font-size: 12px;
  color: var(--muted);
}
.count-strip strong {
  color: var(--text);
  margin-left: 5px;
}
.check-field {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 13px;
  line-height: 1.6;
}
.check-field input {
  width: auto;
}
.eid-section > .actions {
  margin-top: 25px;
}
.eid-section > .form-grid {
  margin-top: 15px;
}
.eid-section > h3 {
  margin-top: 32px;
}
.table-wrap code {
  font-size: 10px;
}
.table-wrap td {
  font-size: 12px;
}
@media (max-width: 650px) {
  .eid-section {
    padding: 18px;
  }
  .credential-grid {
    grid-template-columns: 1fr;
  }
  .club-form {
    flex-direction: column;
    align-items: stretch;
  }
  .profile-heading {
    gap: 12px;
  }
  .profile-heading img,
  .profile-avatar {
    width: 52px;
    height: 52px;
  }
  .progress-steps {
    gap: 5px;
  }
  .progress-steps span {
    font-size: 10px;
  }
  .credential-bottom {
    flex-direction: column;
  }
  .record-card {
    padding: 14px;
  }
}
</style>
