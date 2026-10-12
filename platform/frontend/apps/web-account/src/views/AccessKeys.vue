<script setup lang="ts">
/**
 * AccessKey 管理 (02§5 account security; 07§2.2 AK/SK lifecycle).
 * Lists one identity's AccessKey pairs (AK + SecretKey) — the account root,
 * or a RAM user when `ramUserId` is given (RamUsers embeds this view in a
 * dialog). Mirrors the cloud-side limit of 2 AKs per identity, counting
 * enabled AND disabled keys, and the rotation-grace model: rotating an AK
 * mints a replacement while keeping the old one active through a grace window
 * (≤72h, `graceUntil`) so dependent SDK calls don't break mid-migration.
 * SecretKey is surfaced exactly once at creation time and never retrievable
 * again.
 *
 * All lifecycle operations are backed by svc-iam (/api/ak): list, create,
 * disable, enable, rotate, delete. The client never invents an AK id or
 * secret — the server issues both and returns the secret once (07§2.7).
 */
import { computed, onMounted, ref, watch } from "vue";
import {
  ElButton,
  ElDialog,
  ElMessage,
  ElMessageBox,
  ElTable,
  ElTableColumn,
  ElTag,
  ElTooltip,
} from "element-plus";
import { useAccountAuth } from "../stores/auth";
import { formatTime } from "../format";
import {
  akCapReached,
  akCreateBody,
  akErrorMessage,
  akListPath,
  maskAccessKeyId,
  MAX_AKS_PER_IDENTITY,
  NEVER_USED,
  toAccessKeyView,
  type AccessKeyDto,
  type AccessKeyListDto,
  type AccessKeyView,
} from "../accessKeys";

const props = defineProps<{
  /** Manage this RAM user's keys (svc-iam ?ramUserId= / {ramUserId}); absent = the account root. */
  ramUserId?: number;
  /** Rendered inside another page (the RamUsers dialog): no page heading. */
  embedded?: boolean;
}>();

const auth = useAccountAuth();

type AccessKey = AccessKeyView;

const MAX_AKS = MAX_AKS_PER_IDENTITY;

// AccessKeys of one identity (07§2.2). Loaded from svc-iam GET /api/ak
// (returns {accessKeys:[...], total}); created/rotated via POST (the server
// issues the AK/SK pair and returns the secretKey exactly once — 07§2.2).
// Disabled/enabled/deleted via the dedicated endpoints. svc-iam caps keys at
// 2 per identity counting enabled AND disabled ones (create / rotate answer
// 409 Auth.AKLimitExceeded beyond that) — only deleting a key frees a slot.
const accessKeys = ref<AccessKey[]>([]);

async function fetchAccessKeys() {
  if (!auth.isAuthenticated) return;
  try {
    const data = await auth.request<AccessKeyListDto>(akListPath(props.ramUserId));
    accessKeys.value = (data?.accessKeys ?? []).map(toAccessKeyView);
  } catch (e) {
    ElMessage.error(`加载 AccessKey 失败：${akErrorMessage(e)}`);
  }
}

onMounted(fetchAccessKeys);
// The RamUsers dialog reuses one instance across users.
watch(() => props.ramUserId, () => {
  accessKeys.value = [];
  void fetchAccessKeys();
});

const atMax = computed(() => akCapReached(accessKeys.value));
const CAP_HINT = `每个身份最多 ${MAX_AKS_PER_IDENTITY} 个 AccessKey(含已禁用),请先删除不再使用的 AccessKey 后再创建。`;

// --- create / rotate dialog ---
type DialogMode = "create" | "rotate";
const dialogVisible = ref(false);
const dialogMode = ref<DialogMode>("create");
const rotatingTargetId = ref<string | null>(null);
const pendingKey = ref<AccessKey | null>(null);

const dialogTitle = computed(() => (dialogMode.value === "rotate" ? "轮换 AccessKey" : "创建 AccessKey"));

async function openCreate() {
  if (atMax.value) {
    ElMessage.warning(CAP_HINT);
    return;
  }
  // The server issues the AK/SK pair (07§2.2); secretKey is returned exactly
  // once and is never retrievable again, so it is surfaced in this dialog.
  try {
    const created = await auth.request<AccessKeyDto>("/api/ak", {
      method: "POST",
      body: JSON.stringify(akCreateBody(props.ramUserId)),
    });
    const id = created.akId ?? "";
    pendingKey.value = {
      id,
      maskedId: maskAccessKeyId(id),
      secret: created.secretKey ?? "",
      createdAt: formatTime(created.createdAt),
      lastUsed: NEVER_USED,
      status: "active",
      rotationGrace: false,
      graceUntil: "",
    };
    dialogMode.value = "create";
    rotatingTargetId.value = null;
    dialogVisible.value = true;
  } catch (e) {
    ElMessage.error(`创建 AccessKey 失败：${akErrorMessage(e)}`);
    void fetchAccessKeys(); // e.g. a key created elsewhere filled the cap
  }
}

async function openRotate(row: AccessKey) {
  // Rotation mints a 2nd AK while keeping the old one active in a grace window.
  // The backend issues the new AK/SK (07§2.2); the old one is marked grace there.
  if (atMax.value) {
    ElMessage.warning(`轮换会新建一个 AccessKey,已达每个身份 ${MAX_AKS_PER_IDENTITY} 个(含已禁用)的上限;请先删除一个 AccessKey 再轮换。`);
    return;
  }
  try {
    const rotated = await auth.request<AccessKeyDto>(`/api/ak/${row.id}/rotate`, {
      method: "POST",
    });
    const id = rotated.akId ?? "";
    pendingKey.value = {
      id,
      maskedId: maskAccessKeyId(id),
      secret: rotated.secretKey ?? "",
      createdAt: formatTime(rotated.createdAt),
      lastUsed: NEVER_USED,
      status: "active",
      rotationGrace: false,
      graceUntil: "",
    };
    dialogMode.value = "rotate";
    rotatingTargetId.value = row.id;
    dialogVisible.value = true;
  } catch (e) {
    ElMessage.error(`轮换 AccessKey 失败：${akErrorMessage(e)}`);
  }
}

async function copyText(text: string, label: string) {
  try {
    await navigator.clipboard.writeText(text);
    ElMessage.success(`${label}已复制到剪贴板`);
  } catch {
    ElMessage.error("复制失败,请手动选择文本复制");
  }
}

// Dialog confirm: the AK has already been created/rotated on the backend (in
// openCreate/openRotate); this just closes the one-time-secret dialog and
// refreshes the list from the server so the new AK and grace flag appear.
function onDialogConfirm() {
  if (!pendingKey.value) return;
  pendingKey.value = null;
  rotatingTargetId.value = null;
  dialogVisible.value = false;
  ElMessage.success(dialogMode.value === "rotate" ? "新 AccessKey 已创建,旧 AK 进入轮换宽限期" : "AccessKey 创建成功");
  void fetchAccessKeys();
}

// --- row operations ---
async function toggleStatus(row: AccessKey) {
  if (row.rotationGrace) {
    ElMessage.warning("该 AK 处于轮换宽限期,请先完成迁移并删除后再操作。");
    return;
  }
  // Disabled keys already count toward the cap, so enabling never exceeds it.
  const action = row.status === "active" ? "disable" : "enable";
  try {
    await auth.request<unknown>(`/api/ak/${row.id}/${action}`, { method: "POST" });
    ElMessage.success(action === "disable" ? "AccessKey 已禁用" : "AccessKey 已启用");
    await fetchAccessKeys();
  } catch (e) {
    ElMessage.error(`操作失败：${akErrorMessage(e)}`);
  }
}

function deleteKey(row: AccessKey) {
  ElMessageBox.confirm(
    `确定要删除 AccessKey ${row.maskedId} 吗?删除后使用该 AK 的应用将立即无法访问辰云资源,此操作不可恢复。`,
    "删除 AccessKey",
    { type: "warning", confirmButtonText: "删除", cancelButtonText: "取消", confirmButtonClass: "el-button--danger" },
  )
    .then(async () => {
      try {
        await auth.request<unknown>(`/api/ak/${row.id}`, { method: "DELETE" });
        ElMessage.success("AccessKey 已删除");
        await fetchAccessKeys();
      } catch (e) {
        ElMessage.error(`删除失败：${akErrorMessage(e)}`);
      }
    })
    .catch(() => {
      /* user cancelled */
    });
}
</script>

<template>
  <div class="ak-page" :class="{ 'ak-embedded': embedded }">
    <div class="ak-header">
      <div class="ak-heading">
        <h1 v-if="!embedded" class="ak-title">AccessKey 管理</h1>
        <p class="ak-sub">
          AccessKey(AK)用于 API/SDK 访问辰云资源。每个身份最多 {{ MAX_AKS }} 个 AK(含已禁用),请定期轮换并妥善保管
          SecretKey,切勿泄露或提交到代码仓库。
        </p>
        <p v-if="auth.user && !embedded" class="ak-owner">当前账号:{{ auth.user.name }} (ID: {{ auth.user.id }})</p>
        <p v-if="atMax" class="ak-cap-hint">{{ CAP_HINT }}</p>
      </div>

      <el-tooltip
        :content="CAP_HINT"
        placement="top"
        :disabled="!atMax"
      >
        <span class="ak-create-wrap">
          <el-button type="primary" :disabled="atMax" @click="openCreate">创建 AccessKey</el-button>
        </span>
      </el-tooltip>
    </div>

    <section class="ak-table-card">
      <el-table :data="accessKeys" border stripe class="ak-table" empty-text="暂无 AccessKey">
      <el-table-column label="AccessKey ID" min-width="220">
        <template #default="{ row }">
          <span class="ak-id">{{ row.maskedId }}</span>
        </template>
      </el-table-column>

      <el-table-column prop="createdAt" label="创建时间" min-width="170" />

      <el-table-column prop="lastUsed" label="最近使用" min-width="170" />

      <el-table-column label="状态" min-width="150">
        <template #default="{ row }">
          <el-tag v-if="row.status === 'active'" type="success" size="small">启用</el-tag>
          <el-tag v-else type="info" size="small">禁用</el-tag>
          <el-tag v-if="row.rotationGrace" type="warning" size="small" class="ak-grace-tag">轮换宽限期</el-tag>
          <div v-if="row.rotationGrace && row.graceUntil" class="ak-grace-until">宽限期至 {{ row.graceUntil }}</div>
        </template>
      </el-table-column>

      <el-table-column label="操作" width="220" fixed="right">
        <template #default="{ row }">
          <el-button
            v-if="row.status === 'active'"
            link
            type="primary"
            size="small"
            @click="toggleStatus(row as AccessKey)"
          >禁用</el-button>
          <el-button v-else link type="primary" size="small" @click="toggleStatus(row as AccessKey)">启用</el-button>

          <el-button
            v-if="row.status === 'active' && !row.rotationGrace"
            link
            type="primary"
            size="small"
            :disabled="atMax"
            @click="openRotate(row as AccessKey)"
          >轮换</el-button>

          <el-button link type="danger" size="small" @click="deleteKey(row as AccessKey)">删除</el-button>
        </template>
      </el-table-column>
      </el-table>
    </section>

    <el-dialog v-model="dialogVisible" :title="dialogTitle" width="560px" :close-on-click-modal="false" append-to-body>
      <div v-if="pendingKey" class="ak-create-body">
        <div class="ak-alert">
          请妥善保存,SecretKey 仅显示一次,关闭后将无法再次查看。
        </div>

        <div class="ak-field">
          <div class="ak-field-label">AccessKey ID</div>
          <div class="ak-field-value">
            <code class="ak-code">{{ pendingKey.id }}</code>
            <el-button link type="primary" size="small" @click="copyText(pendingKey.id, 'AccessKey ID ')">复制</el-button>
          </div>
        </div>

        <div class="ak-field">
          <div class="ak-field-label">AccessKey Secret</div>
          <div class="ak-field-value">
            <code class="ak-code ak-secret">{{ pendingKey.secret }}</code>
            <el-button link type="primary" size="small" @click="copyText(pendingKey.secret, 'SecretKey ')">复制</el-button>
          </div>
        </div>

        <div v-if="dialogMode === 'rotate'" class="ak-rotate-note">
          轮换后旧 AccessKey 将保持启用状态进入宽限期,请在迁移完成并验证新 AK 可用后删除旧 AK。
        </div>
      </div>

      <template #footer>
        <el-button @click="dialogVisible = false">关闭</el-button>
        <el-button type="primary" @click="onDialogConfirm">我已妥善保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.ak-page {
  padding: var(--eu-spacing-6);
}
.ak-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--eu-spacing-6);
  margin-bottom: var(--eu-spacing-5);
}
.ak-heading {
  flex: 1;
  min-width: 0;
}
.ak-title {
  margin: 0;
  font-size: var(--eu-font-size-xl);
  color: var(--eu-text-primary);
}
.ak-sub {
  margin: var(--eu-spacing-2) 0 0;
  font-size: var(--eu-font-size-sm);
  color: var(--eu-text-secondary);
  line-height: 1.6;
}
.ak-owner {
  margin: var(--eu-spacing-2) 0 0;
  font-size: var(--eu-font-size-xs);
  color: var(--eu-text-secondary);
}
.ak-create-wrap {
  display: inline-block;
}
.ak-table-card {
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  padding: var(--eu-spacing-4);
  overflow: hidden;
}
.ak-table {
  width: 100%;
}
/* Let the glass card show through the Element Plus table body. */
.ak-table-card :deep(.el-table),
.ak-table-card :deep(.el-table tr) {
  --el-bg-color: transparent;
  --el-fill-color-blank: transparent;
  background-color: transparent;
}
.ak-id {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: var(--eu-font-size-sm);
  color: var(--eu-text-primary);
}
.ak-grace-tag {
  margin-left: var(--eu-spacing-1);
}
.ak-grace-until {
  margin-top: var(--eu-spacing-1);
  font-size: var(--eu-font-size-xs);
  color: var(--eu-color-warning-text);
}
.ak-cap-hint {
  margin: var(--eu-spacing-2) 0 0;
  font-size: var(--eu-font-size-sm);
  color: var(--eu-color-warning-text);
}
.ak-page.ak-embedded {
  padding: 0;
}
.ak-create-body {
  display: flex;
  flex-direction: column;
  gap: var(--eu-spacing-4);
}
.ak-alert {
  padding: var(--eu-spacing-3) var(--eu-spacing-4);
  background: var(--eu-color-warning-soft);
  border: 1px solid var(--eu-color-warning);
  border-radius: var(--eu-radius-md);
  color: var(--eu-color-warning-text);
  font-size: var(--eu-font-size-sm);
  line-height: 1.6;
}
.ak-field {
  display: flex;
  flex-direction: column;
  gap: var(--eu-spacing-1);
}
.ak-field-label {
  font-size: var(--eu-font-size-sm);
  color: var(--eu-text-secondary);
}
.ak-field-value {
  display: flex;
  align-items: center;
  gap: var(--eu-spacing-2);
  padding: var(--eu-spacing-2) var(--eu-spacing-3);
  background: var(--eu-bg-page);
  border: 1px solid var(--eu-border);
  border-radius: var(--eu-radius-md);
}
.ak-code {
  flex: 1;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: var(--eu-font-size-sm);
  color: var(--eu-text-primary);
  word-break: break-all;
}
.ak-secret {
  letter-spacing: 0.5px;
}
.ak-rotate-note {
  padding: var(--eu-spacing-2) var(--eu-spacing-3);
  background: var(--eu-bg-page);
  border-radius: var(--eu-radius-sm);
  font-size: var(--eu-font-size-xs);
  color: var(--eu-text-secondary);
  line-height: 1.6;
}
</style>
