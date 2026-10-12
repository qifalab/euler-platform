<script setup lang="ts">
/**
 * Create VPC (02§7.4 buy-wizard pattern, productCode=euvpc) — #/euvpc/buy.
 *
 * Wired to the real chain, like every product wizard:
 *  - GET  /api/v1/catalog/skus?productCode=euvpc — euvpc.standard.postpaid
 *  - GET  /api/v1/orchestrator/resources         — the region's VPCs, so the
 *    CIDR block is checked for overlap before an order exists
 *  - POST /api/v1/catalog/quote → /api/v1/orders → pay → /api/v1/orchestrator/fulfill
 *    with params {name, cidrBlock} (stored as resource attributes)
 *
 * EUVPC is REGIONAL and POSTPAID only: no zone picker, no term. The CIDR rules
 * mirror the orchestrator (RFC1918, /16–/28, no overlap in the region); it
 * still answers 400 Resource.InvalidAttribute / 409 — a rejected CIDR is
 * corrected and retried on the same paid order (useOrderCheckout).
 */
import { ref, computed, watch, onMounted, onUnmounted } from "vue";
import { ElSteps, ElStep, ElForm, ElFormItem, ElSelect, ElOption, ElInput, ElButton, ElMessage } from "element-plus";
import { yuanToMinor } from "@eu/sdk";
import {
  useCatalogMeta,
  useOrderCheckout,
  fulfilledMessage,
  attributesOf,
  toParams,
  validateResourceName,
  validateVpcCidr,
  NAME_MAX,
  PRIVATE_CIDRS,
  type CatalogSku,
} from "@eu/console-kit";
import { sdk } from "../sdk";

const PRODUCT = "euvpc";
const DEFAULT_SKU = "euvpc.standard.postpaid";
/** Suggested blocks, one per RFC1918 range. */
const CIDR_PRESETS = ["10.0.0.0/16", "172.16.0.0/16", "192.168.0.0/16"];

const active = ref(0);
const form = ref({ region: "", name: "", cidrBlock: "" });
const cidrTouched = ref(false);

const { regions, load } = useCatalogMeta(PRODUCT);

// --- SKU: the on-sale postpaid VPC SKU (catalogue-driven, code as fallback) ---
const skus = ref<CatalogSku[]>([]);
const specCode = computed(() =>
  skus.value.find((s) => s.skuCode === DEFAULT_SKU)?.skuCode ??
  skus.value.find((s) => s.chargeType === "POSTPAID")?.skuCode ?? "",
);
const chargeType = computed(() => skus.value.find((s) => s.skuCode === specCode.value)?.chargeType ?? "POSTPAID");

// --- existing VPCs of the account (orchestrator records carry attributes) ---
interface OrchResource { resourceId: string; productCode: string; region: string; state: string; attributes?: Record<string, string> }
const vpcs = ref<OrchResource[]>([]);
const vpcsLoaded = ref(false);
/** CIDR blocks already used by live VPCs in the selected region. */
const regionCidrs = computed(() =>
  vpcs.value
    .filter((v) => v.region === form.value.region)
    .map((v) => attributesOf(v).cidrBlock ?? "")
    .filter((c) => c !== ""),
);

const nameError = computed(() => validateResourceName(form.value.name));
const cidrError = computed(() => validateVpcCidr(form.value.cidrBlock, regionCidrs.value));

/** First preset that is free in the region (the form's default). */
function freePreset(): string {
  return CIDR_PRESETS.find((c) => validateVpcCidr(c, regionCidrs.value) === null) ?? "";
}

onMounted(async () => {
  try {
    const res = await sdk.get<CatalogSku[]>(`/api/v1/catalog/skus?productCode=${PRODUCT}`);
    skus.value = (res.data ?? []).filter((s) => s.status === "1");
  } catch (e) {
    ElMessage.error(`加载规格目录失败:${(e as Error).message}`);
  }
  await load();
  if (!regions.value.some((r) => r.regionId === form.value.region)) {
    form.value.region = regions.value[0]?.regionId ?? "";
  }
  try {
    const res = await sdk.get<OrchResource[]>("/api/v1/orchestrator/resources");
    vpcs.value = (res.data ?? []).filter(
      (r) => r.productCode === PRODUCT && !["RELEASED", "RELEASING", "CREATE_FAILED"].includes(String(r.state).toUpperCase()),
    );
  } catch {
    vpcs.value = []; // the orchestrator still enforces the overlap rule
  } finally {
    vpcsLoaded.value = true;
  }
  // The region default may have been picked before the VPC list arrived:
  // re-pick a free block unless the user chose one.
  if (!cidrTouched.value) form.value.cidrBlock = freePreset();
});

// A region switch keeps a user-typed CIDR; an untouched default follows the
// region so it never starts out overlapping.
watch(() => form.value.region, () => {
  if (!cidrTouched.value) form.value.cidrBlock = freePreset();
});

function pickCidr(c: string) {
  form.value.cidrBlock = c;
  cidrTouched.value = true;
}

// --- live trial pricing (svc-catalog 询价; EUVPC is billed per hour) ---
interface QuoteResult { payableAmount: string }
const quote = ref<QuoteResult | null>(null);
const quoting = ref(false);
const perHour = computed(() => {
  const n = Number(quote.value?.payableAmount);
  return quote.value && Number.isFinite(n) ? n : null;
});

let quoteTimer: ReturnType<typeof setTimeout> | null = null;
onUnmounted(() => { if (quoteTimer) clearTimeout(quoteTimer); });
async function refreshQuote() {
  if (!specCode.value || !form.value.region) { quote.value = null; return; }
  quoting.value = true;
  try {
    const res = await sdk.post<QuoteResult>("/api/v1/catalog/quote", {
      productCode: PRODUCT, specCode: specCode.value, chargeType: chargeType.value,
      duration: 1, quantity: 1, regionId: form.value.region,
    });
    quote.value = res.data;
  } catch (e) {
    quote.value = null;
    ElMessage.warning(`询价失败:${(e as Error).message}`);
  } finally {
    quoting.value = false;
  }
}
watch([specCode, () => form.value.region], () => {
  if (quoteTimer) clearTimeout(quoteTimer);
  quoteTimer = setTimeout(refreshQuote, 300);
});

function next() {
  const err = nameError.value ?? cidrError.value;
  if (err) { cidrTouched.value = true; ElMessage.warning(err); return; }
  active.value = 1;
}

function backToList() {
  location.hash = "";
}

const { submitting, pending, checkout } = useOrderCheckout({
  sdk,
  onFulfilled: (res) => {
    ElMessage.success(fulfilledMessage("专有网络", res));
    backToList();
  },
});

/** Resource attributes set at creation. */
function params(): Record<string, string> {
  return toParams({ name: form.value.name, cidrBlock: form.value.cidrBlock });
}

async function submit() {
  if (!specCode.value) { ElMessage.error("专有网络规格未上架,暂无法创建"); return; }
  if (!form.value.region) { ElMessage.warning("请选择地域"); active.value = 0; return; }
  const err = nameError.value ?? cidrError.value;
  if (err) { cidrTouched.value = true; ElMessage.warning(err); active.value = 0; return; }
  const sku = specCode.value;
  const ct = chargeType.value;
  const region = form.value.region;
  // With an order pending this retries it with the (possibly corrected)
  // params — never a second order.
  await checkout(
    async () => {
      const q = await sdk.post<QuoteResult>("/api/v1/catalog/quote", {
        productCode: PRODUCT, specCode: sku, chargeType: ct, duration: 1, quantity: 1, regionId: region,
      });
      const created = await sdk.post<{ orderId: number; orderNo: string; state: string }>("/api/v1/orders", {
        type: "NEW", productCode: PRODUCT, skuCode: sku, chargeType: ct,
        regionId: region, quantity: 1, duration: 1, amountMinor: yuanToMinor(q.data.payableAmount),
      });
      return created.data;
    },
    { productCode: PRODUCT, region, specCode: sku, chargeType: ct, params: params() },
  );
}
</script>

<template>
  <section class="buy">
    <h1>创建专有网络 VPC</h1>
    <div class="buy-layout">
      <div class="buy-main">
        <ElSteps :active="active" align-center class="buy-steps">
          <ElStep title="基础配置" /><ElStep title="确认下单" />
        </ElSteps>

        <ElForm v-show="active === 0" label-position="top" class="buy-form" @submit.prevent>
          <ElFormItem label="地域">
            <ElSelect v-model="form.region"><ElOption v-for="r in regions" :key="r.regionId" :value="r.regionId" :label="r.regionName" /></ElSelect>
            <p class="form-hint">专有网络为 REGIONAL 产品:VPC 覆盖地域内全部可用区,子网再落到具体可用区。</p>
          </ElFormItem>
          <ElFormItem label="名称">
            <ElInput v-model="form.name" :maxlength="NAME_MAX" show-word-limit placeholder="可选,1-64 个字符;留空则以资源 ID 展示" />
            <p v-if="nameError" class="form-hint form-hint-error">{{ nameError }}</p>
          </ElFormItem>
          <ElFormItem label="IPv4 网段 (CIDR)">
            <ElInput v-model="form.cidrBlock" placeholder="如 10.0.0.0/16" @input="cidrTouched = true" />
            <div class="cidr-presets">
              <ElButton v-for="c in CIDR_PRESETS" :key="c" size="small" link type="primary" @click="pickCidr(c)">{{ c }}</ElButton>
            </div>
            <p v-if="cidrTouched && cidrError" class="form-hint form-hint-error">{{ cidrError }}</p>
            <p class="form-hint">
              仅支持私网网段({{ PRIVATE_CIDRS.join("、") }}),掩码 /16 ~ /28,且不能与同地域已有 VPC 重叠;创建后不可修改。
              <template v-if="vpcsLoaded && regionCidrs.length">已占用:{{ regionCidrs.join("、") }}</template>
            </p>
          </ElFormItem>
        </ElForm>

        <div v-show="active === 1" class="buy-confirm">
          <h2>确认配置</h2>
          <ul class="confirm-list">
            <li>地域:{{ form.region }}</li>
            <li>名称:{{ form.name.trim() || "—" }}</li>
            <li>IPv4 网段:{{ form.cidrBlock }}</li>
            <li>规格:{{ specCode || "—" }}</li>
            <li>计费方式:按量付费</li>
          </ul>
        </div>

        <div class="buy-nav">
          <ElButton v-if="active === 0" @click="backToList">取消</ElButton>
          <ElButton v-if="active > 0" @click="active = 0">上一步</ElButton>
          <ElButton v-if="active === 0" type="primary" @click="next">下一步</ElButton>
          <ElButton v-if="active === 1 && !pending" type="primary" :loading="submitting" @click="submit">确认创建</ElButton>
          <ElButton v-if="pending" type="warning" :loading="submitting" @click="submit">{{ pending.stage === "pay" ? "继续支付" : "重试开通" }}(订单 {{ pending.orderNo }})</ElButton>
        </div>
      </div>

      <aside class="buy-summary">
        <h2>配置摘要</h2>
        <dl class="sum-list">
          <div><dt>地域</dt><dd>{{ form.region || "—" }}</dd></div>
          <div><dt>网段</dt><dd>{{ form.cidrBlock || "—" }}</dd></div>
          <div><dt>计费</dt><dd>按量付费</dd></div>
        </dl>
        <div class="sum-total">
          <span>预估每小时</span>
          <strong v-if="quoting">…</strong>
          <strong v-else-if="perHour !== null">¥{{ perHour.toFixed(4) }}</strong>
          <strong v-else>—</strong>
        </div>
        <p class="sum-hint">价格由服务端试算,下单时以收银台为准</p>
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
.cidr-presets { display: flex; gap: 8px; margin-top: 4px; width: 100%; }
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
