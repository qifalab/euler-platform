<script setup lang="ts">
/**
 * RAM 用户列表 (02§5, 07§2.2 子账号).
 * Lists RAM sub-accounts under the principal and provides create/delete demo
 * CRUD. Each user's AccessKeys (svc-iam /api/ak?ramUserId= — 2 per identity,
 * disabled keys included) are managed in a dialog that embeds the
 * AccessKeys view scoped to that user.
 */
import { onMounted, reactive, ref } from "vue";
import {
  ElButton,
  ElCheckbox,
  ElCheckboxGroup,
  ElDialog,
  ElEmpty,
  ElForm,
  ElFormItem,
  ElInput,
  ElMessage,
  ElMessageBox,
  ElTable,
  ElTableColumn,
  ElTag,
  type FormInstance,
  type FormRules,
} from "element-plus";
import { useAccountAuth } from "../stores/auth";
import { formatTime } from "../format";
import AccessKeys from "./AccessKeys.vue";

const auth = useAccountAuth();

interface RamUser {
  id: number;
  username: string;
  displayName: string;
  remark: string;
  createdAt: string;
  status: "active" | "disabled";
  access: ("password" | "program")[];
  /** Attached policy names (the user's effective policy set). */
  policies: string[];
}

// RAM sub-accounts under the principal (07§2.2). Loaded from svc-iam
// GET /api/ram/users; created/deleted via POST/DELETE on the same path.
const users = reactive<RamUser[]>([]);

// svc-iam wire shape: GET returns {users, total}; status is the account enum
// (1 enabled, 2 disabled; soft-deleted users are not listed).
interface RamUserDto {
  id?: number;
  username: string;
  displayName: string;
  remark?: string;
  createdAt?: string;
  status?: number;
  policies?: string[] | null;
  access?: ("password" | "program")[];
}
interface RamUserListDto { users?: RamUserDto[] | null; total?: number }

function toStatus(s: number | undefined): RamUser["status"] {
  return (s ?? 1) === 1 ? "active" : "disabled";
}

async function fetchUsers() {
  if (!auth.isAuthenticated) return;
  try {
    const data = await auth.request<RamUserListDto>("/api/ram/users");
    users.splice(0, users.length, ...(data?.users ?? []).map((u) => ({
      id: u.id ?? 0,
      username: u.username,
      displayName: u.displayName,
      remark: u.remark ?? "",
      createdAt: formatTime(u.createdAt),
      status: toStatus(u.status),
      access: u.access ?? [],
      policies: u.policies ?? [],
    })));
  } catch (e) {
    ElMessage.error(`加载 RAM 用户失败：${(e as Error).message}`);
  }
}

onMounted(fetchUsers);

const dialogVisible = ref(false);

const defaultForm = (): {
  username: string;
  displayName: string;
  remark: string;
  access: ("password" | "program")[];
} => ({
  username: "",
  displayName: "",
  remark: "",
  access: ["password"],
});

const form = reactive(defaultForm());
const formRef = ref<FormInstance>();

const rules: FormRules = {
  username: [
    { required: true, message: "请输入用户名", trigger: "blur" },
    {
      pattern: /^[a-zA-Z][a-zA-Z0-9_-]{2,30}$/,
      message: "字母开头，3-31 位字母、数字、下划线或连字符",
      trigger: "blur",
    },
  ],
  displayName: [{ required: true, message: "请输入显示名", trigger: "blur" }],
};

function openCreate() {
  Object.assign(form, defaultForm());
  dialogVisible.value = true;
}

async function submitCreate() {
  if (!formRef.value) return;
  try {
    await formRef.value.validate();
  } catch {
    return;
  }
  try {
    const created = await auth.request<RamUserDto>("/api/ram/users", {
      method: "POST",
      body: JSON.stringify({
        username: form.username,
        displayName: form.displayName,
        remark: form.remark,
        access: form.access,
      }),
    });
    users.push({
      id: created.id ?? 0,
      username: created.username ?? form.username,
      displayName: created.displayName ?? form.displayName,
      remark: created.remark ?? form.remark,
      createdAt: formatTime(created.createdAt),
      status: toStatus(created.status),
      access: created.access ?? [...form.access],
      policies: created.policies ?? [],
    });
    dialogVisible.value = false;
    ElMessage.success(`RAM 用户「${form.displayName}」已创建`);
  } catch (e) {
    ElMessage.error(`创建失败：${(e as Error).message}`);
  }
}

async function removeUser(row: RamUser) {
  try {
    await ElMessageBox.confirm(
      `确定删除 RAM 用户「${row.displayName}」( ${row.username} )？该操作不可恢复。`,
      "删除 RAM 用户",
      {
        type: "warning",
        confirmButtonText: "删除",
        cancelButtonText: "取消",
        confirmButtonClass: "el-button--danger",
      },
    );
  } catch {
    return;
  }
  try {
    await auth.request<unknown>(`/api/ram/users/${row.id}`, {
      method: "DELETE",
    });
    const idx = users.findIndex((u) => u.id === row.id);
    if (idx !== -1) users.splice(idx, 1);
    ElMessage.success(`已删除「${row.displayName}」`);
  } catch (e) {
    ElMessage.error(`删除失败：${(e as Error).message}`);
  }
}

// Per-user AccessKeys: the AccessKeys view, scoped by ramUserId.
const akUser = ref<RamUser | null>(null);
const akDialogVisible = ref(false);
function openAccessKeys(row: RamUser) {
  akUser.value = row;
  akDialogVisible.value = true;
}

function statusText(s: RamUser["status"]) {
  return s === "active" ? "启用" : "禁用";
}
function statusType(s: RamUser["status"]) {
  return s === "active" ? "success" : "info";
}
function accessText(a: RamUser["access"]) {
  const labels: string[] = [];
  if (a.includes("password")) labels.push("控制台密码");
  if (a.includes("program")) labels.push("编程访问");
  return labels.join("、");
}
</script>

<template>
  <div class="ram-users-page">
    <ElEmpty v-if="!auth.isAuthenticated" description="请先登录" />

    <template v-else>
      <header class="page-header">
        <div class="heading">
          <h1 class="page-title">RAM 用户</h1>
          <p class="page-desc">
            管理主账号下的 RAM 子用户身份；每个子用户可单独管理 AccessKey（每个身份最多 2
            个，含已禁用）。
          </p>
        </div>
        <ElButton type="primary" @click="openCreate">创建 RAM 用户</ElButton>
      </header>

      <section class="table-card">
        <ElTable :data="users" stripe style="width: 100%">
          <ElTableColumn prop="username" label="用户名" min-width="140" />
          <ElTableColumn prop="displayName" label="显示名" min-width="120" />
          <ElTableColumn prop="remark" label="备注" min-width="200" show-overflow-tooltip />
          <ElTableColumn label="策略" min-width="160" show-overflow-tooltip>
            <template #default="{ row }">
              {{ (row as RamUser).policies.length ? (row as RamUser).policies.join("、") : "—" }}
            </template>
          </ElTableColumn>
          <ElTableColumn prop="createdAt" label="创建时间" min-width="160" />
          <ElTableColumn label="状态" width="100">
            <template #default="{ row }">
              <ElTag :type="statusType(row.status)" size="small" effect="light">
                {{ statusText(row.status) }}
              </ElTag>
            </template>
          </ElTableColumn>
          <ElTableColumn label="操作" width="170" fixed="right">
            <template #default="{ row }">
              <ElButton
                link
                type="primary"
                size="small"
                @click="openAccessKeys(row as RamUser)"
              >
                AccessKey
              </ElButton>
              <ElButton
                link
                type="danger"
                size="small"
                @click="removeUser(row as RamUser)"
              >
                删除
              </ElButton>
            </template>
          </ElTableColumn>
        </ElTable>
      </section>

      <ElDialog
        v-model="dialogVisible"
        title="创建 RAM 用户"
        width="480px"
        :close-on-click-modal="false"
      >
        <ElForm
          ref="formRef"
          :model="form"
          :rules="rules"
          label-width="92px"
          label-position="right"
        >
          <ElFormItem label="用户名" prop="username">
            <ElInput
              v-model="form.username"
              placeholder="字母开头，3-31 位字母数字_-"
              autocomplete="off"
            />
          </ElFormItem>
          <ElFormItem label="显示名" prop="displayName">
            <ElInput v-model="form.displayName" placeholder="便于识别的名称" />
          </ElFormItem>
          <ElFormItem label="备注" prop="remark">
            <ElInput
              v-model="form.remark"
              type="textarea"
              :rows="3"
              placeholder="选填，说明该 RAM 用户的用途"
            />
          </ElFormItem>
          <ElFormItem label="访问方式" prop="access">
            <ElCheckboxGroup v-model="form.access">
              <ElCheckbox value="password">控制台密码访问</ElCheckbox>
              <ElCheckbox value="program">编程访问（OpenAPI AccessKey）</ElCheckbox>
            </ElCheckboxGroup>
          </ElFormItem>
        </ElForm>
        <template #footer>
          <ElButton @click="dialogVisible = false">取消</ElButton>
          <ElButton type="primary" @click="submitCreate">确定创建</ElButton>
        </template>
      </ElDialog>

      <ElDialog
        v-model="akDialogVisible"
        :title="akUser ? `AccessKey — ${akUser.displayName}（${akUser.username}）` : 'AccessKey'"
        width="880px"
        destroy-on-close
      >
        <p v-if="akUser && akUser.status !== 'active'" class="ak-user-hint">该子用户已禁用，不能新建 AccessKey；已有的 AccessKey 仍可禁用或删除。</p>
        <AccessKeys v-if="akUser" :ram-user-id="akUser.id" embedded />
      </ElDialog>
    </template>
  </div>
</template>

<style scoped>
.ram-users-page {
  padding: var(--eu-spacing-6);
  min-height: calc(100vh - var(--eu-topbar-height));
}
.page-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--eu-spacing-4);
  margin-bottom: var(--eu-spacing-6);
}
.heading {
  display: flex;
  flex-direction: column;
  gap: 6px;
  min-width: 0;
}
.page-title {
  margin: 0;
  font-size: 22px;
  font-weight: 600;
  color: var(--eu-text-primary);
}
.ak-user-hint {
  margin: 0 0 var(--eu-spacing-3);
  font-size: 13px;
  color: var(--eu-color-warning-text);
}
.page-desc {
  margin: 0;
  font-size: 13px;
  color: var(--eu-text-secondary);
  line-height: 1.5;
}
.table-card {
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  padding: var(--eu-spacing-4);
  overflow: hidden;
}
/* Let the glass card show through the Element Plus table body. */
.table-card :deep(.el-table),
.table-card :deep(.el-table tr) {
  --el-bg-color: transparent;
  --el-fill-color-blank: transparent;
  background-color: transparent;
}
</style>
