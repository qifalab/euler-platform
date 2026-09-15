<script setup lang="ts">
/** 上架审核 — GET /api/v1/marketplace/listings?status=PENDING_APPROVAL + POST /listings/{id}/approve. */
import { ref, onMounted } from "vue";
import { ElMessage, ElMessageBox } from "element-plus";
import { PageHeader } from "@eu/ui";
import { fetchListings, sdk, categoryLabel, bpsPercent, type ListingDTO } from "@/marketplace";

const queue = ref<ListingDTO[]>([]);
const loading = ref(true);
const error = ref<string | null>(null);

async function load() {
  loading.value = true;
  error.value = null;
  try {
    queue.value = await fetchListings("PENDING_APPROVAL");
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}

async function review(l: ListingDTO, approved: boolean) {
  const action = approved ? "通过上架" : "驳回";
  try {
    await ElMessageBox.confirm(`确认${action}「${l.name}」?`, "审核确认", { type: approved ? "success" : "warning" });
  } catch { return; }
  try {
    await sdk.post<ListingDTO>(`/api/v1/marketplace/listings/${l.listingId}/approve`, {
      approved,
      reviewNote: approved ? "资质与 OpenAPI 联调通过" : "履约接口不符合规范",
    });
    ElMessage.success(`已${action}`);
    void load();
  } catch (e) {
    ElMessage.error(`审核失败:${(e as Error).message}`);
  }
}

onMounted(() => void load());
</script>

<template>
  <div class="review">
    <PageHeader title="上架审核" />
    <p class="rv-hint">审核只能作用于待审队列;已通过或已驳回的商品不可重复审批。驳回需给出整改意见。</p>
    <p v-if="error" class="rv-error">加载失败:{{ error }}(请确认 svc-marketplace 在 :9212)</p>
    <p v-else-if="loading" class="rv-loading">加载中…</p>
    <div v-else-if="queue.length" class="rv-table-card">
      <table class="rv-table">
        <thead><tr><th>ID</th><th>商品名称</th><th>类别</th><th>分成</th><th>履约 OpenAPI</th><th>提交时间</th><th>操作</th></tr></thead>
        <tbody>
          <tr v-for="l in queue" :key="l.listingId">
            <td>{{ l.listingId }}</td>
            <td>{{ l.name }}</td>
            <td>{{ categoryLabel(l.category) }}</td>
            <td>{{ bpsPercent(l.partnerRateBps) }}</td>
            <td class="rv-url">{{ l.openapiUrl }}</td>
            <td>{{ l.createdAt ? l.createdAt.slice(0, 10) : "" }}</td>
            <td class="rv-actions">
              <button class="rv-btn approve" @click="review(l, true)">通过</button>
              <button class="rv-btn reject" @click="review(l, false)">驳回</button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-else class="rv-empty">待审队列为空——伙伴在「发布商品」提交后,商品会进入这里。</p>
  </div>
</template>

<style scoped>
.review { padding: var(--eu-spacing-6); max-width: 1200px; }
.rv-hint { color: var(--eu-text-secondary); font-size: 13px; margin: 0 0 var(--eu-spacing-4); }
.rv-table-card {
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  overflow: hidden;
}
.rv-table { width: 100%; border-collapse: collapse; background: transparent; }
.rv-table th, .rv-table td { padding: var(--eu-spacing-3) var(--eu-spacing-4); text-align: left; border-bottom: 1px solid var(--eu-border); }
.rv-table th { background: var(--eu-glass-bg-soft); color: var(--eu-text-secondary); font-size: 12px; font-weight: 500; }
.rv-table td { font-size: 13px; color: var(--eu-text-primary); }
.rv-table tbody tr { transition: background var(--eu-transition); }
.rv-table tbody tr:hover { background: var(--eu-color-brand-soft); }
.rv-table tbody tr:last-child td { border-bottom: none; }
.rv-url { max-width: 260px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.rv-actions { display: flex; gap: var(--eu-spacing-2); }
.rv-btn {
  padding: 5px 12px; border-radius: var(--eu-radius-sm); border: 1px solid transparent;
  font-size: 12px; cursor: pointer; transition: all var(--eu-transition);
}
.rv-btn.approve { background: var(--eu-color-brand); color: var(--eu-color-on-brand); }
.rv-btn.approve:hover { background: var(--eu-color-brand-hover); }
.rv-btn.reject { background: transparent; border-color: var(--eu-color-danger); color: var(--eu-color-danger); }
.rv-btn.reject:hover { background: var(--eu-color-danger-soft); }
.rv-error, .rv-loading, .rv-empty { color: var(--eu-text-secondary); padding: var(--eu-spacing-6); }
.rv-error { color: var(--eu-color-danger); }
</style>
