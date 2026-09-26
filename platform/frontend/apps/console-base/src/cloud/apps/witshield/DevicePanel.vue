<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from "vue";
import { useApp } from "../shared";
import Evidence from "./Evidence.vue";
import {
  actionNames,
  key,
  label,
  split,
  when,
  type Defense,
  type Device,
  type PolicyGrant,
  type Prepared,
  type ScanSchedule,
  type Token,
} from "./model";
const props = defineProps<{ devices: Device[]; initialDeviceId?: string }>();
const emit = defineEmits<{ changed: []; prepared: [Prepared] }>();
const app = useApp();
const selected = ref(""),
  busy = ref(false),
  error = ref(""),
  tokens = ref<Token[]>([]),
  schedules = ref<ScanSchedule[]>([]),
  grants = ref<PolicyGrant[]>([]),
  publicURL = ref(""),
  issued = ref<{ token: string; expiresAt: string }>(),
  simulated = ref<unknown>();
const device = computed(() =>
  props.devices.find((d) => d.id === selected.value),
);
const enrollment = reactive({ name: "Euler 设备接入", expiresIn: "15m" });
const schedule = reactive({ every: "24h", enabled: true });
const allowlist = ref("");
const defense = reactive<Defense>({
  enabled: false,
  emergencyStop: false,
  autoBan: false,
  failureThreshold: 30,
  window: "5m",
  banDuration: "15m",
  maxBansPerHour: 20,
  allowlist: [],
});
const simulation = reactive({
  sourceIp: "",
  failureCount: 30,
  recentBanCount: 0,
  alreadyBanned: false,
});
const action = reactive({
  type: "ssh_password_hardening",
  packages: "",
  rollbackAfterSeconds: 300,
  address: "",
  currentAdminIp: "",
  ttlSeconds: 300,
  reason: "",
  path: "/etc/ssh/sshd_config",
  mode: "600",
  uid: "",
  gid: "",
  pid: 2,
  startTime: "",
  executable: "",
});
const capabilities: Record<string, { name: string; action: string }> = {
  "network.auth_bruteforce": {
    name: "网络登录防护",
    action: "temporary_ip_ban",
  },
  "identity.persistence": {
    name: "身份与持久化",
    action: "ssh_password_hardening",
  },
  "workload.runtime": {
    name: "运行时进程",
    action: "temporary_process_suspend",
  },
  "file.integrity": { name: "文件完整性", action: "file_permission_repair" },
  "vulnerability.remediation": {
    name: "漏洞与更新",
    action: "package_security_upgrade",
  },
};
const quoted = (value: string) => "'" + value.replaceAll("'", "'\\''") + "'";
const command = computed(
  () =>
    `# 使用本次 Euler 构建中的设备工具，勿从旧项目下载安装脚本\n# 部署者可从容器提取，再通过已验证的渠道分发到设备：\n# docker cp <euler-container>:/opt/euler/device-tools/witshield-agent ./witshield-agent\n# 在设备上创建仅当前管理员可读的一次性令牌文件：\numask 077\nread -r -s -p 'Enrollment token: ' WITSHIELD_TOKEN; printf '\\n'\nprintf '%s' "$WITSHIELD_TOKEN" > ./euler-enrollment.token\nunset WITSHIELD_TOKEN\n./witshield-agent \\\n  -controller-url ${quoted(publicURL.value)} \\\n  -enrollment-token-file ./euler-enrollment.token \\\n  -consume-enrollment-token -observer-only \\\n  -name 'my-server' -data-dir ./euler-agent-data`,
);
async function run(fn: () => Promise<void>, message?: string) {
  if (busy.value) return;
  busy.value = true;
  error.value = "";
  try {
    await fn();
    if (message) app.notify(message, "success");
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    busy.value = false;
  }
}
async function load() {
  const results = await Promise.all([
    app.request<{ items: ScanSchedule[] }>("/schedules"),
    app.request<{ publicAgentURL: string }>("/instance"),
  ]);
  schedules.value = results[0].items ?? [];
  publicURL.value = results[1].publicAgentURL;
  if (app.can("manage"))
    tokens.value =
      (await app.request<{ items: Token[] }>("/enrollment-tokens")).items ?? [];
}
async function detail() {
  if (!selected.value) return;
  const id = selected.value;
  const [policy, values] = await Promise.all([
    app.request<Defense>(`/devices/${key(id)}/defense-policy`),
    app.request<{ items: PolicyGrant[] }>(`/devices/${key(id)}/policy-grants`),
  ]);
  if (selected.value !== id) return;
  Object.assign(defense, policy);
  allowlist.value = (policy.allowlist ?? []).join("\n");
  grants.value = values.items ?? [];
  simulated.value = undefined;
}
watch(
  () => props.devices,
  () => {
    if (!props.devices.some((d) => d.id === selected.value))
      selected.value =
        props.devices.find((d) => d.id === props.initialDeviceId)?.id ??
        props.devices[0]?.id ??
        "";
  },
  { immediate: true },
);
watch(selected, () => {
  void detail().catch((e) => (error.value = (e as Error).message));
});
async function enroll() {
  await run(async () => {
    const result = await app.request<{ token: string; enrollmentToken: Token }>(
      "/enrollment-tokens",
      { method: "POST", body: enrollment },
    );
    issued.value = {
      token: result.token,
      expiresAt: result.enrollmentToken.expiresAt,
    };
    await load();
  }, "一次性注册码已创建");
}
async function revokeToken(id: string) {
  if (!confirm("撤销这个注册码？尚未接入的设备将无法使用它。")) return;
  await run(async () => {
    await app.request(`/enrollment-tokens/${key(id)}`, { method: "DELETE" });
    issued.value = undefined;
    await load();
  }, "注册码已撤销");
}
async function revokeDevice() {
  if (
    !device.value ||
    !confirm(
      `撤销设备 ${device.value.name}？该设备后续请求将被拒绝，重新接入需要新注册码。`,
    )
  )
    return;
  await run(async () => {
    await app.request(`/devices/${key(selected.value)}`, { method: "DELETE" });
    emit("changed");
  }, "设备已撤销");
}
async function scan() {
  await run(async () => {
    await app.request(`/devices/${key(selected.value)}/scan`, {
      method: "POST",
      body: {},
    });
    emit("changed");
  }, "扫描已排队");
}
async function addSchedule() {
  await run(async () => {
    await app.request("/schedules", {
      method: "POST",
      body: { deviceId: selected.value, kind: "scan", ...schedule },
    });
    await load();
  }, "扫描计划已创建");
}
async function updateSchedule(item: ScanSchedule) {
  await run(async () => {
    await app.request(`/schedules/${key(item.id)}`, {
      method: "PATCH",
      body: { every: item.every, enabled: item.enabled },
    });
    await load();
  }, "扫描计划已保存");
}
async function deleteSchedule(id: string) {
  if (!confirm("删除这个扫描计划？")) return;
  await run(async () => {
    await app.request(`/schedules/${key(id)}`, { method: "DELETE" });
    await load();
  }, "计划已删除");
}
async function saveDefense() {
  if (
    defense.autoBan &&
    !confirm(
      "启用自动临时封禁会使符合确定性阈值的可信安全事件触发设备动作。确认已核对白名单、TTL 和频率限制？",
    )
  )
    return;
  await run(async () => {
    const {
      enabled,
      emergencyStop,
      autoBan,
      failureThreshold,
      window,
      banDuration,
      maxBansPerHour,
    } = defense;
    await app.request(`/devices/${key(selected.value)}/defense-policy`, {
      method: "PUT",
      body: {
        enabled,
        emergencyStop,
        autoBan,
        failureThreshold,
        window,
        banDuration,
        maxBansPerHour,
        allowlist: split(allowlist.value),
      },
    });
    await detail();
  }, "防御策略已保存");
}
async function emergency() {
  if (
    !defense.emergencyStop &&
    !confirm("启动紧急停止会取消尚未执行的自动防御操作。继续？")
  )
    return;
  await run(async () => {
    await app.request(`/devices/${key(selected.value)}/emergency-stop`, {
      method: "POST",
      body: { active: !defense.emergencyStop },
    });
    await detail();
  }, "紧急停止状态已更新");
}
async function saveGrant(grant: PolicyGrant) {
  if (
    grant.enabled &&
    ["auto_low_risk", "enhanced"].includes(grant.mode) &&
    !confirm(
      "确认授予当前设备这项有限自动响应能力？引擎仍会按对应能力和动作规则校验。",
    )
  )
    return;
  await run(async () => {
    const {
      enabled,
      mode,
      allowedActionTypes,
      maxActionsPerHour,
      emergencyStop,
    } = grant;
    await app.request(
      `/devices/${key(selected.value)}/policy-grants/${key(grant.capability)}`,
      {
        method: "PUT",
        body: {
          enabled,
          mode,
          allowedActionTypes,
          maxActionsPerHour,
          emergencyStop,
        },
      },
    );
    await detail();
  }, "能力授权已保存");
}
async function simulate() {
  await run(async () => {
    simulated.value = await app.request(
      `/devices/${key(selected.value)}/defense-policy/simulate`,
      { method: "POST", body: simulation },
    );
  });
}
async function prepare() {
  await run(async () => {
    let parameters: Record<string, unknown>;
    switch (action.type) {
      case "package_security_upgrade":
        parameters = { packages: split(action.packages) };
        break;
      case "ssh_password_hardening":
        parameters = { rollbackAfterSeconds: action.rollbackAfterSeconds };
        break;
      case "temporary_ip_ban":
        parameters = {
          address: action.address,
          currentAdminIp: action.currentAdminIp,
          ttlSeconds: action.ttlSeconds,
          reason: action.reason,
        };
        break;
      case "file_permission_repair":
        parameters = {
          path: action.path,
          mode: action.mode,
          ...(action.uid !== "" ? { uid: Number(action.uid) } : {}),
          ...(action.gid !== "" ? { gid: Number(action.gid) } : {}),
        };
        break;
      default:
        if (
          !/^\d+$/.test(action.startTime) ||
          !Number.isSafeInteger(Number(action.startTime))
        )
          throw new Error("进程启动标识必须为可精确表示的整数");
        parameters = {
          pid: action.pid,
          startTime: Number(action.startTime),
          executable: action.executable,
          ttlSeconds: action.ttlSeconds,
          reason: action.reason,
        };
    }
    emit(
      "prepared",
      await app.request<Prepared>("/actions", {
        method: "POST",
        body: { deviceId: selected.value, type: action.type, parameters },
      }),
    );
  });
}
async function copy(value: string) {
  try {
    await navigator.clipboard.writeText(value);
    app.notify("已复制", "success");
  } catch {
    error.value = "无法访问剪贴板，请手动复制";
  }
}
onMounted(() => {
  void run(async () => {
    await load();
    await detail();
  });
});
</script>
<template>
  <div>
    <p v-if="error" class="ws-error" role="alert">{{ error }}</p>
    <section class="panel">
      <div class="toolbar">
        <h3>设备接入</h3>
        <button :disabled="busy" @click="run(load)">刷新</button>
      </div>
      <p>
        每个项目有独立设备入口和数据。注册码只能用一次；人员登录会话不能充当设备身份。
      </p>
      <label
        >本项目设备 Controller URL<input
          :value="publicURL"
          readonly
          @focus="($event.target as HTMLInputElement).select()"
      /></label>
      <form v-if="app.can('manage')" class="ws-grid" @submit.prevent="enroll">
        <label
          >注册码用途<input
            v-model="enrollment.name"
            maxlength="100"
            required /></label
        ><label
          >有效期<select v-model="enrollment.expiresIn">
            <option value="15m">15 分钟</option>
            <option value="1h">1 小时</option>
            <option value="24h">24 小时</option>
          </select></label
        ><button class="primary" :disabled="busy">生成一次性注册码</button>
      </form>
      <div v-if="issued" class="status-note">
        <strong>请现在复制注册码，到期 {{ when(issued.expiresAt) }}</strong>
        <p class="ws-secret">{{ issued.token }}</p>
        <div class="actions">
          <button @click="copy(issued.token)">复制注册码</button
          ><button @click="issued = undefined">隐藏注册码</button>
        </div>
      </div>
      <details>
        <summary>安装与接入指南</summary>
        <p>
          使用当前 Euler 构建附带的 Agent。下面以只读 observer
          模式启动，可先查看资产与报告。启用修复还需要部署受限
          Helper、设备本地授权和控制台逐项审批；不要简单地给浏览器远程 root
          权限。
        </p>
        <pre class="ws-code">{{ command }}</pre>
        <button @click="copy(command)">复制接入步骤</button>
      </details>
      <div v-if="app.can('manage')" class="table-wrap">
        <table>
          <thead>
            <tr>
              <th>用途</th>
              <th>标识</th>
              <th>使用情况</th>
              <th>有效期</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="item in tokens"
              :key="item.id"
              :data-testid="'enrollment-' + item.id"
            >
              <td>{{ item.name }}</td>
              <td>{{ item.hint }}</td>
              <td>{{ item.uses }} / {{ item.maxUses }}</td>
              <td>{{ when(item.expiresAt) }}</td>
              <td>
                <button
                  :disabled="busy || !!item.revokedAt"
                  @click="revokeToken(item.id)"
                >
                  {{ item.revokedAt ? "已撤销" : "撤销" }}
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>
    <section class="panel">
      <div class="toolbar">
        <h3>设备详情</h3>
        <select v-model="selected" aria-label="选择设备">
          <option value="">请选择设备</option>
          <option v-for="d in devices" :key="d.id" :value="d.id">
            {{ d.name }} · {{ label(d.status) }}
          </option>
        </select>
      </div>
      <p v-if="!device" class="empty">
        接入设备后可设置扫描、查看授权并准备安全操作。
      </p>
      <template v-else
        ><div class="ws-grid">
          <div>
            <small>系统</small>
            <p>{{ device.hostname }} · {{ device.os }} / {{ device.arch }}</p>
          </div>
          <div>
            <small>Agent</small>
            <p>
              {{ device.agentVersion }} ·
              {{ device.observerOnly ? "只读观测" : "原生设备" }}
            </p>
          </div>
          <div>
            <small>最近心跳</small>
            <p>{{ when(device.lastSeenAt) }}</p>
          </div>
        </div>
        <div class="actions">
          <button
            v-if="app.can('write')"
            :disabled="busy || device.status === 'revoked'"
            @click="scan"
          >
            立即扫描</button
          ><button
            v-if="app.can('manage')"
            :disabled="busy || device.status === 'revoked'"
            @click="revokeDevice"
          >
            撤销设备
          </button>
        </div></template
      >
    </section>
    <template v-if="device"
      ><section class="panel">
        <h3>扫描计划</h3>
        <form
          v-if="app.can('write')"
          class="ws-grid"
          @submit.prevent="addSchedule"
        >
          <label
            >扫描间隔<input
              v-model="schedule.every"
              required
              placeholder="24h / 168h / 30m" /></label
          ><label class="ws-check"
            ><input v-model="schedule.enabled" type="checkbox" />启用</label
          ><button :disabled="busy">添加计划</button>
        </form>
        <p class="muted">
          每日为 24h，每周为 168h。自定义间隔使用 h / m /
          s，服务端会校验允许范围。
        </p>
        <div
          v-for="item in schedules.filter((s) => s.deviceId === selected)"
          :key="item.id"
          :data-testid="'schedule-' + item.id"
          class="ws-row"
        >
          <label
            >间隔<input
              v-model="item.every"
              :disabled="!app.can('write')" /></label
          ><label class="ws-check"
            ><input
              v-model="item.enabled"
              type="checkbox"
              :disabled="!app.can('write')"
            />启用</label
          >
          <div>
            <p>下次 {{ when(item.nextRunAt) }}</p>
            <small>上次 {{ when(item.lastRunAt) }}</small>
          </div>
          <div v-if="app.can('write')" class="actions">
            <button :disabled="busy" @click="updateSchedule(item)">保存</button
            ><button :disabled="busy" @click="deleteSchedule(item.id)">
              删除
            </button>
          </div>
        </div>
      </section>
      <section class="panel">
        <div class="toolbar">
          <h3>能力授权</h3>
          <span v-if="device.observerOnly" class="ws-badge"
            >只读设备不能执行修复</span
          >
        </div>
        <p class="muted">
          授权按设备和能力独立设置。准备操作不会执行，人工批准与设备校验仍然必要。
        </p>
        <form
          v-for="grant in grants"
          :key="grant.capability"
          class="ws-grant"
          @submit.prevent="saveGrant(grant)"
        >
          <h4>
            {{ capabilities[grant.capability]?.name ?? grant.capability }}
          </h4>
          <fieldset :disabled="!app.can('manage') || busy">
            <div class="ws-grid">
              <label class="ws-check"
                ><input
                  v-model="grant.enabled"
                  type="checkbox"
                />启用能力</label
              ><label
                >自主级别<select v-model="grant.mode">
                  <option value="observe">观察</option>
                  <option value="assist">协助</option>
                  <option value="auto_low_risk" :disabled="device.observerOnly">
                    有限自动
                  </option>
                  <option
                    v-if="grant.capability !== 'network.auth_bruteforce'"
                    value="enhanced"
                    :disabled="device.observerOnly"
                  >
                    增强调查
                  </option>
                </select></label
              ><label
                >每小时动作上限<input
                  v-model.number="grant.maxActionsPerHour"
                  type="number"
                  min="1"
                  :max="
                    grant.capability === 'network.auth_bruteforce' ? 100 : 1000
                  "
                  required
              /></label>
            </div>
            <label v-if="capabilities[grant.capability]" class="ws-check"
              ><input
                v-model="grant.allowedActionTypes"
                type="checkbox"
                :value="capabilities[grant.capability].action"
              />允许
              {{ actionNames[capabilities[grant.capability].action] }}</label
            ><label class="ws-check"
              ><input
                v-model="grant.emergencyStop"
                type="checkbox"
              />暂停此能力的自动响应</label
            ><button v-if="app.can('manage')" :disabled="busy">
              保存本项授权
            </button>
          </fieldset>
        </form>
      </section>
      <section class="panel">
        <div class="toolbar">
          <h3>SSH 暴力破解防御</h3>
          <button v-if="app.can('manage')" :disabled="busy" @click="emergency">
            {{ defense.emergencyStop ? "解除紧急停止" : "紧急停止自动防御" }}
          </button>
        </div>
        <form @submit.prevent="saveDefense">
          <fieldset :disabled="!app.can('manage') || busy">
            <div class="actions">
              <label class="ws-check"
                ><input
                  v-model="defense.enabled"
                  type="checkbox"
                />启用防御</label
              ><label class="ws-check"
                ><input
                  v-model="defense.autoBan"
                  type="checkbox"
                  :disabled="device.observerOnly"
                />符合阈值时自动临时封禁</label
              >
            </div>
            <div class="ws-grid">
              <label
                >失败阈值<input
                  v-model.number="defense.failureThreshold"
                  type="number"
                  min="1"
                  required /></label
              ><label
                >时间窗口<input
                  v-model="defense.window"
                  required
                  placeholder="5m" /></label
              ><label
                >封禁时长<input
                  v-model="defense.banDuration"
                  required
                  placeholder="15m" /></label
              ><label
                >每小时最多封禁<input
                  v-model.number="defense.maxBansPerHour"
                  type="number"
                  min="1"
                  max="100"
                  required
              /></label>
            </div>
            <label
              >IP / CIDR 白名单<textarea
                v-model="allowlist"
                rows="3"
                placeholder="每行一个地址或网段，始终保护管理员出口地址"
              ></textarea></label
            ><button v-if="app.can('manage')" :disabled="busy">
              保存防御策略
            </button>
          </fieldset>
        </form>
        <details v-if="app.can('manage')">
          <summary>模拟策略判断，不执行封禁</summary>
          <form @submit.prevent="simulate">
            <div class="ws-grid">
              <label
                >来源 IP<input v-model="simulation.sourceIp" required /></label
              ><label
                >失败次数<input
                  v-model.number="simulation.failureCount"
                  type="number"
                  min="0"
                  required /></label
              ><label
                >近期封禁次数<input
                  v-model.number="simulation.recentBanCount"
                  type="number"
                  min="0"
                  required
              /></label>
            </div>
            <label class="ws-check"
              ><input
                v-model="simulation.alreadyBanned"
                type="checkbox"
              />已经封禁</label
            ><button :disabled="busy">运行模拟</button>
          </form>
          <Evidence v-if="simulated" :value="simulated" />
        </details>
      </section>
      <section v-if="app.can('manage')" class="panel">
        <h3>准备安全操作</h3>
        <p>
          填写强类型参数后查看真实预览，再在独立确认框批准。本页面不会执行任意
          Shell 命令。
        </p>
        <p v-if="device.observerOnly" class="status-note">
          当前为 observer-only 设备，只能扫描与调查，不允许修复。
        </p>
        <form @submit.prevent="prepare">
          <fieldset
            :disabled="
              busy || device.observerOnly || device.status === 'revoked'
            "
          >
            <label
              >操作类型<select v-model="action.type">
                <option
                  v-for="(name, value) in actionNames"
                  :key="value"
                  :value="value"
                >
                  {{ name }}
                </option>
              </select></label
            ><label v-if="action.type === 'package_security_upgrade'"
              >明确的软件包名<textarea
                v-model="action.packages"
                required
                rows="3"
                placeholder="每行一个包名；不会升级未列出的软件包"
              ></textarea></label
            ><label v-if="action.type === 'ssh_password_hardening'"
              >未确认时自动回滚（秒）<input
                v-model.number="action.rollbackAfterSeconds"
                type="number"
                min="30"
                max="600"
                required
            /></label>
            <div v-if="action.type === 'temporary_ip_ban'" class="ws-grid">
              <label
                >要封禁的公网 IP<input
                  v-model="action.address"
                  required /></label
              ><label
                >当前管理员出口 IP<input
                  v-model="action.currentAdminIp"
                  required
              /></label>
            </div>
            <div
              v-if="action.type === 'file_permission_repair'"
              class="ws-grid"
            >
              <label
                >允许范围内的文件路径<input
                  v-model="action.path"
                  required /></label
              ><label
                >八进制权限<input
                  v-model="action.mode"
                  pattern="(0o)?0?[0-7]{3}"
                  required /></label
              ><label
                >UID（可选，仅允许的目标）<input
                  v-model="action.uid"
                  type="number"
                  min="0" /></label
              ><label
                >GID（可选，仅允许的目标）<input
                  v-model="action.gid"
                  type="number"
                  min="0"
              /></label>
            </div>
            <div
              v-if="action.type === 'temporary_process_suspend'"
              class="ws-grid"
            >
              <label
                >目标 PID<input
                  v-model.number="action.pid"
                  type="number"
                  min="2"
                  required /></label
              ><label
                >精确进程启动标识<input
                  v-model="action.startTime"
                  inputmode="numeric"
                  required /></label
              ><label
                >完整可执行文件路径<input
                  v-model="action.executable"
                  required
                  placeholder="/tmp/... /var/tmp/... /dev/shm/..."
              /></label>
            </div>
            <template
              v-if="
                ['temporary_ip_ban', 'temporary_process_suspend'].includes(
                  action.type,
                )
              "
              ><label
                >自动恢复 TTL（秒）<input
                  v-model.number="action.ttlSeconds"
                  type="number"
                  min="30"
                  :max="action.type === 'temporary_ip_ban' ? 86400 : 900"
                  required /></label
              ><label
                >操作原因<input
                  v-model="action.reason"
                  maxlength="256" /></label></template
            ><button class="primary" :disabled="busy || device.observerOnly">
              生成预览，下一步审批
            </button>
          </fieldset>
        </form>
      </section></template
    >
  </div>
</template>
<style>
.ws-grid {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 16px;
  align-items: end;
  margin: 16px 0;
}
.ws-grant {
  border-top: 1px solid var(--cloud-border);
  padding: 14px 0;
}
.ws-grant h4 {
  margin: 4px 0 14px;
}
.ws-code {
  white-space: pre-wrap;
  word-break: break-word;
  background: #172239;
  color: #e5edff;
  border-radius: 9px;
  padding: 18px;
  font-size: 12px;
  line-height: 1.8;
}
.ws-secret {
  font-family: monospace;
  word-break: break-all;
}
.ws-app details {
  margin-top: 18px;
}
.ws-app textarea {
  width: 100%;
  box-sizing: border-box;
}
.ws-app .status-note {
  padding: 14px;
  border: 1px solid #e8d6ac;
  background: #fff9ed;
  border-radius: 9px;
}
@media (max-width: 900px) {
  .ws-grid {
    grid-template-columns: 1fr 1fr;
  }
}
@media (max-width: 600px) {
  .ws-grid {
    grid-template-columns: 1fr;
  }
}
</style>
