<script setup lang="ts">
/**
 * Instance detail (02§7.3): info header + tab group (监控/配置/操作日志).
 *
 * Wired to the real resource lifecycle endpoint:
 *   GET /api/v1/orchestrator/resources/{id}  — svc-orchestrator (03§5.2)
 * The orchestrator is the sole owner of the resource lifecycle ledger, so this
 * is the authoritative source of state, spec, billing start, etc. EUBACKUP
 * carries chargeType=POSTPAID (按量, billed by stored capacity).
 *
 * Policy + snapshots:
 *   PATCH  .../resources/{id}/attributes        — 修改备份策略 (schedule, retentionDays)
 *   POST   .../resources/{id}/backups           — 立即备份
 *   GET    .../resources/{id}/backups           — 快照列表
 *   DELETE .../resources/{id}/backups/{backupId} — 删除快照
 */
import { ref, computed, onMounted, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { ElTabs, ElTabPane, ElDescriptions, ElDescriptionsItem, ElTag, ElEmpty, ElMessage, ElMessageBox, ElButton, ElTable, ElTableColumn } from "element-plus";
import { formatTime, InstanceActions, attributesOf } from "@eu/console-kit";
import { sdk } from "../sdk";
import { describeSchedule } from "../backupPolicy";
import { backupErrorMessage, createBackup, deleteBackup, listBackups, snapshotStatusLabel, type BackupSnapshot } from "../backups";
import BackupPolicyDialog from "./BackupPolicyDialog.vue";

interface ResourceDetail {
  resourceId: string;
  productCode: string;
  region: string;
  chargeType: string; // POSTPAID
  state: string;      // RUNNING | CREATING | ...
  specCode: string;
  billingStart: string;
  createdAt: string;
  version: number;
  lastBackupAt?: string;
  attributes?: Record<string, string>;
}

const route = useRoute();
const router = useRouter();
const id = computed(() => String(route.params.id ?? ""));

const inst = ref<ResourceDetail | null>(null);
const loading = ref(true);
const error = ref<string | null>(null);
const activeTab = ref("monitor");

/** (Re)load the resource record — on mount and after every lifecycle action. */
async function fetchDetail() {
  try {
    const res = await sdk.get<ResourceDetail>(`/api/v1/orchestrator/resources/${id.value}`);
    inst.value = res.data;
    error.value = null;
  } catch (e) {
    // Branch on the structured error the SDK attaches (status/code): matching
    // the message TEXT missed 404s whose wording differs from those literals.
    const err = e as { status?: number; code?: string; message?: string };
    error.value = err.message ?? "加载失败";
    if (err.status === 404 || err.code === "Resource.NotFound") {
      ElMessage.error("策略不存在");
      router.push("/instances");
    }
  }
}

onMounted(async () => {
  if (!id.value) { error.value = "缺少策略 ID"; loading.value = false; return; }
  await fetchDetail();
  loading.value = false;
});

// RFC3339 → viewer-local time; "—" for unset ("" / zero-year) values. The old
// textual strip showed UTC wall-clock (with a stray "Z") as if it were local.
const fmt = formatTime;
const chargeLabel = computed(() => inst.value?.chargeType === "POSTPAID" ? "按量" : "—");

// --- policy attributes (set at creation, editable) ---
const attrs = computed(() => attributesOf(inst.value));
const retentionLabel = computed(() => (attrs.value.retentionDays ? `${attrs.value.retentionDays} 天` : "—"));
/** A policy accepts backups / edits only while it is live. */
const live = computed(() => inst.value?.state === "RUNNING" || inst.value?.state === "STOPPED");
const policyVisible = ref(false);
function onPolicySaved(detail: unknown) {
  if (detail && typeof detail === "object" && "resourceId" in detail) inst.value = detail as ResourceDetail;
  void fetchDetail();
}

// --- snapshots ---
const snapshots = ref<BackupSnapshot[]>([]);
const snapLoading = ref(false);
const snapError = ref<string | null>(null);
const snapLoaded = ref(false);
const backingUp = ref(false);
const deletingId = ref<string | null>(null);

async function loadSnapshots() {
  if (!id.value) return;
  snapLoading.value = true;
  try {
    snapshots.value = await listBackups(sdk, id.value);
    snapError.value = null;
  } catch (e) {
    snapError.value = backupErrorMessage(e, "加载快照");
  } finally {
    snapLoading.value = false;
    snapLoaded.value = true;
  }
}
// Load the list the first time its tab is opened.
watch(activeTab, (t) => { if (t === "snapshots" && !snapLoaded.value) void loadSnapshots(); });

async function backupNow() {
  backingUp.value = true;
  try {
    const snap = await createBackup(sdk, id.value);
    ElMessage.success(`已创建快照 ${snap?.backupId ?? ""}`.trim());
    activeTab.value = "snapshots";
    await Promise.all([loadSnapshots(), fetchDetail()]);
  } catch (e) {
    ElMessage.error(backupErrorMessage(e, "立即备份"));
  } finally {
    backingUp.value = false;
  }
}

async function removeSnapshot(s: BackupSnapshot) {
  try {
    await ElMessageBox.confirm(`删除后快照 ${s.backupId} 不可恢复,确定删除吗?`, "删除快照", {
      type: "warning", confirmButtonText: "确定删除", cancelButtonText: "取消",
    });
  } catch {
    return; // 用户取消
  }
  deletingId.value = s.backupId;
  try {
    await deleteBackup(sdk, id.value, s.backupId);
    ElMessage.success("快照已删除");
    await loadSnapshots();
  } catch (e) {
    ElMessage.error(backupErrorMessage(e, "删除快照"));
  } finally {
    deletingId.value = null;
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
        <ElButton size="small" type="primary" :loading="backingUp" :disabled="!live" @click="backupNow">立即备份</ElButton>
        <ElButton size="small" :disabled="!live" @click="policyVisible = true">修改策略</ElButton>
        <InstanceActions
          :sdk="sdk"
          :resource-id="id"
          :instance="inst"
          :actions="['release']"
          noun="策略"
          @changed="fetchDetail"
          @released="router.push('/instances')"
        />
      </div>
    </header>

    <p v-if="error" class="detail-error">加载失败:{{ error }}(请确认 svc-orchestrator 在 :9203)</p>

    <ElTabs v-else v-model="activeTab" class="detail-tabs">
      <ElTabPane label="监控" name="monitor">
        <ElEmpty description="监控指标由云监控(svc-monitor)提供,该端点接入后在此展示" />
      </ElTabPane>
      <ElTabPane label="配置信息" name="config">
        <ElDescriptions v-if="inst" :column="2" border>
          <ElDescriptionsItem label="策略规格">{{ inst.specCode }}</ElDescriptionsItem>
          <ElDescriptionsItem label="计费方式">{{ chargeLabel }}</ElDescriptionsItem>
          <ElDescriptionsItem label="产品">{{ inst.productCode }}</ElDescriptionsItem>
          <ElDescriptionsItem label="地域">{{ inst.region }}</ElDescriptionsItem>
          <ElDescriptionsItem label="计费起始">{{ fmt(inst.billingStart) }}</ElDescriptionsItem>
          <ElDescriptionsItem label="创建时间">{{ fmt(inst.createdAt) }}</ElDescriptionsItem>
          <ElDescriptionsItem label="备份周期">{{ describeSchedule(attrs.schedule) }}</ElDescriptionsItem>
          <ElDescriptionsItem label="保留期">{{ retentionLabel }}</ElDescriptionsItem>
          <ElDescriptionsItem label="最近备份">{{ fmt(inst.lastBackupAt ?? "") }}</ElDescriptionsItem>
          <ElDescriptionsItem label="资源版本">{{ inst.version }}</ElDescriptionsItem>
        </ElDescriptions>
        <p v-else class="detail-loading">加载中…</p>
      </ElTabPane>
      <ElTabPane label="快照列表" name="snapshots">
        <div v-if="snapError" class="snap-error">
          <span>{{ snapError }}</span>
          <ElButton size="small" :loading="snapLoading" @click="loadSnapshots">重试</ElButton>
        </div>
        <ElTable v-else-if="snapLoading || snapshots.length" v-loading="snapLoading" :data="snapshots" stripe>
          <ElTableColumn prop="backupId" label="快照 ID" min-width="200" />
          <ElTableColumn label="创建时间" min-width="160">
            <template #default="{ row }">{{ fmt((row as BackupSnapshot).createdAt) }}</template>
          </ElTableColumn>
          <ElTableColumn label="状态" width="100">
            <template #default="{ row }">{{ snapshotStatusLabel((row as BackupSnapshot).status) }}</template>
          </ElTableColumn>
          <ElTableColumn label="到期时间" min-width="160">
            <template #default="{ row }">{{ fmt((row as BackupSnapshot).expiresAt) }}</template>
          </ElTableColumn>
          <ElTableColumn label="操作" width="90">
            <template #default="{ row }">
              <ElButton
                link
                type="danger"
                size="small"
                :loading="deletingId === (row as BackupSnapshot).backupId"
                :disabled="deletingId !== null"
                @click="removeSnapshot(row as BackupSnapshot)"
              >删除</ElButton>
            </template>
          </ElTableColumn>
        </ElTable>
        <ElEmpty v-else description="暂无快照,点击「立即备份」或等待策略按周期自动备份" />
      </ElTabPane>
      <ElTabPane label="操作日志" name="logs">
        <ElEmpty description="暂无操作日志(操作审计由 svc-audit 提供,该端点接入后在此展示)" />
      </ElTabPane>
    </ElTabs>

    <a class="detail-back" href="#" @click.prevent="router.push('/instances')">← 返回策略列表</a>

    <BackupPolicyDialog v-model="policyVisible" :resource-id="id" :attributes="attrs" @saved="onPolicySaved" />
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
.snap-error { color: var(--eu-color-danger); padding: 16px 0; display: flex; align-items: center; gap: 12px; font-size: 13px; }
.detail-back { display: inline-block; margin-top: 16px; color: var(--eu-color-brand); text-decoration: none; font-size: 13px; }
</style>
