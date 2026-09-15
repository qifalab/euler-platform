<script setup lang="ts">
/**
 * Buy wizard (02§7.4): step form + live price summary on the right.
 *
 * Wired to the real backend chain:
 *  - GET  /api/v1/catalog/skus?productCode=scecs — spec catalogue
 *  - POST /api/v1/catalog/quote                  — 询价 (real pricing engine)
 *  - POST /api/v1/orders                         — create order (real orderId)
 *  - POST /api/v1/orders/{id}/pay                — mark PAID
 *  - POST /api/v1/orchestrator/fulfill           — saga → resource RUNNING
 *
 * Price is server-trial-computed (目录价 → 促销 → 代金券, 01§12.3); the client
 * never invents a unit price. The order id is issued by svc-order, not Date.now().
 */
import { ref, computed, watch, onMounted, onUnmounted } from "vue";
import { useRouter } from "vue-router";
import { ElSteps, ElStep, ElForm, ElFormItem, ElSelect, ElOption, ElInput, ElInputNumber, ElRadioGroup, ElRadio, ElButton, ElMessage } from "element-plus";
import { sdk } from "../sdk";
import { yuanToMinor } from "@sc/sdk";
import { useCatalogMeta } from "@sc/console-kit";

const router = useRouter();
const submitting = ref(false);
const active = ref(0);

const form = ref({
  region: "", zone: "",
  spec: "", cpu: 0, memory: 0,
  disk: 40, bandwidth: 5,
  image: "", password: "", confirm: "",
  period: 1, chargeType: "prepaid",
});

// --- region / zone / image metadata (catalogue-driven, no hardcoded lists) ---
const { regions, images, placement, load, zonesOf } = useCatalogMeta("scecs", { withImages: true });

// --- spec catalogue (real, from svc-catalog) ---

interface Sku {
  skuCode: string;
  productCode: string;
  chargeType: string; // PREPAID | POSTPAID
  specJson: string;   // e.g. {"cpu":2,"mem_gb":4}
  status: string;
}
interface SkuSpec { cpu: number; mem_gb: number }

const skus = ref<Sku[]>([]);

// Derive a displayable spec per charge type: the catalogue lists prepaid and
// postpaid variants of the same spec as separate SKUs. Group by parsed spec.
const specs = computed(() => {
  const bySpec = new Map<string, { code: string; label: string; cpu: number; mem_gb: number; prepaid: string; postpaid: string }>();
  for (const s of skus.value) {
    const spec = parseSpec(s.specJson);
    const key = `${spec.cpu}c${spec.mem_gb}g`;
    const entry = bySpec.get(key) ?? { code: "", label: `${spec.cpu}c${spec.mem_gb}g 通用型`, cpu: spec.cpu, mem_gb: spec.mem_gb, prepaid: "", postpaid: "" };
    if (s.chargeType === "PREPAID" && !entry.prepaid) {
      entry.prepaid = s.skuCode;
      // The spec used for the <select> value is the prepaid SKU code; we keep
      // both codes so the quote can target the right charge type.
      if (!entry.code) entry.code = s.skuCode;
    } else if (s.chargeType === "POSTPAID" && !entry.postpaid) {
      entry.postpaid = s.skuCode;
      if (!entry.code) entry.code = s.skuCode;
    }
    bySpec.set(key, entry);
  }
  return [...bySpec.values()];
});

function parseSpec(json: string): SkuSpec {
  try {
    const p = JSON.parse(json) as Partial<SkuSpec>;
    return { cpu: p.cpu ?? 0, mem_gb: p.mem_gb ?? 0 };
  } catch {
    return { cpu: 0, mem_gb: 0 };
  }
}

/** Resolve the SKU code for the currently-selected charge type + spec. */
function skuForCurrent(): string {
  const entry = specs.value.find((s) => s.code === form.value.spec || s.prepaid === form.value.spec || s.postpaid === form.value.spec);
  if (!entry) return "";
  return form.value.chargeType === "prepaid" ? (entry.prepaid || entry.code) : (entry.postpaid || entry.code);
}

onMounted(async () => {
  try {
    const res = await sdk.get<Sku[]>("/api/v1/catalog/skus?productCode=scecs");
    skus.value = res.data ?? [];
    // Default-select the first spec once the catalogue lands.
    if (specs.value.length && !form.value.spec) {
      form.value.spec = specs.value[0].code;
      form.value.cpu = specs.value[0].cpu;
      form.value.memory = specs.value[0].mem_gb;
    }
  } catch (e) {
    ElMessage.error(`加载规格目录失败:${(e as Error).message}`);
  }
  // Region / zone / image metadata: align defaults to the catalogue rows
  // (load() swallows its own errors and just sets `error`; safe to await).
  await load();
  if (!regions.value.some((r) => r.regionId === form.value.region)) {
    form.value.region = regions.value[0]?.regionId ?? "";
  }
  const zones = zonesOf(form.value.region);
  if (!zones.some((z) => z.zoneId === form.value.zone)) {
    form.value.zone = zones[0]?.zoneId ?? "";
  }
  if (!images.value.some((im) => im.imageId === form.value.image)) {
    form.value.image = images.value[0]?.imageId ?? "";
  }
});

// Region switch: keep the zone valid for the newly-chosen region.
watch(() => form.value.region, () => {
  const zones = zonesOf(form.value.region);
  if (!zones.some((z) => z.zoneId === form.value.zone)) {
    form.value.zone = zones[0]?.zoneId ?? "";
  }
});

watch(() => form.value.spec, (code) => {
  const entry = specs.value.find((s) => s.code === code);
  if (entry) { form.value.cpu = entry.cpu; form.value.memory = entry.mem_gb; }
});

// --- live trial pricing (real, from svc-catalog 询价) ---

interface QuoteResult {
  payableAmount: string; // yuan decimal, per period for prepaid / per hour for postpaid
  listAmount?: string;
}
const quote = ref<QuoteResult | null>(null);
const quoting = ref(false);

const total = computed(() => {
  if (!quote.value) return null;
  const payable = Number(quote.value.payableAmount);
  if (!Number.isFinite(payable)) return null;
  // The quote request already carries `duration`, so payableAmount is the
  // FULL payable for the chosen term (or per-hour for postpaid) — the client
  // never multiplies again (server-authoritative pricing, 01§12.3).
  return payable;
});

let quoteTimer: ReturnType<typeof setTimeout> | null = null;
// Leaving the wizard mid-debounce must not fire a quote request against an
// unmounted component (its failure toast would flash over the next page).
onUnmounted(() => {
  if (quoteTimer) clearTimeout(quoteTimer);
});
async function refreshQuote() {
  const specCode = skuForCurrent();
  if (!specCode) { quote.value = null; return; }
  quoting.value = true;
  try {
    const res = await sdk.post<QuoteResult>("/api/v1/catalog/quote", {
      productCode: "scecs",
      specCode,
      chargeType: form.value.chargeType === "prepaid" ? "PREPAID" : "POSTPAID",
      duration: form.value.chargeType === "prepaid" ? form.value.period : 1,
      quantity: 1,
      regionId: form.value.region,
    });
    quote.value = res.data;
  } catch (e) {
    quote.value = null;
    ElMessage.warning(`询价失败:${(e as Error).message}`);
  } finally {
    quoting.value = false;
  }
}

// Re-quote (debounced) when the price-determining inputs change.
watch(
  [() => form.value.spec, () => form.value.chargeType, () => form.value.period, () => form.value.region],
  () => {
    if (quoteTimer) clearTimeout(quoteTimer);
    quoteTimer = setTimeout(refreshQuote, 300);
  },
);

function next() { if (active.value < 3) active.value++; }
function prev() { if (active.value > 0) active.value--; }

// Order paid but fulfill failed — keep the order so the user can retry the
// saga without paying again (pay succeeded; only provisioning is pending).
const pendingFulfill = ref<{ orderId: number; orderNo: string; specCode: string } | null>(null);

async function fulfill(order: { orderId: number; orderNo: string; specCode: string }) {
  // The orchestrator runs the saga (order PAID→FULFILLING→COMPLETED,
  // resource CREATING→RUNNING) and returns the new resource id.
  const res = await sdk.post<{ resourceId: string; state: string; orderState: string }>(
    "/api/v1/orchestrator/fulfill",
    {
      orderId: order.orderId, orderNo: order.orderNo,
      productCode: "scecs", region: form.value.region,
      specCode: order.specCode, chargeType: form.value.chargeType.toUpperCase(),
      zone: form.value.zone, image: form.value.image,
      disk: form.value.disk, bandwidth: form.value.bandwidth,
      password: form.value.password,
    },
  );
  pendingFulfill.value = null;
  ElMessage.success(`实例已创建:${res.data.resourceId.slice(0, 24)}…(状态 ${res.data.state})`);
  router.push("/instances");
}

async function retryFulfill() {
  if (!pendingFulfill.value) return;
  submitting.value = true;
  try {
    await fulfill(pendingFulfill.value);
  } catch (e) {
    ElMessage.error(`开通重试失败:${(e as Error).message}`);
  } finally {
    submitting.value = false;
  }
}

async function submit() {
  if (!form.value.password || form.value.password !== form.value.confirm) {
    ElMessage.warning("请输入并确认登录密码"); active.value = 2; return;
  }
  const specCode = skuForCurrent();
  if (!specCode) { ElMessage.error("请选择实例规格"); active.value = 0; return; }
  submitting.value = true;
  try {
    // 1) Quote — the authoritative payable amount comes from the pricing
    // engine, already covering the full duration; the client never multiplies.
    const q = await sdk.post<QuoteResult>("/api/v1/catalog/quote", {
      productCode: "scecs", specCode,
      chargeType: form.value.chargeType === "prepaid" ? "PREPAID" : "POSTPAID",
      duration: form.value.chargeType === "prepaid" ? form.value.period : 1,
      quantity: 1, regionId: form.value.region,
    });
    // svc-order takes amountMinor in 分 (1 分 = 0.01 元); the quote is a yuan
    // decimal. Converting (not rounding to yuan) is what keeps the order's
    // payable equal to the price the customer was shown.
    const amountMinor = yuanToMinor(q.data.payableAmount);

    // 2) Create the order — svc-order issues the real orderId/orderNo.
    const created = await sdk.post<{ orderId: number; orderNo: string; state: string }>(
      "/api/v1/orders",
      {
        type: "NEW", productCode: "scecs", skuCode: specCode,
        regionId: form.value.region, quantity: 1,
        duration: form.value.chargeType === "prepaid" ? form.value.period : 1,
        amountMinor,
      },
    );

    // 3) Pay — transitions the order to PAID (the only provisioning trigger, D8).
    await sdk.post(`/api/v1/orders/${created.data.orderId}/pay`);

    // 4) Fulfill. If this step fails, the order is already PAID — remember it
    // and surface a retry entry instead of forcing the user to re-order.
    const order = { orderId: created.data.orderId, orderNo: created.data.orderNo, specCode };
    try {
      await fulfill(order);
    } catch (e) {
      pendingFulfill.value = order;
      ElMessage.error(`订单已支付(${order.orderNo}),但开通失败:${(e as Error).message},可点击"重试开通"`);
    }
  } catch (e) {
    ElMessage.error((e as Error).message || "创建失败");
  } finally {
    submitting.value = false;
  }
}
</script>

<template>
  <section class="buy">
    <h1>创建实例</h1>
    <div class="buy-layout">
      <div class="buy-main">
        <ElSteps :active="active" align-center class="buy-steps">
          <ElStep title="基础配置" /><ElStep title="存储与网络" /><ElStep title="系统与凭证" /><ElStep title="确认下单" />
        </ElSteps>

        <ElForm v-show="active === 0" label-position="top" class="buy-form">
          <ElFormItem label="地域"><ElSelect v-model="form.region"><ElOption v-for="r in regions" :key="r.regionId" :value="r.regionId" :label="r.regionName" /></ElSelect></ElFormItem>
          <ElFormItem v-if="placement === null || placement.zoneRequired" label="可用区"><ElSelect v-model="form.zone"><ElOption v-for="z in zonesOf(form.region)" :key="z.zoneId" :value="z.zoneId" :label="z.zoneName" /></ElSelect></ElFormItem>
          <ElFormItem label="实例规格">
            <ElSelect v-model="form.spec" :loading="skus.length === 0">
              <ElOption v-for="s in specs" :key="s.code" :value="s.code" :label="s.label" />
            </ElSelect>
          </ElFormItem>
        </ElForm>

        <ElForm v-show="active === 1" label-position="top" class="buy-form">
          <ElFormItem label="系统盘容量 (GiB)"><ElInputNumber v-model="form.disk" :min="20" :max="500" /></ElFormItem>
          <ElFormItem label="公网带宽 (Mbit/s)"><ElInputNumber v-model="form.bandwidth" :min="1" :max="100" /></ElFormItem>
        </ElForm>

        <ElForm v-show="active === 2" label-position="top" class="buy-form">
          <ElFormItem label="镜像"><ElSelect v-model="form.image"><ElOption v-for="im in images" :key="im.imageId" :value="im.imageId" :label="im.name" /></ElSelect></ElFormItem>
          <ElFormItem label="登录密码"><ElInput v-model="form.password" type="password" show-password /></ElFormItem>
          <ElFormItem label="确认密码"><ElInput v-model="form.confirm" type="password" show-password /></ElFormItem>
        </ElForm>

        <div v-show="active === 3" class="buy-confirm">
          <h2>确认配置</h2>
          <ul class="confirm-list">
            <li>地域可用区:{{ form.region }} / {{ form.zone }}</li>
            <li>规格:{{ specs.find(s=>s.code===form.spec)?.label }}</li>
            <li>系统盘:{{ form.disk }} GiB</li>
            <li>公网带宽:{{ form.bandwidth }} Mbit/s</li>
            <li>镜像:{{ form.image }}</li>
          </ul>
          <ElRadioGroup v-model="form.chargeType" class="confirm-charge">
            <ElRadio value="prepaid">包年包月</ElRadio>
            <ElRadio value="postpaid">按量付费</ElRadio>
          </ElRadioGroup>
          <ElFormItem v-if="form.chargeType === 'prepaid'" label="购买时长(月)"><ElInputNumber v-model="form.period" :min="1" :max="36" /></ElFormItem>
        </div>

        <div class="buy-nav">
          <ElButton v-if="active > 0" @click="prev">上一步</ElButton>
          <ElButton v-if="active < 3" type="primary" @click="next">下一步</ElButton>
          <ElButton v-if="active === 3 && !pendingFulfill" type="primary" :loading="submitting" @click="submit">确认下单</ElButton>
          <ElButton v-if="pendingFulfill" type="warning" :loading="submitting" @click="retryFulfill">重试开通(订单 {{ pendingFulfill.orderNo }})</ElButton>
        </div>
      </div>

      <aside class="buy-summary">
        <h2>配置摘要</h2>
        <dl class="sum-list">
          <div><dt>规格</dt><dd>{{ specs.find(s=>s.code===form.spec)?.label ?? "—" }}</dd></div>
          <div><dt>系统盘</dt><dd>{{ form.disk }} GiB</dd></div>
          <div><dt>带宽</dt><dd>{{ form.bandwidth }} Mbit/s</dd></div>
        </dl>
        <div class="sum-total">
          <span>{{ form.chargeType === "prepaid" ? "应付总额" : "预估每小时" }}</span>
          <strong v-if="quoting">…</strong>
          <strong v-else-if="total !== null">¥{{ total.toFixed(2) }}</strong>
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
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  box-shadow: var(--sc-shadow-sm);
  padding: 24px;
}
.buy-confirm h2 { font-size: 16px; margin: 0 0 16px; }
.confirm-list { list-style: none; padding: 0; margin: 0 0 16px; }
.confirm-list li { padding: 6px 0; border-bottom: 1px solid var(--sc-border); font-size: 13px; }
.confirm-charge { margin-bottom: 16px; }
.buy-nav { margin-top: 24px; display: flex; gap: 12px; }
.buy-summary {
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  box-shadow: var(--sc-shadow-sm);
  padding: 20px;
  height: fit-content;
  position: sticky;
  top: var(--sc-spacing-6);
}
.buy-summary h2 { font-size: 15px; margin: 0 0 16px; }
.sum-list { margin: 0; }
.sum-list div { display: flex; justify-content: space-between; padding: 8px 0; font-size: 13px; border-bottom: 1px solid var(--sc-border); }
.sum-list dt { color: var(--sc-text-secondary); }
.sum-total { display: flex; justify-content: space-between; align-items: baseline; margin-top: 16px; }
.sum-total strong { font-size: 22px; color: var(--sc-color-danger); }
.sum-hint { font-size: 12px; color: var(--sc-text-disabled); margin: 8px 0 0; }
</style>
