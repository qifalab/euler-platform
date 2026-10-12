<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from "vue";
import { useApp } from "../shared";
const props = defineProps<{ siteId: string }>();
const app = useApp();
type Group = { name: string; count: number; uniqueVisitors: number };
type Point = {
  date: string;
  pageViews: number;
  uniqueVisitors: number;
  events: number;
};
type Analysis = {
  period: {
    from: string;
    to: string;
    timezone: string;
    startInclusive: string;
    endExclusive: string;
  };
  trend: Point[];
  pages: Group[];
  sources: Group[];
  devices: Group[];
  browsers: Group[];
  events: Group[];
  pageViews: number;
  uniqueVisitors: number;
  eventCount: number;
};
type Settings = { retentionDays: number; eventNames: string[] };
type Funnel = {
  steps: { name: string; visitors: number; conversion: number }[];
  definition: string;
};
const localDate = (date: Date) =>
  `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}-${String(date.getDate()).padStart(2, "0")}`;
const today = new Date();
const before = new Date();
before.setDate(today.getDate() - 29);
const filter = reactive({
  from: localDate(before),
  to: localDate(today),
  timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC",
});
const data = ref<Analysis | null>(null),
  settings = ref<Settings>({ retentionDays: 365, eventNames: [] });
const eventText = ref(""),
  retention = ref(365),
  error = ref(""),
  loading = ref(false),
  saving = ref(false),
  downloaded = ref(false);
const funnelForm = reactive({ steps: ["", ""], windowMinutes: 60 });
const funnel = ref<Funnel | null>(null),
  funnelBusy = ref(false);
const query = () => new URLSearchParams(filter).toString();
const base = () => `/sites/${props.siteId}`;
const number = (value?: number) =>
  new Intl.NumberFormat("zh-CN").format(value ?? 0);
const allNames = computed(() => [
  ...new Set([
    ...settings.value.eventNames,
    ...(data.value?.events.map((e) => e.name) ?? []),
  ]),
]);
const groupPanels = computed(() => [
  { title: "访问来源", values: data.value?.sources ?? [] },
  { title: "设备类型", values: data.value?.devices ?? [] },
  { title: "浏览器", values: data.value?.browsers ?? [] },
]);
const maximum = computed(() =>
  Math.max(
    1,
    ...(data.value?.trend.map((p) =>
      Math.max(p.pageViews, p.uniqueVisitors),
    ) ?? [1]),
  ),
);
function points(key: "pageViews" | "uniqueVisitors") {
  const rows = data.value?.trend ?? [];
  return rows
    .map(
      (p, i) =>
        `${30 + (i * 840) / Math.max(1, rows.length - 1)},${210 - (p[key] / maximum.value) * 175}`,
    )
    .join(" ");
}
function fail(e: unknown) {
  error.value = e instanceof Error ? e.message : "请求未完成，请稍后重试";
}
async function refresh() {
  error.value = "";
  loading.value = true;
  funnel.value = null;
  downloaded.value = false;
  const params = query();
  try {
    const a = await app.request<Analysis>(`${base()}/analytics?${params}`);
    if (params === query()) {
      data.value = a;
    }
  } catch (e) {
    data.value = null;
    fail(e);
  } finally {
    loading.value = false;
  }
}
async function loadSettings() {
  try {
    settings.value = await app.request<Settings>(`${base()}/settings`);
    retention.value = settings.value.retentionDays;
    eventText.value = settings.value.eventNames.join("\n");
    funnelForm.steps = [
      settings.value.eventNames[0] ?? "",
      settings.value.eventNames[1] ?? "",
    ];
  } catch (e) {
    fail(e);
  }
}
async function saveSettings() {
  if (
    retention.value < settings.value.retentionDays &&
    !window.confirm(
      `将留存缩短到 ${retention.value} 天，超期访问及事件数据会被永久清理。继续保存？`,
    )
  )
    return;
  saving.value = true;
  error.value = "";
  try {
    settings.value = await app.request<Settings>(`${base()}/settings`, {
      method: "PUT",
      body: {
        retentionDays: retention.value,
        eventNames: eventText.value
          .split(/[\s,，]+/)
          .map((n) => n.trim())
          .filter(Boolean),
      },
    });
    app.notify("事件和留存设置已保存", "success");
    await refresh();
  } catch (e) {
    fail(e);
  } finally {
    saving.value = false;
  }
}
async function clean() {
  saving.value = true;
  try {
    const result = await app.request<{ deleted: number }>(`${base()}/cleanup`, {
      method: "POST",
    });
    app.notify(`已清理 ${number(result.deleted)} 条过期记录`, "success");
    await refresh();
  } catch (e) {
    fail(e);
  } finally {
    saving.value = false;
  }
}
async function calculate() {
  error.value = "";
  funnelBusy.value = true;
  funnel.value = null;
  const params = query(),
    body = JSON.stringify(funnelForm);
  try {
    const result = await app.request<Funnel>(`${base()}/funnel?${params}`, {
      method: "POST",
      body: JSON.parse(body),
    });
    if (params === query() && body === JSON.stringify(funnelForm))
      funnel.value = result;
  } catch (e) {
    fail(e);
  } finally {
    funnelBusy.value = false;
  }
}
async function download() {
  try {
    await app.download(
      `${base()}/export.csv?${query()}`,
      `statistics-${filter.from}-${filter.to}.csv`,
    );
    downloaded.value = true;
  } catch (e) {
    fail(e);
  }
}
watch(filter, () => {
  data.value = null;
  funnel.value = null;
  downloaded.value = false;
});
watch(funnelForm, () => {
  funnel.value = null;
});
onMounted(async () => {
  await loadSettings();
  await refresh();
});
</script>

<template>
  <section class="analytics" aria-label="日期分析与事件转化">
    <p v-if="error" class="status-note error" role="alert">{{ error }}</p>
    <form class="panel filters" @submit.prevent="refresh">
      <label class="field"
        >开始日期<input v-model="filter.from" type="date" required
      /></label>
      <label class="field"
        >结束日期<input
          v-model="filter.to"
          type="date"
          :min="filter.from"
          required
      /></label>
      <label class="field"
        >统计时区<input
          v-model="filter.timezone"
          list="stats-timezones"
          required
          placeholder="Asia/Shanghai"
      /></label>
      <datalist id="stats-timezones">
        <option value="Asia/Shanghai" />
        <option value="UTC" />
        <option value="America/New_York" />
        <option value="Europe/London" />
      </datalist>
      <button class="button primary" :disabled="loading">
        {{ loading ? "查询中…" : "查询分析" }}
      </button>
      <button
        type="button"
        class="button"
        :disabled="!data || loading"
        @click="download"
      >
        导出 CSV
      </button>
      <p class="muted full">
        按所选时区的自然日计算，包含开始日与结束日。UV
        在整个区间内去重，不能把每日 UV 相加；单次分析最多 20 万条记录。
      </p>
      <p v-if="downloaded" class="full" role="status">CSV 下载已开始。</p>
    </form>
    <template v-if="data">
      <div class="summary">
        <article class="panel">
          <span>区间 PV</span><strong>{{ number(data.pageViews) }}</strong>
        </article>
        <article class="panel">
          <span>区间 UV</span><strong>{{ number(data.uniqueVisitors) }}</strong>
        </article>
        <article class="panel">
          <span>自定义事件</span><strong>{{ number(data.eventCount) }}</strong>
        </article>
      </div>
      <article class="panel">
        <div class="toolbar">
          <h3>每日访问趋势</h3>
          <p class="muted">
            <span class="pv-key">● PV</span> ·
            <span class="uv-key">● UV</span> · {{ data.period.timezone }}
          </p>
        </div>
        <p v-if="!data.pageViews" class="empty">当前日期内没有访问记录。</p>
        <svg
          v-else
          class="trend"
          viewBox="0 0 900 250"
          role="img"
          aria-label="每日 PV 与 UV 趋势，下方提供完整数据表"
        >
          <title>每日 PV 与 UV 趋势</title>
          <line
            x1="30"
            y1="210"
            x2="870"
            y2="210"
            stroke="currentColor"
            opacity=".2"
          />
          <line
            x1="30"
            y1="35"
            x2="870"
            y2="35"
            stroke="currentColor"
            opacity=".1"
          />
          <text x="30" y="22">{{ number(maximum) }}</text>
          <polyline
            :points="points('pageViews')"
            fill="none"
            stroke="#8064da"
            stroke-width="3"
          />
          <polyline
            :points="points('uniqueVisitors')"
            fill="none"
            stroke="#168e85"
            stroke-width="3"
          />
          <text x="30" y="240">{{ data.period.from }}</text>
          <text x="870" y="240" text-anchor="end">{{ data.period.to }}</text>
        </svg>
        <details>
          <summary>查看每日数据</summary>
          <div class="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>日期</th>
                  <th>PV</th>
                  <th>UV</th>
                  <th>事件数</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="point in data.trend" :key="point.date">
                  <td>{{ point.date }}</td>
                  <td>{{ number(point.pageViews) }}</td>
                  <td>{{ number(point.uniqueVisitors) }}</td>
                  <td>{{ number(point.events) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </details>
        <p class="muted">
          有效数据区间：{{ data.period.startInclusive }} ≤ 时间 &lt;
          {{ data.period.endExclusive }}。留存范围以前的数据不参与统计。
        </p>
      </article>
      <div class="groups">
        <article v-for="group in groupPanels" :key="group.title" class="panel">
          <h3>{{ group.title }}</h3>
          <p v-if="!group.values.length" class="muted">当前区间暂无数据</p>
          <div
            v-for="row in group.values.slice(0, 10)"
            :key="row.name"
            class="bar-row"
          >
            <div>
              <span>{{ row.name }}</span
              ><strong>{{ number(row.count) }}</strong>
            </div>
            <progress
              :value="row.count"
              :max="Math.max(1, group.values[0]?.count ?? 1)"
              :aria-label="`${row.name}：${row.count} 次访问`"
            /><small>UV {{ number(row.uniqueVisitors) }}</small>
          </div>
        </article>
      </div>
      <p class="muted">
        来源展示 Referrer 的域名；设备和浏览器按 User-Agent
        粗分类，可被客户端伪造。页面显示前 10 个来源，CSV 包含全部聚合项。
      </p>
      <article class="panel">
        <h3>区间页面排行 · 前 20</h3>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>页面</th>
                <th>PV</th>
                <th>UV</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="row in data.pages.slice(0, 20)" :key="row.name">
                <td class="break">{{ row.name }}</td>
                <td>{{ number(row.count) }}</td>
                <td>{{ number(row.uniqueVisitors) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-if="!data.pages.slice(0, 20).length" class="muted">
          当前区间暂无页面访问
        </p>
      </article>
      <article class="panel">
        <h3>自定义事件</h3>
        <p class="muted">
          仅接受管理员登记的事件名，不接受任意属性、邮箱、身份信息或客户端时间。事件与访问共享采集限额。
        </p>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>事件</th>
                <th>次数</th>
                <th>独立访客</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="row in data.events" :key="row.name">
                <td>{{ row.name }}</td>
                <td>{{ number(row.count) }}</td>
                <td>{{ number(row.uniqueVisitors) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-if="!data.events.length" class="muted">
          尚无事件。登记事件名后，在已安装采集脚本的网站中调用：
        </p>
        <pre><code>await window.eulerTrackEvent('{{ settings.eventNames[0] ?? "signup_started" }}');</code></pre>
      </article>
    </template>
    <article class="panel">
      <h3>顺序转化漏斗</h3>
      <p class="muted">
        选择 2–8
        个事件。同一访客须按顺序完成；所有步骤都在当前日期区间内，首步到末步须在转化窗口内。每位访客每步只计一次。
      </p>
      <form @submit.prevent="calculate">
        <div class="funnel-fields">
          <label v-for="(_, i) in funnelForm.steps" :key="i" class="field"
            >第 {{ i + 1 }} 步<select
              v-model="funnelForm.steps[i]"
              :aria-label="`第 ${i + 1} 步`"
              required
            >
              <option disabled value="">选择事件</option>
              <option v-for="name in allNames" :key="name" :value="name">
                {{ name }}
              </option>
            </select></label
          ><label class="field"
            >转化窗口（分钟）<input
              v-model.number="funnelForm.windowMinutes"
              type="number"
              min="1"
              max="10080"
              required
          /></label>
        </div>
        <div class="actions">
          <button
            type="button"
            class="button"
            :disabled="funnelForm.steps.length >= 8"
            @click="funnelForm.steps.push('')"
          >
            添加步骤</button
          ><button
            type="button"
            class="button"
            :disabled="funnelForm.steps.length <= 2"
            @click="funnelForm.steps.pop()"
          >
            移除末步</button
          ><button
            class="button primary"
            :disabled="funnelBusy || !allNames.length || !data"
          >
            {{ funnelBusy ? "计算中…" : "计算转化" }}
          </button>
        </div>
      </form>
      <div v-if="funnel" class="funnel-result" aria-live="polite">
        <div v-for="(step, i) in funnel.steps" :key="i" class="bar-row">
          <div>
            <span>{{ i + 1 }} · {{ step.name }}</span
            ><strong
              >{{ number(step.visitors) }} 人 ·
              {{ (step.conversion * 100).toFixed(1) }}%</strong
            >
          </div>
          <progress
            :value="step.conversion"
            max="1"
            :aria-label="`${step.name} 转化率 ${(step.conversion * 100).toFixed(1)}%`"
          />
        </div>
        <p class="muted">百分比为相对首步的转化率。{{ funnel.definition }}</p>
      </div>
    </article>
    <form v-if="app.can('manage')" class="panel" @submit.prevent="saveSettings">
      <h3>事件登记与数据留存</h3>
      <div class="form-grid">
        <label class="field"
          >允许的事件名<textarea
            v-model="eventText"
            aria-label="允许的事件名"
            rows="5"
            placeholder="signup_started&#10;signup_completed"
          /><small
            >每行一个，最多 32 个。仅小写字母、数字、下划线，以字母开头，最长 40
            字符。请用稳定的业务动作名称。</small
          ></label
        ><label class="field"
          >保留天数<input
            v-model.number="retention"
            aria-label="保留天数"
            type="number"
            min="7"
            max="730"
            required
          /><small
            >7–730 天，默认 365
            天。缩短后过期明细会永久删除；后台维护与采集、报表请求会分批清理过期数据；也可手动清理。</small
          ></label
        >
      </div>
      <div class="actions">
        <button class="button primary" :disabled="saving">保存并应用留存</button
        ><button type="button" class="button" :disabled="saving" @click="clean">
          清理过期数据
        </button>
      </div>
      <p class="muted">
        每站点每天（UTC）最多采集 100000 条，每来源 IP 每分钟最多 120
        条，访问和事件合计。
      </p>
    </form>
  </section>
</template>

<style scoped>
.analytics {
  display: grid;
  gap: 20px;
}
.panel {
  padding: 24px;
}
h3 {
  margin: 0 0 12px;
}
.filters {
  display: flex;
  gap: 14px;
  align-items: end;
  flex-wrap: wrap;
}
.filters .field {
  min-width: 160px;
}
.full {
  width: 100%;
  margin: 0;
}
.summary,
.groups {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 18px;
}
.summary .panel {
  display: grid;
  gap: 12px;
}
.summary span,
.muted,
small {
  color: var(--muted);
  font-size: 13px;
  line-height: 1.7;
}
.summary strong {
  font-size: 32px;
  color: #8064da;
}
.summary article:nth-child(2) strong {
  color: #168e85;
}
.summary article:nth-child(3) strong {
  color: #b17b22;
}
.trend {
  width: 100%;
  max-height: 300px;
}
.trend text {
  fill: currentColor;
  font-size: 12px;
}
.pv-key {
  color: #8064da;
}
.uv-key {
  color: #168e85;
}
.bar-row {
  margin: 18px 0;
  overflow-wrap: anywhere;
}
.bar-row > div {
  display: flex;
  justify-content: space-between;
  gap: 12px;
  font-size: 13px;
}
.bar-row small {
  font-size: 11px;
}
progress {
  display: block;
  width: 100%;
  height: 8px;
  margin-top: 7px;
  accent-color: #8064da;
}
.groups article:nth-child(2) progress {
  accent-color: #168e85;
}
.groups article:nth-child(3) progress {
  accent-color: #b17b22;
}
.break {
  overflow-wrap: anywhere;
  max-width: 650px;
}
.funnel-fields {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
  gap: 16px;
}
.actions {
  display: flex;
  gap: 10px;
  margin-top: 18px;
  flex-wrap: wrap;
}
.funnel-result {
  margin-top: 28px;
}
summary {
  cursor: pointer;
  font-size: 13px;
}
pre {
  overflow: auto;
  padding: 14px;
  background: var(--surface-alt, #f4f5fa);
  border-radius: 8px;
}
.error {
  color: #b52c4b;
}
select {
  font: inherit;
  padding: 8px;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--surface, #fff);
  color: inherit;
}
@media (max-width: 850px) {
  .summary,
  .groups {
    grid-template-columns: 1fr;
  }
  .filters .field {
    width: 100%;
  }
  .panel {
    padding: 18px;
  }
}
</style>
