<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { useApp } from "../shared";
import Evidence from "./Evidence.vue";
import {
  key,
  label,
  when,
  type Device,
  type SecurityIncident,
  type IncidentDetail,
  type Prepared,
  type AIInvestigationPolicy,
  type AIInvestigationUsage,
} from "./model";
defineProps<{ devices: Device[] }>();
const emit = defineEmits<{ prepared: [Prepared]; changed: [] }>();
const app = useApp();
const incidents = ref<SecurityIncident[]>([]),
  detail = ref<IncidentDetail>(),
  busy = ref(false),
  loading = ref(false),
  error = ref(""),
  deviceFilter = ref(""),
  statusFilter = ref(""),
  statusNote = ref("");
const policy = reactive<AIInvestigationPolicy>({
  profile: "balanced",
  dailyTokenBudget: 100000,
  emergencyReserveTokens: 10000,
  shareNetworkIndicators: false,
  shareAccountNames: false,
  updatedAt: "",
});
const usage = ref<AIInvestigationUsage>();
const chat = reactive({ deviceId: "", message: "" }),
  messages = ref<{ role: "user" | "assistant"; text: string }[]>([]);
const filtered = computed(() =>
  incidents.value.filter(
    (i) =>
      (!deviceFilter.value || i.deviceId === deviceFilter.value) &&
      (!statusFilter.value || i.status === statusFilter.value),
  ),
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
async function load() {
  loading.value = true;
  try {
    const [list, p] = await Promise.all([
      app.request<{ items: SecurityIncident[] }>("/incidents?limit=200"),
      app.request<{
        policy: AIInvestigationPolicy;
        usage: AIInvestigationUsage;
      }>("/ai/investigation-policy"),
    ]);
    incidents.value = list.items ?? [];
    Object.assign(policy, p.policy);
    usage.value = p.usage;
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}
async function open(id: string) {
  await run(async () => {
    detail.value = await app.request<IncidentDetail>(`/incidents/${key(id)}`);
    statusNote.value = "";
  });
}
async function investigate() {
  if (!detail.value) return;
  const id = detail.value.incident.id;
  await run(async () => {
    await app.request(`/incidents/${key(id)}/investigate`, {
      method: "POST",
      body: {},
      timeoutMs: 180000,
    });
    detail.value = await app.request<IncidentDetail>(`/incidents/${key(id)}`);
    await load();
    emit("changed");
  }, "调查已完成，请核对证据、结论与不确定性");
}
async function setStatus(status: "open" | "resolved" | "dismissed") {
  if (!detail.value) return;
  const id = detail.value.incident.id;
  if (
    status !== "open" &&
    !confirm(
      `将当前事件标记为${status === "resolved" ? "已解决" : "已忽略"}？不会改变设备或执行修复。`,
    )
  )
    return;
  await run(async () => {
    await app.request(`/incidents/${key(id)}`, {
      method: "PATCH",
      body: { status, summary: statusNote.value },
    });
    detail.value = await app.request<IncidentDetail>(`/incidents/${key(id)}`);
    await load();
    emit("changed");
  }, "事件状态已更新");
}
async function prepare(planId: string, stepId: string) {
  await run(async () => {
    emit(
      "prepared",
      await app.request<Prepared>(
        `/response-plans/${key(planId)}/steps/${key(stepId)}/prepare`,
        { method: "POST", body: {} },
      ),
    );
  });
}
async function savePolicy() {
  await run(async () => {
    const {
      profile,
      dailyTokenBudget,
      emergencyReserveTokens,
      shareNetworkIndicators,
      shareAccountNames,
    } = policy;
    await app.request("/ai/investigation-policy", {
      method: "PUT",
      body: {
        profile,
        dailyTokenBudget,
        emergencyReserveTokens,
        shareNetworkIndicators,
        shareAccountNames,
      },
    });
    await load();
  }, "调查策略已保存");
}
async function send() {
  const message = chat.message.trim();
  if (!message) return;
  await run(async () => {
    messages.value.push({ role: "user", text: message });
    chat.message = "";
    const result = await app.request<{ message: string; canExecute: false }>(
      "/ai/chat",
      {
        method: "POST",
        body: { message, deviceId: chat.deviceId || undefined, findingIds: [] },
        timeoutMs: 180000,
      },
    );
    messages.value.push({ role: "assistant", text: result.message });
  });
}
onMounted(load);
</script>
<template>
  <div>
    <p v-if="error" class="ws-error" role="alert">{{ error }}</p>
    <section class="panel">
      <div class="toolbar">
        <div>
          <h3>安全事件</h3>
          <p class="muted">
            调查区分可信证据与推断。响应方案必须经过参数检查和独立审批。
          </p>
        </div>
        <button :disabled="loading || busy" @click="load">刷新</button>
      </div>
      <div class="ws-grid">
        <label
          >设备<select v-model="deviceFilter">
            <option value="">全部设备</option>
            <option v-for="d in devices" :key="d.id" :value="d.id">
              {{ d.name }}
            </option>
          </select></label
        ><label
          >事件状态<select v-model="statusFilter">
            <option value="">全部状态</option>
            <option
              v-for="state in [
                'open',
                'investigating',
                'awaiting_approval',
                'responding',
                'monitoring',
                'resolved',
                'dismissed',
              ]"
              :key="state"
              :value="state"
            >
              {{ label(state) }}
            </option>
          </select></label
        >
      </div>
      <div class="table-wrap">
        <table>
          <thead>
            <tr>
              <th>级别</th>
              <th>事件</th>
              <th>状态</th>
              <th>信号数</th>
              <th>最近活动</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="incident in filtered" :key="incident.id">
              <td>
                <span :class="['ws-badge', incident.severity]">{{
                  label(incident.severity)
                }}</span>
              </td>
              <td>
                <button
                  class="ws-link"
                  :disabled="busy"
                  @click="open(incident.id)"
                >
                  {{ incident.title }}
                </button>
                <p class="muted">
                  {{
                    devices.find((d) => d.id === incident.deviceId)?.name ??
                    incident.deviceId
                  }}
                </p>
              </td>
              <td>{{ label(incident.status) }}</td>
              <td>{{ incident.signalCount }}</td>
              <td>{{ when(incident.lastSeenAt) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <p v-if="!filtered.length" class="empty">
        暂无匹配事件。扫描与传感器上报后会按证据关联生成事件。
      </p>
    </section>
    <section class="panel">
      <h3>AI 安全问答</h3>
      <p class="muted">
        回答基于最少必要证据；问答不会执行操作。消息仅保留在当前页面会话。
      </p>
      <form @submit.prevent="send">
        <fieldset :disabled="!app.can('write') || busy">
          <label
            >关联设备（可选）<select v-model="chat.deviceId">
              <option value="">项目安全概况</option>
              <option v-for="d in devices" :key="d.id" :value="d.id">
                {{ d.name }}
              </option>
            </select></label
          >
          <div
            v-for="(message, index) in messages"
            :key="index"
            :class="['ws-chat', message.role]"
          >
            <strong>{{
              message.role === "user" ? "你的问题" : "AI 安全工程师"
            }}</strong>
            <p class="ws-preserve">{{ message.text }}</p>
          </div>
          <label
            >问题<textarea
              v-model="chat.message"
              rows="3"
              maxlength="8000"
              required
              placeholder="例如：这台设备有哪些需要优先核查的风险？"
            ></textarea></label
          ><button
            v-if="app.can('write')"
            class="primary"
            :disabled="busy || !chat.message.trim()"
          >
            {{ busy ? "正在处理…" : "发送问题" }}
          </button>
        </fieldset>
      </form>
    </section>
    <section class="panel">
      <h3>调查策略与预算</h3>
      <div class="ws-grid">
        <div>
          <small>今天常规用量</small>
          <p>{{ usage?.regularTokensUsed ?? 0 }} tokens</p>
        </div>
        <div>
          <small>今天紧急用量</small>
          <p>{{ usage?.emergencyTokensUsed ?? 0 }} tokens</p>
        </div>
        <div>
          <small>调查调用</small>
          <p>{{ usage?.investigationCalls ?? 0 }} 次</p>
        </div>
      </div>
      <form @submit.prevent="savePolicy">
        <fieldset :disabled="!app.can('manage') || busy">
          <div class="ws-grid">
            <label
              >调查档位<select v-model="policy.profile">
                <option value="economy">节省 · 关注较高风险</option>
                <option value="balanced">均衡</option>
                <option value="sensitive">敏感 · 更积极调查</option>
              </select></label
            ><label
              >每日常规 Token 预算<input
                v-model.number="policy.dailyTokenBudget"
                type="number"
                min="0"
                required /></label
            ><label
              >紧急保留 Token<input
                v-model.number="policy.emergencyReserveTokens"
                type="number"
                min="0"
                required
            /></label>
          </div>
          <label class="ws-check"
            ><input
              v-model="policy.shareNetworkIndicators"
              type="checkbox"
            />允许向配置的 AI 服务分享网络标识</label
          ><label class="ws-check"
            ><input
              v-model="policy.shareAccountNames"
              type="checkbox"
            />允许向配置的 AI 服务分享账号名称</label
          ><button v-if="app.can('manage')" :disabled="busy">
            保存调查策略
          </button>
        </fieldset>
      </form>
    </section>
    <div
      v-if="detail"
      class="ws-overlay"
      @click.self="!busy && (detail = undefined)"
    >
      <section
        class="panel ws-dialog"
        role="dialog"
        aria-modal="true"
        aria-label="事件调查详情"
      >
        <div class="toolbar">
          <h2>{{ detail.incident.title }}</h2>
          <button :disabled="busy" @click="detail = undefined">关闭</button>
        </div>
        <p>
          {{ label(detail.incident.severity) }} ·
          {{ label(detail.incident.status) }} ·
          {{ when(detail.incident.lastSeenAt) }}
        </p>
        <p>{{ detail.incident.summary }}</p>
        <div class="actions">
          <button
            v-if="app.can('write')"
            class="primary"
            :disabled="busy"
            @click="investigate"
          >
            {{ busy ? "正在处理…" : "发起 AI 调查" }}</button
          ><button :disabled="busy" @click="open(detail.incident.id)">
            刷新详情
          </button>
        </div>
        <h3>事件信号</h3>
        <article
          v-for="signal in detail.signals"
          :key="signal.id"
          class="ws-row"
        >
          <div>
            <strong>{{ signal.summary }}</strong>
            <p>
              <span class="ws-badge">{{
                signal.trust === "verified" ? "可信证据" : "未验证观察"
              }}</span>
              {{ signal.source }} · {{ when(signal.occurredAt) }}
            </p>
            <p class="muted">{{ signal.subject || signal.type }}</p>
          </div>
        </article>
        <p v-if="!detail.signals.length" class="muted">尚无关联信号</p>
        <article
          v-for="investigation in detail.investigations"
          :key="investigation.id"
          class="ws-investigation"
        >
          <h3>调查 · {{ label(investigation.status) }}</h3>
          <p class="muted">
            {{ investigation.model || "尚未完成模型调用" }} ·
            {{ when(investigation.completedAt || investigation.startedAt) }} ·
            置信度 {{ investigation.confidence }}
          </p>
          <p v-if="investigation.error" class="ws-error">
            {{ investigation.error }}
          </p>
          <h4>调查假设</h4>
          <p>{{ investigation.hypothesis || "暂无" }}</p>
          <h4>观察到的事实</h4>
          <ul>
            <li
              v-for="(value, index) in investigation.observations ?? []"
              :key="index"
            >
              {{ value }}
            </li>
          </ul>
          <h4>仍不确定</h4>
          <ul>
            <li
              v-for="(value, index) in investigation.uncertainties ?? []"
              :key="index"
            >
              {{ value }}
            </li>
          </ul>
          <h4>结论</h4>
          <p class="ws-preserve">
            {{ investigation.conclusion || "尚无结论" }}
          </p>
          <h4>下一步核查</h4>
          <ul>
            <li
              v-for="(value, index) in investigation.nextChecks ?? []"
              :key="index"
            >
              {{ value }}
            </li>
          </ul>
          <details>
            <summary>查看工具调用轨迹</summary>
            <div
              v-for="(tool, index) in investigation.toolCalls ?? []"
              :key="index"
              class="ws-row"
            >
              <div>
                <strong>{{ tool.tool }}</strong>
                <p>{{ tool.summary }}</p>
                <small
                  >{{ when(tool.startedAt) }} → {{ when(tool.endedAt) }}</small
                >
              </div>
            </div>
          </details>
        </article>
        <article
          v-for="plan in detail.responsePlans"
          :key="plan.id"
          class="ws-investigation"
        >
          <h3>{{ plan.title }}</h3>
          <p>{{ plan.rationale }}</p>
          <p>风险 {{ label(plan.risk) }} · 状态 {{ label(plan.status) }}</p>
          <div
            v-for="step in plan.steps"
            :key="step.id"
            class="ws-response-step"
          >
            <h4>{{ step.title }}</h4>
            <p>{{ step.rationale }}</p>
            <Evidence :value="step.parameters" />
            <p v-if="step.actionId" class="status-note">
              已经关联动作
              {{ step.actionId }}。请到操作审计检查执行状态，避免重复准备。
            </p>
            <button
              v-else-if="app.can('manage')"
              :disabled="
                busy ||
                devices.find((d) => d.id === detail?.incident.deviceId)
                  ?.observerOnly
              "
              @click="prepare(plan.id, step.id)"
            >
              准备此步骤并查看审批预览
            </button>
          </div>
        </article>
        <h3>事件时间线</h3>
        <ol>
          <li v-for="event in detail.timeline" :key="event.id">
            <strong>{{ event.summary }}</strong>
            <p class="muted">
              {{ event.actor }} · {{ event.type }} · {{ when(event.createdAt) }}
            </p>
          </li>
        </ol>
        <form v-if="app.can('write')" @submit.prevent>
          <label
            >状态变更说明<textarea
              v-model="statusNote"
              rows="2"
              maxlength="2000"
            ></textarea>
          </label>
          <div class="actions">
            <button
              type="button"
              :disabled="busy"
              @click="setStatus('resolved')"
            >
              标记已解决</button
            ><button
              type="button"
              :disabled="busy"
              @click="setStatus('dismissed')"
            >
              忽略事件</button
            ><button type="button" :disabled="busy" @click="setStatus('open')">
              重新打开
            </button>
          </div>
        </form>
      </section>
    </div>
  </div>
</template>
<style scoped>
.ws-chat {
  padding: 14px 16px;
  border-radius: 10px;
  background: #f1f5fc;
  margin: 14px 0;
}
.ws-chat.user {
  background: #f0edff;
}
.ws-chat p {
  margin-bottom: 0;
}
.ws-investigation {
  border-top: 1px solid var(--cloud-border);
  padding-top: 22px;
  margin-top: 22px;
}
.ws-response-step {
  padding: 16px;
  background: var(--cloud-panel-soft, #f5f7fb);
  border-radius: 10px;
  margin: 12px 0;
}
.ws-dialog ul,
.ws-dialog ol {
  padding-left: 22px;
  font-size: 13px;
  line-height: 1.8;
}
.ws-dialog h4 {
  margin: 17px 0 7px;
}
</style>
