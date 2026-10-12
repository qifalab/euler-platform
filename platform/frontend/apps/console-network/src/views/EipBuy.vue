<script setup lang="ts">
/**
 * Apply for an elastic public IP (02§7.4 buy-wizard pattern, productCode=eueip)
 * — #/eueip/buy.
 *
 *  - GET  /api/v1/catalog/skus?productCode=eueip — eueip.bandwidth.prepaid
 *    (按固定带宽,包年包月) / eueip.traffic.postpaid (按流量,按量付费)
 *  - GET  /api/v1/catalog/placement?productCode=eueip — ZONAL: the AZ is required
 *  - POST /api/v1/catalog/quote → /api/v1/orders (zoneId) → pay →
 *    /api/v1/orchestrator/fulfill with params {bandwidthMbps, name}
 *
 * The charge type picks the SKU (and so the billing mode); only the
 * fixed-bandwidth SKU takes a bandwidth (1–1000 Mbit/s, the orchestrator's
 * bound). Price is server-trial-computed.
 */
import { ref, computed, watch, onMounted, onUnmounted } from "vue";
import { ElSteps, ElStep, ElForm, ElFormItem, ElSelect, ElOption, ElInput, ElInputNumber, ElRadioGroup, ElRadio, ElButton, ElMessage } from "element-plus";
import { yuanToMinor } from "@eu/sdk";
import {
  useCatalogMeta,
  useOrderCheckout,
  fulfilledMessage,
  CouponPicker,
  OrderAmountSummary,
  parseSpec,
  toParams,
  validateIntRange,
  validateResourceName,
  NAME_MAX,
  type CatalogSku,
} from "@eu/console-kit";
import { sdk } from "../sdk";

const PRODUCT = "eueip";
const BANDWIDTH_MIN = 1;
const BANDWIDTH_MAX = 1000;

const active = ref(0);
const form = ref({
  region: "", zone: "",
  chargeType: "prepaid" as "prepaid" | "postpaid",
  bandwidthMbps: 5,
  period: 1,
  name: "",
});

const { regions, placement, load, zonesOf } = useCatalogMeta(PRODUCT);
const zoneRequired = computed(() => placement.value?.zoneRequired ?? true);

// --- SKUs: one per charge type; the spec says how it is billed ---
const skus = ref<CatalogSku[]>([]);
function skuOf(ct: "PREPAID" | "POSTPAID", preferred: string): CatalogSku | undefined {
  return skus.value.find((s) => s.skuCode === preferred && s.chargeType === ct) ?? skus.value.find((s) => s.chargeType === ct);
}
const prepaidSku = computed(() => skuOf("PREPAID", "eueip.bandwidth.prepaid"));
const postpaidSku = computed(() => skuOf("POSTPAID", "eueip.traffic.postpaid"));
const currentSku = computed(() => (form.value.chargeType === "prepaid" ? prepaidSku.value : postpaidSku.value));
const billing = computed(() => String(parseSpec(currentSku.value?.specJson ?? "{}").billing ?? ""));
/** Fixed-bandwidth billing takes a bandwidth; traffic billing does not. */
const usesBandwidth = computed(() => billing.value !== "traffic");
const billingLabel = (sku: CatalogSku | undefined) =>
  String(parseSpec(sku?.specJson ?? "{}").billing ?? "") === "traffic" ? "按使用流量" : "按固定带宽";

const nameError = computed(() => validateResourceName(form.value.name));
const bandwidthError = computed(() =>
  usesBandwidth.value ? validateIntRange(form.value.bandwidthMbps, BANDWIDTH_MIN, BANDWIDTH_MAX, "带宽") : null,
);

// Follow the catalogue when a charge type has no SKU.
watch([prepaidSku, postpaidSku], ([pre, post]) => {
  if (form.value.chargeType === "prepaid" && !pre && post) form.value.chargeType = "postpaid";
  else if (form.value.chargeType === "postpaid" && !post && pre) form.value.chargeType = "prepaid";
});

onMounted(async () => {
  try {
    const res = await sdk.get<CatalogSku[]>(`/api/v1/catalog/skus?productCode=${PRODUCT}`);
    skus.value = (res.data ?? []).filter((s) => s.status === "1");
    const mbps = Number(parseSpec(prepaidSku.value?.specJson ?? "{}").mbps);
    if (Number.isInteger(mbps) && mbps >= BANDWIDTH_MIN && mbps <= BANDWIDTH_MAX) form.value.bandwidthMbps = mbps;
  } catch (e) {
    ElMessage.error(`加载规格目录失败:${(e as Error).message}`);
  }
  await load();
  if (!regions.value.some((r) => r.regionId === form.value.region)) {
    form.value.region = regions.value[0]?.regionId ?? "";
  }
  alignZone();
});

function alignZone() {
  const zones = zonesOf(form.value.region);
  if (!zones.some((z) => z.zoneId === form.value.zone)) form.value.zone = zones[0]?.zoneId ?? "";
}
watch(() => form.value.region, alignZone);

// --- live trial pricing ---
interface QuoteResult { payableAmount: string }
const quote = ref<QuoteResult | null>(null);
const quoting = ref(false);
const total = computed(() => {
  const n = Number(quote.value?.payableAmount);
  return quote.value && Number.isFinite(n) ? n : null;
});
const duration = computed(() => (currentSku.value?.chargeType === "PREPAID" ? form.value.period : 1));

let quoteTimer: ReturnType<typeof setTimeout> | null = null;
onUnmounted(() => { if (quoteTimer) clearTimeout(quoteTimer); });
async function refreshQuote() {
  const sku = currentSku.value;
  if (!sku || !form.value.region) { quote.value = null; return; }
  // ZONAL: wait for the zone instead of a request that can only 400.
  if (!form.value.zone && zoneRequired.value) { quote.value = null; return; }
  quoting.value = true;
  try {
    const res = await sdk.post<QuoteResult>("/api/v1/catalog/quote", {
      productCode: PRODUCT, specCode: sku.skuCode, chargeType: sku.chargeType,
      duration: duration.value, quantity: 1, regionId: form.value.region, zoneId: form.value.zone,
    });
    quote.value = res.data;
  } catch (e) {
    quote.value = null;
    ElMessage.warning(`询价失败:${(e as Error).message}`);
  } finally {
    quoting.value = false;
  }
}
watch([currentSku, duration, () => form.value.region, () => form.value.zone], () => {
  if (quoteTimer) clearTimeout(quoteTimer);
  quoteTimer = setTimeout(refreshQuote, 300);
});

function firstError(): string | null {
  if (!form.value.region) return "请选择地域";
  if (zoneRequired.value && !form.value.zone) return "请选择可用区";
  return nameError.value ?? bandwidthError.value;
}

function next() {
  const err = firstError();
  if (err) { ElMessage.warning(err); return; }
  active.value = 1;
}

function backToList() {
  location.hash = "";
}

// Coupons (优惠券) apply to prepaid orders only — a postpaid order has no
// upfront amount. The picker's selection is read when the order is created;
// svc-order's verdict (Order.CouponInvalid / CouponExpired) comes back through
// the composable.
const couponIds = ref<string[]>([]);
const {
  submitting, pending, checkout,
  lastOrder, couponError, couponBlocked, couponRevision, cancelBlockedOrder,
} = useOrderCheckout({
  sdk,
  couponIds: () => (currentSku.value?.chargeType === "PREPAID" ? couponIds.value : []),
  onFulfilled: (res, order) => {
    ElMessage.success(fulfilledMessage("弹性公网 IP", res, order));
    backToList();
  },
});

async function cancelExpiredOrder() {
  if (await cancelBlockedOrder()) ElMessage.success("订单已取消,优惠券已释放,请重新选择优惠券后下单");
}

/** Resource attributes set at creation (bandwidth only for fixed-bandwidth billing). */
function params(): Record<string, string> {
  return toParams({
    name: form.value.name,
    bandwidthMbps: usesBandwidth.value ? form.value.bandwidthMbps : undefined,
  });
}

async function submit() {
  const sku = currentSku.value;
  if (!sku) { ElMessage.error("所选计费方式暂无在售规格"); active.value = 0; return; }
  const err = firstError();
  if (err) { ElMessage.warning(err); active.value = 0; return; }
  const { region, zone } = form.value;
  const dur = duration.value;
  await checkout(
    async (extras) => {
      const q = await sdk.post<QuoteResult>("/api/v1/catalog/quote", {
        productCode: PRODUCT, specCode: sku.skuCode, chargeType: sku.chargeType,
        duration: dur, quantity: 1, regionId: region, zoneId: zone,
      });
      const created = await sdk.post<{ orderId: number; orderNo: string; state: string }>("/api/v1/orders", {
        type: "NEW", productCode: PRODUCT, skuCode: sku.skuCode, chargeType: sku.chargeType,
        regionId: region, zoneId: zone, quantity: 1, duration: dur, amountMinor: yuanToMinor(q.data.payableAmount),
        ...extras, // couponIds (prepaid only)
      });
      return created.data;
    },
    { productCode: PRODUCT, region, zone, specCode: sku.skuCode, chargeType: sku.chargeType, params: params() },
  );
}
</script>

<template>
  <section class="buy">
    <h1>申请弹性公网 IP</h1>
    <div class="buy-layout">
      <div class="buy-main">
        <ElSteps :active="active" align-center class="buy-steps">
          <ElStep title="基础配置" /><ElStep title="确认下单" />
        </ElSteps>

        <ElForm v-show="active === 0" label-position="top" class="buy-form" @submit.prevent>
          <ElFormItem label="地域"><ElSelect v-model="form.region"><ElOption v-for="r in regions" :key="r.regionId" :value="r.regionId" :label="r.regionName" /></ElSelect></ElFormItem>
          <ElFormItem v-if="zoneRequired" label="可用区">
            <ElSelect v-model="form.zone">
              <ElOption v-for="z in zonesOf(form.region)" :key="z.zoneId" :value="z.zoneId" :label="z.zoneName" />
            </ElSelect>
            <p class="form-hint">弹性公网 IP 为 ZONAL 产品:地址落在所选可用区,仅可绑定同可用区的云资源。</p>
          </ElFormItem>
          <ElFormItem label="计费方式">
            <ElRadioGroup v-model="form.chargeType">
              <ElRadio value="prepaid" :disabled="!prepaidSku">包年包月({{ billingLabel(prepaidSku) }})</ElRadio>
              <ElRadio value="postpaid" :disabled="!postpaidSku">按量付费({{ billingLabel(postpaidSku) }})</ElRadio>
            </ElRadioGroup>
          </ElFormItem>
          <ElFormItem v-if="usesBandwidth" label="带宽 (Mbit/s)">
            <ElInputNumber v-model="form.bandwidthMbps" :min="BANDWIDTH_MIN" :max="BANDWIDTH_MAX" :precision="0" />
            <p v-if="bandwidthError" class="form-hint form-hint-error">{{ bandwidthError }}</p>
            <p class="form-hint">固定带宽 {{ BANDWIDTH_MIN }}-{{ BANDWIDTH_MAX }} Mbit/s。</p>
          </ElFormItem>
          <ElFormItem v-if="form.chargeType === 'prepaid'" label="购买时长(月)"><ElInputNumber v-model="form.period" :min="1" :max="36" /></ElFormItem>
          <ElFormItem label="名称">
            <ElInput v-model="form.name" :maxlength="NAME_MAX" show-word-limit placeholder="可选,1-64 个字符" />
            <p v-if="nameError" class="form-hint form-hint-error">{{ nameError }}</p>
          </ElFormItem>
        </ElForm>

        <div v-show="active === 1" class="buy-confirm">
          <h2>确认配置</h2>
          <ul class="confirm-list">
            <li>地域可用区:{{ form.region }} / {{ form.zone || "—" }}</li>
            <li>计费方式:{{ form.chargeType === "prepaid" ? `包年包月 · ${form.period} 个月` : "按量付费" }}({{ billingLabel(currentSku) }})</li>
            <li v-if="usesBandwidth">带宽:{{ form.bandwidthMbps }} Mbit/s</li>
            <li>名称:{{ form.name.trim() || "—" }}</li>
            <li>规格:{{ currentSku?.skuCode ?? "—" }}</li>
          </ul>
          <ElForm v-if="currentSku?.chargeType === 'PREPAID'" label-position="top" @submit.prevent>
            <ElFormItem label="优惠券">
              <CouponPicker
                v-model="couponIds" :sdk="sdk" :product-code="PRODUCT" :sku-code="currentSku.skuCode" charge-type="PREPAID"
                :amount="quote?.payableAmount" :disabled="!!pending" :error="couponError" :refresh-key="couponRevision"
              />
            </ElFormItem>
          </ElForm>
        </div>

        <div class="buy-nav">
          <ElButton v-if="active === 0" @click="backToList">取消</ElButton>
          <ElButton v-if="active > 0" @click="active = 0">上一步</ElButton>
          <ElButton v-if="active === 0" type="primary" @click="next">下一步</ElButton>
          <ElButton v-if="active === 1 && !pending && !couponBlocked" type="primary" :loading="submitting" @click="submit">确认下单</ElButton>
          <ElButton v-if="pending" type="warning" :loading="submitting" @click="submit">{{ pending.stage === "pay" ? "继续支付" : "重试开通" }}(订单 {{ pending.orderNo }})</ElButton>
          <ElButton v-if="couponBlocked && !pending" type="danger" plain :loading="submitting" @click="cancelExpiredOrder">取消订单 {{ couponBlocked.orderNo }} 并重新下单</ElButton>
        </div>
      </div>

      <aside class="buy-summary">
        <h2>配置摘要</h2>
        <dl class="sum-list">
          <div><dt>可用区</dt><dd>{{ form.zone || "—" }}</dd></div>
          <div><dt>计费</dt><dd>{{ billingLabel(currentSku) }}</dd></div>
          <div v-if="usesBandwidth"><dt>带宽</dt><dd>{{ form.bandwidthMbps }} Mbit/s</dd></div>
        </dl>
        <div class="sum-total">
          <span>{{ form.chargeType === "prepaid" ? "应付总额" : "预估每小时" }}</span>
          <strong v-if="quoting">…</strong>
          <strong v-else-if="total !== null">¥{{ total.toFixed(form.chargeType === "prepaid" ? 2 : 4) }}</strong>
          <strong v-else>—</strong>
        </div>
        <p class="sum-hint">价格由服务端试算,下单时以收银台为准</p>
        <p v-if="currentSku?.chargeType === 'PREPAID' && couponIds.length && !lastOrder" class="sum-hint">已选 {{ couponIds.length }} 张优惠券,抵扣金额以订单为准</p>
        <OrderAmountSummary :order="pending ?? couponBlocked ?? lastOrder" />
      </aside>
    </div>
  </section>
</template>

<style scoped>
.buy { padding: 16px 24px; }
.buy h1 { font-size: 18px; margin: 0 0 16px; }
.buy-layout { display: grid; grid-template-columns: 1fr 300px; gap: 24px; }
.buy-steps { margin-bottom: 24px; }
.buy-form, .buy-confirm {
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  padding: 24px;
}
.buy-confirm h2 { font-size: 16px; margin: 0 0 16px; }
.confirm-list { list-style: none; padding: 0; margin: 0 0 16px; }
.confirm-list li { padding: 6px 0; border-bottom: 1px solid var(--eu-border); font-size: 13px; }
.form-hint { font-size: 12px; color: var(--eu-text-disabled); margin: 4px 0 0; width: 100%; }
.form-hint-error { color: var(--eu-color-danger); }
.buy-nav { margin-top: 24px; display: flex; gap: 12px; }
.buy-summary {
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  padding: 20px;
  height: fit-content;
  position: sticky;
  top: var(--eu-spacing-6);
}
.buy-summary h2 { font-size: 15px; margin: 0 0 16px; }
.sum-list { margin: 0; }
.sum-list div { display: flex; justify-content: space-between; padding: 8px 0; font-size: 13px; border-bottom: 1px solid var(--eu-border); }
.sum-list dt { color: var(--eu-text-secondary); }
.sum-total { display: flex; justify-content: space-between; align-items: baseline; margin-top: 16px; }
.sum-total strong { font-size: 22px; color: var(--eu-color-danger); }
.sum-hint { font-size: 12px; color: var(--eu-text-disabled); margin: 8px 0 0; }
</style>
