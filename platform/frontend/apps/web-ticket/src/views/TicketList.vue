<script setup lang="ts">
/** Ticket list (02§1.2). Wired to svc-ticket GET /api/v1/tickets. */
import { ref, onMounted } from "vue";
import { useRouter } from "vue-router";
import { ElButton } from "element-plus";
import { createSDK } from "@sc/sdk";
import { PageHeader } from "@sc/ui";
const router = useRouter();
const sdk = createSDK({ baseURL: "" });

// Backend ticketDTO from svc-ticket /api/v1/tickets.
interface TicketDTO {
  ticket_id: string;
  category: string;
  priority: string; // HIGH / NORMAL / LOW
  status: string;   // OPEN / PROCESSING / WAITING_REPLY / CLOSED
  assignee: string;
  sla_deadline: string;
  created_at: string;
  updated_at: string;
  message: string; // first thread message (acts as the title)
}
interface TicketsEnvelope { tickets: TicketDTO[] }

const tickets = ref<Row[]>([]);
const loading = ref(true);
const error = ref<string | null>(null);

// Row maps backend fields onto the existing table columns.
interface Row {
  id: string; title: string; type: string;
  priority: string; state: string; created: string;
}

// Priority/status labels mapped to the existing (Chinese) display values.
const PRIORITY_LABEL: Record<string, string> = { HIGH: "紧急", NORMAL: "普通", LOW: "低" };
const STATUS_LABEL: Record<string, string> = {
  OPEN: "待处理", PROCESSING: "处理中", WAITING_REPLY: "待回复", CLOSED: "已关闭",
};
const PRIORITY_CLASS: Record<string, string> = { 紧急: "p-urgent", 普通: "p-normal", 低: "p-normal" };

function toRow(t: TicketDTO): Row {
  return {
    id: t.ticket_id,
    title: t.message || t.category,
    type: t.category,
    priority: PRIORITY_LABEL[t.priority] ?? t.priority,
    state: STATUS_LABEL[t.status] ?? t.status,
    created: t.created_at ? t.created_at.slice(0, 10) : "",
  };
}

onMounted(async () => {
  try {
    const res = await sdk.get<TicketsEnvelope>("/api/v1/tickets");
    tickets.value = (res.data?.tickets ?? []).map(toRow);
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
});
</script>

<template>
  <div class="ticket-list">
    <PageHeader title="我的工单">
      <template #actions>
        <ElButton type="primary" @click="router.push('/create')">提交工单</ElButton>
      </template>
    </PageHeader>
    <p v-if="error" class="tl-error">加载失败:{{ error }}(请确认 svc-ticket 在 :9209)</p>
    <p v-else-if="loading" class="tl-loading">加载中…</p>
    <div v-else-if="tickets.length" class="tl-table-card">
      <table class="tl-table">
        <thead><tr><th>工单号</th><th>标题</th><th>类型</th><th>优先级</th><th>状态</th><th>提交时间</th></tr></thead>
        <tbody>
          <tr v-for="t in tickets" :key="t.id">
            <td>{{ t.id }}</td><td>{{ t.title }}</td><td>{{ t.type }}</td>
            <td><span class="tl-priority" :class="PRIORITY_CLASS[t.priority]">{{ t.priority }}</span></td>
            <td>{{ t.state }}</td><td>{{ t.created }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-else class="tl-empty">暂无工单</p>
  </div>
</template>

<style scoped>
.ticket-list { padding: var(--sc-spacing-6); max-width: 1200px; }
.tl-table-card {
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  box-shadow: var(--sc-shadow-sm);
  overflow: hidden;
}
.tl-table { width: 100%; border-collapse: collapse; background: transparent; }
.tl-table th, .tl-table td { padding: var(--sc-spacing-3) var(--sc-spacing-4); text-align: left; border-bottom: 1px solid var(--sc-border); }
.tl-table th { background: var(--sc-glass-bg-soft); color: var(--sc-text-secondary); font-size: 12px; font-weight: 500; }
.tl-table td { font-size: 13px; color: var(--sc-text-primary); }
.tl-table tbody tr { transition: background var(--sc-transition); }
.tl-table tbody tr:hover { background: var(--sc-color-brand-soft); }
.tl-table tbody tr:last-child td { border-bottom: none; }
.tl-priority { font-size: 12px; padding: 2px 8px; border-radius: var(--sc-radius-sm); }
.p-urgent { color: var(--sc-color-danger); background: var(--sc-color-danger-soft); }
.p-normal { color: var(--sc-text-secondary); background: var(--sc-glass-bg-soft); }
.tl-error, .tl-loading, .tl-empty { color: var(--sc-text-secondary); padding: var(--sc-spacing-6); }
.tl-error { color: var(--sc-color-danger); }
</style>
