<script setup lang="ts">
/** 订单结算 — POST /api/v1/marketplace/settlements (分账: 伙伴/平台按 bps 拆分). */
import { ref, onMounted } from "vue";
import { useRoute } from "vue-router";
import { ElMessage } from "element-plus";
import { PageHeader } from "@sc/ui";
import { sdk, fetchListings, yuan, bpsPercent, type ListingDTO, type SettlementDTO } from "@/marketplace";

const route = useRoute();
const listings = ref<ListingDTO[]>([]);
const form = ref({ orderId: "", listingId: 0, amount: 100 });
const result = ref<SettlementDTO | null>(null);
const submitting = ref(false);
const loading = ref(true);
const error = ref<string | null>(null);

async function load() {
  loading.value = true;
  error.value = null;
  try {
    listings.value = await fetchListings(); // APPROVED — 只有已上架商品可结算
    if (typeof route.query.listingId === "string") {
      form.value.listingId = Number(route.query.listingId) || 0;
    }
    if (!form.value.listingId && listings.value.length) {
      form.value.listingId = listings.value[0].listingId;
    }
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}

async function settle() {
  if (!form.value.orderId) { ElMessage.warning("请填写订单号"); return; }
  if (!form.value.listingId) { ElMessage.warning("请选择商品(仅已上架商品可结算)"); return; }
  if (form.value.amount <= 0) { ElMessage.warning("订单金额需大于 0"); return; }
  submitting.value = true;
  try {
    const res = await sdk.post<SettlementDTO>("/api/v1/marketplace/settlements", {
      orderId: form.value.orderId,
      listingId: form.value.listingId,
      grossMicro: Math.round(form.value.amount * 1e6), // 元 → micro-units
    });
    result.value = res.data;
    ElMessage.success("结算完成(同一订单重复提交幂等返回)");
  } catch (e) {
    ElMessage.error(`结算失败:${(e as Error).message}`);
  } finally {
    submitting.value = false;
  }
}

onMounted(() => void load());
</script>

<template>
  <div class="settle">
    <PageHeader title="订单结算" />
    <p class="st-hint">按商品的分账比例将订单金额拆给伙伴与平台;结算按订单号幂等,重复提交不会二次打款。</p>
    <p v-if="error" class="st-error">加载失败:{{ error }}(请确认 svc-marketplace 在 :9212)</p>
    <p v-else-if="loading" class="st-loading">加载中…</p>
    <template v-else>
      <form class="st-form" @submit.prevent="settle">
        <label class="st-field"><span>订单号</span>
          <input v-model="form.orderId" placeholder="如 ord-20260820-001" />
        </label>
        <label class="st-field"><span>商品(已上架)</span>
          <select v-model.number="form.listingId">
            <option v-for="l in listings" :key="l.listingId" :value="l.listingId">
              {{ l.name }}(分成 {{ bpsPercent(l.partnerRateBps) }})
            </option>
          </select>
        </label>
        <label class="st-field"><span>订单金额(元)</span>
          <input v-model.number="form.amount" type="number" min="0.01" step="0.01" />
        </label>
        <button class="st-submit" type="submit" :disabled="submitting || !listings.length">
          {{ submitting ? "结算中…" : "结算" }}
        </button>
      </form>
      <div v-if="result" class="st-result">
        <h3>结算结果 · {{ result.orderId }}</h3>
        <div class="st-split">
          <div class="st-side partner">
            <span class="st-side-label">伙伴所得</span>
            <span class="st-side-amount">{{ yuan(result.partner) }}</span>
            <span class="st-side-rate">{{ bpsPercent(result.partnerRateBps) }}</span>
          </div>
          <div class="st-side platform">
            <span class="st-side-label">平台所得</span>
            <span class="st-side-amount">{{ yuan(result.platform) }}</span>
            <span class="st-side-rate">{{ bpsPercent(10000 - result.partnerRateBps) }}</span>
          </div>
        </div>
        <p class="st-gross">订单总额 {{ yuan(result.gross) }} · 结算单号 {{ result.settlementId }}</p>
      </div>
      <p v-else-if="!listings.length" class="st-empty">暂无可结算商品——请先在「上架审核」通过商品。</p>
    </template>
  </div>
</template>

<style scoped>
.settle { padding: var(--sc-spacing-6); max-width: 720px; }
.st-hint { color: var(--sc-text-secondary); font-size: 13px; margin: 0 0 var(--sc-spacing-4); }
.st-form {
  display: flex; flex-direction: column; gap: var(--sc-spacing-4);
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  box-shadow: var(--sc-shadow-sm);
  padding: var(--sc-spacing-6);
}
.st-field { display: flex; flex-direction: column; gap: 6px; font-size: 13px; color: var(--sc-text-secondary); }
.st-field input, .st-field select {
  padding: 10px 12px; background: var(--sc-bg-container); border: 1px solid var(--sc-border);
  border-radius: var(--sc-radius-md); font-size: 14px; color: var(--sc-text-primary);
  outline: none; font-family: inherit;
  transition: border-color var(--sc-transition), box-shadow var(--sc-transition);
}
.st-field input:focus, .st-field select:focus {
  border-color: var(--sc-color-brand); box-shadow: 0 0 0 3px var(--sc-color-brand-soft);
}
.st-submit {
  align-self: flex-start; padding: 11px 24px; border: none; border-radius: var(--sc-radius-md);
  background: var(--sc-color-brand); color: var(--sc-color-on-brand); font-size: 14px; cursor: pointer;
  transition: background var(--sc-transition);
}
.st-submit:hover { background: var(--sc-color-brand-hover); }
.st-submit:disabled { opacity: 0.6; cursor: not-allowed; }
.st-result { margin-top: var(--sc-spacing-5); }
.st-result h3 { font-size: 15px; font-weight: 500; color: var(--sc-text-primary); margin: 0 0 var(--sc-spacing-3); }
.st-split { display: grid; grid-template-columns: 1fr 1fr; gap: var(--sc-spacing-4); }
.st-side {
  display: flex; flex-direction: column; gap: var(--sc-spacing-1);
  border-radius: var(--sc-radius-lg); padding: var(--sc-spacing-5);
  border: 1px solid var(--sc-glass-border);
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
}
.st-side.partner { border-color: var(--sc-color-brand); }
.st-side-label { font-size: 12px; color: var(--sc-text-secondary); }
.st-side-amount { font-size: 24px; font-weight: 600; color: var(--sc-color-brand); }
.st-side.platform .st-side-amount { color: var(--sc-text-primary); }
.st-side-rate { font-size: 12px; color: var(--sc-text-secondary); }
.st-gross { margin: var(--sc-spacing-3) 0 0; font-size: 12px; color: var(--sc-text-secondary); }
.st-error, .st-loading, .st-empty { color: var(--sc-text-secondary); padding: var(--sc-spacing-6); }
.st-error { color: var(--sc-color-danger); }
</style>
