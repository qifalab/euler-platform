<script setup lang="ts">
/**
 * Alert-rule create / edit form (监控规则) — /rules/create, /rules/:id/edit.
 *
 *   POST /api/v1/monitor/rules       — create (an upsert on productCode +
 *                                      resourceType + metric)
 *   PUT  /api/v1/monitor/rules/{id}  — edit threshold / operator / periods /
 *                                      channels, guarded by the row version
 *                                      (409 Monitor.VersionConflict)
 *   GET  /api/v1/monitor/rules       — the rule being edited (no by-id read)
 *   GET  /api/v1/catalog/products    — product → resource type
 *   GET  /api/v1/monitor/templates   — preset metrics per product
 *
 * Client checks mirror svc-monitor (ruleForm.ts); the server stays
 * authoritative and its 400 message is shown as-is.
 */
import { computed, onMounted, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { ElButton, ElCheckbox, ElCheckboxGroup, ElForm, ElFormItem, ElInput, ElInputNumber, ElMessage, ElOption, ElSelect } from "element-plus";
import { PageHeader } from "@eu/ui";
import { sdk } from "../sdk";
import MetricsPanel from "../components/MetricsPanel.vue";
import { isSupportedMetric } from "../metrics";
import {
  COMPARISON_OPERATORS,
  EVAL_PERIODS_MAX,
  MAX_METRIC,
  MAX_RESOURCE_TYPE,
  NOTIFICATION_CHANNELS,
  PERIOD_MAX,
  buildCreateRuleBody,
  buildUpdateRuleBody,
  emptyRuleForm,
  findSameKeyRule,
  ruleErrorMessage,
  ruleToForm,
  validateRuleForm,
  type MonitorRule,
  type RuleFormValues,
} from "../ruleForm";

interface CatalogProduct { productCode: string; productName: string; resourceType: string }
interface RuleTemplate { productCode: string; metric: string; threshold?: string; period?: number }

const route = useRoute();
const router = useRouter();
const editId = computed(() => (route.params.id ? Number(route.params.id) : null));
const isEdit = computed(() => editId.value !== null);

const form = ref<RuleFormValues>(emptyRuleForm());
const editing = ref<MonitorRule | null>(null);
const rules = ref<MonitorRule[]>([]);
const products = ref<CatalogProduct[]>([]);
const templates = ref<RuleTemplate[]>([]);
const loading = ref(true);
const loadError = ref("");
const saving = ref(false);
const formError = ref("");

/** Metric suggestions for the product: presets + the account's own rules. */
const metricOptions = computed(() => {
  const p = form.value.productCode;
  const set = new Set<string>();
  for (const t of templates.value) if (t.productCode === p && t.metric) set.add(t.metric);
  for (const r of rules.value) if (r.productCode === p && r.metric) set.add(r.metric);
  return [...set];
});

/** On create, the existing rule with the same key (the POST re-parameters it). */
const sameKeyRule = computed(() => (isEdit.value ? undefined : findSameKeyRule(rules.value, form.value)));

// Picking a product fills its resource type from the catalogue (still editable
// when the catalogue is unavailable).
watch(() => form.value.productCode, (code) => {
  if (isEdit.value) return;
  const p = products.value.find((x) => x.productCode === code);
  if (p?.resourceType) form.value.resourceType = p.resourceType;
});

// Choosing a preset metric pre-fills its threshold / period (create only,
// never over a value the user typed).
watch(() => form.value.metric, (m) => {
  if (isEdit.value) return;
  const t = templates.value.find((x) => x.productCode === form.value.productCode && x.metric === m);
  if (!t) return;
  if (!form.value.threshold && t.threshold) form.value.threshold = t.threshold;
  if (t.period && t.period > 0) form.value.period = t.period;
});

async function load() {
  loading.value = true;
  loadError.value = "";
  const [rulesRes, productsRes, templatesRes] = await Promise.allSettled([
    sdk.get<MonitorRule[]>("/api/v1/monitor/rules"),
    sdk.get<CatalogProduct[]>("/api/v1/catalog/products"),
    sdk.get<RuleTemplate[]>("/api/v1/monitor/templates"),
  ]);
  if (productsRes.status === "fulfilled") products.value = productsRes.value.data ?? [];
  if (templatesRes.status === "fulfilled") templates.value = templatesRes.value.data ?? [];
  if (rulesRes.status === "fulfilled") {
    rules.value = rulesRes.value.data ?? [];
  } else if (isEdit.value) {
    loadError.value = `规则加载失败:${(rulesRes.reason as Error)?.message ?? "请求失败"}`;
  }
  if (isEdit.value && !loadError.value) {
    const r = rules.value.find((x) => x.ruleId === editId.value) ?? null;
    if (!r) {
      ElMessage.error("规则不存在或已删除");
      void router.push("/rules");
      return;
    }
    editing.value = r;
    form.value = ruleToForm(r);
  }
  loading.value = false;
}
onMounted(load);

const productLabel = (code: string) => {
  const p = products.value.find((x) => x.productCode === code);
  return p ? `${p.productName}(${p.productCode})` : code;
};

async function submit() {
  const msg = validateRuleForm(form.value, isEdit.value ? "update" : "create");
  formError.value = msg ?? "";
  if (msg) return;
  saving.value = true;
  try {
    if (isEdit.value && editing.value) {
      await sdk.put(`/api/v1/monitor/rules/${editing.value.ruleId}`, buildUpdateRuleBody(form.value, editing.value.version));
      ElMessage.success("规则已更新");
    } else {
      await sdk.post("/api/v1/monitor/rules", buildCreateRuleBody(form.value));
      ElMessage.success(sameKeyRule.value ? "已更新同指标的已有规则" : "规则已创建");
    }
    void router.push("/rules");
  } catch (e) {
    formError.value = ruleErrorMessage(e);
  } finally {
    saving.value = false;
  }
}

/** After a version conflict: reload the rule and show the latest values. */
async function reloadRule() {
  formError.value = "";
  await load();
}
</script>

<template>
  <section class="rule-form">
    <PageHeader :title="isEdit ? '编辑告警规则' : '创建告警规则'" />
    <p v-if="loading" class="rf-hint">加载中…</p>
    <div v-else-if="loadError" class="rf-error-box">
      <p class="rf-error">{{ loadError }}</p>
      <ElButton size="small" @click="load">重试</ElButton>
    </div>
    <ElForm v-else label-position="top" class="rf-card" @submit.prevent>
      <ElFormItem label="产品">
        <ElSelect v-model="form.productCode" filterable allow-create default-first-option :disabled="isEdit" placeholder="请选择产品">
          <ElOption v-for="p in products" :key="p.productCode" :value="p.productCode" :label="productLabel(p.productCode)" />
        </ElSelect>
      </ElFormItem>
      <ElFormItem label="资源类型">
        <ElInput v-model="form.resourceType" :maxlength="MAX_RESOURCE_TYPE" :disabled="isEdit" placeholder="选择产品后自动带出,如 instance" />
      </ElFormItem>
      <ElFormItem label="监控指标">
        <ElSelect v-model="form.metric" filterable allow-create default-first-option :disabled="isEdit" placeholder="选择或输入指标,如 cpu_utilization">
          <ElOption v-for="m in metricOptions" :key="m" :value="m" :label="m" />
        </ElSelect>
        <p class="rf-field-hint">指标名最多 {{ MAX_METRIC }} 个字符;同一产品/资源类型/指标只能有一条规则。</p>
        <p v-if="sameKeyRule" class="rf-field-hint rf-warn">该指标已有规则(#{{ sameKeyRule.ruleId }}),提交将更新这条规则的阈值与通知设置。</p>
      </ElFormItem>
      <!-- Preview the metric on one of the account's resources (resourceId + window), to pick a threshold. -->
      <ElFormItem v-if="form.productCode && isSupportedMetric(form.metric)" label="指标预览">
        <MetricsPanel :key="form.productCode" class="rf-preview" :product-code="form.productCode" :metric="form.metric" />
      </ElFormItem>
      <div class="rf-row">
        <ElFormItem label="比较方式">
          <ElSelect v-model="form.comparisonOperator">
            <ElOption v-for="o in COMPARISON_OPERATORS" :key="o.value" :value="o.value" :label="o.label" />
          </ElSelect>
        </ElFormItem>
        <ElFormItem label="阈值">
          <ElInput v-model="form.threshold" placeholder="如 80 或 0.5" />
          <p class="rf-field-hint">十进制数字,最多 4 位小数。</p>
        </ElFormItem>
      </div>
      <div class="rf-row">
        <ElFormItem label="统计周期(秒)">
          <ElInputNumber v-model="form.period" :min="1" :max="PERIOD_MAX" :precision="0" />
        </ElFormItem>
        <ElFormItem label="连续周期数">
          <ElInputNumber v-model="form.evalPeriods" :min="1" :max="EVAL_PERIODS_MAX" :precision="0" />
          <p class="rf-field-hint">连续满足 N 个周期才触发告警(防抖)。</p>
        </ElFormItem>
      </div>
      <ElFormItem label="通知渠道">
        <ElCheckboxGroup v-model="form.notificationChannels">
          <ElCheckbox v-for="c in NOTIFICATION_CHANNELS" :key="c.value" :value="c.value">{{ c.label }}</ElCheckbox>
        </ElCheckboxGroup>
      </ElFormItem>

      <p v-if="formError" class="rf-error" role="alert">
        {{ formError }}
        <ElButton v-if="isEdit && formError.includes('刷新')" link type="primary" size="small" @click="reloadRule">刷新规则</ElButton>
      </p>
      <div class="rf-actions">
        <ElButton @click="router.push('/rules')">取消</ElButton>
        <ElButton type="primary" :loading="saving" @click="submit">{{ isEdit ? "保存" : "创建" }}</ElButton>
      </div>
    </ElForm>
  </section>
</template>

<style scoped>
.rule-form { padding: 0; }
.rf-hint { color: var(--eu-text-secondary); font-size: 13px; padding: 16px 0; }
.rf-card {
  max-width: 640px;
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  padding: 24px;
}
.rf-row { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
.rf-preview { width: 100%; }
.rf-field-hint { font-size: 12px; color: var(--eu-text-disabled); margin: 4px 0 0; width: 100%; }
.rf-warn { color: var(--eu-color-warning); }
.rf-error { color: var(--eu-color-danger); font-size: 13px; margin: 0 0 12px; }
.rf-error-box {
  padding: 32px; text-align: center;
  border: 1px solid var(--eu-border); border-radius: var(--eu-radius-lg);
}
.rf-actions { display: flex; gap: 12px; }
</style>
