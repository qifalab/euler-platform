<script setup lang="ts">
/**
 * 策略模拟器 (07§3, M-7.6).
 * Lets an operator test "would principal X be allowed to perform action Y on
 * resource Z?" BEFORE committing a policy, so a mis-scoped policy is caught at
 * design time. Deny-first (07§3.3): the default is the same as production's
 * default — explicit Deny wins; no match → deny. The backend resolves the
 * named policies against the account's stored set and runs pkg-go/authz.Simulate.
 */
import { onMounted, reactive, ref } from "vue";
import {
  ElButton,
  ElCheckboxGroup,
  ElCheckbox,
  ElEmpty,
  ElFormItem,
  ElInput,
  ElMessage,
  ElTag,
} from "element-plus";
import { useAccountAuth } from "../stores/auth";

const auth = useAccountAuth();

interface PolicyOption {
  name: string;
  type: string;
}
const policies = reactive<PolicyOption[]>([]);

const form = reactive({
  principal: "ops-admin",
  selected: [] as string[],
  action: "scecs:StartInstance",
  resource: "sc:ecs:cn-north-1:100123:instance/i-1",
});

interface Verdict {
  allowed: boolean;
  decidingStatement: number;
  decidingPolicy: string;
  reason: string;
  principal: string;
}
const verdict = ref<Verdict | null>(null);
const loading = ref(false);

async function readEnvelope<T>(res: Response): Promise<T> {
  const body = await res.json();
  if (!res.ok || body.Code !== "OK") {
    throw new Error(body.Message ?? `request failed (HTTP ${res.status})`);
  }
  return body.Data as T;
}
function authHeaders(): HeadersInit {
  return auth.accessToken ? { Authorization: `Bearer ${auth.accessToken}` } : {};
}

async function fetchPolicies() {
  if (!auth.isAuthenticated) return;
  try {
    const data = await readEnvelope<{ policies: PolicyOption[]; total: number }>(
      await fetch("/api/ram/policies", { headers: authHeaders() }),
    );
    policies.splice(0, policies.length, ...(data?.policies ?? []).map((p) => ({ name: p.name, type: p.type })));
    // Default-select all so the simulator has a policy set on first load.
    form.selected = policies.map((p) => p.name);
  } catch (e) {
    ElMessage.error(`加载策略失败：${(e as Error).message}`);
  }
}

onMounted(fetchPolicies);

async function run() {
  if (!form.action || !form.resource) {
    ElMessage.warning("请填写 action 与 resource");
    return;
  }
  if (form.selected.length === 0) {
    ElMessage.warning("请至少选择一个策略");
    return;
  }
  loading.value = true;
  try {
    const data = await readEnvelope<Verdict>(
      await fetch("/api/ram/simulate", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...authHeaders() },
        body: JSON.stringify({
          principal: form.principal,
          policies: form.selected,
          action: form.action,
          resource: form.resource,
        }),
      }),
    );
    verdict.value = data;
  } catch (e) {
    verdict.value = null;
    ElMessage.error(`模拟失败：${(e as Error).message}`);
  } finally {
    loading.value = false;
  }
}

const reasonLabel: Record<string, string> = {
  allow: "显式允许",
  explicit_deny: "显式拒绝（拒绝优先）",
  default_deny: "默认拒绝（无匹配）",
  no_policy: "无策略",
};
</script>

<template>
  <div class="simulator-page">
    <ElEmpty v-if="!auth.isAuthenticated" description="请先登录" />

    <template v-else>
      <header class="page-header">
        <div class="heading">
          <h1 class="page-title">策略模拟器</h1>
          <p class="page-desc">
            在提交策略前验证其效果：Deny-first（拒绝优先），默认拒绝与生产一致。后端
            使用与网关同一引擎（pkg-go/authz）求值，模拟结果即生产结果。
          </p>
        </div>
      </header>

      <div class="sim-layout">
        <section class="sim-form">
          <ElFormItem label="主体（Principal）">
            <ElInput v-model="form.principal" placeholder="如 ops-admin（仅用于展示/审计）" />
          </ElFormItem>
          <ElFormItem label="操作（Action）">
            <ElInput v-model="form.action" placeholder="如 scecs:StartInstance" />
          </ElFormItem>
          <ElFormItem label="资源（Resource）">
            <ElInput v-model="form.resource" placeholder="如 sc:ecs:cn-north-1:100123:instance/i-1" />
          </ElFormItem>
          <ElFormItem label="评估策略">
            <ElCheckboxGroup v-model="form.selected">
              <ElCheckbox v-for="p in policies" :key="p.name" :value="p.name">
                {{ p.name }}
                <ElTag size="small" effect="plain" style="margin-left: 6px">{{ p.type }}</ElTag>
              </ElCheckbox>
            </ElCheckboxGroup>
            <span v-if="policies.length === 0" style="color: var(--sc-text-disabled)">暂无策略</span>
          </ElFormItem>
          <ElButton type="primary" :loading="loading" @click="run">模拟求值</ElButton>
        </section>

        <aside class="sim-result">
          <h2>求值结果</h2>
          <template v-if="verdict">
            <div class="verdict-row">
              <span class="label">结果</span>
              <ElTag :type="verdict.allowed ? 'success' : 'danger'" size="large" effect="dark">
                {{ verdict.allowed ? "允许" : "拒绝" }}
              </ElTag>
            </div>
            <div class="verdict-row">
              <span class="label">原因</span>
              <span>{{ reasonLabel[verdict.reason] ?? verdict.reason }}</span>
            </div>
            <div class="verdict-row">
              <span class="label">决定策略</span>
              <span>{{ verdict.decidingPolicy || "（默认拒绝，无策略命中）" }}</span>
            </div>
            <div class="verdict-row">
              <span class="label">决定语句</span>
              <span>{{ verdict.decidingStatement >= 0 ? `Statement #${verdict.decidingStatement}` : "—" }}</span>
            </div>
          </template>
          <p v-else class="empty-hint">填写参数后点击「模拟求值」</p>
        </aside>
      </div>
    </template>
  </div>
</template>

<style scoped>
.simulator-page {
  padding: var(--sc-spacing-6);
  min-height: calc(100vh - var(--sc-topbar-height));
}
.page-header { margin-bottom: var(--sc-spacing-6); }
.heading { display: flex; flex-direction: column; gap: 6px; }
.page-title { margin: 0; font-size: 22px; font-weight: 600; color: var(--sc-text-primary); }
.page-desc { margin: 0; font-size: 13px; color: var(--sc-text-secondary); line-height: 1.5; }
.sim-layout { display: grid; grid-template-columns: 1fr 320px; gap: var(--sc-spacing-5); }
.sim-form, .sim-result {
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  box-shadow: var(--sc-shadow-sm);
  padding: var(--sc-spacing-5);
}
.sim-result h2 { font-size: 15px; margin: 0 0 16px; }
.verdict-row {
  display: flex; align-items: center; justify-content: space-between;
  padding: 10px 0; border-bottom: 1px solid var(--sc-border); font-size: 13px;
}
.verdict-row .label { color: var(--sc-text-secondary); }
.empty-hint { color: var(--sc-text-disabled); font-size: 13px; margin: 0; }
</style>
