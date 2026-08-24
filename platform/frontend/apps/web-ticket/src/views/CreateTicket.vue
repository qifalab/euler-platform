<script setup lang="ts">
/** Create ticket form (02§1.2). Posts to svc-ticket POST /api/v1/tickets.
 *  下拉枚举来自 GET /api/v1/tickets/meta,option.value 即后端枚举值。 */
import { ref } from "vue";
import { useRouter } from "vue-router";
import { ElMessage } from "element-plus";
import { createSDK } from "@sc/sdk";
import { useTicketMeta } from "@/useTicketMeta";
const router = useRouter();
const sdk = createSDK({ baseURL: "" });
const { categories, priorities, ready } = useTicketMeta();

const form = ref({ type: "", priority: "", title: "", desc: "", contact: "" });
const submitting = ref(false);

// 枚举就绪后对齐表单初值,防止提交空值(meta 失败则留空由提交校验兜底)。
ready.then(() => {
  if (!form.value.type && categories.value.length) form.value.type = categories.value[0].value;
  if (!form.value.priority && priorities.value.length) form.value.priority = priorities.value[0].value;
});

async function submit() {
  if (!form.value.type || !form.value.priority) { ElMessage.warning("请选择工单类型与优先级"); return; }
  if (!form.value.title || !form.value.desc) { ElMessage.warning("请填写标题和描述"); return; }
  submitting.value = true;
  try {
    const res = await sdk.post<{ ticket_id: string }>("/api/v1/tickets", {
      category: form.value.type,
      priority: form.value.priority,
      title: form.value.title,
      message: form.value.desc,
    });
    const id = res.data?.ticket_id ?? "";
    ElMessage.success(`工单已提交,编号 ${id}`);
    router.push("/list");
  } catch (e) {
    ElMessage.error(`提交失败:${(e as Error).message}`);
  } finally {
    submitting.value = false;
  }
}
</script>

<template>
  <div class="create-ticket">
    <h1>提交工单</h1>
    <form class="ct-form" @submit.prevent="submit">
      <label class="ct-field"><span>工单类型</span>
        <select v-model="form.type">
          <option v-for="opt in categories" :key="opt.value" :value="opt.value">{{ opt.label }}</option>
        </select>
      </label>
      <label class="ct-field"><span>优先级</span>
        <select v-model="form.priority">
          <option v-for="opt in priorities" :key="opt.value" :value="opt.value">{{ opt.label }}</option>
        </select>
      </label>
      <label class="ct-field"><span>标题</span>
        <input v-model="form.title" placeholder="简要描述问题" />
      </label>
      <label class="ct-field"><span>问题描述</span>
        <textarea v-model="form.desc" rows="6" placeholder="详细描述问题现象、复现步骤与期望结果"></textarea>
      </label>
      <label class="ct-field"><span>联系方式</span>
        <input v-model="form.contact" placeholder="手机或邮箱" />
      </label>
      <button class="ct-submit" type="submit" :disabled="submitting">{{ submitting ? "提交中…" : "提交" }}</button>
    </form>
  </div>
</template>

<style scoped>
.create-ticket { padding: var(--sc-spacing-6); max-width: 720px; }
.create-ticket h1 { font-size: 20px; font-weight: 500; color: var(--sc-text-primary); margin: 0 0 var(--sc-spacing-5); }
.ct-form {
  display: flex;
  flex-direction: column;
  gap: var(--sc-spacing-4);
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  box-shadow: var(--sc-shadow-sm);
  padding: var(--sc-spacing-6);
}
.ct-field { display: flex; flex-direction: column; gap: 6px; font-size: 13px; color: var(--sc-text-secondary); }
.ct-field input, .ct-field select, .ct-field textarea {
  padding: 10px 12px;
  background: var(--sc-bg-container);
  border: 1px solid var(--sc-border);
  border-radius: var(--sc-radius-md);
  font-size: 14px;
  color: var(--sc-text-primary);
  outline: none;
  font-family: inherit;
  transition: border-color var(--sc-transition), box-shadow var(--sc-transition);
}
.ct-field input:focus, .ct-field select:focus, .ct-field textarea:focus {
  border-color: var(--sc-color-brand);
  box-shadow: 0 0 0 3px var(--sc-color-brand-soft);
}
.ct-submit {
  align-self: flex-start; padding: 11px 24px; border: none; border-radius: var(--sc-radius-md);
  background: var(--sc-color-brand); color: var(--sc-text-on-brand); font-size: 14px; cursor: pointer;
  transition: background var(--sc-transition);
}
.ct-submit:hover { background: var(--sc-color-brand-hover); }
.ct-submit:disabled { opacity: 0.6; cursor: not-allowed; }
</style>
