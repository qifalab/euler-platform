<script setup lang="ts">
import { onMounted, ref, reactive } from "vue";
import { useApp } from "../shared";
const props = defineProps<{ kind: "database" | "storage" }>();
const app = useApp();
type Quota = {
  mysqlMB: number;
  mysqlCount: number;
  postgresMB: number;
  postgresCount: number;
  storageBytes: number;
  buckets: number;
};
type Template = {
  id: string;
  name: string;
  description: string;
  quota: Quota;
  days: number;
  active: boolean;
};
type Grant = {
  id: string;
  name: string;
  quota: Quota;
  expiresAt: string;
  active: boolean;
};
type Code = {
  id: string;
  code: string;
  templateId: string;
  templateName: string;
  uses: number;
  maxUses: number;
  active: boolean;
  expiresAt: string;
  note: string;
};
const grants = ref<Grant[]>([]),
  templates = ref<Template[]>([]),
  codes = ref<Code[]>([]),
  error = ref(""),
  busy = ref(false),
  redeemCode = ref(""),
  editing = ref(false);
const emptyQuota = (): Quota => ({
  mysqlMB: 0,
  mysqlCount: 0,
  postgresMB: 0,
  postgresCount: 0,
  storageBytes: 0,
  buckets: 0,
});
const form = reactive<Template>({
  id: "",
  name: "",
  description: "",
  quota: emptyQuota(),
  days: 30,
  active: true,
});
const codeOffset = ref(0),
  codeTotal = ref(0),
  codeTemplate = ref(""),
  includeUsed = ref(true);
const codeForm = reactive({
  templateId: "",
  count: 1,
  maxUses: 1,
  expiresAt: "",
  note: "",
});
function units(q: Quota) {
  return props.kind === "database"
    ? `MySQL ${q.mysqlMB} MB / ${q.mysqlCount} 个 · PostgreSQL ${q.postgresMB} MB / ${q.postgresCount} 个`
    : `${(q.storageBytes / 1024 / 1024).toLocaleString()} MB · ${q.buckets} 个存储桶`;
}
async function loadCodes() {
  const data = await app.request<{ items: Code[]; total: number }>(
    `/admin/codes?limit=100&offset=${codeOffset.value}&templateId=${encodeURIComponent(codeTemplate.value)}&includeUsed=${includeUsed.value}`,
  );
  codes.value = data.items;
  codeTotal.value = data.total;
}
async function load() {
  error.value = "";
  try {
    grants.value = (await app.request<{ items: Grant[] }>("/packages")).items;
    if (app.can("admin")) {
      templates.value = (
        await app.request<{ items: Template[] }>("/admin/templates")
      ).items;
      await loadCodes();
    }
  } catch (e) {
    error.value = e instanceof Error ? e.message : "无法读取资源包";
  }
}
async function act(fn: () => Promise<unknown>, message: string) {
  if (busy.value) return;
  busy.value = true;
  error.value = "";
  try {
    await fn();
    app.notify(message);
    await load();
  } catch (e) {
    error.value = e instanceof Error ? e.message : "操作失败";
  } finally {
    busy.value = false;
  }
}
function edit(t?: Template) {
  Object.assign(
    form,
    t
      ? { ...t, quota: { ...t.quota } }
      : {
          id: "",
          name: "",
          description: "",
          quota: emptyQuota(),
          days: 30,
          active: true,
        },
  );
  editing.value = true;
}
async function save() {
  await act(async () => {
    await app.request(
      form.id ? `/admin/templates/${form.id}` : "/admin/templates",
      { method: form.id ? "PUT" : "POST", body: form },
    );
    editing.value = false;
  }, "资源包模板已保存");
}
async function generate() {
  codeOffset.value = 0;
  await act(
    () =>
      app.request("/admin/codes", {
        method: "POST",
        body: {
          ...codeForm,
          expiresAt: codeForm.expiresAt
            ? new Date(codeForm.expiresAt).toISOString()
            : "",
        },
      }),
    "兑换码已生成",
  );
}
function date(s: string) {
  return s ? new Date(s).toLocaleString() : "不限";
}
async function copy(value: string) {
  try {
    await navigator.clipboard.writeText(value);
    app.notify("已复制");
  } catch {
    app.notify("复制失败，请手动选择文本", "error");
  }
}
onMounted(load);
</script>
<template>
  <section class="packages">
    <p v-if="error" role="alert" class="error">{{ error }}</p>
    <header>
      <div>
        <h2>项目资源包</h2>
        <p>资源包增量归当前项目；过期后恢复基础配额。</p>
      </div>
      <button @click="load" :disabled="busy">刷新</button>
    </header>
    <form
      v-if="app.can('manage')"
      class="inline"
      @submit.prevent="
        act(async () => {
          await app.request('/packages/redeem', {
            method: 'POST',
            body: { code: redeemCode },
          });
          redeemCode = '';
        }, '兑换成功')
      "
    >
      <label
        >兑换码<input
          v-model="redeemCode"
          required
          maxlength="100"
          placeholder="输入资源包兑换码"
          autocomplete="off" /></label
      ><button :disabled="busy || !redeemCode.trim()">兑换到当前项目</button>
    </form>
    <div class="table">
      <table>
        <thead>
          <tr>
            <th>资源包</th>
            <th>配额增量</th>
            <th>有效期</th>
            <th>状态</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="g in grants" :key="g.id">
            <td>{{ g.name }}</td>
            <td>{{ units(g.quota) }}</td>
            <td>{{ date(g.expiresAt) }}</td>
            <td>{{ g.active ? "生效中" : "已过期" }}</td>
          </tr>
          <tr v-if="!grants.length">
            <td colspan="4">当前项目还没有资源包。</td>
          </tr>
        </tbody>
      </table>
    </div>
    <template v-if="app.can('admin')">
      <header>
        <div>
          <h2>资源包运营</h2>
          <p>仅产品运营授权人员可管理模板、兑换码和赠送。</p>
        </div>
        <button @click="edit()">新建模板</button>
      </header>
      <div class="table">
        <table>
          <thead>
            <tr>
              <th>模板</th>
              <th>增量</th>
              <th>天数</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="t in templates" :key="t.id">
              <td>
                {{ t.name }}<small>{{ t.description }}</small>
              </td>
              <td>{{ units(t.quota) }}</td>
              <td>{{ t.days }}</td>
              <td>{{ t.active ? "启用" : "停用" }}</td>
              <td class="actions">
                <button @click="edit(t)">编辑</button
                ><button
                  :disabled="busy"
                  @click="
                    act(
                      () =>
                        app.request(
                          `/admin/templates/${t.id}${t.active ? '' : '/enable'}`,
                          { method: t.active ? 'DELETE' : 'POST' },
                        ),
                      '状态已更新',
                    )
                  "
                >
                  {{ t.active ? "停用" : "启用" }}</button
                ><button
                  v-if="t.active"
                  :disabled="busy"
                  @click="
                    act(
                      () =>
                        app.request('/admin/grants', {
                          method: 'POST',
                          body: { templateId: t.id },
                        }),
                      '资源包已赠送到当前项目',
                    )
                  "
                >
                  赠送到本项目
                </button>
              </td>
            </tr>
            <tr v-if="!templates.length">
              <td colspan="5">先创建一个资源包模板。</td>
            </tr>
          </tbody>
        </table>
      </div>
      <form v-if="editing" class="editor" @submit.prevent="save">
        <h3>{{ form.id ? "编辑模板" : "新建模板" }}</h3>
        <div class="grid">
          <label
            >名称<input v-model="form.name" required maxlength="100" /></label
          ><label
            >有效天数<input
              v-model.number="form.days"
              type="number"
              min="1"
              max="36500"
              required /></label
          ><template v-if="kind === 'database'"
            ><label
              >MySQL 容量增量（MB）<input
                v-model.number="form.quota.mysqlMB"
                type="number"
                min="0"
                required /></label
            ><label
              >MySQL 库数量增量<input
                v-model.number="form.quota.mysqlCount"
                type="number"
                min="0"
                required /></label
            ><label
              >PostgreSQL 容量增量（MB）<input
                v-model.number="form.quota.postgresMB"
                type="number"
                min="0"
                required /></label
            ><label
              >PostgreSQL 库数量增量<input
                v-model.number="form.quota.postgresCount"
                type="number"
                min="0"
                required /></label></template
          ><template v-else
            ><label
              >容量增量（字节）<input
                v-model.number="form.quota.storageBytes"
                type="number"
                min="0"
                required /></label
            ><label
              >存储桶数量增量<input
                v-model.number="form.quota.buckets"
                type="number"
                min="0"
                required /></label></template
          ><label
            >说明<textarea v-model="form.description" maxlength="2000" />
          </label>
        </div>
        <div class="actions">
          <button :disabled="busy">保存模板</button
          ><button type="button" @click="editing = false">取消</button>
        </div>
      </form>
      <form class="editor" @submit.prevent="generate">
        <h3>批量生成兑换码</h3>
        <div class="grid">
          <label
            >模板<select v-model="codeForm.templateId" required>
              <option value="">选择启用的模板</option>
              <option
                v-for="t in templates.filter((v) => v.active)"
                :key="t.id"
                :value="t.id"
              >
                {{ t.name }}
              </option>
            </select></label
          ><label
            >生成数量<input
              v-model.number="codeForm.count"
              type="number"
              min="1"
              max="100"
              required /></label
          ><label
            >每码可用项目数<input
              v-model.number="codeForm.maxUses"
              type="number"
              min="1"
              max="100000"
              required /></label
          ><label
            >兑换截止时间（可选）<input
              v-model="codeForm.expiresAt"
              type="datetime-local" /></label
          ><label>备注<input v-model="codeForm.note" maxlength="1000" /></label>
        </div>
        <button :disabled="busy || !codeForm.templateId">生成兑换码</button>
      </form>
      <header>
        <h3>兑换码 · {{ codeTotal }} 个</h3>
        <div class="actions">
          <select
            v-model="codeTemplate"
            aria-label="按模板筛选兑换码"
            @change="
              codeOffset = 0;
              act(loadCodes, '筛选已更新');
            "
          >
            <option value="">全部模板</option>
            <option v-for="t in templates" :key="t.id" :value="t.id">
              {{ t.name }}
            </option></select
          ><label class="inline"
            ><input
              v-model="includeUsed"
              type="checkbox"
              @change="
                codeOffset = 0;
                act(loadCodes, '筛选已更新');
              "
            />含失效与已用尽</label
          >
        </div>
        <button
          :disabled="!codes.length"
          @click="copy(codes.map((c) => c.code).join('\n'))"
        >
          复制本页
        </button>
      </header>
      <div class="table">
        <table>
          <thead>
            <tr>
              <th>兑换码</th>
              <th>模板</th>
              <th>使用数</th>
              <th>有效期</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="c in codes" :key="c.id">
              <td>
                <code>{{ c.code }}</code
                ><small>{{ c.note }}</small>
              </td>
              <td>{{ c.templateName }}</td>
              <td>{{ c.uses }} / {{ c.maxUses }}</td>
              <td>
                {{ date(c.expiresAt)
                }}<small>{{ c.active ? "启用" : "已停用" }}</small>
              </td>
              <td class="actions">
                <button @click="copy(c.code)">复制</button
                ><button
                  v-if="c.active"
                  :disabled="busy"
                  @click="
                    act(
                      () =>
                        app.request(`/admin/codes/${c.id}`, {
                          method: 'DELETE',
                        }),
                      '兑换码已停用',
                    )
                  "
                >
                  停用
                </button>
              </td>
            </tr>
            <tr v-if="!codes.length">
              <td colspan="5">尚未生成兑换码。</td>
            </tr>
          </tbody>
        </table>
      </div>
      <div class="actions">
        <button
          :disabled="codeOffset === 0 || busy"
          @click="
            codeOffset = Math.max(0, codeOffset - 100);
            act(loadCodes, '已切换页面');
          "
        >
          上一页</button
        ><span
          >{{ codeOffset + 1 }}–{{
            Math.min(codeOffset + codes.length, codeTotal)
          }}
          / {{ codeTotal }}</span
        ><button
          :disabled="codeOffset + codes.length >= codeTotal || busy"
          @click="
            codeOffset += 100;
            act(loadCodes, '已切换页面');
          "
        >
          下一页
        </button>
      </div>
    </template>
  </section>
</template>
<style scoped>
.packages {
  display: grid;
  gap: 18px;
}
.packages header {
  display: flex;
  justify-content: space-between;
  gap: 16px;
  align-items: center;
}
.packages h2,
.packages h3,
.packages p {
  margin: 0;
}
.packages p,
small {
  color: var(--cloud-muted);
}
small {
  display: block;
}
.inline,
.actions {
  display: flex;
  gap: 8px;
  align-items: end;
  flex-wrap: wrap;
}
.grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(210px, 1fr));
  gap: 14px;
}
.editor {
  display: grid;
  gap: 14px;
  padding: 20px;
  border: 1px solid var(--cloud-border);
  border-radius: 14px;
  background: var(--cloud-panel-soft);
}
label {
  display: grid;
  gap: 6px;
  font-size: 13px;
}
input,
select,
textarea {
  padding: 9px;
  border: 1px solid var(--cloud-border);
  border-radius: 8px;
  background: var(--cloud-panel);
  color: var(--cloud-text);
  min-width: 0;
}
button {
  padding: 8px 12px;
  border-radius: 8px;
  border: 1px solid var(--cloud-border);
  background: var(--cloud-panel);
  color: var(--cloud-text);
}
button:hover {
  background: var(--cloud-hover);
}
.table {
  overflow: auto;
  border: 1px solid var(--cloud-border);
  border-radius: 12px;
}
table {
  width: 100%;
  border-collapse: collapse;
}
th,
td {
  padding: 12px;
  text-align: left;
  border-bottom: 1px solid var(--cloud-border);
  vertical-align: top;
}
th {
  font-size: 12px;
  color: var(--cloud-muted);
  background: var(--cloud-panel-soft);
}
.error {
  color: var(--cloud-danger);
  padding: 12px;
  background: var(--cloud-panel-soft);
}
</style>
