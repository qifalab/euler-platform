<script setup lang="ts">
/** Ticket list (02§1.2). Wired to svc-ticket GET /api/v1/tickets. */
import { ref, computed, onMounted } from "vue";
import { useRouter } from "vue-router";
import { ElButton } from "element-plus";
import { createSDK } from "@eu/sdk";
import { PageHeader } from "@eu/ui";
import { useTicketMeta } from "@/useTicketMeta";
const router = useRouter();
const sdk = createSDK({ baseURL: "" });
// 枚举来自 svc-ticket /api/v1/tickets/meta(模块级缓存,与 CreateTicket 共享一次请求)。
const { priorities, statuses, ready } = useTicketMeta();

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

// 枚举 value→label 派生映射,查不到回退原值显示。
const priorityLabel = computed(() => new Map(priorities.value.map((p) => [p.value, p.label] as const)));
const statusLabel = computed(() => new Map(statuses.value.map((s) => [s.value, s.label] as const)));
// 优先级 tag 颜色纯视觉映射,保留。
const PRIORITY_CLASS: Record<string, string> = { 紧急: "p-urgent", 普通: "p-normal", 低: "p-normal" };

function toRow(t: TicketDTO): Row {
  return {
    id: t.ticket_id,
    title: t.message || t.category,
    type: t.category,
    priority: priorityLabel.value.get(t.priority) ?? t.priority,
    state: statusLabel.value.get(t.status) ?? t.status,
    created: t.created_at ? t.created_at.slice(0, 10) : "",
  };
}

onMounted(async () => {
  try {
    await ready; // 先等枚举就绪(内部已容错),再拉列表保证 label 就位
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
.ticket-list { padding: var(--eu-spacing-6); max-width: 1200px; }
.tl-table-card {
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  overflow: hidden;
}
.tl-table { width: 100%; border-collapse: collapse; background: transparent; }
.tl-table th, .tl-table td { padding: var(--eu-spacing-3) var(--eu-spacing-4); text-align: left; border-bottom: 1px solid var(--eu-border); }
.tl-table th { background: var(--eu-glass-bg-soft); color: var(--eu-text-secondary); font-size: 12px; font-weight: 500; }
.tl-table td { font-size: 13px; color: var(--eu-text-primary); }
.tl-table tbody tr { transition: background var(--eu-transition); }
.tl-table tbody tr:hover { background: var(--eu-color-brand-soft); }
.tl-table tbody tr:last-child td { border-bottom: none; }
.tl-priority { font-size: 12px; padding: 2px 8px; border-radius: var(--eu-radius-sm); }
.p-urgent { color: var(--eu-color-danger); background: var(--eu-color-danger-soft); }
.p-normal { color: var(--eu-text-secondary); background: var(--eu-glass-bg-soft); }
.tl-error, .tl-loading, .tl-empty { color: var(--eu-text-secondary); padding: var(--eu-spacing-6); }
.tl-error { color: var(--eu-color-danger); }
</style>
