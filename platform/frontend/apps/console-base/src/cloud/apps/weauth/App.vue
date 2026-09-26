<script setup lang="ts">
import {
  computed,
  onBeforeUnmount,
  onMounted,
  reactive,
  ref,
  watch,
} from "vue";
import { useApp } from "../shared";

interface Settings {
  name: string;
  enabled: boolean;
  baseDifficulty: number;
  maxDifficulty: number;
  challengeTimeout: number;
  tokenTimeout: number;
  batchModeEnabled: boolean;
  batchDifficulty: number;
  minBatchCount: number;
  maxBatchCount: number;
}
interface Policy {
  enabled: boolean;
  timeWindowSeconds: number;
  requestThreshold: number;
  difficultyIncrement: number;
  batchIncrement: number;
  failureWeight: number;
}
interface Site extends Settings {
  id: string;
  sitekey: string;
  domains: string[];
  policy: Policy;
  createdAt: string;
}
interface Stats {
  totalChallenges: number;
  solvedChallenges: number;
  totalVerifications: number;
  successVerifications: number;
  last24hChallenges: number;
  last24hSolved: number;
}
interface IPRecord {
  ip: string;
  windowStart: number;
  requests: number;
  failures: number;
  lastAt: number;
}
const app = useApp();
const currentHostname = location.hostname;
const sites = ref<Site[]>([]),
  selectedID = ref(""),
  selected = ref<Site | null>(null),
  tab = ref("overview"),
  loading = ref(true),
  busy = ref(false),
  error = ref(""),
  secret = ref(""),
  createOpen = ref(false),
  newDomain = ref("");
const stats = ref<Stats | null>(null),
  records = ref<IPRecord[]>([]),
  preview = ref<HTMLIFrameElement | null>(null),
  previewToken = ref(""),
  previewMessage = ref("点击验证组件，完成真实工作量证明。"),
  previewVersion = ref(0),
  previewAction = ref("login"),
  previewTheme = ref("light"),
  previewColor = ref("blue"),
  previewCustomColor = ref(""),
  previewSize = ref("normal"),
  previewAutoStart = ref(false);
const tokenResult = ref<{
  success: boolean;
  hostname?: string;
  action?: string;
  "error-codes"?: string[];
} | null>(null);
const defaults = (): Settings => ({
  name: "",
  enabled: true,
  baseDifficulty: 4,
  maxDifficulty: 6,
  challengeTimeout: 300,
  tokenTimeout: 300,
  batchModeEnabled: true,
  batchDifficulty: 4,
  minBatchCount: 1,
  maxBatchCount: 10,
});
const settings = reactive(defaults()),
  creation = reactive({ ...defaults(), domainsText: "" });
const policy = reactive<Policy>({
  enabled: true,
  timeWindowSeconds: 300,
  requestThreshold: 10,
  difficultyIncrement: 1,
  batchIncrement: 1,
  failureWeight: 2,
});
const tabs = [
  { id: "overview", label: "概览" },
  { id: "settings", label: "验证配置" },
  { id: "domains", label: "域名与密钥" },
  { id: "risk", label: "IP 风控" },
  { id: "integration", label: "集成与预览" },
];
const successRate = computed(() =>
  stats.value?.totalVerifications
    ? (
        (stats.value.successVerifications / stats.value.totalVerifications) *
        100
      ).toFixed(1) + "%"
    : "—",
);
const previewHostAllowed = computed(
  () =>
    selected.value?.domains.some(
      (d) =>
        d === location.hostname ||
        (d.startsWith("*.") && location.hostname.endsWith(d.slice(1))),
    ) ?? false,
);
const previewURL = computed(
  () =>
    `${app.publicBase}/widget.html?${new URLSearchParams({ sitekey: selected.value?.sitekey ?? "", origin: location.origin, action: previewAction.value, theme: previewTheme.value, themeColor: previewColor.value, customColor: previewCustomColor.value, size: previewSize.value, autoStart: String(previewAutoStart.value), v: String(previewVersion.value) })}`,
);
const browserSnippet = computed(
  () =>
    `<script src="${app.publicBase}/weauth.js" defer><\/script>\n<div class="weauth" data-sitekey="${selected.value?.sitekey ?? "YOUR_SITEKEY"}"\n     data-action="login" data-theme="light"></div>\n<!-- 表单包含 weauth-response 字段；提交到你的业务服务端验证。 -->`,
);
const explicitSnippet = computed(
  () =>
    `const widget = weauth.render('#verification', {\n  sitekey: '${selected.value?.sitekey ?? "YOUR_SITEKEY"}',\n  action: 'login',\n  theme: 'light',\n  themeColor: 'blue', // violet / emerald / rose / orange / cyan\n  // customColor: '#3B82F6', size: 'flexible', autoStart: true,\n  callback(token) { /* 把 token 提交到你的业务服务端 */ },\n  'expired-callback'() { /* 清空已保存的令牌 */ }\n});\nweauth.execute(widget); // 主动验证\nweauth.reset(widget);   // 重置\nweauth.getResponse(widget);\nweauth.remove(widget);`,
);
const serverSnippet = computed(
  () =>
    `const verification = await fetch('${app.publicBase}/pow/siteverify', {\n  method: 'POST',\n  headers: { 'Content-Type': 'application/json' },\n  body: JSON.stringify({\n    secret: process.env.WEAUTH_SECRET, // 只保存在服务端\n    token: request.body['weauth-response']\n  })\n}).then(response => response.json());\nif (!verification.success ||\n    verification.hostname !== 'your-domain.example' ||\n    verification.action !== 'login') {\n  throw new Error('验证未通过');\n}\n// 验证令牌已消耗，现在执行实际业务操作。`,
);

function settingsOf(site: Site): Settings {
  const value = defaults();
  for (const key of Object.keys(value) as (keyof Settings)[])
    (value as unknown as Record<string, unknown>)[key] = site[key];
  return value;
}
function fail(value: unknown) {
  error.value = value instanceof Error ? value.message : "操作失败，请重试";
  app.notify(error.value, "error");
}
async function perform(action: () => Promise<void>, message?: string) {
  if (busy.value) return;
  busy.value = true;
  error.value = "";
  try {
    await action();
    if (message) app.notify(message, "success");
  } catch (e) {
    fail(e);
  } finally {
    busy.value = false;
  }
}
async function refreshSites() {
  sites.value = (await app.request<{ items: Site[] }>("/sites")).items;
  if (!sites.value.some((v) => v.id === selectedID.value))
    selectedID.value = sites.value[0]?.id ?? "";
}
async function refreshDetail() {
  const id = selectedID.value;
  if (!id) {
    selected.value = null;
    return;
  }
  const [site, summary] = await Promise.all([
    app.request<Site>(`/sites/${id}`),
    app.request<Stats>(`/sites/${id}/stats`),
  ]);
  if (selectedID.value !== id) return;
  selected.value = site;
  stats.value = summary;
  Object.assign(settings, settingsOf(site));
  Object.assign(policy, site.policy);
  if (app.can("manage")) {
    const response = await app.request<{ items: IPRecord[] }>(
      `/sites/${id}/ip-records`,
    );
    if (selectedID.value === id) records.value = response.items;
  }
}
async function reload() {
  await perform(async () => {
    await refreshSites();
    await refreshDetail();
  });
}
watch(selectedID, async () => {
  secret.value = "";
  selected.value = null;
  stats.value = null;
  records.value = [];
  previewToken.value = "";
  tokenResult.value = null;
  previewVersion.value++;
  try {
    await refreshDetail();
  } catch (e) {
    fail(e);
  }
});
watch(
  [
    previewAction,
    previewTheme,
    previewColor,
    previewCustomColor,
    previewSize,
    previewAutoStart,
  ],
  () => {
    previewToken.value = "";
    tokenResult.value = null;
  },
);
async function create() {
  await perform(async () => {
    const { domainsText, ...value } = creation;
    const site = await app.request<Site>("/sites", {
      method: "POST",
      body: {
        ...value,
        domains: domainsText
          .split(/[\n,]/)
          .map((v) => v.trim())
          .filter(Boolean),
      },
    });
    createOpen.value = false;
    Object.assign(creation, defaults(), { domainsText: "" });
    await refreshSites();
    selectedID.value = site.id;
    tab.value = "integration";
  }, "站点已创建，密钥可在“域名与密钥”中单独查看。");
}
async function saveSettings() {
  await perform(async () => {
    await app.request(`/sites/${selectedID.value}`, {
      method: "PUT",
      body: { ...settings },
    });
    await refreshSites();
    await refreshDetail();
  }, "验证配置已保存");
}
async function savePolicy() {
  await perform(async () => {
    await app.request(`/sites/${selectedID.value}/ip-policy`, {
      method: "PUT",
      body: { ...policy },
    });
    await refreshDetail();
  }, "IP 风控策略已保存");
}
async function addDomain(value = newDomain.value) {
  await perform(async () => {
    await app.request(`/sites/${selectedID.value}/domains`, {
      method: "POST",
      body: { domain: value },
    });
    newDomain.value = "";
    await refreshDetail();
  }, "域名已加入白名单");
}
async function removeDomain(value: string) {
  if (!confirm(`移除 ${value} 后，该域名将不能申请新的验证。继续吗？`)) return;
  await perform(async () => {
    await app.request(
      `/sites/${selectedID.value}/domains/${encodeURIComponent(value)}`,
      { method: "DELETE" },
    );
    await refreshDetail();
  }, "域名已移除");
}
async function revealSecret() {
  await perform(async () => {
    const id = selectedID.value;
    const result = await app.request<{ secret: string }>(`/sites/${id}/secret`);
    if (selectedID.value === id) secret.value = result.secret;
  });
}
async function rotateSecret() {
  if (
    !confirm(
      "轮换后，旧服务端密钥立即失效。请准备同步更新业务服务配置。确定继续？",
    )
  )
    return;
  secret.value = "";
  await perform(async () => {
    const id = selectedID.value;
    const result = await app.request<{ secret: string }>(
      `/sites/${id}/regenerate-secret`,
      { method: "POST", body: { confirm: true } },
    );
    if (selectedID.value === id) secret.value = result.secret;
  }, "密钥已轮换，请更新业务服务配置");
}
async function deleteSite() {
  if (
    !selected.value ||
    prompt(
      `此操作会删除站点、挑战和统计。请输入站点名称“${selected.value.name}”确认：`,
    ) !== selected.value.name
  )
    return;
  await perform(async () => {
    await app.request(`/sites/${selectedID.value}`, { method: "DELETE" });
    selectedID.value = "";
    await refreshSites();
  }, "站点已删除");
}
async function clearRecord(ip: string) {
  if (!confirm(`清除 ${ip} 的风控计数？该 IP 的难度将重新计算。`)) return;
  await perform(async () => {
    await app.request(
      `/sites/${selectedID.value}/ip-records/${encodeURIComponent(ip)}`,
      { method: "DELETE" },
    );
    await refreshDetail();
  }, "IP 计数已清除");
}
async function copy(value: string) {
  try {
    await navigator.clipboard.writeText(value);
    app.notify("已复制", "success");
  } catch {
    app.notify("无法访问剪贴板，请手动复制", "error");
  }
}
function onWidgetMessage(event: MessageEvent) {
  if (
    event.origin !== new URL(app.publicBase).origin ||
    event.source !== preview.value?.contentWindow ||
    event.data?.type !== "euler-weauth"
  )
    return;
  if (event.data.event === "success") {
    previewToken.value = event.data.token;
    tokenResult.value = null;
    previewMessage.value = "真实验证已通过。可在服务端测试一次性令牌。";
    void refreshDetail().catch(fail);
  } else if (event.data.event === "expired") {
    previewToken.value = "";
    previewMessage.value = "令牌已过期，请重新验证。";
  } else if (event.data.event === "error") {
    previewToken.value = "";
    previewMessage.value = event.data.message;
  }
}
async function testToken() {
  await perform(async () => {
    const id = selectedID.value;
    const result = await app.request<{
      success: boolean;
      hostname?: string;
      action?: string;
      "error-codes"?: string[];
    }>(`/sites/${id}/test-token`, {
      method: "POST",
      body: { token: previewToken.value },
    });
    if (selectedID.value === id) {
      tokenResult.value = result;
      previewToken.value = "";
    }
  });
}
onMounted(async () => {
  window.addEventListener("message", onWidgetMessage);
  try {
    await refreshSites();
  } catch (e) {
    fail(e);
  } finally {
    loading.value = false;
  }
});
onBeforeUnmount(() => {
  window.removeEventListener("message", onWidgetMessage);
  secret.value = "";
  previewToken.value = "";
});
</script>

<template>
  <section class="weauth-app">
    <header class="app-heading">
      <div>
        <span class="eyebrow">WEAUTH · 人机验证</span>
        <h1>让每一次访问更可信</h1>
        <p>
          管理验证站点、工作量证明与 IP 风控。身份和权限由当前 Euler
          项目统一管理。
        </p>
      </div>
      <div class="actions">
        <button class="button" :disabled="busy" @click="reload">刷新</button
        ><button
          v-if="app.can('write')"
          class="button primary"
          @click="createOpen = true"
        >
          新建站点
        </button>
      </div>
    </header>
    <p v-if="error" role="alert" class="alert danger">{{ error }}</p>
    <div v-if="loading" class="empty" role="status">正在加载验证站点…</div>
    <div v-else-if="!sites.length" class="empty">
      <div class="empty-icon">✓</div>
      <h2>创建第一个验证站点</h2>
      <p>
        为网站获得公开 Sitekey，配置允许访问的域名，再把验证组件加入业务表单。
      </p>
      <button
        v-if="app.can('write')"
        class="button primary"
        @click="createOpen = true"
      >
        创建验证站点
      </button>
    </div>
    <div v-else class="workspace">
      <aside class="site-list">
        <div class="list-heading">
          验证站点 <span>{{ sites.length }}</span>
        </div>
        <button
          v-for="site in sites"
          :key="site.id"
          class="site-item"
          :class="{ active: site.id === selectedID }"
          @click="selectedID = site.id"
        >
          <span class="site-avatar">{{
            site.name.slice(0, 1).toUpperCase()
          }}</span
          ><span
            ><strong>{{ site.name }}</strong
            ><small>{{ site.domains[0] || "尚未配置域名" }}</small></span
          ><i
            :class="{ enabled: site.enabled }"
            :title="site.enabled ? '已启用' : '已停用'"
          />
        </button>
      </aside>
      <main v-if="selected" class="site-main">
        <div class="site-header">
          <div>
            <h2>
              {{ selected.name }}
              <span class="badge" :class="selected.enabled ? 'good' : ''">{{
                selected.enabled ? "已启用" : "已停用"
              }}</span>
            </h2>
            <p class="mono sitekey">
              {{ selected.sitekey }}
              <button class="link" @click="copy(selected.sitekey)">
                复制 Sitekey
              </button>
            </p>
          </div>
          <span class="muted">{{
            selected.batchModeEnabled ? "批量验证" : "动态难度"
          }}</span>
        </div>
        <nav class="tabs" aria-label="站点功能">
          <button
            v-for="item in tabs"
            :key="item.id"
            :class="{ active: tab === item.id }"
            @click="tab = item.id"
          >
            {{ item.label }}
          </button>
        </nav>
        <div v-if="tab === 'overview'" class="tab-body">
          <div class="metrics">
            <article>
              <small>24 小时挑战</small
              ><strong>{{ stats?.last24hChallenges ?? "—" }}</strong
              ><span>最近 24 小时的验证请求</span>
            </article>
            <article>
              <small>24 小时通过</small
              ><strong>{{ stats?.last24hSolved ?? "—" }}</strong
              ><span>已完成工作量证明</span>
            </article>
            <article>
              <small>累计验证次数</small
              ><strong>{{ stats?.totalVerifications ?? "—" }}</strong
              ><span>{{ stats?.totalChallenges ?? 0 }} 次挑战</span>
            </article>
            <article>
              <small>验证通过率</small><strong>{{ successRate }}</strong
              ><span>{{ stats?.successVerifications ?? 0 }} 次有效证明</span>
            </article>
          </div>
          <div class="two-columns">
            <article class="panel">
              <h3>当前防护配置</h3>
              <dl>
                <div>
                  <dt>验证模式</dt>
                  <dd>
                    {{
                      selected.batchModeEnabled
                        ? "固定难度 · 动态批次"
                        : "IP 风险动态难度"
                    }}
                  </dd>
                </div>
                <div>
                  <dt>难度</dt>
                  <dd>
                    {{
                      selected.batchModeEnabled
                        ? selected.batchDifficulty
                        : `${selected.baseDifficulty} → ${selected.maxDifficulty}`
                    }}
                  </dd>
                </div>
                <div>
                  <dt>挑战 / 令牌有效期</dt>
                  <dd>
                    {{ selected.challengeTimeout }} /
                    {{ selected.tokenTimeout }} 秒
                  </dd>
                </div>
                <div>
                  <dt>IP 风控</dt>
                  <dd>{{ selected.policy.enabled ? "已开启" : "已关闭" }}</dd>
                </div>
                <div>
                  <dt>域名白名单</dt>
                  <dd>{{ selected.domains.length }} 个域名</dd>
                </div>
              </dl>
              <button class="link" @click="tab = 'settings'">
                调整验证配置 →
              </button>
            </article>
            <article class="panel setup-card">
              <h3>接入你的应用</h3>
              <p>
                公开 Sitekey
                用于浏览器组件。服务端密钥负责消费一次性验证令牌，两者职责独立。
              </p>
              <ol>
                <li>设置允许使用的域名</li>
                <li>嵌入真实验证组件</li>
                <li>服务端校验令牌、域名及 action</li>
              </ol>
              <button class="button primary" @click="tab = 'integration'">
                查看集成指南
              </button>
            </article>
          </div>
        </div>
        <form
          v-if="tab === 'settings'"
          class="tab-body"
          @submit.prevent="saveSettings"
        >
          <article class="panel">
            <h3>基本设置</h3>
            <div class="form-grid">
              <label
                >站点名称<input
                  v-model="settings.name"
                  required
                  maxlength="100"
                  :disabled="!app.can('write')" /></label
              ><label class="check"
                ><input
                  v-model="settings.enabled"
                  type="checkbox"
                  :disabled="!app.can('write')"
                />启用验证站点</label
              >
            </div>
            <p class="muted">
              停用后，新的挑战、证明提交和服务端令牌验证都会被拒绝。
            </p>
          </article>
          <article class="panel">
            <h3>工作量证明</h3>
            <div class="mode-options">
              <label :class="{ chosen: settings.batchModeEnabled }"
                ><input
                  v-model="settings.batchModeEnabled"
                  type="radio"
                  :value="true"
                  :disabled="!app.can('write')"
                /><strong>批量模式</strong
                ><span>固定单次难度，根据 IP 风险增加证明数量。</span></label
              ><label :class="{ chosen: !settings.batchModeEnabled }"
                ><input
                  v-model="settings.batchModeEnabled"
                  type="radio"
                  :value="false"
                  :disabled="!app.can('write')"
                /><strong>动态模式</strong
                ><span>一次证明，根据 IP 风险增加哈希难度。</span></label
              >
            </div>
            <div v-if="settings.batchModeEnabled" class="form-grid three">
              <label
                >单次难度（1–5）<input
                  v-model.number="settings.batchDifficulty"
                  type="number"
                  min="1"
                  max="5"
                  required
                  :disabled="!app.can('write')" /></label
              ><label
                >最少批次（1–10）<input
                  v-model.number="settings.minBatchCount"
                  type="number"
                  min="1"
                  max="10"
                  required
                  :disabled="!app.can('write')" /></label
              ><label
                >最多批次（≤20）<input
                  v-model.number="settings.maxBatchCount"
                  type="number"
                  :min="settings.minBatchCount"
                  max="20"
                  required
                  :disabled="!app.can('write')"
              /></label>
            </div>
            <div v-else class="form-grid">
              <label
                >基础难度（1–10）<input
                  v-model.number="settings.baseDifficulty"
                  type="number"
                  min="1"
                  max="10"
                  required
                  :disabled="!app.can('write')" /></label
              ><label
                >最大难度（≤12）<input
                  v-model.number="settings.maxDifficulty"
                  type="number"
                  :min="settings.baseDifficulty"
                  max="12"
                  required
                  :disabled="!app.can('write')"
              /></label>
            </div>
            <p class="hint">
              难度每增加 1，预期计算量约增加 16
              倍。过高的难度可能导致普通设备在有效期内无法完成验证。
            </p>
          </article>
          <article class="panel">
            <h3>有效期</h3>
            <div class="form-grid">
              <label
                >挑战有效期（10–3600 秒）<input
                  v-model.number="settings.challengeTimeout"
                  type="number"
                  min="10"
                  max="3600"
                  required
                  :disabled="!app.can('write')" /></label
              ><label
                >令牌有效期（10–3600 秒）<input
                  v-model.number="settings.tokenTimeout"
                  type="number"
                  min="10"
                  max="3600"
                  required
                  :disabled="!app.can('write')"
              /></label>
            </div>
            <p class="muted">
              每个令牌仅可由服务端成功验证一次，过期或重复验证都会失败。
            </p>
          </article>
          <div class="actions">
            <button
              v-if="app.can('write')"
              class="button primary"
              :disabled="busy"
            >
              保存验证配置</button
            ><button
              v-if="app.can('manage')"
              type="button"
              class="button destructive"
              :disabled="busy"
              @click="deleteSite"
            >
              删除站点
            </button>
          </div>
        </form>
        <div v-if="tab === 'domains'" class="tab-body">
          <article class="panel">
            <h3>域名白名单</h3>
            <p class="muted">
              输入纯域名，如 example.com 或
              *.example.com。通配规则仅匹配子域名；没有域名时拒绝所有挑战。
            </p>
            <form
              v-if="app.can('write')"
              class="inline-form"
              @submit.prevent="addDomain()"
            >
              <input
                v-model="newDomain"
                placeholder="app.example.com"
                required
                maxlength="255"
                aria-label="允许的域名"
              /><button class="button primary" :disabled="busy">
                添加域名
              </button>
            </form>
            <ul class="domain-list">
              <li v-for="value in selected.domains" :key="value">
                <span class="mono">{{ value }}</span
                ><button
                  v-if="app.can('write')"
                  class="link danger-text"
                  :disabled="busy"
                  @click="removeDomain(value)"
                >
                  移除
                </button>
              </li>
            </ul>
            <p v-if="!selected.domains.length" class="alert">
              请先添加业务域名，验证组件才能正常工作。
            </p>
          </article>
          <article class="panel">
            <h3>站点密钥</h3>
            <label
              >Sitekey · 可在浏览器公开
              <div class="copy-field">
                <code>{{ selected.sitekey }}</code
                ><button class="button" @click="copy(selected.sitekey)">
                  复制
                </button>
              </div></label
            >
            <p class="hint">
              Secret
              仅供业务服务端使用，不要写入前端代码、公开仓库或日志。查看和轮换会记录审计。
            </p>
            <template v-if="app.can('secrets')"
              ><div v-if="secret" class="secret-value">
                <code>{{ secret }}</code>
                <div class="actions">
                  <button class="button" @click="copy(secret)">复制密钥</button
                  ><button class="button" @click="secret = ''">隐藏</button>
                </div>
              </div>
              <div class="actions">
                <button
                  v-if="!secret"
                  class="button"
                  :disabled="busy"
                  @click="revealSecret"
                >
                  查看服务端密钥</button
                ><button
                  class="button destructive"
                  :disabled="busy"
                  @click="rotateSecret"
                >
                  轮换密钥
                </button>
              </div></template
            >
            <p v-else class="muted">你的项目权限不包含查看或轮换密钥。</p>
          </article>
        </div>
        <div v-if="tab === 'risk'" class="tab-body">
          <form class="panel" @submit.prevent="savePolicy">
            <div class="panel-title">
              <h3>IP 风控策略</h3>
              <label class="check"
                ><input
                  v-model="policy.enabled"
                  type="checkbox"
                  :disabled="!app.can('manage')"
                />启用</label
              >
            </div>
            <p class="muted">
              在时间窗口内，累计请求与失败权重决定难度或批次数增量。平台仅信任部署时明确配置的代理地址。
            </p>
            <div class="form-grid three">
              <label
                >时间窗口（秒）<input
                  v-model.number="policy.timeWindowSeconds"
                  type="number"
                  min="10"
                  max="86400"
                  required
                  :disabled="!app.can('manage')" /></label
              ><label
                >请求阈值<input
                  v-model.number="policy.requestThreshold"
                  type="number"
                  min="1"
                  max="100000"
                  required
                  :disabled="!app.can('manage')" /></label
              ><label
                >失败权重<input
                  v-model.number="policy.failureWeight"
                  type="number"
                  min="0"
                  max="100"
                  step="0.1"
                  required
                  :disabled="!app.can('manage')" /></label
              ><label
                >动态难度增量<input
                  v-model.number="policy.difficultyIncrement"
                  type="number"
                  min="0"
                  max="10"
                  required
                  :disabled="!app.can('manage')" /></label
              ><label
                >批次数增量<input
                  v-model.number="policy.batchIncrement"
                  type="number"
                  min="0"
                  max="20"
                  required
                  :disabled="!app.can('manage')"
              /></label>
            </div>
            <button
              v-if="app.can('manage')"
              class="button primary"
              :disabled="busy"
            >
              保存风控策略
            </button>
          </form>
          <article class="panel">
            <h3>IP 请求记录</h3>
            <p class="muted">
              最近 30 天内最多展示 1000 条记录。清除计数会重新计算该 IP 的风险。
            </p>
            <p v-if="!app.can('manage')" class="alert">
              查看 IP 记录需要项目管理权限。
            </p>
            <div v-else class="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>IP 地址</th>
                    <th>请求</th>
                    <th>失败</th>
                    <th>最近请求</th>
                    <th>操作</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="record in records" :key="record.ip">
                    <td class="mono">{{ record.ip }}</td>
                    <td>{{ record.requests }}</td>
                    <td>{{ record.failures }}</td>
                    <td>
                      {{ new Date(record.lastAt * 1000).toLocaleString() }}
                    </td>
                    <td>
                      <button
                        class="link danger-text"
                        :disabled="busy"
                        @click="clearRecord(record.ip)"
                      >
                        清除计数
                      </button>
                    </td>
                  </tr>
                  <tr v-if="!records.length">
                    <td colspan="5" class="muted">还没有 IP 请求记录</td>
                  </tr>
                </tbody>
              </table>
            </div>
          </article>
        </div>
        <div v-if="tab === 'integration'" class="tab-body">
          <article class="panel">
            <h3>真实验证预览</h3>
            <p class="muted">
              这里使用当前站点的实际配置和公开验证协议，会生成真实挑战并计入统计。
            </p>
            <div v-if="!previewHostAllowed" class="alert">
              控制台域名 {{ currentHostname }} 尚未加入白名单。<button
                v-if="app.can('write')"
                class="link"
                :disabled="busy"
                @click="addDomain(currentHostname)"
              >
                允许此域名以预览
              </button>
            </div>
            <div class="form-grid">
              <label
                >业务 action<input
                  v-model="previewAction"
                  maxlength="100"
                  placeholder="login" /></label
              ><label
                >组件主题<select v-model="previewTheme">
                  <option value="light">浅色</option>
                  <option value="dark">深色</option>
                  <option value="auto">跟随系统</option>
                </select></label
              ><label
                >主题色<select v-model="previewColor">
                  <option value="blue">蓝色</option>
                  <option value="violet">紫色</option>
                  <option value="emerald">翠绿</option>
                  <option value="rose">玫瑰红</option>
                  <option value="orange">橙色</option>
                  <option value="cyan">青色</option>
                </select></label
              ><label
                >自定义颜色（可选）<input
                  v-model="previewCustomColor"
                  placeholder="#3B82F6"
                  maxlength="7"
                  pattern="#[0-9A-Fa-f]{3}([0-9A-Fa-f]{3})?" /></label
              ><label
                >组件尺寸<select v-model="previewSize">
                  <option value="normal">标准</option>
                  <option value="compact">紧凑</option>
                  <option value="flexible">自适应</option>
                </select></label
              ><label class="check"
                ><input
                  v-model="previewAutoStart"
                  type="checkbox"
                />自动开始真实验证</label
              >
            </div>
            <div class="preview-area">
              <iframe
                v-if="previewHostAllowed && selected.enabled"
                ref="preview"
                :src="previewURL"
                title="真实 WeAuth 验证预览"
                sandbox="allow-scripts allow-same-origin"
                :width="previewSize === 'compact' ? '290' : '360'"
                :height="previewSize === 'compact' ? '90' : '124'"
                :style="
                  previewSize === 'flexible' ? { width: '100%' } : undefined
                "
              />
              <p v-else>
                {{
                  selected.enabled
                    ? "添加预览域名后即可开始。"
                    : "请先启用站点。"
                }}
              </p>
              <p>{{ previewMessage }}</p>
              <div class="actions">
                <button
                  class="button"
                  @click="
                    previewVersion++;
                    previewToken = '';
                    tokenResult = null;
                  "
                >
                  重置预览</button
                ><button
                  v-if="app.can('secrets')"
                  class="button primary"
                  :disabled="!previewToken || busy"
                  @click="testToken"
                >
                  服务端验证并消耗令牌
                </button>
              </div>
              <p
                v-if="tokenResult"
                :class="['alert', tokenResult.success ? 'success' : 'danger']"
              >
                {{
                  tokenResult.success
                    ? `验证成功 · 来源 ${tokenResult.hostname} · action ${tokenResult.action || "未指定"}。令牌已经消耗。`
                    : `验证失败：${tokenResult["error-codes"]?.join(", ")}`
                }}
              </p>
            </div>
          </article>
          <article class="panel">
            <div class="panel-title">
              <h3>1. 浏览器自动嵌入</h3>
              <button class="link" @click="copy(browserSnippet)">
                复制代码
              </button>
            </div>
            <p>
              把组件放进业务表单，提交时一并发送
              <code>weauth-response</code> 字段。生产页面需要 HTTPS。
            </p>
            <pre><code>{{ browserSnippet }}</code></pre>
          </article>
          <article class="panel">
            <div class="panel-title">
              <h3>2. 手动控制组件</h3>
              <button class="link" @click="copy(explicitSnippet)">
                复制代码
              </button>
            </div>
            <p>
              加载脚本后调用 render，适用于单页应用；离开页面时调用 remove。
            </p>
            <pre><code>{{ explicitSnippet }}</code></pre>
          </article>
          <article class="panel">
            <div class="panel-title">
              <h3>3. 服务端验证</h3>
              <button class="link" @click="copy(serverSnippet)">
                复制代码
              </button>
            </div>
            <p>
              验证成功后同时核对 hostname 和
              action，再执行登录、注册等业务。Secret 保存在服务端环境变量中。
            </p>
            <pre><code>{{ serverSnippet }}</code></pre>
            <p class="hint">
              令牌只能成功验证一次。错误包括
              invalid-input-secret、invalid-input-response 和
              timeout-or-duplicate。可传 remoteip，必须与创建挑战时的可信客户端
              IP 一致。
            </p>
          </article>
        </div>
      </main>
      <div v-else class="empty" role="status">正在加载站点…</div>
    </div>
    <div
      v-if="createOpen"
      class="modal-backdrop"
      @click.self="createOpen = false"
    >
      <form
        class="modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby="create-title"
        @submit.prevent="create"
      >
        <div class="panel-title">
          <h2 id="create-title">创建验证站点</h2>
          <button type="button" class="link" @click="createOpen = false">
            关闭
          </button>
        </div>
        <label
          >站点名称<input
            v-model="creation.name"
            autofocus
            required
            maxlength="100"
            placeholder="例如：用户中心" /></label
        ><label
          >允许的域名<textarea
            v-model="creation.domainsText"
            rows="4"
            placeholder="example.com&#10;*.example.com"
          />
        </label>
        <p class="muted">每行一个域名，不包含协议和路径。稍后可继续添加。</p>
        <div class="form-grid">
          <label
            >验证模式<select v-model="creation.batchModeEnabled">
              <option :value="true">批量模式</option>
              <option :value="false">动态难度</option>
            </select></label
          ><label v-if="creation.batchModeEnabled"
            >固定难度<input
              v-model.number="creation.batchDifficulty"
              type="number"
              min="1"
              max="5"
              required /></label
          ><label v-else
            >基础难度<input
              v-model.number="creation.baseDifficulty"
              type="number"
              min="1"
              max="6"
              required
          /></label>
        </div>
        <p class="hint">
          创建后可调整批次数、有效期及风控策略。服务端密钥只向具备密钥权限的成员单独展示。
        </p>
        <div class="actions">
          <button type="button" class="button" @click="createOpen = false">
            取消</button
          ><button class="button primary" :disabled="busy">创建站点</button>
        </div>
      </form>
    </div>
  </section>
</template>

<style scoped>
.weauth-app {
  --wa-blue: #4f63df;
  --wa-text: #26334d;
  --wa-muted: #6c7b95;
  --wa-border: #e0e6f0;
  color: var(--wa-text);
}
.app-heading,
.site-header,
.panel-title,
.list-heading,
.actions {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
}
.app-heading {
  align-items: flex-start;
  margin-bottom: 26px;
}
.eyebrow {
  font-size: 11px;
  font-weight: 800;
  letter-spacing: 0.14em;
  color: var(--wa-blue);
}
h1 {
  font-size: 26px;
  line-height: 1.3;
  margin: 8px 0 10px;
  letter-spacing: -0.03em;
}
h2 {
  font-size: 20px;
  margin: 0 0 10px;
}
h3 {
  font-size: 16px;
  margin: 0 0 18px;
}
p {
  line-height: 1.7;
  font-size: 13px;
}
.app-heading p {
  color: var(--wa-muted);
  margin: 0;
}
.actions {
  justify-content: flex-start;
  flex-wrap: wrap;
}
.button {
  border: 1px solid var(--wa-border);
  background: #fff;
  color: var(--wa-text);
  padding: 9px 15px;
  border-radius: 9px;
  font-size: 13px;
  font-weight: 600;
  cursor: pointer;
  white-space: nowrap;
}
.button.primary {
  background: var(--wa-blue);
  border-color: var(--wa-blue);
  color: white;
}
.button.destructive {
  color: #b83a4b;
  border-color: #f0c6cc;
}
button:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
.link {
  border: 0;
  background: transparent;
  color: var(--wa-blue);
  font-size: 12px;
  cursor: pointer;
  padding: 5px;
}
.danger-text {
  color: #b83a4b;
}
.workspace {
  display: grid;
  grid-template-columns: 230px minmax(0, 1fr);
  border: 1px solid var(--wa-border);
  border-radius: 16px;
  background: #fff;
  overflow: hidden;
  min-height: 550px;
}
.site-list {
  background: #f7f9fd;
  border-right: 1px solid var(--wa-border);
  padding: 18px 10px;
}
.list-heading {
  padding: 0 10px 17px;
  font-size: 12px;
  font-weight: 700;
  color: var(--wa-muted);
}
.list-heading span {
  background: #e9edf8;
  border-radius: 6px;
  padding: 2px 7px;
}
.site-item {
  width: 100%;
  display: flex;
  align-items: center;
  gap: 10px;
  border: 1px solid transparent;
  background: transparent;
  border-radius: 10px;
  padding: 12px 10px;
  margin-bottom: 5px;
  text-align: left;
  color: var(--wa-text);
  cursor: pointer;
}
.site-item.active {
  background: #fff;
  border-color: #d9e0f3;
  box-shadow: 0 3px 10px #17296108;
}
.site-item strong {
  font-size: 13px;
  display: block;
  max-width: 136px;
  overflow: hidden;
  text-overflow: ellipsis;
}
.site-item small {
  display: block;
  font-size: 10px;
  color: var(--wa-muted);
  margin-top: 5px;
  max-width: 136px;
  overflow: hidden;
  text-overflow: ellipsis;
}
.site-avatar {
  display: grid;
  place-items: center;
  background: #e8edfd;
  color: var(--wa-blue);
  border-radius: 9px;
  width: 32px;
  height: 32px;
  font-weight: 800;
  flex-shrink: 0;
}
.site-item i {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: #b4bfd0;
  margin-left: auto;
  flex-shrink: 0;
}
.site-item i.enabled {
  background: #27a785;
}
.site-main {
  min-width: 0;
}
.site-header {
  padding: 25px 26px 12px;
  flex-wrap: wrap;
}
.badge {
  display: inline-block;
  font-size: 10px;
  vertical-align: middle;
  padding: 4px 8px;
  border-radius: 20px;
  background: #edf0f6;
  color: #6a7891;
  font-weight: 600;
  margin-left: 8px;
}
.badge.good {
  background: #e5f6ef;
  color: #228362;
}
.sitekey {
  font-size: 11px;
  overflow-wrap: anywhere;
  margin: 0;
}
.mono {
  font-family: ui-monospace, SFMono-Regular, Consolas, monospace;
}
.muted {
  color: var(--wa-muted);
  font-size: 12px;
}
.tabs {
  display: flex;
  gap: 24px;
  padding: 0 26px;
  border-bottom: 1px solid var(--wa-border);
  overflow-x: auto;
}
.tabs button {
  white-space: nowrap;
  border: 0;
  border-bottom: 2px solid transparent;
  background: none;
  padding: 17px 0 13px;
  font-size: 13px;
  color: var(--wa-muted);
  cursor: pointer;
}
.tabs button.active {
  color: var(--wa-blue);
  border-bottom-color: var(--wa-blue);
  font-weight: 700;
}
.tab-body {
  padding: 24px;
  background: #fbfcff;
}
.metrics {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 12px;
  margin-bottom: 20px;
}
.metrics article {
  border: 1px solid var(--wa-border);
  border-radius: 12px;
  background: #fff;
  padding: 16px;
}
.metrics small {
  font-size: 11px;
  color: var(--wa-muted);
}
.metrics strong {
  display: block;
  font-size: 26px;
  margin: 10px 0;
  letter-spacing: -0.03em;
}
.metrics span {
  font-size: 10px;
  color: var(--wa-muted);
}
.panel {
  border: 1px solid var(--wa-border);
  border-radius: 12px;
  background: #fff;
  padding: 22px;
  margin-bottom: 18px;
}
.panel:last-child {
  margin-bottom: 0;
}
.two-columns {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 18px;
}
.two-columns .panel {
  margin: 0;
}
.setup-card {
  background: linear-gradient(135deg, #f0f3ff, #f7fcfc);
}
.setup-card ol {
  padding-left: 20px;
  font-size: 13px;
  line-height: 2.3;
  margin-bottom: 20px;
}
dl {
  margin: 0 0 20px;
}
dl div {
  display: flex;
  justify-content: space-between;
  gap: 16px;
  padding: 10px 0;
  border-bottom: 1px solid #edf0f7;
  font-size: 12px;
}
dt {
  color: var(--wa-muted);
}
dd {
  margin: 0;
  text-align: right;
}
label {
  display: flex;
  flex-direction: column;
  gap: 8px;
  font-size: 12px;
  font-weight: 600;
  margin-bottom: 18px;
}
input,
select,
textarea {
  box-sizing: border-box;
  font: inherit;
  font-weight: 400;
  border: 1px solid #ccd6e7;
  border-radius: 8px;
  background: #fff;
  padding: 10px 12px;
  color: var(--wa-text);
  width: 100%;
  min-width: 0;
}
input:focus,
select:focus,
textarea:focus {
  outline: 2px solid #bdc9ff;
  outline-offset: 1px;
}
input:disabled,
select:disabled {
  background: #f6f7fa;
  color: #7e8ba2;
}
.check {
  flex-direction: row;
  align-items: center;
  gap: 8px;
  align-self: center;
}
.check input {
  width: 16px;
  height: 16px;
}
.form-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 18px;
}
.form-grid.three {
  grid-template-columns: repeat(3, 1fr);
}
.mode-options {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 16px;
  margin-bottom: 16px;
}
.mode-options label {
  border: 1px solid var(--wa-border);
  border-radius: 10px;
  padding: 17px;
  cursor: pointer;
  position: relative;
  margin: 0;
}
.mode-options label.chosen {
  border-color: var(--wa-blue);
  background: #f6f7ff;
}
.mode-options input {
  position: absolute;
  width: 15px;
  right: 12px;
  top: 12px;
}
.mode-options span {
  font-size: 11px;
  line-height: 1.7;
  color: var(--wa-muted);
  font-weight: 400;
}
.hint {
  font-size: 12px;
  color: #667694;
  background: #f4f7fc;
  border-radius: 8px;
  padding: 12px 14px;
  line-height: 1.8;
}
.inline-form {
  display: flex;
  gap: 10px;
  margin-top: 18px;
}
.inline-form input {
  flex: 1;
}
.domain-list {
  padding: 0;
  list-style: none;
  margin: 20px 0 0;
}
.domain-list li {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  border-bottom: 1px solid var(--wa-border);
  padding: 13px 0;
  font-size: 13px;
}
.copy-field,
.secret-value {
  display: flex;
  align-items: center;
  gap: 12px;
  justify-content: space-between;
  border: 1px solid var(--wa-border);
  padding: 10px;
  border-radius: 9px;
  overflow-wrap: anywhere;
}
.copy-field code,
.secret-value code {
  min-width: 0;
  word-break: break-all;
  font-size: 12px;
}
.secret-value {
  margin-bottom: 18px;
  background: #fff9ef;
}
.secret-value .actions {
  flex-shrink: 0;
}
.table-wrap {
  overflow-x: auto;
}
table {
  border-collapse: collapse;
  width: 100%;
  font-size: 12px;
  text-align: left;
}
th,
td {
  padding: 12px;
  border-bottom: 1px solid var(--wa-border);
  white-space: nowrap;
}
th {
  background: #f7f9fd;
  color: var(--wa-muted);
  font-weight: 600;
}
.alert {
  padding: 13px 16px;
  background: #fff7e7;
  border: 1px solid #f1dfb9;
  border-radius: 9px;
  font-size: 12px;
  line-height: 1.7;
}
.alert.danger {
  background: #fff0f1;
  border-color: #f0c7cc;
  color: #a63648;
}
.alert.success {
  background: #eaf8f0;
  border-color: #c4e7d3;
  color: #26774f;
}
.preview-area {
  background: #f4f7fc;
  padding: 20px;
  border-radius: 10px;
  margin-top: 10px;
}
.preview-area iframe {
  border: 0;
  max-width: 100%;
  display: block;
}
.preview-area p {
  font-size: 12px;
  color: var(--wa-muted);
}
pre {
  background: #172239;
  color: #dbe6ff;
  padding: 18px;
  border-radius: 10px;
  overflow: auto;
  font-size: 11px;
  line-height: 1.8;
  margin: 14px 0 0;
}
.panel-title h3 {
  margin: 0;
}
.panel-title {
  margin-bottom: 14px;
}
.empty {
  padding: 70px 26px;
  text-align: center;
  background: #fff;
  border-radius: 15px;
  color: var(--wa-muted);
}
.empty p {
  max-width: 430px;
  margin: 12px auto 24px;
}
.empty h2 {
  color: var(--wa-text);
}
.empty-icon {
  width: 58px;
  height: 58px;
  border-radius: 16px;
  background: #e9f0ff;
  color: var(--wa-blue);
  display: grid;
  place-items: center;
  font-size: 30px;
  margin: 0 auto 22px;
}
.modal-backdrop {
  position: fixed;
  inset: 0;
  background: #17253e80;
  z-index: 100;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px;
}
.modal {
  width: 520px;
  max-width: 100%;
  max-height: 90vh;
  overflow: auto;
  background: white;
  padding: 28px;
  border-radius: 16px;
  box-shadow: 0 20px 70px #15254240;
}
.modal h2 {
  margin: 0;
}
.modal > .actions {
  justify-content: flex-end;
}
@media (max-width: 1100px) {
  .workspace {
    grid-template-columns: 190px minmax(0, 1fr);
  }
  .metrics {
    grid-template-columns: 1fr 1fr;
  }
  .two-columns {
    grid-template-columns: 1fr;
  }
  .form-grid.three {
    grid-template-columns: 1fr 1fr;
  }
  .site-item strong,
  .site-item small {
    max-width: 100px;
  }
  .site-header,
  .tab-body {
    padding: 18px;
  }
  .tabs {
    padding: 0 18px;
    gap: 18px;
  }
}
@media (max-width: 760px) {
  .app-heading {
    display: block;
  }
  .app-heading > .actions {
    margin-top: 18px;
  }
  .workspace {
    grid-template-columns: 1fr;
  }
  .site-list {
    display: flex;
    overflow: auto;
    gap: 8px;
    align-items: center;
    border-right: 0;
    border-bottom: 1px solid var(--wa-border);
    padding: 10px;
  }
  .list-heading {
    display: none;
  }
  .site-item {
    width: auto;
    min-width: 170px;
    margin: 0;
  }
  .form-grid,
  .form-grid.three,
  .mode-options {
    grid-template-columns: 1fr;
  }
  .panel {
    padding: 16px;
  }
  .site-header > .muted {
    display: none;
  }
  .tab-body {
    padding: 14px;
  }
  .metrics {
    gap: 8px;
  }
  .metrics article {
    padding: 13px;
  }
  .secret-value {
    align-items: flex-start;
    flex-direction: column;
  }
  .modal {
    padding: 20px;
  }
}
</style>
