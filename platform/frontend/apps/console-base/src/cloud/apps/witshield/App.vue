<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";
import { useApp } from "../shared";
import DevicePanel from "./DevicePanel.vue";
import EngineerPanel from "./EngineerPanel.vue";
import SettingsPanel from "./SettingsPanel.vue";
import Evidence from "./Evidence.vue";
import ActionReview from "./ActionReview.vue";
import {
  actionNames,
  label,
  when,
  key,
  type Device,
  type Finding,
  type Report,
  type Action,
  type Audit,
  type Prepared,
  type SystemHealth,
  type SensorHealth,
} from "./model";
const app = useApp();
const tab = ref("overview");
const tabs = [
  ["overview", "安全概览"],
  ["findings", "风险发现"],
  ["reports", "安全报告"],
  ["devices", "设备与计划"],
  ["engineer", "AI 安全工程师"],
  ["audit", "操作审计"],
  ["settings", "设置"],
];
const aiExplanation = ref(""),
  managedDeviceID = ref("");
const devices = ref<Device[]>([]),
  findings = ref<Finding[]>([]),
  reports = ref<Report[]>([]),
  actions = ref<Action[]>([]),
  audit = ref<Audit[]>([]),
  sensors = ref<SensorHealth[]>([]),
  health = ref<SystemHealth>();
const loading = ref(false),
  error = ref(""),
  busy = ref(false),
  search = ref(""),
  deviceFilter = ref(""),
  severityFilter = ref(""),
  selectedFinding = ref<Finding>(),
  selectedReport = ref<Report>(),
  selectedAction = ref<{ action: Action; audit: Audit[] }>(),
  prepared = ref<Prepared>(),
  sshConfirmed = ref(false),
  eventCursor = ref("");
interface Observation {
  id: string;
  deviceId: string;
  type: string;
  sourceIp?: string;
  occurredAt: string;
  payload: Record<string, unknown>;
}
const events = ref<Observation[]>([]);
const filteredFindings = computed(() =>
  findings.value.filter(
    (f) =>
      (!deviceFilter.value || f.deviceId === deviceFilter.value) &&
      (!severityFilter.value || f.severity === severityFilter.value) &&
      (!search.value ||
        (f.title + " " + f.description)
          .toLowerCase()
          .includes(search.value.toLowerCase())),
  ),
);
const filteredReports = computed(() =>
  reports.value.filter(
    (r) => !deviceFilter.value || r.deviceId === deviceFilter.value,
  ),
);
const critical = computed(
  () =>
    findings.value.filter(
      (f) => f.status === "open" && ["critical", "high"].includes(f.severity),
    ).length,
);
const workerNames: Record<string, string> = {
  maintenance: "数据维护",
  notification_smtp: "邮件通知",
  notification_webhook: "Webhook 通知",
  scheduler: "扫描调度",
  security_engineer: "安全调查",
};
const deviceName = (id: string) =>
  devices.value.find((d) => d.id === id)?.name ?? id;
async function items<T>(path: string) {
  return (await app.request<{ items: T[] }>(path)).items ?? [];
}
async function load() {
  loading.value = true;
  error.value = "";
  try {
    const allDevices = await items<Device>("/devices");
    const allFindings: Finding[] = [];
    const latestReports: Report[] = [];
    for (let offset = 0; offset < allDevices.length; offset += 8) {
      const batch = allDevices.slice(offset, offset + 8);
      const found = await Promise.all(
        batch.map((d) =>
          items<Finding>("/findings?deviceId=" + key(d.id) + "&limit=2000"),
        ),
      );
      allFindings.push(...found.flat());
      const latest = await Promise.all(
        batch.map((d) =>
          items<Report>("/reports?deviceId=" + key(d.id) + "&limit=1"),
        ),
      );
      latestReports.push(...latest.flat());
    }
    const data = await Promise.all([
      Promise.resolve(allDevices),
      Promise.resolve(allFindings),
      Promise.resolve(
        deviceFilter.value
          ? await items<Report>(
              "/reports?deviceId=" + key(deviceFilter.value) + "&limit=100",
            )
          : latestReports,
      ),
      items<Action>("/actions?limit=100"),
      items<Audit>("/audit?limit=100"),
      items<SensorHealth>("/sensors"),
      app.request<SystemHealth>("/system/health"),
    ]);
    [
      devices.value,
      findings.value,
      reports.value,
      actions.value,
      audit.value,
      sensors.value,
      health.value,
    ] = data;
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}
async function run(work: () => Promise<void>) {
  if (busy.value) return;
  busy.value = true;
  error.value = "";
  try {
    await work();
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    busy.value = false;
  }
}
async function scan(id: string) {
  await run(async () => {
    await app.request(`/devices/${key(id)}/scan`, { method: "POST", body: {} });
    app.notify("扫描已进入设备队列");
    await load();
  });
}
async function openReport(id: string) {
  await run(async () => {
    selectedReport.value = await app.request<Report>(`/reports/${key(id)}`);
  });
}
async function openAction(id: string) {
  await run(async () => {
    selectedAction.value = await app.request(`/actions/${key(id)}`);
    sshConfirmed.value = false;
  });
}
async function actionOperation(operation: "rollback" | "confirm") {
  const selected = selectedAction.value;
  if (!selected) return;
  if (
    operation === "rollback" &&
    !confirm(
      "确认对这次操作发起回滚？引擎会校验可恢复状态，设备实际结果将进入审计。",
    )
  )
    return;
  if (operation === "confirm" && !sshConfirmed.value) return;
  await run(async () => {
    await app.request(`/actions/${key(selected.action.id)}/${operation}`, {
      method: "POST",
      body: {},
    });
    app.notify("请求已提交，等待设备确认");
    selectedAction.value = await app.request(
      `/actions/${key(selected.action.id)}`,
    );
    await load();
  });
}
async function loadEvents(more = false) {
  await run(async () => {
    const result = await app.request<{
      items: Observation[];
      nextCursor: string;
    }>(
      "/security-events?limit=50" +
        (deviceFilter.value ? "&deviceId=" + key(deviceFilter.value) : "") +
        (more && eventCursor.value ? "&cursor=" + key(eventCursor.value) : ""),
    );
    events.value = more
      ? [...events.value, ...(result.items ?? [])]
      : (result.items ?? []);
    eventCursor.value = result.nextCursor;
  });
}
async function explainFinding() {
  const finding = selectedFinding.value;
  if (!finding) return;
  await run(async () => {
    const result = await app.request<{ message: string }>("/ai/chat", {
      method: "POST",
      body: {
        message:
          "请解释这项风险，区分观察证据、推断和需要核实的部分，并给出安全处置建议。",
        deviceId: finding.deviceId,
        findingIds: [finding.id],
      },
      timeoutMs: 180000,
    });
    if (selectedFinding.value?.id === finding.id)
      aiExplanation.value = result.message;
  });
}
function coverage(report: Report) {
  const s = report.summary;
  if (!s) return "覆盖未知";
  const total = Number(s.checks),
    done = Number(s.completedChecks);
  return Number.isFinite(total) &&
    total > 0 &&
    Number.isFinite(done) &&
    done >= 0 &&
    done <= total
    ? `${done} / ${total} 项 (${Math.round((done / total) * 100)}%)`
    : "覆盖未知";
}
watch(selectedFinding, () => {
  aiExplanation.value = "";
});
watch(deviceFilter, () => {
  void run(async () => {
    if (deviceFilter.value)
      reports.value = await items<Report>(
        "/reports?deviceId=" + key(deviceFilter.value) + "&limit=100",
      );
    else await load();
  });
});
onMounted(load);
</script>
<template>
  <div class="native-app ws-app">
    <header class="toolbar">
      <div>
        <h2>WitShield 安全工作台</h2>
        <p class="muted">当前项目的设备、风险调查与人工审批</p>
      </div>
      <button :disabled="loading || busy" @click="load">
        {{ loading ? "正在刷新…" : "刷新数据" }}
      </button>
    </header>
    <nav class="app-tabs" aria-label="安全功能">
      <button
        v-for="item in tabs"
        :key="item[0]"
        :class="{ active: tab === item[0] }"
        @click="tab = item[0]"
      >
        {{ item[1] }}
      </button>
    </nav>
    <p v-if="error" role="alert" class="ws-error">
      {{ error }} <button @click="load">重试</button>
    </p>
    <p v-if="loading && !devices.length" role="status">
      正在读取当前项目的安全数据…
    </p>
    <template v-if="tab === 'overview'">
      <div class="ws-metrics">
        <article class="panel">
          <span>已接入设备</span><strong>{{ devices.length }}</strong
          ><small
            >{{
              devices.filter((d) => d.status === "online").length
            }}
            台在线</small
          >
        </article>
        <article class="panel">
          <span>严重 / 高风险</span
          ><strong class="ws-risk">{{ critical }}</strong
          ><small>来自当前扫描记录</small>
        </article>
        <article class="panel">
          <span>待人工处理</span
          ><strong>{{
            actions.filter((a) =>
              ["draft", "awaiting_confirmation", "indeterminate"].includes(
                a.status,
              ),
            ).length
          }}</strong
          ><small>审批、连通确认及未知结果</small>
        </article>
        <article class="panel">
          <span>引擎健康</span
          ><strong>{{
            health?.status === "ok" ? "正常" : health ? "需关注" : "读取中"
          }}</strong
          ><small>{{ when(health?.checkedAt) }}</small>
        </article>
      </div>
      <div class="ws-columns">
        <section class="panel">
          <div class="toolbar">
            <h3>设备安全状态</h3>
            <button @click="tab = 'devices'">管理设备</button>
          </div>
          <p v-if="!devices.length" class="empty">
            接入第一台设备后，扫描报告和实时传感器将在这里汇总。
          </p>
          <div v-for="device in devices" :key="device.id" class="ws-row">
            <div>
              <strong>{{ device.name }}</strong>
              <p class="muted">
                {{ device.hostname }} · {{ device.os }} ·
                {{ device.observerOnly ? "只读观测" : "原生 Agent" }}
              </p>
            </div>
            <span :class="['ws-badge', device.status]">{{
              label(device.status)
            }}</span
            ><button
              v-if="app.can('write')"
              :disabled="busy || device.status === 'revoked'"
              @click="scan(device.id)"
            >
              扫描
            </button>
          </div>
        </section>
        <section class="panel">
          <h3>工作进程</h3>
          <div
            v-for="worker in health?.workers ?? []"
            :key="worker.name"
            class="ws-row"
          >
            <div>
              <strong>{{ workerNames[worker.name] ?? worker.name }}</strong>
              <p class="muted">最近成功 {{ when(worker.lastSuccessAt) }}</p>
              <p v-if="worker.error" class="ws-error">{{ worker.error }}</p>
            </div>
            <span class="ws-badge">{{ label(worker.status) }}</span>
          </div>
        </section>
      </div>
      <section class="panel">
        <h3>传感器覆盖</h3>
        <p class="muted">设备心跳正常不代表每个证据源都在正常工作。</p>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>设备 / 传感器</th>
                <th>状态</th>
                <th>最近成功</th>
                <th>事件数</th>
                <th>说明</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="s in sensors" :key="s.deviceId + s.sensorId">
                <td>{{ deviceName(s.deviceId) }} / {{ s.name }}</td>
                <td>{{ label(s.state) }}</td>
                <td>{{ when(s.lastSuccessAt) }}</td>
                <td>{{ s.eventCount }}</td>
                <td>{{ s.error || "—" }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-if="!sensors.length" class="empty">设备尚未上报传感器状态</p>
      </section>
    </template>
    <template v-else-if="tab === 'findings' || tab === 'reports'">
      <section class="panel">
        <div class="toolbar">
          <h3>{{ tab === "findings" ? "风险发现" : "安全报告" }}</h3>
          <div class="actions">
            <select v-model="deviceFilter" aria-label="筛选设备">
              <option value="">全部设备</option>
              <option v-for="d in devices" :key="d.id" :value="d.id">
                {{ d.name }}
              </option></select
            ><template v-if="tab === 'findings'"
              ><select v-model="severityFilter" aria-label="风险等级">
                <option value="">全部等级</option>
                <option
                  v-for="v in ['critical', 'high', 'medium', 'low', 'info']"
                  :key="v"
                  :value="v"
                >
                  {{ label(v) }}
                </option></select
              ><input
                v-model="search"
                placeholder="搜索标题或描述"
                aria-label="搜索发现"
            /></template>
          </div>
        </div>
        <div v-if="tab === 'findings'" class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>风险</th>
                <th>发现</th>
                <th>设备</th>
                <th>状态</th>
                <th>最近发现</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="finding in filteredFindings" :key="finding.id">
                <td>
                  <span :class="['ws-badge', finding.severity]">{{
                    label(finding.severity)
                  }}</span>
                </td>
                <td>
                  <button class="ws-link" @click="selectedFinding = finding">
                    {{ finding.title }}
                  </button>
                  <p class="muted">{{ finding.category }}</p>
                </td>
                <td>{{ deviceName(finding.deviceId) }}</td>
                <td>{{ label(finding.status) }}</td>
                <td>{{ when(finding.lastSeenAt) }}</td>
              </tr>
            </tbody>
          </table>
          <p v-if="!filteredFindings.length" class="empty">
            暂无匹配的风险发现；未完成扫描的设备不代表安全。
          </p>
        </div>
        <div v-else class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>设备</th>
                <th>分数 / 覆盖</th>
                <th>开始时间</th>
                <th>完成时间</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="report in filteredReports" :key="report.id">
                <td>{{ deviceName(report.deviceId) }}</td>
                <td>
                  {{ report.score }}
                  <p class="muted">{{ coverage(report) }}</p>
                </td>
                <td>{{ when(report.startedAt) }}</td>
                <td>{{ when(report.completedAt) }}</td>
                <td>
                  <button :disabled="busy" @click="openReport(report.id)">
                    查看报告
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
          <p class="muted">
            全部设备展示各自最新报告；选择单台设备可读取最近 100
            份历史报告。覆盖未知或检查失败时，分数不能代表完整安全评估。
          </p>
          <p v-if="!filteredReports.length" class="empty">暂无扫描报告</p>
        </div>
      </section>
    </template>
    <DevicePanel
      v-else-if="tab === 'devices'"
      :devices="devices"
      :initial-device-id="managedDeviceID"
      @changed="load"
      @prepared="prepared = $event"
    />
    <EngineerPanel
      v-else-if="tab === 'engineer'"
      :devices="devices"
      @prepared="prepared = $event"
      @changed="load"
    />
    <template v-else-if="tab === 'audit'">
      <section class="panel">
        <h3>审批与执行记录</h3>
        <p class="status-note">
          结果未知的操作必须先核查设备。SSH 加固完成后需要重新建立 SSH
          会话并确认连通；确认回执到达前仍显示“正在确认”。
        </p>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>操作</th>
                <th>设备</th>
                <th>状态</th>
                <th>更新时间</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="action in actions" :key="action.id">
                <td>{{ actionNames[action.type] ?? action.type }}</td>
                <td>{{ deviceName(action.deviceId) }}</td>
                <td>
                  <span :class="['ws-badge', action.status]">{{
                    label(action.status)
                  }}</span>
                </td>
                <td>{{ when(action.updatedAt) }}</td>
                <td>
                  <button @click="openAction(action.id)">查看与处理</button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-if="!actions.length" class="empty">暂无审批或执行记录</p>
      </section>
      <section class="panel">
        <h3>操作审计</h3>
        <div v-for="event in audit" :key="event.id" class="ws-row">
          <div>
            <strong>{{ event.summary || event.event }}</strong>
            <p class="muted">
              {{ event.actor }} ·
              {{ event.deviceId ? deviceName(event.deviceId) : "项目" }} ·
              {{ when(event.createdAt) }}
            </p>
            <details v-if="event.details">
              <summary>审计详情</summary>
              <Evidence :value="event.details" />
            </details>
          </div>
        </div>
        <p v-if="!audit.length" class="empty">暂无操作审计</p>
      </section>
      <section class="panel">
        <div class="toolbar">
          <h3>原始安全观察</h3>
          <div class="actions">
            <select v-model="deviceFilter" aria-label="观察设备">
              <option value="">全部设备</option>
              <option v-for="d in devices" :key="d.id" :value="d.id">
                {{ d.name }}
              </option></select
            ><button :disabled="busy" @click="loadEvents()">读取观察</button>
          </div>
        </div>
        <p class="muted">
          未经验证的日志观察仅作为证据展示，不自动授予执行权限。
        </p>
        <details v-for="event in events" :key="event.id" class="ws-row">
          <summary>
            {{ event.type }} · {{ deviceName(event.deviceId) }} ·
            {{ when(event.occurredAt) }}
          </summary>
          <Evidence :value="event.payload" />
        </details>
        <button v-if="eventCursor" :disabled="busy" @click="loadEvents(true)">
          加载更多
        </button>
      </section>
    </template>
    <SettingsPanel v-else-if="tab === 'settings'" />
    <div
      v-if="selectedFinding"
      class="ws-overlay"
      @click.self="selectedFinding = undefined"
    >
      <section
        role="dialog"
        aria-modal="true"
        aria-label="风险详情"
        class="panel ws-dialog"
      >
        <div class="toolbar">
          <h2>{{ selectedFinding.title }}</h2>
          <button @click="selectedFinding = undefined">关闭</button>
        </div>
        <p>
          {{ label(selectedFinding.severity) }} ·
          {{ deviceName(selectedFinding.deviceId) }} ·
          {{ label(selectedFinding.status) }}
        </p>
        <p>{{ selectedFinding.description }}</p>
        <h3>观察证据</h3>
        <p class="ws-preserve">
          {{ selectedFinding.evidence || "没有额外证据" }}
        </p>
        <button
          v-if="app.can('write')"
          :disabled="busy"
          @click="explainFinding"
        >
          AI 解释这项风险
        </button>
        <p v-if="aiExplanation" class="ws-preserve status-note">
          {{ aiExplanation }}
        </p>
        <h3>修复建议</h3>
        <p class="ws-preserve">
          {{ selectedFinding.remediation || "请核对证据后制定处置方案" }}
        </p>
        <button
          v-if="app.can('manage')"
          @click="
            managedDeviceID = selectedFinding.deviceId;
            tab = 'devices';
            selectedFinding = undefined;
          "
        >
          打开设备并准备操作
        </button>
      </section>
    </div>
    <div
      v-if="selectedReport"
      class="ws-overlay"
      @click.self="selectedReport = undefined"
    >
      <section
        role="dialog"
        aria-modal="true"
        aria-label="扫描报告"
        class="panel ws-dialog"
      >
        <div class="toolbar">
          <h2>{{ deviceName(selectedReport.deviceId) }} · 扫描报告</h2>
          <button @click="selectedReport = undefined">关闭</button>
        </div>
        <p>
          安全分数 {{ selectedReport.score }} ·
          {{ when(selectedReport.completedAt) }}
        </p>
        <Evidence :value="selectedReport.summary" />
        <h3>本次发现</h3>
        <article
          v-for="f in selectedReport.findings ?? []"
          :key="f.id"
          class="ws-row"
        >
          <div>
            <strong>{{ label(f.severity) }} · {{ f.title }}</strong>
            <p>{{ f.description }}</p>
            <details>
              <summary>证据与建议</summary>
              <p class="ws-preserve">{{ f.evidence }}</p>
              <p>{{ f.remediation }}</p>
            </details>
          </div>
        </article>
      </section>
    </div>
    <div
      v-if="selectedAction"
      class="ws-overlay"
      @click.self="selectedAction = undefined"
    >
      <section
        role="dialog"
        aria-modal="true"
        aria-label="操作详情"
        class="panel ws-dialog"
      >
        <div class="toolbar">
          <h2>{{ actionNames[selectedAction.action.type] }}</h2>
          <button @click="selectedAction = undefined">关闭</button>
        </div>
        <p>
          {{ deviceName(selectedAction.action.deviceId) }} ·
          {{ label(selectedAction.action.status) }}
        </p>
        <Evidence :value="selectedAction.action.preview" />
        <h3>批准参数</h3>
        <Evidence :value="selectedAction.action.parameters" />
        <p v-if="selectedAction.action.error" class="ws-error">
          {{ selectedAction.action.error }}
        </p>
        <p
          v-if="selectedAction.action.status === 'indeterminate'"
          class="status-note"
        >
          命令已可能执行，结果无法确认。请先核查设备和审计，不自动重复执行。
        </p>
        <template
          v-if="selectedAction.action.status === 'awaiting_confirmation'"
          ><p>
            请在 {{ when(selectedAction.action.confirmBy) }} 前用新的 SSH
            会话确认连通，否则设备自动回滚。
          </p>
          <label class="ws-check"
            ><input v-model="sshConfirmed" type="checkbox" />我已用新的 SSH
            会话成功连接目标设备</label
          ><button
            :disabled="busy || !sshConfirmed || !app.can('manage')"
            @click="actionOperation('confirm')"
          >
            提交连通确认
          </button></template
        ><button
          v-if="
            selectedAction.action.rollbackPayload &&
            ['succeeded', 'awaiting_confirmation'].includes(
              selectedAction.action.status,
            ) &&
            app.can('manage')
          "
          :disabled="busy"
          @click="actionOperation('rollback')"
        >
          请求回滚</button
        ><button :disabled="busy" @click="openAction(selectedAction.action.id)">
          刷新执行状态
        </button>
        <h3>执行审计</h3>
        <Evidence :value="selectedAction.audit" />
      </section>
    </div>
    <ActionReview
      v-if="prepared"
      :prepared="prepared"
      @close="prepared = undefined"
      @approved="load"
    />
  </div>
</template>
<style>
.ws-app h2,
.ws-app h3 {
  margin: 0 0 12px;
}
.ws-app .muted,
.ws-app small {
  color: var(--cloud-muted);
  font-size: 12px;
}
.ws-app .panel {
  padding: 22px;
}
.ws-app p {
  line-height: 1.7;
}
.ws-app input:not([type="checkbox"]),
.ws-app select,
.ws-app textarea {
  background: var(--cloud-panel);
  border: 1px solid var(--cloud-border);
  border-radius: 7px;
  padding: 9px 11px;
  color: var(--cloud-text);
  font: inherit;
}
.ws-app label:not(.ws-check) {
  display: grid;
  gap: 7px;
  font-size: 13px;
}
.ws-app button {
  border: 1px solid var(--cloud-border);
  background: var(--cloud-panel);
  border-radius: 7px;
  padding: 8px 12px;
  color: var(--cloud-text);
  cursor: pointer;
  font: inherit;
}
.ws-app button.primary {
  background: var(--cloud-blue, #4f46e5);
  color: white;
  border-color: transparent;
}
.ws-app button:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}
.ws-app button.ws-link {
  padding: 0;
  border: 0;
  color: var(--cloud-blue, #4f46e5);
  text-align: left;
}
.ws-metrics {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 16px;
}
.ws-metrics article {
  display: grid;
  gap: 10px;
}
.ws-metrics strong {
  font-size: 30px;
}
.ws-columns {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 20px;
}
.ws-row {
  display: flex;
  gap: 16px;
  align-items: center;
  justify-content: space-between;
  padding: 16px 0;
  border-bottom: 1px solid var(--cloud-border);
}
.ws-row:last-child {
  border-bottom: 0;
}
.ws-row p {
  margin: 5px 0;
}
.ws-badge {
  display: inline-block;
  padding: 4px 9px;
  border-radius: 6px;
  background: var(--cloud-panel-soft, #f3f4f6);
  font-size: 12px;
  white-space: nowrap;
}
.ws-badge.critical,
.ws-badge.high,
.ws-badge.indeterminate,
.ws-badge.failed,
.ws-error,
.ws-risk {
  color: #c43c3c;
}
.ws-badge.online,
.ws-badge.succeeded {
  color: #178363;
}
.ws-error {
  padding: 12px 14px;
  background: #fff1f0;
  border-radius: 8px;
}
.ws-overlay {
  position: fixed;
  inset: 0;
  background: #10182780;
  z-index: 80;
  display: grid;
  place-items: center;
  padding: 24px;
}
.ws-dialog {
  width: min(860px, 100%);
  max-height: 88vh;
  overflow: auto;
  background: var(--cloud-panel, #fff);
  border-radius: 14px;
  box-shadow: 0 30px 90px #0003;
}
.ws-dialog > p,
.ws-dialog > h3,
.ws-dialog > .actions {
  margin-top: 20px;
}
.ws-check {
  display: flex;
  gap: 8px;
  align-items: center;
  margin: 14px 0;
}
.ws-preserve {
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
.ws-app details summary {
  cursor: pointer;
}
.ws-app details {
  display: block;
}
.ws-app fieldset {
  border: 1px solid var(--cloud-border);
  border-radius: 10px;
  padding: 18px;
  min-width: 0;
}
.ws-app fieldset:disabled {
  opacity: 0.65;
}
@media (max-width: 900px) {
  .ws-metrics {
    grid-template-columns: repeat(2, 1fr);
  }
  .ws-columns {
    grid-template-columns: 1fr;
  }
}
@media (max-width: 600px) {
  .ws-overlay {
    padding: 12px;
  }
  .ws-app .panel {
    padding: 16px;
  }
  .ws-metrics {
    gap: 10px;
  }
  .ws-row {
    flex-wrap: wrap;
  }
}
</style>
