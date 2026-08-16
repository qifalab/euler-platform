<script setup lang="ts">
/**
 * RAM 角色列表 (07§3.2 RBAC skeleton, M-7.6).
 * Lists the roles under the account and provides create CRUD. A role groups
 * policy names; the 策略模拟器 (PolicySimulator.vue) evaluates requests against
 * the named policies' documents.
 */
import { onMounted, reactive, ref } from "vue";
import {
  ElButton,
  ElDialog,
  ElEmpty,
  ElForm,
  ElFormItem,
  ElInput,
  ElMessage,
  ElTable,
  ElTableColumn,
  ElTag,
  type FormInstance,
  type FormRules,
} from "element-plus";
import { useAccountAuth } from "../stores/auth";

const auth = useAccountAuth();

interface RamRole {
  id: number;
  name: string;
  description: string;
  policies: string[];
  status: number;
  createdAt: string;
}

const roles = reactive<RamRole[]>([]);

interface RoleDto {
  id?: number;
  name: string;
  description?: string;
  policies?: string[];
  status?: number;
  createdAt?: string;
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
  return auth.accessToken ? { Authorization: `Bearer ${auth.accessToken}` } : {};
}

async function fetchRoles() {
  if (!auth.isAuthenticated) return;
  try {
    const data = await readEnvelope<{ roles: RoleDto[]; total: number }>(
      await fetch("/api/ram/roles", { headers: authHeaders() }),
    );
    roles.splice(0, roles.length, ...(data?.roles ?? []).map((r) => ({
      id: r.id ?? 0,
      name: r.name,
      description: r.description ?? "",
      policies: r.policies ?? [],
      status: r.status ?? 1,
      createdAt: r.createdAt ?? "—",
    })));
  } catch (e) {
    ElMessage.error(`加载 RAM 角色失败：${(e as Error).message}`);
  }
}

onMounted(fetchRoles);

const dialogVisible = ref(false);
const defaultForm = () => ({ name: "", description: "", policiesText: "" });
const form = reactive(defaultForm());
const formRef = ref<FormInstance>();

const rules: FormRules = {
  name: [
    { required: true, message: "请输入角色名", trigger: "blur" },
    {
      pattern: /^[a-zA-Z][a-zA-Z0-9_-]{2,30}$/,
      message: "字母开头，3-31 位字母、数字、下划线或连字符",
      trigger: "blur",
    },
  ],
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
  const policies = form.policiesText
    .split(/[,\n\s]+/)
    .map((s) => s.trim())
    .filter(Boolean);
  try {
    const created = await readEnvelope<RoleDto>(
      await fetch("/api/ram/roles", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...authHeaders() },
        body: JSON.stringify({ name: form.name, description: form.description, policies }),
      }),
    );
    roles.push({
      id: created.id ?? 0,
      name: created.name ?? form.name,
      description: created.description ?? form.description,
      policies: created.policies ?? policies,
      status: created.status ?? 1,
      createdAt: created.createdAt ?? "—",
    });
    dialogVisible.value = false;
    ElMessage.success(`角色「${form.name}」已创建`);
  } catch (e) {
    ElMessage.error(`创建失败：${(e as Error).message}`);
  }
}
</script>

<template>
  <div class="ram-roles-page">
    <ElEmpty v-if="!auth.isAuthenticated" description="请先登录" />

    <template v-else>
      <header class="page-header">
        <div class="heading">
          <h1 class="page-title">RAM 角色</h1>
          <p class="page-desc">
            管理主账号下的角色（用户 → 角色 → 策略）。策略模拟见「策略模拟器」，可
            在提交前验证"主体 X 是否可对资源 Z 执行操作 Y"。
          </p>
        </div>
        <ElButton type="primary" @click="openCreate">创建角色</ElButton>
      </header>

      <section class="table-card">
        <ElTable :data="roles" stripe style="width: 100%">
          <ElTableColumn prop="name" label="角色名" min-width="140" />
          <ElTableColumn prop="description" label="描述" min-width="160" show-overflow-tooltip />
          <ElTableColumn label="策略" min-width="200">
            <template #default="{ row }">
              <ElTag
                v-for="p in (row as RamRole).policies"
                :key="p"
                size="small"
                effect="light"
                style="margin-right: 6px"
              >
                {{ p }}
              </ElTag>
              <span v-if="(row as RamRole).policies.length === 0" style="color: var(--sc-text-disabled)">—</span>
            </template>
          </ElTableColumn>
          <ElTableColumn prop="createdAt" label="创建时间" min-width="160" />
        </ElTable>
      </section>

      <ElDialog v-model="dialogVisible" title="创建角色" width="480px" :close-on-click-modal="false">
        <ElForm ref="formRef" :model="form" :rules="rules" label-width="92px" label-position="right">
          <ElFormItem label="角色名" prop="name">
            <ElInput v-model="form.name" placeholder="字母开头，3-31 位字母数字_-" autocomplete="off" />
          </ElFormItem>
          <ElFormItem label="描述" prop="description">
            <ElInput v-model="form.description" type="textarea" :rows="2" placeholder="选填" />
          </ElFormItem>
          <ElFormItem label="附加策略" prop="policiesText">
            <ElInput
              v-model="form.policiesText"
              type="textarea"
              :rows="3"
              placeholder="策略名，逗号或换行分隔，如 ScEcsFullAccess"
            />
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
.ram-roles-page {
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
.heading { display: flex; flex-direction: column; gap: 6px; min-width: 0; }
.page-title { margin: 0; font-size: 22px; font-weight: 600; color: var(--sc-text-primary); }
.page-desc { margin: 0; font-size: 13px; color: var(--sc-text-secondary); line-height: 1.5; }
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
.table-card :deep(.el-table),
.table-card :deep(.el-table tr) {
  --el-bg-color: transparent;
  --el-fill-color-blank: transparent;
  background-color: transparent;
}
</style>
