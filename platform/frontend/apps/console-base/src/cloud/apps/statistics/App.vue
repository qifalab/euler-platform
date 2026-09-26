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

type Site = {
  id: string;
  name: string;
  domains: string[];
  publicId: string;
  enabled: boolean;
  createdAt: string;
};
type Report = {
  pageViews: number;
  uniqueVisitors: number;
  topPages: {
    url: string;
    count: number;
    uniqueVisitors: number;
    lastVisit: string;
  }[];
  pagination: {
    currentPage: number;
    pageSize: number;
    total: number;
    totalPages: number;
  };
  refreshedAt: string;
};
type Integration = {
  trackerURL: string;
  widgetURL: string;
  helperURL: string;
  collectURL: string;
  pageViewsURL: string;
  note: string;
};
const app = useApp();
const sites = ref<Site[]>([]),
  selectedID = ref(""),
  tab = ref("overview"),
  report = ref<Report | null>(null),
  integration = ref<Integration | null>(null);
const loading = ref(false),
  saving = ref(false),
  error = ref(""),
  editor = ref(false),
  editingID = ref(""),
  page = ref(1),
  pageSize = ref(20),
  autoRefresh = ref(true);
const form = reactive({ name: "", domains: "", enabled: true });
const selected = computed(() =>
  sites.value.find((s) => s.id === selectedID.value),
);
const number = (n?: number) => new Intl.NumberFormat("zh-CN").format(n ?? 0);
const date = (s?: string) =>
  s ? new Date(s).toLocaleString("zh-CN") : "尚未采集";
const script = (url?: string) =>
  url ? `<script defer src="${url}"><` + "/script>" : "";
function explain(e: unknown) {
  error.value = e instanceof Error ? e.message : "请求失败，请稍后重试";
}
async function loadSites() {
  loading.value = true;
  error.value = "";
  try {
    sites.value = (await app.request<{ sites: Site[] }>("/sites")).sites;
    if (!sites.value.some((s) => s.id === selectedID.value))
      selectedID.value = sites.value[0]?.id ?? "";
  } catch (e) {
    explain(e);
  } finally {
    loading.value = false;
  }
}
async function loadReport() {
  const id = selectedID.value;
  if (!id) return;
  try {
    const value = await app.request<Report>(
      `/sites/${id}/report?page=${page.value}&pageSize=${pageSize.value}`,
    );
    if (id === selectedID.value) {
      report.value = value;
      page.value = value.pagination.currentPage;
      error.value = "";
    }
  } catch (e) {
    explain(e);
  }
}
async function loadIntegration() {
  const id = selectedID.value;
  if (!id) return;
  try {
    const value = await app.request<Integration>(`/sites/${id}/integration`);
    if (id === selectedID.value) integration.value = value;
  } catch (e) {
    explain(e);
  }
}
watch(selectedID, () => {
  page.value = 1;
  report.value = null;
  integration.value = null;
  void loadReport();
  void loadIntegration();
});
watch(pageSize, () => {
  page.value = 1;
  void loadReport();
});
function openEditor(site?: Site) {
  editingID.value = site?.id ?? "";
  form.name = site?.name ?? "";
  form.domains = site?.domains.join("\n") ?? "";
  form.enabled = site?.enabled ?? true;
  editor.value = true;
}
async function save() {
  saving.value = true;
  error.value = "";
  try {
    const value = await app.request<Site>(
      editingID.value ? `/sites/${editingID.value}` : "/sites",
      {
        method: editingID.value ? "PATCH" : "POST",
        body: {
          name: form.name.trim(),
          domains: form.domains
            .split(/[\n,，]/)
            .map((d) => d.trim())
            .filter(Boolean),
          enabled: form.enabled,
        },
      },
    );
    editor.value = false;
    await loadSites();
    selectedID.value = value.id;
    app.notify("站点设置已保存", "success");
  } catch (e) {
    explain(e);
  } finally {
    saving.value = false;
  }
}
async function copy(value: string) {
  try {
    await navigator.clipboard.writeText(value);
    app.notify("已复制", "success");
  } catch {
    app.notify("无法自动复制，请选中代码复制", "error");
  }
}
function turn(next: number) {
  page.value = next;
  void loadReport();
}
let timer: ReturnType<typeof setInterval> | undefined;
onMounted(() => {
  void loadSites();
  timer = setInterval(() => {
    if (autoRefresh.value && !document.hidden) void loadReport();
  }, 60000);
});
onBeforeUnmount(() => clearInterval(timer));
</script>

<template>
  <section class="native-app statistics-app" aria-label="访问统计">
    <div v-if="error" class="status-note error" role="alert">
      {{ error }}
      <button
        class="button"
        @click="
          loadSites();
          loadReport();
        "
      >
        重试
      </button>
    </div>
    <div class="toolbar">
      <div>
        <h2>让每一次访问都有迹可循</h2>
        <p class="muted">为当前项目管理站点，查看页面访问量与独立访客。</p>
      </div>
      <button
        v-if="app.can('manage')"
        class="button primary"
        @click="openEditor()"
      >
        ＋ 添加站点
      </button>
    </div>
    <form v-if="editor" class="panel site-editor" @submit.prevent="save">
      <div class="toolbar">
        <h3>{{ editingID ? "站点设置" : "添加站点" }}</h3>
        <button type="button" class="button" @click="editor = false">
          取消
        </button>
      </div>
      <div class="form-grid">
        <label class="field"
          >站点名称<input
            v-model="form.name"
            required
            maxlength="100"
            placeholder="例如：开发者文档"
        /></label>
        <label class="field"
          >采集状态<select v-model="form.enabled" aria-label="采集状态">
            <option :value="true">启用采集</option>
            <option :value="false">暂停采集</option>
          </select></label
        >
        <label class="field full"
          >允许采集的域名<textarea
            v-model="form.domains"
            required
            placeholder="docs.example.com&#10;www.example.com"
          /><small
            >每行一个精确域名，可含端口。不填协议、路径或通配符。</small
          ></label
        >
      </div>
      <button class="button primary" :disabled="saving">
        {{ saving ? "保存中…" : "保存站点" }}
      </button>
    </form>
    <div v-if="loading && !sites.length" class="panel empty" role="status">
      正在加载站点…
    </div>
    <div v-else-if="!sites.length" class="panel empty">
      <span class="empty-icon">◉</span>
      <h3>从第一个站点开始</h3>
      <p>添加域名并安装采集脚本，即可查看真实访问数据。</p>
      <button
        v-if="app.can('manage')"
        class="button primary"
        @click="openEditor()"
      >
        添加站点
      </button>
      <p v-else>请项目管理员添加站点。</p>
    </div>
    <template v-else>
      <div class="panel toolbar site-switcher">
        <label class="field"
          >当前站点<select v-model="selectedID">
            <option v-for="site in sites" :key="site.id" :value="site.id">
              {{ site.name }}
            </option>
          </select></label
        ><span class="badge" :class="{ paused: !selected?.enabled }">{{
          selected?.enabled ? "正在采集" : "已暂停"
        }}</span
        ><span class="muted domains">{{ selected?.domains.join(" · ") }}</span
        ><button
          v-if="app.can('manage')"
          class="button"
          @click="openEditor(selected)"
        >
          站点设置
        </button>
      </div>
      <nav class="app-tabs" aria-label="统计页面">
        <button
          :class="{ active: tab === 'overview' }"
          @click="tab = 'overview'"
        >
          访问概览</button
        ><button
          :class="{ active: tab === 'integration' }"
          @click="tab = 'integration'"
        >
          接入与计数器
        </button>
      </nav>
      <template v-if="tab === 'overview'">
        <div class="metric-grid">
          <article class="metric violet">
            <span>累计访问量 · PV</span
            ><strong>{{ number(report?.pageViews) }}</strong
            ><small>每次页面浏览计为一次访问</small>
          </article>
          <article class="metric teal">
            <span>独立访客 · UV</span
            ><strong>{{ number(report?.uniqueVisitors) }}</strong
            ><small>按当前站点的访客标识去重</small>
          </article>
          <article class="metric amber">
            <span>覆盖页面</span
            ><strong>{{ number(report?.pagination.total) }}</strong
            ><small>忽略查询参数和页面片段</small>
          </article>
        </div>
        <div class="panel">
          <div class="toolbar">
            <div>
              <h3>页面排行</h3>
              <p class="muted">最近更新：{{ date(report?.refreshedAt) }}</p>
            </div>
            <div class="actions">
              <label
                ><input v-model="autoRefresh" type="checkbox" /> 每 60
                秒刷新</label
              ><button class="button" @click="loadReport">立即刷新</button
              ><select v-model.number="pageSize" aria-label="每页条数">
                <option
                  v-for="size in [10, 20, 50, 100]"
                  :key="size"
                  :value="size"
                >
                  每页 {{ size }} 条
                </option>
              </select>
            </div>
          </div>
          <div v-if="report && !report.topPages.length" class="empty">
            <h3>还没有访问记录</h3>
            <p>请确认站点已启用，并在允许的域名安装采集脚本。</p>
            <button class="button" @click="tab = 'integration'">
              查看接入步骤
            </button>
          </div>
          <div v-else class="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>#</th>
                  <th>页面 URL</th>
                  <th>PV</th>
                  <th>UV</th>
                  <th>最近访问</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="(row, index) in report?.topPages" :key="row.url">
                  <td>{{ (page - 1) * pageSize + index + 1 }}</td>
                  <td class="url-cell" :title="row.url">{{ row.url }}</td>
                  <td>{{ number(row.count) }}</td>
                  <td>{{ number(row.uniqueVisitors) }}</td>
                  <td class="date-cell">{{ date(row.lastVisit) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <div class="pagination">
            <button
              class="button"
              :disabled="page <= 1"
              @click="turn(page - 1)"
            >
              上一页</button
            ><span
              >第 {{ page }} /
              {{ Math.max(1, report?.pagination.totalPages ?? 1) }} 页</span
            ><button
              class="button"
              :disabled="page >= (report?.pagination.totalPages ?? 1)"
              @click="turn(page + 1)"
            >
              下一页
            </button>
          </div>
        </div>
      </template>
      <div v-else-if="integration" class="integration-grid">
        <article class="panel">
          <div class="step">01 · 页面采集</div>
          <h3>安装轻量采集脚本</h3>
          <p>
            将下面代码放入网站页面。访客标识仅用于当前站点，页面查询参数不会上报。
          </p>
          <pre><code>{{ script(integration.trackerURL) }}</code></pre>
          <button class="button" @click="copy(script(integration.trackerURL))">
            复制采集代码
          </button>
          <p class="muted">
            单页应用切换路由后可调用
            <code>window.eulerTrackPage()</code>，每次调用计一次 PV。
          </p>
        </article>
        <article class="panel">
          <div class="step teal-text">02 · 页面计数器</div>
          <h3>显示当前页面浏览量</h3>
          <p>
            右下角挂件每 60 秒更新。也可以自行放置
            <code>id="page-view-counter"</code> 元素。
          </p>
          <pre><code>{{ script(integration.widgetURL) }}</code></pre>
          <button class="button" @click="copy(script(integration.widgetURL))">
            复制计数器代码
          </button>
        </article>
        <article class="panel full">
          <div class="step amber-text">03 · 自定义展示</div>
          <h3>把浏览量融入自己的页面</h3>
          <pre><code>{{ script(integration.helperURL) }}
const views = await window.getPageViews();
// 也支持 window.getPageViews(views =&gt; { ... });</code></pre>
          <button
            class="button"
            @click="
              copy(
                script(integration.helperURL) +
                  '\nconst views = await window.getPageViews();',
              )
            "
          >
            复制轻量查询代码
          </button>
          <p class="muted">
            公开计数接口：<code
              >{{ integration.pageViewsURL }}?url=页面完整地址</code
            >
          </p>
          <p class="status-note">
            {{ integration.note }} 来源域名校验用于限制浏览器接入，公开采集 ID
            不具有报表或设置权限。
          </p>
        </article>
      </div>
    </template>
  </section>
</template>

<style scoped>
h2,
h3 {
  margin: 0 0 8px;
}
.muted {
  color: var(--muted);
  font-size: 13px;
  line-height: 1.7;
}
.statistics-app .panel {
  padding: 24px;
}
.site-editor {
  border-top: 3px solid #7c64ed;
}
.site-editor .button.primary {
  margin-top: 20px;
}
.site-switcher {
  gap: 18px;
}
.site-switcher .field {
  min-width: 220px;
}
.domains {
  flex: 1;
}
.metric-grid {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 18px;
}
.metric {
  border: 1px solid var(--border);
  border-radius: 14px;
  padding: 24px;
  display: grid;
  gap: 12px;
  background: var(--surface, #fff);
  position: relative;
  overflow: hidden;
}
.metric:after {
  content: "";
  width: 100px;
  height: 100px;
  position: absolute;
  right: -24px;
  top: -25px;
  border-radius: 50%;
  background: currentColor;
  opacity: 0.065;
}
.metric span {
  font-size: 13px;
  color: var(--muted);
}
.metric strong {
  font-size: 38px;
  font-weight: 650;
  letter-spacing: -1.5px;
}
.metric small {
  font-size: 12px;
  color: var(--muted);
}
.violet {
  color: #7854d8;
}
.teal {
  color: #159188;
}
.amber {
  color: #bf8525;
}
.badge.paused {
  background: #fff3d6;
  color: #9f731c;
}
.url-cell {
  max-width: 460px;
  overflow-wrap: anywhere;
}
.date-cell {
  white-space: nowrap;
  font-size: 12px;
}
.integration-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 20px;
}
.integration-grid .full {
  grid-column: 1/-1;
}
.integration-grid p {
  font-size: 13px;
  line-height: 1.9;
}
.step {
  color: #7854d8;
  letter-spacing: 1px;
  font-size: 12px;
  font-weight: 700;
  margin-bottom: 16px;
}
.teal-text {
  color: #159188;
}
.amber-text {
  color: #bf8525;
}
.empty-icon {
  font-size: 45px;
  color: #8266dc;
}
.error {
  color: #b52c4b;
  background: #fff1f3;
}
.field small {
  font-weight: 400;
  color: var(--muted);
  line-height: 1.7;
}
select {
  font: inherit;
  padding: 8px 12px;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--surface, #fff);
  color: inherit;
}
.actions label {
  font-size: 12px;
}
@media (max-width: 800px) {
  .metric-grid {
    grid-template-columns: 1fr;
  }
  .integration-grid {
    grid-template-columns: 1fr;
  }
  .site-switcher .field {
    width: 100%;
  }
  .metric strong {
    font-size: 32px;
  }
  .statistics-app .panel {
    padding: 18px;
  }
}
@media (prefers-color-scheme: dark) {
  .error {
    background: #4a2030;
  }
  .badge.paused {
    background: #49371a;
  }
}
</style>
