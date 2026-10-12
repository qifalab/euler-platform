<script setup lang="ts">
/**
 * Create RDS instance (02§7.4 buy-wizard pattern, productCode=eurds) —
 * #/eurds/buy. Same chain as console-redis:
 *  - GET  /api/v1/catalog/skus?productCode=eurds — eurds.mysql8.small.prepaid|postpaid
 *  - GET  /api/v1/catalog/placement?productCode=eurds — ZONAL: the AZ is required
 *  - POST /api/v1/catalog/quote → /api/v1/orders (zoneId) → pay →
 *    /api/v1/orchestrator/fulfill with params {engineVersion, name}
 *
 * The catalogue lists the prepaid and postpaid variant of each spec as
 * separate SKUs; the charge type picks one (never quoting a SKU under the
 * other charge type — 400 Catalog.ChargeTypeMismatch). Price is
 * server-trial-computed; the client never invents a unit price.
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
  validateResourceName,
  NAME_MAX,
  type CatalogSku,
} from "@eu/console-kit";
import { sdk } from "../sdk";

const PRODUCT = "eurds";
/** Engine versions the orchestrator accepts (eurds engineVersion). */
const ENGINE_VERSIONS = ["8.0", "5.7"] as const;

const active = ref(0);
const form = ref({
  region: "", zone: "",
  spec: "",
  engineVersion: "8.0" as (typeof ENGINE_VERSIONS)[number],
  name: "",
  chargeType: "prepaid" as "prepaid" | "postpaid",
  period: 1,
});

const { regions, placement, load, zonesOf } = useCatalogMeta(PRODUCT);
const zoneRequired = computed(() => placement.value?.zoneRequired ?? true);

// --- spec catalogue: group the prepaid/postpaid SKUs of one spec ---
const skus = ref<CatalogSku[]>([]);
interface SpecGroup { key: string; label: string; prepaid: string; postpaid: string }
const specs = computed<SpecGroup[]>(() => {
  const groups = new Map<string, SpecGroup>();
  for (const s of skus.value) {
    const spec = parseSpec(s.specJson);
    const key = `${String(spec.cpu ?? "?")}c${String(spec.mem_gb ?? "?")}g${spec.ha ? "-ha" : ""}`;
    const g = groups.get(key) ?? {
      key,
      label: `MySQL ${String(spec.cpu ?? "?")} 核 ${String(spec.mem_gb ?? "?")} GB${spec.ha ? " 高可用版" : " 基础版"}`,
      prepaid: "", postpaid: "",
    };
    if (s.chargeType === "PREPAID" && !g.prepaid) g.prepaid = s.skuCode;
    else if (s.chargeType === "POSTPAID" && !g.postpaid) g.postpaid = s.skuCode;
    groups.set(key, g);
  }
  return [...groups.values()];
});
const currentSpec = computed(() => specs.value.find((s) => s.key === form.value.spec));
/** The SKU of the selected spec in the selected charge type ("" when none). */
const specCode = computed(() => (form.value.chargeType === "prepaid" ? currentSpec.value?.prepaid : currentSpec.value?.postpaid) ?? "");
const skuChargeType = computed(() => skus.value.find((s) => s.skuCode === specCode.value)?.chargeType ?? "");
const duration = computed(() => (skuChargeType.value === "PREPAID" ? form.value.period : 1));

// A spec offered in only one charge type: follow it.
watch([currentSpec, () => form.value.chargeType], ([g]) => {
  if (!g) return;
  if (form.value.chargeType === "prepaid" && !g.prepaid && g.postpaid) form.value.chargeType = "postpaid";
  else if (form.value.chargeType === "postpaid" && !g.postpaid && g.prepaid) form.value.chargeType = "prepaid";
});

const nameError = computed(() => validateResourceName(form.value.name));

onMounted(async () => {
  try {
    const res = await sdk.get<CatalogSku[]>(`/api/v1/catalog/skus?productCode=${PRODUCT}`);
    skus.value = (res.data ?? []).filter((s) => s.status === "1");
    if (!form.value.spec && specs.value.length) form.value.spec = specs.value[0].key;
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

let quoteTimer: ReturnType<typeof setTimeout> | null = null;
onUnmounted(() => { if (quoteTimer) clearTimeout(quoteTimer); });
async function refreshQuote() {
  if (!specCode.value || !form.value.region) { quote.value = null; return; }
  if (!form.value.zone && zoneRequired.value) { quote.value = null; return; }
  quoting.value = true;
  try {
    const res = await sdk.post<QuoteResult>("/api/v1/catalog/quote", {
      productCode: PRODUCT, specCode: specCode.value, chargeType: skuChargeType.value,
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
watch([specCode, duration, () => form.value.region, () => form.value.zone], () => {
  if (quoteTimer) clearTimeout(quoteTimer);
  quoteTimer = setTimeout(refreshQuote, 300);
});

function firstError(): string | null {
  if (!form.value.region) return "请选择地域";
  if (zoneRequired.value && !form.value.zone) return "请选择可用区";
  if (!currentSpec.value) return "请选择实例规格";
  return nameError.value;
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
  couponIds: () => (skuChargeType.value === "PREPAID" ? couponIds.value : []),
  onFulfilled: (res, order) => {
    ElMessage.success(fulfilledMessage("数据库实例", res, order));
    backToList();
  },
});

async function cancelExpiredOrder() {
  if (await cancelBlockedOrder()) ElMessage.success("订单已取消,优惠券已释放,请重新选择优惠券后下单");
}

/** Resource attributes set at creation. */
function params(): Record<string, string> {
  return toParams({ engineVersion: form.value.engineVersion, name: form.value.name });
}

async function submit() {
  const err = firstError();
  if (err) { ElMessage.warning(err); active.value = 0; return; }
  const sku = specCode.value;
  if (!sku) { ElMessage.error("所选规格暂无该计费方式"); return; }
  const ct = skuChargeType.value;
  const dur = duration.value;
  const { region, zone } = form.value;
  await checkout(
    async (extras) => {
      const q = await sdk.post<QuoteResult>("/api/v1/catalog/quote", {
        productCode: PRODUCT, specCode: sku, chargeType: ct, duration: dur, quantity: 1, regionId: region, zoneId: zone,
      });
      const created = await sdk.post<{ orderId: number; orderNo: string; state: string }>("/api/v1/orders", {
        type: "NEW", productCode: PRODUCT, skuCode: sku, chargeType: ct,
        regionId: region, zoneId: zone, quantity: 1, duration: dur, amountMinor: yuanToMinor(q.data.payableAmount),
        ...extras, // couponIds (prepaid only)
      });
      return created.data;
    },
    { productCode: PRODUCT, region, zone, specCode: sku, chargeType: ct, params: params() },
  );
}
</script>

<template>
  <section class="buy">
    <h1>创建云数据库 RDS 实例</h1>
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
            <p class="form-hint">云数据库为 ZONAL 产品:主节点落所选可用区,高可用版备节点落对侧可用区。</p>
          </ElFormItem>
          <ElFormItem label="数据库引擎">
            <ElRadioGroup v-model="form.engineVersion">
              <ElRadio v-for="v in ENGINE_VERSIONS" :key="v" :value="v">MySQL {{ v }}</ElRadio>
            </ElRadioGroup>
          </ElFormItem>
          <ElFormItem label="实例规格">
            <ElSelect v-model="form.spec" :loading="skus.length === 0">
              <ElOption v-for="s in specs" :key="s.key" :value="s.key" :label="s.label" />
            </ElSelect>
          </ElFormItem>
          <ElFormItem label="实例名称">
            <ElInput v-model="form.name" :maxlength="NAME_MAX" show-word-limit placeholder="可选,1-64 个字符;留空则以资源 ID 展示" />
            <p v-if="nameError" class="form-hint form-hint-error">{{ nameError }}</p>
          </ElFormItem>
        </ElForm>

        <div v-show="active === 1" class="buy-confirm">
          <h2>确认配置</h2>
          <ul class="confirm-list">
            <li>地域可用区:{{ form.region }} / {{ form.zone || "—" }}</li>
            <li>引擎:MySQL {{ form.engineVersion }}</li>
            <li>规格:{{ currentSpec?.label ?? "—" }}</li>
            <li>实例名称:{{ form.name.trim() || "—" }}</li>
          </ul>
          <ElRadioGroup v-model="form.chargeType" class="confirm-charge">
            <ElRadio value="prepaid" :disabled="!!currentSpec && !currentSpec.prepaid">包年包月</ElRadio>
            <ElRadio value="postpaid" :disabled="!!currentSpec && !currentSpec.postpaid">按量付费</ElRadio>
          </ElRadioGroup>
          <ElFormItem v-if="form.chargeType === 'prepaid'" label="购买时长(月)"><ElInputNumber v-model="form.period" :min="1" :max="36" /></ElFormItem>
          <ElFormItem v-if="skuChargeType === 'PREPAID'" label="优惠券">
            <CouponPicker
              v-model="couponIds" :sdk="sdk" :product-code="PRODUCT" :sku-code="specCode" charge-type="PREPAID"
              :amount="quote?.payableAmount" :disabled="!!pending" :error="couponError" :refresh-key="couponRevision"
            />
          </ElFormItem>
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
          <div><dt>引擎</dt><dd>MySQL {{ form.engineVersion }}</dd></div>
          <div><dt>规格</dt><dd>{{ currentSpec?.label ?? "—" }}</dd></div>
          <div><dt>可用区</dt><dd>{{ form.zone || "—" }}</dd></div>
        </dl>
        <div class="sum-total">
          <span>{{ form.chargeType === "prepaid" ? "应付总额" : "预估每小时" }}</span>
          <strong v-if="quoting">…</strong>
          <strong v-else-if="total !== null">¥{{ total.toFixed(2) }}</strong>
          <strong v-else>—</strong>
        </div>
        <p class="sum-hint">价格由服务端试算,下单时以收银台为准</p>
        <p v-if="skuChargeType === 'PREPAID' && couponIds.length && !lastOrder" class="sum-hint">已选 {{ couponIds.length }} 张优惠券,抵扣金额以订单为准</p>
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
.confirm-charge { margin-bottom: 16px; }
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
