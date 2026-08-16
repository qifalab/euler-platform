<script setup lang="ts">
/**
 * RAM 用户列表 (02§5, 07§2.2 子账号).
 * Lists RAM sub-accounts under the principal and provides create/delete demo
 * CRUD. AccessKey management (incl. the max-2-AK rule) lives in AccessKeys;
 * this view only manages the user identity rows.
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

const auth = useAccountAuth();

interface RamUser {
  id: number;
  username: string;
  displayName: string;
  remark: string;
  createdAt: string;
  status: "active" | "disabled";
  access: ("password" | "program")[];
}

// RAM sub-accounts under the principal (07§2.2). Loaded from svc-iam
// GET /api/ram/users; created/deleted via POST/DELETE on the same path.
const users = reactive<RamUser[]>([]);

interface RamUserDto {
  id?: number;
  username: string;
  displayName: string;
  remark?: string;
  createdAt?: string;
  status?: "active" | "disabled";
  access?: ("password" | "program")[];
}

// Unwrap the platform envelope {RequestId,Code,Message,Data} (03§9.3).
async function readEnvelope<T>(res: Response): Promise<T> {
  const body = await res.json();
  if (!res.ok || body.Code !== "OK") {
    throw new Error(body.Message ?? `request failed (HTTP ${res.status})`);
  }
  return body.Data as T;
}

function authHeaders(): HeadersInit {
  return auth.accessToken
    ? { Authorization: `Bearer ${auth.accessToken}` }
    : {};
}

async function fetchUsers() {
  if (!auth.isAuthenticated) return;
  try {
    const data = await readEnvelope<RamUserDto[]>(await fetch("/api/ram/users", { headers: authHeaders() }));
    users.splice(0, users.length, ...(data ?? []).map((u) => ({
      id: u.id ?? 0,
      username: u.username,
      displayName: u.displayName,
      remark: u.remark ?? "",
      createdAt: u.createdAt ?? "—",
      status: u.status ?? "active",
      access: u.access ?? [],
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
    const created = await readEnvelope<RamUserDto>(
      await fetch("/api/ram/users", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...authHeaders() },
        body: JSON.stringify({
          username: form.username,
          displayName: form.displayName,
          remark: form.remark,
          access: form.access,
        }),
      }),
    );
    users.push({
      id: created.id ?? 0,
      username: created.username ?? form.username,
      displayName: created.displayName ?? form.displayName,
      remark: created.remark ?? form.remark,
      createdAt: created.createdAt ?? "—",
      status: created.status ?? "active",
      access: created.access ?? [...form.access],
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
    await readEnvelope<unknown>(
      await fetch(`/api/ram/users/${row.id}`, {
        method: "DELETE",
        headers: authHeaders(),
      }),
    );
    const idx = users.findIndex((u) => u.id === row.id);
    if (idx !== -1) users.splice(idx, 1);
    ElMessage.success(`已删除「${row.displayName}」`);
  } catch (e) {
    ElMessage.error(`删除失败：${(e as Error).message}`);
  }
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
            管理主账号下的 RAM 子用户身份；AccessKey 与最多 2 组密钥规则见「AccessKey
            管理」。
          </p>
        </div>
        <ElButton type="primary" @click="openCreate">创建 RAM 用户</ElButton>
      </header>

      <section class="table-card">
        <ElTable :data="users" stripe style="width: 100%">
          <ElTableColumn prop="username" label="用户名" min-width="140" />
          <ElTableColumn prop="displayName" label="显示名" min-width="120" />
          <ElTableColumn prop="remark" label="备注" min-width="200" show-overflow-tooltip />
          <ElTableColumn prop="createdAt" label="创建时间" min-width="160" />
          <ElTableColumn label="状态" width="100">
            <template #default="{ row }">
              <ElTag :type="statusType(row.status)" size="small" effect="light">
                {{ statusText(row.status) }}
              </ElTag>
            </template>
          </ElTableColumn>
          <ElTableColumn label="操作" width="100" fixed="right">
            <template #default="{ row }">
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
    </template>
  </div>
</template>

<style scoped>
.ram-users-page {
  padding: var(--sc-spacing-6);
  min-height: calc(100vh - var(--sc-topbar-height));
}
.page-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--sc-spacing-4);
  margin-bottom: var(--sc-spacing-6);
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
  color: var(--sc-text-primary);
}
.page-desc {
  margin: 0;
  font-size: 13px;
  color: var(--sc-text-secondary);
  line-height: 1.5;
}
.table-card {
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  box-shadow: var(--sc-shadow-sm);
  padding: var(--sc-spacing-4);
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
