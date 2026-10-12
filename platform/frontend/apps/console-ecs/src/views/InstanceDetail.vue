<script setup lang="ts">
/**
 * Instance detail (02§7.3): info header + tab group (监控/配置/网络/操作日志).
 *
 * Wired to the real resource lifecycle endpoint:
 *   GET /api/v1/orchestrator/resources/{id}  — svc-orchestrator (03§5.2)
 * The orchestrator is the sole owner of the resource lifecycle ledger, so this
 * is the authoritative source of state, spec, billing start, etc.
 *
 * Lifecycle actions hit the orchestrator directly (03§5.2):
 *   POST /api/v1/orchestrator/resources/{id}/actions — stop/start/restart/upgrade
 *   POST /api/v1/orchestrator/resources/{id}/release — 释放
 * The same state machine guards both the saga and the console buttons; a 409
 * means the current state does not allow the action.
 *
 * Monitoring (EUMON metrics), network details, and operation logs are not part
 * of the orchestrator's resource record — those tabs are placeholders for the
 * svc-monitor / audit services that own them, rather than fabricated rows.
 */
import { ref, computed, onMounted } from "vue";
import { useRoute, useRouter } from "vue-router";
import { ElTabs, ElTabPane, ElDescriptions, ElDescriptionsItem, ElButton, ElTag, ElEmpty, ElMessage, ElMessageBox, ElDialog, ElRadioGroup, ElRadio } from "element-plus";
import { formatTime, navigateShell, canRunAction } from "@eu/console-kit";
import { sdk } from "../sdk";

interface ResourceDetail {
  resourceId: string;
  productCode: string;
  region: string;
  chargeType: string; // PREPAID | POSTPAID
  state: string;      // RUNNING | STOPPED | CREATING | ...
  specCode: string;
  billingStart: string;
  /** Prepaid term end (RFC3339); "" for postpaid / not yet delivered. */
  expiredAt?: string;
  createdAt: string;
  version: number;
}

const route = useRoute();
const router = useRouter();
const id = computed(() => String(route.params.id ?? ""));

const inst = ref<ResourceDetail | null>(null);
const loading = ref(true);
const error = ref<string | null>(null);
const activeTab = ref("monitor");

/** 进行中的操作(stop/start/restart/release),用于按钮 loading 防重复提交。 */
const acting = ref<"stop" | "start" | "restart" | "release" | null>(null);

const state = computed(() => inst.value?.state ?? "");
/** 仅 RUNNING/STOPPED 可操作;CREATING/RELEASING 等过渡态全部禁用。 */
const operable = computed(() => state.value === "RUNNING" || state.value === "STOPPED");
/**
 * 释放另外放开 CREATE_FAILED(开通失败)/ LOCKED(欠费锁定)/ EXPIRED(已到期):
 * 这些终局状态只能由客户释放清理(svc-orchestrator release 接受这些状态)。
 */
const releasable = computed(() => canRunAction("release", state.value));
const releaseReason: Record<string, string> = { CREATE_FAILED: "开通失败,", LOCKED: "已因欠费锁定,", EXPIRED: "已到期," };

async function fetchDetail() {
  if (!id.value) return;
  try {
    const res = await sdk.get<ResourceDetail>(`/api/v1/orchestrator/resources/${id.value}`);
    inst.value = res.data;
    error.value = null;
  } catch (e) {
    const err = e as { message?: string; code?: string; status?: number };
    const msg = err.message ?? "加载失败";
    error.value = msg;
    if (err.status === 404 || err.code === "Resource.NotFound" || msg.includes("不存在")) {
      ElMessage.error("实例不存在");
      router.push("/instances");
    }
  }
}

onMounted(async () => {
  if (!id.value) { error.value = "缺少实例 ID"; loading.value = false; return; }
  await fetchDetail();
  loading.value = false;
});

// RFC3339 → viewer-local time; "—" for unset ("" / zero-year) values. The old
// textual strip showed UTC wall-clock (with a stray "Z") as if it were local.
const fmt = formatTime;
const chargeLabel = computed(() => inst.value?.chargeType === "PREPAID" ? "包年包月" : inst.value?.chargeType === "POSTPAID" ? "按量付费" : "—");

/** 后端错误体 message 透出;SDK 兜底文案时退回错误码。 */
function errMsg(e: unknown): string {
  const err = e as { message?: string; code?: string };
  const msg = err?.message ?? "";
  if (!msg || msg === "request failed") return err?.code ?? "请求失败";
  return msg;
}

// --- 电源/释放操作(走 orchestrator actions/release,同一状态机) ---

const actionTips: Record<"stop" | "start" | "restart", string> = {
  stop: "停止后运行中的应用将被中断,确定停止该实例吗?",
  start: "确定启动该实例吗?",
  restart: "重启会中断运行中的应用,确定重启该实例吗?",
};
const actionNames: Record<"stop" | "start" | "restart", string> = { stop: "停止", start: "启动", restart: "重启" };

async function runAction(action: "stop" | "start" | "restart") {
  try {
    await ElMessageBox.confirm(actionTips[action], "操作确认", {
      type: "warning", confirmButtonText: "确定", cancelButtonText: "取消",
    });
  } catch {
    return; // 用户取消
  }
  acting.value = action;
  try {
    await sdk.post(`/api/v1/orchestrator/resources/${id.value}/actions`, { action });
    ElMessage.success(`${actionNames[action]}指令已下发`);
    await fetchDetail();
  } catch (e) {
    ElMessage.error(errMsg(e));
  } finally {
    acting.value = null;
  }
}

async function releaseInstance() {
  try {
    const reason = releaseReason[state.value];
    await ElMessageBox.confirm(
      reason
        ? `该实例${reason}释放后 ${id.value} 及其数据不可恢复,确定释放吗?`
        : `释放后实例 ${id.value} 及其数据不可恢复,确定释放吗?`,
      "危险操作",
      { type: "warning", confirmButtonText: "确定释放", cancelButtonText: "取消" },
    );
  } catch {
    return; // 用户取消
  }
  acting.value = "release";
  try {
    await sdk.post(`/api/v1/orchestrator/resources/${id.value}/release`);
    ElMessage.success("释放指令已下发");
    router.push("/instances");
  } catch (e) {
    ElMessage.error(errMsg(e));
  } finally {
    acting.value = null;
  }
}

// --- 变配:catalog 规格目录 → actions upgrade ---

interface Sku {
  skuCode: string;
  productCode: string;
  chargeType: string; // PREPAID | POSTPAID
  specJson: string;   // e.g. {"cpu":2,"mem_gb":4}
  status: string;     // "1" = 上架
}
interface UpgradeSku { skuCode: string; cpu: number; memGb: number; label: string }

const upgradeVisible = ref(false);
const skuLoading = ref(false);
const upgrading = ref(false);
const catalogSkus = ref<Sku[]>([]);
const targetSpec = ref("");

/**
 * 包年包月实例不能直接变配:订单按原规格付了整个周期,orchestrator 对
 * PREPAID 的 upgrade 一律 409(需走变配订单补差价,尚未接入)。
 */
const upgradeBlocked = computed(() => inst.value?.chargeType === "PREPAID");

/**
 * 可选目标规格:在售、与实例同计费方式(orchestrator 拒绝跨计费方式的 SKU),
 * 且不是当前规格。按当前实例实时过滤,变配后列表随之更新。
 */
const upgradeSkus = computed<UpgradeSku[]>(() =>
  catalogSkus.value
    .filter((s) => s.status === "1" && s.chargeType === inst.value?.chargeType && s.skuCode !== inst.value?.specCode)
    .map((s) => {
      let cpu = 0, memGb = 0;
      try {
        const p = JSON.parse(s.specJson) as Partial<{ cpu: number; mem_gb: number }>;
        cpu = p.cpu ?? 0; memGb = p.mem_gb ?? 0;
      } catch { /* 解析失败按 0 排序置底 */ }
      return { skuCode: s.skuCode, cpu, memGb, label: `${cpu} 核 ${memGb} GiB` };
    })
    .sort((a, b) => a.cpu - b.cpu || a.memGb - b.memGb),
);

async function openUpgrade() {
  if (upgradeBlocked.value) {
    ElMessage.warning("包年包月实例暂不支持直接变配,请通过变配订单办理");
    return;
  }
  upgradeVisible.value = true;
  targetSpec.value = "";
  if (catalogSkus.value.length) return; // 已加载过,复用
  skuLoading.value = true;
  try {
    const res = await sdk.get<Sku[]>("/api/v1/catalog/skus?productCode=euecs");
    catalogSkus.value = res.data ?? [];
  } catch (e) {
    ElMessage.error(`加载规格目录失败:${errMsg(e)}`);
  } finally {
    skuLoading.value = false;
  }
}

async function confirmUpgrade() {
  if (!targetSpec.value) { ElMessage.warning("请选择目标规格"); return; }
  upgrading.value = true;
  try {
    await sdk.post(`/api/v1/orchestrator/resources/${id.value}/actions`, {
      action: "upgrade", specCode: targetSpec.value,
    });
    ElMessage.success("变配指令已下发");
    upgradeVisible.value = false;
    await fetchDetail();
  } catch (e) {
    ElMessage.error(errMsg(e));
  } finally {
    upgrading.value = false;
  }
}

// --- 续费:无"立即续费"端点,跳费用中心(web-billing)办理 ---
// registry fallback 里 web-billing 的 activeRules 仅 ["/billing"](无 renew
// 子路径)。本子应用是 history 路由,改 location.hash 只会改自己的片段、
// 主应用无感知(死链);费用中心是主应用一级路由,需导航主应用窗口。
function goRenew() {
  if (!navigateShell("/billing")) {
    ElMessage.info("请在控制台「费用中心」办理续费");
  }
}
</script>

<template>
  <section class="detail">
    <header class="detail-head">
      <div>
        <h1 class="detail-title">{{ inst?.resourceId ?? id }}
          <ElTag v-if="inst" size="small" type="success">{{ inst.state }}</ElTag>
        </h1>
        <p class="detail-id">ID:{{ id }} · {{ inst?.region ?? "—" }}</p>
      </div>
      <div class="detail-actions">
        <!-- 状态感知:RUNNING 显示 停止/重启;STOPPED 显示 启动;其余状态禁用 -->
        <ElButton v-if="state === 'RUNNING'" size="small" :loading="acting === 'stop'" :disabled="acting !== null" @click="runAction('stop')">停止</ElButton>
        <ElButton v-if="state === 'STOPPED'" size="small" type="primary" :loading="acting === 'start'" :disabled="acting !== null" @click="runAction('start')">启动</ElButton>
        <ElButton v-if="state === 'RUNNING'" size="small" :loading="acting === 'restart'" :disabled="acting !== null" @click="runAction('restart')">重启</ElButton>
        <ElButton
          size="small"
          :disabled="!operable || acting !== null || upgradeBlocked"
          :title="upgradeBlocked ? '包年包月实例暂不支持直接变配' : undefined"
          @click="openUpgrade"
        >变配</ElButton>
        <ElButton size="small" type="primary" :disabled="!operable || acting !== null" @click="goRenew">续费</ElButton>
        <ElButton size="small" type="danger" :loading="acting === 'release'" :disabled="!releasable || acting !== null" @click="releaseInstance">释放</ElButton>
      </div>
    </header>

    <p v-if="error" class="detail-error">加载失败:{{ error }}(请确认 svc-orchestrator 在 :9203)</p>

    <ElTabs v-else v-model="activeTab" class="detail-tabs">
      <ElTabPane label="监控" name="monitor">
        <ElEmpty description="监控指标由云监控(svc-monitor)提供,该端点接入后在此展示" />
      </ElTabPane>
      <ElTabPane label="配置信息" name="config">
        <ElDescriptions v-if="inst" :column="2" border>
          <ElDescriptionsItem label="实例规格">{{ inst.specCode }}</ElDescriptionsItem>
          <ElDescriptionsItem label="计费方式">{{ chargeLabel }}</ElDescriptionsItem>
          <ElDescriptionsItem label="产品">{{ inst.productCode }}</ElDescriptionsItem>
          <ElDescriptionsItem label="地域">{{ inst.region }}</ElDescriptionsItem>
          <ElDescriptionsItem label="计费起始">{{ fmt(inst.billingStart) }}</ElDescriptionsItem>
          <ElDescriptionsItem v-if="inst.chargeType === 'PREPAID'" label="到期时间">{{ fmt(inst.expiredAt) }}</ElDescriptionsItem>
          <ElDescriptionsItem label="创建时间">{{ fmt(inst.createdAt) }}</ElDescriptionsItem>
          <ElDescriptionsItem label="资源版本">{{ inst.version }}</ElDescriptionsItem>
        </ElDescriptions>
        <p v-else class="detail-loading">加载中…</p>
      </ElTabPane>
      <ElTabPane label="网络" name="network">
        <ElEmpty description="网络配置(专有网络/内网 IP/弹性公网 IP/安全组)由 svc-vpc 提供,该端点接入后在此展示" />
      </ElTabPane>
      <ElTabPane label="操作日志" name="logs">
        <ElEmpty description="暂无操作日志(操作审计由 svc-audit 提供,该端点接入后在此展示)" />
      </ElTabPane>
    </ElTabs>

    <a class="detail-back" href="#" @click.prevent="router.push('/instances')">← 返回实例列表</a>

    <!-- 变配:规格目录单选(排除当前规格,按 cpu/mem 升序) -->
    <ElDialog v-model="upgradeVisible" title="变配实例" width="480px">
      <p class="upgrade-current">当前规格:{{ inst?.specCode ?? "—" }},请选择目标规格</p>
      <p v-if="skuLoading" class="upgrade-hint">规格加载中…</p>
      <ElRadioGroup v-else-if="upgradeSkus.length" v-model="targetSpec" class="upgrade-list">
        <ElRadio v-for="s in upgradeSkus" :key="s.skuCode" :value="s.skuCode" class="upgrade-item">
          {{ s.label }}({{ s.skuCode }})
        </ElRadio>
      </ElRadioGroup>
      <p v-else class="upgrade-hint">暂无可选规格</p>
      <template #footer>
        <ElButton @click="upgradeVisible = false">取消</ElButton>
        <ElButton type="primary" :loading="upgrading" @click="confirmUpgrade">确认变配</ElButton>
      </template>
    </ElDialog>
  </section>
</template>

<style scoped>
.detail { padding: 16px 24px; }
.detail-head { display: flex; align-items: flex-start; justify-content: space-between; margin-bottom: 16px; }
.detail-title { font-size: 18px; margin: 0; display: flex; align-items: center; gap: 8px; }
.detail-id { font-size: 12px; color: var(--eu-text-secondary); margin: 4px 0 0; }
.detail-actions { display: flex; gap: 8px; }
.detail-error { color: var(--eu-color-danger); padding: 24px 0; }
.detail-loading { color: var(--eu-text-secondary); padding: 24px 0; }
.detail-tabs { background: var(--eu-bg-container); border-radius: var(--eu-radius-md); padding: 0 16px 16px; }
.detail-back { display: inline-block; margin-top: 16px; color: var(--eu-color-brand); text-decoration: none; font-size: 13px; }
.upgrade-current { font-size: 13px; color: var(--eu-text-secondary); margin: 0 0 12px; }
.upgrade-hint { font-size: 13px; color: var(--eu-text-secondary); }
.upgrade-list { display: flex; flex-direction: column; gap: 4px; align-items: stretch; }
</style>
