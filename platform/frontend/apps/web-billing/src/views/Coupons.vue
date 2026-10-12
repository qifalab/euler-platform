<script setup lang="ts">
/** 卡券 — 账户下的优惠券(代金券 / 满减券 / 折扣券)。
 *  Wired to svc-order GET /api/v1/coupons?status=all,按状态分页签展示。
 *  可用券在包年包月(预付费)下单时于收银台选择抵扣;下单即锁定,支付后核销,
 *  取消订单释放。 */
import { computed, onMounted, ref } from "vue";
import { ElTabs, ElTabPane, ElTag } from "element-plus";
import { sdk } from "@/sdk";
import { PageHeader } from "@eu/ui";
import { fetchCoupons, type Coupon } from "@eu/console-kit";
import { useProductLabels } from "@/useProductLabels";
import { COUPON_TABS, groupCoupons, toCouponRow, type CouponTab } from "@/couponView";

const { label } = useProductLabels();

const coupons = ref<Coupon[]>([]);
const loading = ref(true);
const error = ref<string | null>(null);
const active = ref<CouponTab>("USABLE");

async function load() {
  loading.value = true;
  error.value = null;
  try {
    coupons.value = await fetchCoupons(sdk, { status: "all" });
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}
onMounted(load);

const groups = computed(() => groupCoupons(coupons.value));
// label() reads the shared product-name map, so rows re-render once it loads.
const rows = computed(() => groups.value[active.value].map((c) => toCouponRow(c, label)));
const kindTag = (kind: string) => (kind === "DISCOUNT" ? "warning" : kind === "FULL_REDUCTION" ? "danger" : "success");
</script>

<template>
  <div class="coupons">
    <PageHeader title="卡券" subtitle="优惠券可在包年包月订单下单时抵扣:每单最多 5 张,折扣券、满减券各限 1 张,代金券可叠加。" />
    <p v-if="error" class="coupons-error">加载失败:{{ error }}(请确认 svc-order 在 :9204)</p>
    <ElTabs v-model="active" class="coupons-tabs">
      <ElTabPane v-for="t in COUPON_TABS" :key="t.status" :name="t.status" :label="`${t.label}(${groups[t.status].length})`" />
    </ElTabs>
    <p v-if="loading" class="coupons-loading">加载中…</p>
    <div v-else-if="rows.length" class="coupons-table-card">
      <table class="coupons-table">
        <thead>
          <tr>
            <th>类型</th><th>面额 / 优惠</th><th>适用范围</th><th>有效期至</th><th>来源</th><th>券号</th>
            <th v-if="active === 'LOCKED'">锁定订单</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="r in rows" :key="r.couponId">
            <td><ElTag size="small" effect="plain" :type="kindTag(r.kind)">{{ r.kindLabel }}</ElTag></td>
            <td class="coupon-value">{{ r.value }}</td>
            <td>{{ r.scope }}</td>
            <td :class="{ 'coupon-soon': r.expiresSoon }">{{ r.expireAt }}<span v-if="r.expiresSoon">(即将过期)</span></td>
            <td>{{ r.source }}</td>
            <td class="coupon-id">{{ r.couponId }}</td>
            <td v-if="active === 'LOCKED'">{{ r.lockedOrderId }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-else-if="!error" class="coupons-empty">暂无{{ COUPON_TABS.find((t) => t.status === active)?.label }}的卡券</p>
  </div>
</template>

<style scoped>
.coupons { padding: var(--eu-spacing-6); max-width: 1200px; }
.coupons-error, .coupons-loading, .coupons-empty { color: var(--eu-text-secondary); padding: var(--eu-spacing-6); }
.coupons-error { color: var(--eu-color-danger); }
.coupons-tabs { margin-bottom: var(--eu-spacing-4); }
.coupons-table-card {
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  overflow: hidden;
}
.coupons-table { width: 100%; border-collapse: collapse; background: transparent; }
.coupons-table th, .coupons-table td { padding: var(--eu-spacing-3) var(--eu-spacing-4); text-align: left; border-bottom: 1px solid var(--eu-border); }
.coupons-table th { background: var(--eu-glass-bg-soft); color: var(--eu-text-secondary); font-size: 12px; font-weight: 500; }
.coupons-table td { font-size: 13px; color: var(--eu-text-primary); }
.coupons-table tbody tr { transition: background var(--eu-transition); }
.coupons-table tbody tr:hover { background: var(--eu-color-brand-soft); }
.coupons-table tbody tr:last-child td { border-bottom: none; }
.coupon-value { font-weight: 500; }
.coupon-id { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 12px; color: var(--eu-text-secondary); }
.coupon-soon { color: var(--eu-color-warning-text); }
</style>
