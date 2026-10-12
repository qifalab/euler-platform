<script setup lang="ts">
/**
 * InstanceActions — the lifecycle action bar of a product instance detail page
 * (02§7.3), wired like the ECS console:
 *   POST /api/v1/orchestrator/resources/{id}/actions — stop/start/restart/upgrade
 *   POST /api/v1/orchestrator/resources/{id}/release — 释放
 * Buttons follow the orchestrator's state rules (resourceActions.ts): power
 * buttons only appear in the state that allows them, the rest disable; every
 * action is confirmed, release as a dangerous one. Release stays available for
 * CREATE_FAILED / LOCKED / EXPIRED so a dead-end resource can be cleaned up. 续费 opens the shell's
 * billing centre (there is no in-place renew endpoint).
 *
 * `actions` lists the buttons a product offers; the page refetches on
 * `changed` and leaves on `released`.
 */
import { computed, ref } from "vue";
import { ElButton, ElDialog, ElMessage, ElMessageBox, ElRadio, ElRadioGroup } from "element-plus";
import type { SDK } from "@eu/sdk";
import { navigateShell } from "./navigation";
import {
  canRunAction,
  upgradeOptions,
  type ActionTarget,
  type CatalogSku,
  type InstanceActionButton,
  type SpecLabel,
} from "./resourceActions";

const props = withDefaults(
  defineProps<{
    sdk: Pick<SDK, "get" | "post">;
    resourceId: string;
    instance: ActionTarget | null;
    actions: InstanceActionButton[];
    /** What the resource is called in confirmations (实例 / 策略 …). */
    noun?: string;
    /** Upgrade dialog label for a catalogue spec. */
    specLabel?: SpecLabel;
  }>(),
  { noun: "实例" },
);
const emit = defineEmits<{ changed: []; released: [] }>();

type Power = "stop" | "start" | "restart";
const acting = ref<Power | "release" | null>(null);
const busy = computed(() => acting.value !== null);
const state = computed(() => props.instance?.state ?? "");
const allowed = (a: InstanceActionButton) => canRunAction(a, state.value, props.instance?.chargeType);
const actionPath = computed(() => `/api/v1/orchestrator/resources/${encodeURIComponent(props.resourceId)}`);

function errText(e: unknown): string {
  const err = (e ?? {}) as { message?: string; code?: string };
  return err.message || err.code || "操作失败";
}

const powerNames: Record<Power, string> = { stop: "停止", start: "启动", restart: "重启" };
const powerTips = computed<Record<Power, string>>(() => ({
  stop: `停止后服务将中断,确定停止该${props.noun}吗?`,
  start: `确定启动该${props.noun}吗?`,
  restart: `重启会中断服务,确定重启该${props.noun}吗?`,
}));

async function runPower(action: Power) {
  try {
    await ElMessageBox.confirm(powerTips.value[action], "操作确认", {
      type: "warning", confirmButtonText: "确定", cancelButtonText: "取消",
    });
  } catch {
    return; // 用户取消
  }
  acting.value = action;
  try {
    await props.sdk.post(`${actionPath.value}/actions`, { action });
    ElMessage.success(`${powerNames[action]}指令已下发`);
    emit("changed");
  } catch (e) {
    ElMessage.error(errText(e));
  } finally {
    acting.value = null;
  }
}

/** Why this release is being offered, prefixed to the danger confirmation. */
const releaseReason: Record<string, string> = {
  CREATE_FAILED: "开通失败,",
  LOCKED: "已因欠费锁定,",
  EXPIRED: "已到期,",
};

async function release() {
  const reason = releaseReason[state.value];
  try {
    await ElMessageBox.confirm(
      reason
        ? `该${props.noun}${reason}释放后 ${props.resourceId} 及其数据不可恢复,确定释放吗?`
        : `释放后${props.noun} ${props.resourceId} 及其数据不可恢复,确定释放吗?`,
      "危险操作",
      { type: "warning", confirmButtonText: "确定释放", cancelButtonText: "取消" },
    );
  } catch {
    return; // 用户取消
  }
  acting.value = "release";
  try {
    await props.sdk.post(`${actionPath.value}/release`);
    ElMessage.success("释放指令已下发");
    emit("released");
  } catch (e) {
    ElMessage.error(errText(e));
  } finally {
    acting.value = null;
  }
}

function goRenew() {
  if (!navigateShell("/billing")) ElMessage.info("请在控制台「费用中心」办理续费");
}

// --- 变配: catalogue specs of the same product/charge type → actions upgrade ---
const upgradeVisible = ref(false);
const skuLoading = ref(false);
const upgrading = ref(false);
const catalogSkus = ref<CatalogSku[]>([]);
const targetSpec = ref("");
const upgradeChoices = computed(() =>
  props.instance ? upgradeOptions(catalogSkus.value, props.instance, props.specLabel) : [],
);
const upgradeTitle = computed(() =>
  props.instance?.chargeType === "PREPAID" ? "包年包月实例暂不支持直接变配,请通过变配订单办理" : undefined,
);

async function openUpgrade() {
  if (!props.instance) return;
  upgradeVisible.value = true;
  targetSpec.value = "";
  if (catalogSkus.value.length) return; // loaded once, reused
  skuLoading.value = true;
  try {
    const res = await props.sdk.get<CatalogSku[]>(
      `/api/v1/catalog/skus?productCode=${encodeURIComponent(props.instance.productCode)}`,
    );
    catalogSkus.value = res.data ?? [];
  } catch (e) {
    ElMessage.error(`加载规格目录失败:${errText(e)}`);
  } finally {
    skuLoading.value = false;
  }
}

async function confirmUpgrade() {
  if (!targetSpec.value) {
    ElMessage.warning("请选择目标规格");
    return;
  }
  upgrading.value = true;
  try {
    await props.sdk.post(`${actionPath.value}/actions`, { action: "upgrade", specCode: targetSpec.value });
    ElMessage.success("变配指令已下发");
    upgradeVisible.value = false;
    emit("changed");
  } catch (e) {
    ElMessage.error(errText(e));
  } finally {
    upgrading.value = false;
  }
}
</script>

<template>
  <div class="eu-instance-actions">
    <template v-for="a in actions" :key="a">
      <ElButton v-if="a === 'stop' && state === 'RUNNING'" size="small" :loading="acting === 'stop'" :disabled="busy" @click="runPower('stop')">停止</ElButton>
      <ElButton v-else-if="a === 'start' && state === 'STOPPED'" size="small" type="primary" :loading="acting === 'start'" :disabled="busy" @click="runPower('start')">启动</ElButton>
      <ElButton v-else-if="a === 'restart' && state === 'RUNNING'" size="small" :loading="acting === 'restart'" :disabled="busy" @click="runPower('restart')">重启</ElButton>
      <ElButton v-else-if="a === 'upgrade'" size="small" :disabled="busy || !allowed('upgrade')" :title="upgradeTitle" @click="openUpgrade">变配</ElButton>
      <ElButton v-else-if="a === 'renew'" size="small" type="primary" :disabled="busy || !allowed('renew')" @click="goRenew">续费</ElButton>
      <ElButton v-else-if="a === 'release'" size="small" type="danger" :loading="acting === 'release'" :disabled="busy || !allowed('release')" @click="release">释放</ElButton>
    </template>

    <ElDialog v-model="upgradeVisible" title="变配" width="480px" append-to-body>
      <p class="eu-ia-hint">当前规格:{{ instance?.specCode ?? "—" }},请选择目标规格</p>
      <p v-if="skuLoading" class="eu-ia-hint">规格加载中…</p>
      <ElRadioGroup v-else-if="upgradeChoices.length" v-model="targetSpec" class="eu-ia-list">
        <ElRadio v-for="s in upgradeChoices" :key="s.skuCode" :value="s.skuCode">{{ s.label }}({{ s.skuCode }})</ElRadio>
      </ElRadioGroup>
      <p v-else class="eu-ia-hint">暂无可选规格</p>
      <template #footer>
        <ElButton @click="upgradeVisible = false">取消</ElButton>
        <ElButton type="primary" :loading="upgrading" @click="confirmUpgrade">确认变配</ElButton>
      </template>
    </ElDialog>
  </div>
</template>

<style>
/* Lays the buttons out as direct children of the page's action row. */
.eu-instance-actions { display: contents; }
.eu-ia-hint { font-size: 13px; color: var(--eu-text-secondary); margin: 0 0 12px; }
.eu-ia-list { display: flex; flex-direction: column; gap: 4px; align-items: stretch; }
</style>
