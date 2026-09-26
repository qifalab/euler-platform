<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { useApp } from "../shared";
import { split, when } from "./model";
interface AI {
  protocol: string;
  baseUrl: string;
  model: string;
  keyConfigured?: boolean;
  apiKeyHint?: string;
  customHeaders?: Record<string, string>;
  verifiedAt?: string;
}
interface Notification {
  webhookEnabled: boolean;
  webhookUrl: string;
  webhookSecretConfigured?: boolean;
  smtpEnabled: boolean;
  smtpHost: string;
  smtpPort: number;
  smtpUsername: string;
  smtpPasswordConfigured?: boolean;
  smtpFrom: string;
  smtpTo: string[];
}
const app = useApp();
const busy = ref(false),
  loading = ref(false),
  error = ref(""),
  storedAI = ref<AI>(),
  storedNotifications = ref<Notification>(),
  aiResult = ref<{ ok: boolean; latencyMs: number; model: string }>();
const ai = reactive({
    protocol: "openai_responses",
    baseUrl: "https://api.openai.com/v1",
    model: "",
    apiKey: "",
    clearApiKey: false,
  }),
  headerMode = ref("preserve"),
  headers = ref<{ name: string; value: string }[]>([]);
const notification = reactive<
  Notification & {
    webhookSecret: string;
    smtpPassword: string;
    clearWebhookSecret: boolean;
    clearSmtpPassword: boolean;
    recipients: string;
  }
>({
  webhookEnabled: false,
  webhookUrl: "",
  smtpEnabled: false,
  smtpHost: "",
  smtpPort: 587,
  smtpUsername: "",
  smtpFrom: "",
  smtpTo: [],
  webhookSecret: "",
  smtpPassword: "",
  clearWebhookSecret: false,
  clearSmtpPassword: false,
  recipients: "",
});
const storedHeaderNames = computed(() =>
  Object.keys(storedAI.value?.customHeaders ?? {}),
);
function origin(value: string) {
  try {
    return new URL(value).origin;
  } catch {
    return value;
  }
}
const aiOriginChanged = computed(
  () =>
    !!storedAI.value && origin(storedAI.value.baseUrl) !== origin(ai.baseUrl),
);
const webhookChanged = computed(
  () =>
    !!storedNotifications.value &&
    (storedNotifications.value.webhookUrl ?? "") !==
      notification.webhookUrl.trim(),
);
const smtpChanged = computed(
  () =>
    !!storedNotifications.value &&
    ((storedNotifications.value.smtpHost ?? "").toLowerCase() !==
      notification.smtpHost.trim().toLowerCase() ||
      storedNotifications.value.smtpPort !== notification.smtpPort ||
      storedNotifications.value.smtpUsername !== notification.smtpUsername),
);
async function run(work: () => Promise<void>, message?: string) {
  if (busy.value) return;
  busy.value = true;
  error.value = "";
  try {
    await work();
    if (message) app.notify(message, "success");
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    busy.value = false;
  }
}
function applyAI(value: AI) {
  storedAI.value = value;
  Object.assign(ai, {
    protocol: value.protocol || "openai_responses",
    baseUrl: value.baseUrl || "",
    model: value.model || "",
    apiKey: "",
    clearApiKey: false,
  });
  headerMode.value = "preserve";
  headers.value = [];
}
function applyNotifications(value: Notification) {
  storedNotifications.value = value;
  Object.assign(notification, {
    webhookEnabled: !!value.webhookEnabled,
    webhookUrl: value.webhookUrl || "",
    smtpEnabled: !!value.smtpEnabled,
    smtpHost: value.smtpHost || "",
    smtpPort: value.smtpPort || 587,
    smtpUsername: value.smtpUsername || "",
    smtpFrom: value.smtpFrom || "",
    smtpTo: value.smtpTo ?? [],
    recipients: (value.smtpTo ?? []).join("\n"),
    webhookSecret: "",
    smtpPassword: "",
    clearWebhookSecret: false,
    clearSmtpPassword: false,
  });
}
async function load() {
  loading.value = true;
  try {
    const [first, second] = await Promise.all([
      app.request<AI>("/ai/settings"),
      app.request<Notification>("/notifications/settings"),
    ]);
    applyAI(first);
    applyNotifications(second);
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}
function aiBody() {
  if (ai.apiKey && ai.clearApiKey)
    throw new Error("新密钥和清除密钥不能同时选择");
  if (
    aiOriginChanged.value &&
    storedAI.value?.keyConfigured &&
    !ai.apiKey &&
    !ai.clearApiKey
  )
    throw new Error("更换 AI 服务来源时必须重新输入或清除旧密钥");
  if (
    aiOriginChanged.value &&
    storedHeaderNames.value.length &&
    headerMode.value === "preserve"
  )
    throw new Error("更换 AI 服务来源时必须替换或清除自定义请求头");
  let customHeaders: Record<string, string> | undefined;
  if (headerMode.value !== "preserve") {
    customHeaders = {};
    if (headerMode.value === "replace") {
      for (const item of headers.value) {
        const name = item.name.trim();
        if (
          !name ||
          !item.value ||
          Object.keys(customHeaders).some(
            (key) => key.toLowerCase() === name.toLowerCase(),
          )
        )
          throw new Error("请求头名称和值不能为空，名称不能重复");
        customHeaders[name] = item.value;
      }
    }
  }
  return {
    protocol: ai.protocol,
    baseUrl: ai.baseUrl,
    model: ai.model,
    ...(ai.apiKey ? { apiKey: ai.apiKey } : {}),
    ...(ai.clearApiKey ? { clearApiKey: true } : {}),
    ...(customHeaders !== undefined ? { customHeaders } : {}),
  };
}
async function saveAI() {
  await run(async () => {
    applyAI(
      await app.request<AI>("/ai/settings", { method: "PUT", body: aiBody() }),
    );
    aiResult.value = undefined;
  }, "AI 服务配置已保存");
}
async function testAI() {
  await run(async () => {
    aiResult.value = await app.request("/ai/test", {
      method: "POST",
      body: {},
      timeoutMs: 90000,
    });
    applyAI(await app.request<AI>("/ai/settings"));
  }, "已测试当前保存的 AI 配置");
}
async function saveNotification() {
  await run(async () => {
    if (
      (notification.webhookSecret && notification.clearWebhookSecret) ||
      (notification.smtpPassword && notification.clearSmtpPassword)
    )
      throw new Error("新凭据和清除凭据不能同时选择");
    if (
      webhookChanged.value &&
      storedNotifications.value?.webhookSecretConfigured &&
      !notification.webhookSecret &&
      !notification.clearWebhookSecret
    )
      throw new Error("更换 Webhook 接收地址时必须重新输入或清除签名密钥");
    if (
      smtpChanged.value &&
      storedNotifications.value?.smtpPasswordConfigured &&
      !notification.smtpPassword &&
      !notification.clearSmtpPassword
    )
      throw new Error("更换 SMTP 主机、端口或账号时必须重新输入或清除密码");
    const {
      webhookEnabled,
      webhookUrl,
      smtpEnabled,
      smtpHost,
      smtpPort,
      smtpUsername,
      smtpFrom,
    } = notification;
    const body = {
      webhookEnabled,
      webhookUrl,
      smtpEnabled,
      smtpHost,
      smtpPort,
      smtpUsername,
      smtpFrom,
      smtpTo: split(notification.recipients),
      ...(notification.webhookSecret
        ? { webhookSecret: notification.webhookSecret }
        : {}),
      ...(notification.smtpPassword
        ? { smtpPassword: notification.smtpPassword }
        : {}),
      ...(notification.clearWebhookSecret ? { clearWebhookSecret: true } : {}),
      ...(notification.clearSmtpPassword ? { clearSmtpPassword: true } : {}),
    };
    applyNotifications(
      await app.request<Notification>("/notifications/settings", {
        method: "PUT",
        body,
      }),
    );
  }, "通知渠道已保存");
}
async function testNotification() {
  if (!confirm("向已保存并启用的通知渠道发送一条测试消息？")) return;
  await run(async () => {
    await app.request("/notifications/test", {
      method: "POST",
      body: {},
      timeoutMs: 60000,
    });
  }, "测试消息已发送");
}
onMounted(load);
</script>
<template>
  <div>
    <p v-if="error" class="ws-error" role="alert">{{ error }}</p>
    <p v-if="loading" role="status">正在读取项目配置…</p>
    <section class="panel">
      <div class="toolbar">
        <h3>AI 服务</h3>
        <button :disabled="busy || loading" @click="load">重新读取</button>
      </div>
      <p class="muted">
        支持 OpenAI Responses、Chat Completions 与 Anthropic
        Messages。密钥和自定义请求头加密保存，读取只返回是否配置及提示信息。
      </p>
      <form @submit.prevent="saveAI">
        <fieldset :disabled="!app.can('manage') || busy">
          <div class="ws-grid">
            <label
              >协议<select v-model="ai.protocol">
                <option value="openai_responses">OpenAI Responses</option>
                <option value="openai_chat">OpenAI Chat Completions</option>
                <option value="anthropic_messages">Anthropic Messages</option>
              </select></label
            ><label
              >模型<input
                v-model="ai.model"
                required
                maxlength="200"
                placeholder="部署者配置的模型名称"
            /></label>
          </div>
          <label
            >服务 Endpoint<input
              v-model="ai.baseUrl"
              type="url"
              required
              placeholder="https://api.example.com/v1"
          /></label>
          <p v-if="aiOriginChanged" class="status-note">
            服务来源已改变，请为新服务重新输入凭据或明确清除。旧凭据不会自动转发到新地址。
          </p>
          <div class="ws-grid">
            <label
              >新的 API Key<input
                v-model="ai.apiKey"
                type="password"
                autocomplete="new-password"
                :disabled="ai.clearApiKey"
                :placeholder="
                  storedAI?.keyConfigured
                    ? '留空保留已保存的密钥'
                    : '尚未配置密钥'
                " /></label
            ><label class="ws-check"
              ><input v-model="ai.clearApiKey" type="checkbox" />清除已保存 API
              Key</label
            >
          </div>
          <p class="muted">
            {{
              storedAI?.keyConfigured
                ? `已有密钥 ${storedAI.apiKeyHint || ""}`
                : "没有保存的 API Key"
            }}
            · 最近连接验证 {{ when(storedAI?.verifiedAt) }}
          </p>
          <h4>自定义请求头</h4>
          <p class="muted">
            已保存名称：{{
              storedHeaderNames.join("、") || "无"
            }}。保存时不会把掩码当成真实值回传。
          </p>
          <label
            >更新方式<select v-model="headerMode">
              <option value="preserve">保持不变</option>
              <option value="replace">替换为新请求头</option>
              <option value="clear">清除所有请求头</option>
            </select></label
          ><template v-if="headerMode === 'replace'"
            ><div v-for="(item, index) in headers" :key="index" class="ws-grid">
              <label
                >名称<input
                  v-model="item.name"
                  required
                  placeholder="X-API-Version" /></label
              ><label
                >值<input
                  v-model="item.value"
                  type="password"
                  autocomplete="new-password"
                  required /></label
              ><button type="button" @click="headers.splice(index, 1)">
                移除
              </button>
            </div>
            <button
              type="button"
              :disabled="headers.length >= 20"
              @click="headers.push({ name: '', value: '' })"
            >
              添加请求头
            </button></template
          >
          <div v-if="app.can('manage')" class="actions settings-actions">
            <button class="primary" :disabled="busy">保存 AI 配置</button
            ><button
              type="button"
              :disabled="busy || !storedAI?.model"
              @click="testAI"
            >
              测试已保存配置
            </button>
          </div>
        </fieldset>
      </form>
      <p v-if="aiResult" class="status-note">
        {{ aiResult.ok ? "连接测试成功" : "连接测试失败" }} · 模型
        {{ aiResult.model }} · 延迟 {{ aiResult.latencyMs }} ms
      </p>
    </section>
    <section class="panel">
      <h3>通知渠道</h3>
      <p class="muted">
        通过 HMAC 签名 Webhook 或 SMTP
        发送安全通知。更换接收端需要重新确认对应凭据。
      </p>
      <form @submit.prevent="saveNotification">
        <fieldset :disabled="!app.can('manage') || busy">
          <h4>Webhook</h4>
          <label class="ws-check"
            ><input v-model="notification.webhookEnabled" type="checkbox" />启用
            Webhook</label
          ><label
            >接收 URL<input
              v-model="notification.webhookUrl"
              type="url"
              :required="notification.webhookEnabled"
              placeholder="https://notifications.example.com/security"
          /></label>
          <p
            v-if="
              webhookChanged && storedNotifications?.webhookSecretConfigured
            "
            class="status-note"
          >
            Webhook 地址已改变。请输入新签名密钥；或停用渠道并明确清除旧密钥。
          </p>
          <div class="ws-grid">
            <label
              >新的签名密钥<input
                v-model="notification.webhookSecret"
                type="password"
                autocomplete="new-password"
                minlength="16"
                :disabled="notification.clearWebhookSecret"
                :placeholder="
                  storedNotifications?.webhookSecretConfigured
                    ? '留空保留已保存的签名密钥'
                    : '至少 16 个字符'
                " /></label
            ><label class="ws-check"
              ><input
                v-model="notification.clearWebhookSecret"
                type="checkbox"
              />清除 Webhook 密钥</label
            >
          </div>
          <p class="muted">
            签名密钥：{{
              storedNotifications?.webhookSecretConfigured
                ? "已配置"
                : "未配置"
            }}。启用的 Webhook 必须有签名密钥。
          </p>
          <h4>SMTP 邮件</h4>
          <label class="ws-check"
            ><input v-model="notification.smtpEnabled" type="checkbox" />启用
            SMTP 通知</label
          >
          <div class="ws-grid">
            <label
              >SMTP 主机<input
                v-model="notification.smtpHost"
                :required="notification.smtpEnabled"
                placeholder="smtp.example.com" /></label
            ><label
              >端口<input
                v-model.number="notification.smtpPort"
                type="number"
                min="1"
                max="65535"
                required /></label
            ><label
              >账号<input
                v-model="notification.smtpUsername"
                autocomplete="off" /></label
            ><label
              >新的 SMTP 密码<input
                v-model="notification.smtpPassword"
                type="password"
                autocomplete="new-password"
                :disabled="notification.clearSmtpPassword"
                :placeholder="
                  storedNotifications?.smtpPasswordConfigured
                    ? '留空保留已保存密码'
                    : '未保存密码'
                " /></label
            ><label class="ws-check"
              ><input
                v-model="notification.clearSmtpPassword"
                type="checkbox"
              />清除 SMTP 密码</label
            >
          </div>
          <p
            v-if="smtpChanged && storedNotifications?.smtpPasswordConfigured"
            class="status-note"
          >
            SMTP 主机、端口或账号已改变，需要重新输入或清除密码。
          </p>
          <label
            >发件人<input
              v-model="notification.smtpFrom"
              type="email"
              :required="notification.smtpEnabled"
              placeholder="security@example.com" /></label
          ><label
            >收件人<textarea
              v-model="notification.recipients"
              rows="3"
              :required="notification.smtpEnabled"
              placeholder="每行一个邮箱地址"
            ></textarea>
          </label>
          <div v-if="app.can('manage')" class="actions settings-actions">
            <button class="primary" :disabled="busy">保存通知渠道</button
            ><button
              type="button"
              :disabled="
                busy ||
                !(
                  storedNotifications?.webhookEnabled ||
                  storedNotifications?.smtpEnabled
                )
              "
              @click="testNotification"
            >
              发送测试通知
            </button>
          </div>
        </fieldset>
      </form>
    </section>
    <section class="panel">
      <h3>人员与设备身份</h3>
      <p>
        人员登录、项目成员与权限由 Euler 和新版
        E时代通行证统一管理。这里不创建另一套管理员密码。设备继续使用独立密钥、签名和一次性注册码，授权变更与执行结果保留审计。
      </p>
    </section>
  </div>
</template>
<style scoped>
.settings-actions {
  margin-top: 20px;
}
h4 {
  margin: 22px 0 12px;
}
</style>
