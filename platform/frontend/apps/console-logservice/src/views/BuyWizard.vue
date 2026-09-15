<script setup lang="ts">
/**
 * Buy wizard (02§7.4): step form + live price summary on the right.
 *
 * Wired to the real backend chain (same shape as console-backup, productCode=sclog):
 *  - GET  /api/v1/catalog/skus?productCode=sclog — spec catalogue
 *  - POST /api/v1/catalog/quote                  — 询价 (real pricing engine)
 *  - POST /api/v1/orders                         — create order (real orderId)
 *  - POST /api/v1/orders/{id}/pay                — mark PAID
 *  - POST /api/v1/orchestrator/fulfill           — saga → resource RUNNING
 *
 * SCLOG is POSTPAID ONLY and REGIONAL (log ingestion is global within a region;
 * cross-AZ storage is a replica flag, not a placement constraint), so there is
 * NO zone picker. The catalogue's per-HOUR pricing rule returns the hourly
 * payable as payableAmount; the summary shows 预估每小时. Price is
 * server-trial-computed; the client never invents a unit price. The order id is
 * issued by svc-order.
 */
import { ref, computed, watch, onMounted, onUnmounted } from "vue";
import { useRouter } from "vue-router";
import { ElSteps, ElStep, ElForm, ElFormItem, ElSelect, ElOption, ElInputNumber, ElSwitch, ElButton, ElMessage } from "element-plus";
import { createSDK, yuanToMinor } from "@sc/sdk";
import { useCatalogMeta } from "@sc/console-kit";

const router = useRouter();
const sdk = createSDK({ baseURL: "" });
const submitting = ref(false);
const active = ref(0);

const form = ref({
  region: "",
  spec: "", edition: "",
  retentionDays: 30,
  storageGb: 200,
  crossAz: false,
});

// --- region metadata (catalogue-driven, no hardcoded lists). SCLOG is
// REGIONAL: no zone picker at all, so only the region list is needed. ---
const { regions, load } = useCatalogMeta("sclog");

// --- spec catalogue (real, from svc-catalog) ---

interface Sku {
  skuCode: string;
  productCode: string;
  chargeType: string; // POSTPAID only for SCLOG
  specJson: string;
  status: string;
}
interface SkuSpec { tier?: string; cross_az?: boolean }

const skus = ref<Sku[]>([]);

const specs = computed(() => {
  // SCLOG has only postpaid SKUs. The spec list is the SKU list, labelled
  // by edition (standard / pro).
  return skus.value.map((s) => {
    const spec = parseSpec(s.specJson);
    const label = spec.tier === "pro" ? "专业版" : "标准版";
    return { code: s.skuCode, label, edition: spec.tier ?? "standard", crossAz: !!spec.cross_az };
  });
});

function parseSpec(json: string): SkuSpec {
  try {
    return JSON.parse(json) as Partial<SkuSpec>;
  } catch {
    return {};
  }
}

onMounted(async () => {
  try {
    const res = await sdk.get<Sku[]>("/api/v1/catalog/skus?productCode=sclog");
    skus.value = res.data ?? [];
    if (specs.value.length && !form.value.spec) {
      form.value.spec = specs.value[0].code;
      form.value.edition = specs.value[0].edition;
      form.value.crossAz = specs.value[0].crossAz;
    }
  } catch (e) {
    ElMessage.error(`加载规格目录失败:${(e as Error).message}`);
  }
  // Region metadata: align the default to the first catalogue row (load()
  // swallows its own errors and just sets `error`; safe to await).
  await load();
  if (!regions.value.some((r) => r.regionId === form.value.region)) {
    form.value.region = regions.value[0]?.regionId ?? "";
  }
});

// When the spec changes, sync the edition + crossAz flag (the pro edition
// turns the cross-AZ storage replica on).
watch(() => form.value.spec, (code) => {
  const entry = specs.value.find((s) => s.code === code);
  if (entry) {
    form.value.edition = entry.edition;
    form.value.crossAz = entry.crossAz;
  }
});

// --- live trial pricing (real, from svc-catalog 询价) ---

interface QuoteResult {
  payableAmount: string; // per-hour yuan, decimal
  durationUnit?: string;
  listAmount?: string;
}
const quote = ref<QuoteResult | null>(null);
const quoting = ref(false);

const perHour = computed(() => {
  if (!quote.value) return null;
  const v = Number(quote.value.payableAmount);
  return Number.isFinite(v) ? v : null;
});

let quoteTimer: ReturnType<typeof setTimeout> | null = null;
// Leaving the wizard mid-debounce must not fire a quote request against an
// unmounted component (its failure toast would flash over the next page).
onUnmounted(() => {
  if (quoteTimer) clearTimeout(quoteTimer);
});
async function refreshQuote() {
  const specCode = form.value.spec;
  if (!specCode) { quote.value = null; return; }
  quoting.value = true;
  try {
    const res = await sdk.post<QuoteResult>("/api/v1/catalog/quote", {
      productCode: "sclog",
      specCode,
      chargeType: "POSTPAID",
      quantity: 1,
      regionId: form.value.region,
      // REGIONAL product: NO zoneId (log ingestion is global; zone is not its concern).
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
  [() => form.value.spec, () => form.value.region, () => form.value.storageGb],
  () => {
    if (quoteTimer) clearTimeout(quoteTimer);
    quoteTimer = setTimeout(refreshQuote, 300);
  },
);

function next() { if (active.value < 2) active.value++; }
function prev() { if (active.value > 0) active.value--; }

async function submit() {
  const specCode = form.value.spec;
  if (!specCode) { ElMessage.error("请选择日志服务规格"); active.value = 0; return; }
  if (form.value.retentionDays <= 0) { ElMessage.warning("保留期必须大于 0"); active.value = 1; return; }
  if (form.value.storageGb < 10) { ElMessage.warning("存储容量至少 10GB"); active.value = 1; return; }
  submitting.value = true;
  try {
    // 1) Quote — the authoritative per-hour payable comes from the pricing engine.
    const q = await sdk.post<QuoteResult>("/api/v1/catalog/quote", {
      productCode: "sclog", specCode,
      chargeType: "POSTPAID", quantity: 1,
      regionId: form.value.region,
    });

    // 2) Create the order — svc-order issues the real orderId/orderNo.
    const amountMinor = Math.max(1, yuanToMinor(q.data.payableAmount));
    const created = await sdk.post<{ orderId: number; orderNo: string; state: string }>(
      "/api/v1/orders",
      {
        type: "NEW", productCode: "sclog", skuCode: specCode,
        regionId: form.value.region, quantity: 1,
        duration: 1, amountMinor,
      },
    );

    // 3) Pay — transitions the order to PAID (the only provisioning trigger, D8).
    await sdk.post(`/api/v1/orders/${created.data.orderId}/pay`, {
      paymentId: `pay-${created.data.orderId}`,
      paidAmountMinor: amountMinor,
    });

    // 4) Fulfill — the orchestrator runs the saga and returns the new resource id.
    // The productCode=sclog routes it to the DriverK8s binding. The instance
    // params (retention/storage/crossAz) flow through to the reconcile loop.
    const res = await sdk.post<{ resourceId: string; state: string; orderState: string }>(
      "/api/v1/orchestrator/fulfill",
      {
        orderId: created.data.orderId, orderNo: created.data.orderNo,
        productCode: "sclog", region: form.value.region,
        specCode, chargeType: "POSTPAID",
        retentionDays: form.value.retentionDays,
        storageGb: form.value.storageGb,
        crossAz: form.value.crossAz,
      },
    );
    ElMessage.success(`日志服务已创建:${res.data.resourceId.slice(0, 24)}…(状态 ${res.data.state})`);
    router.push("/instances");
  } catch (e) {
    ElMessage.error((e as Error).message || "创建失败");
  } finally {
    submitting.value = false;
  }
}
</script>

<template>
  <section class="buy">
    <h1>创建日志服务</h1>
    <div class="buy-layout">
      <div class="buy-main">
        <ElSteps :active="active" align-center class="buy-steps">
          <ElStep title="基础配置" /><ElStep title="日志策略" /><ElStep title="确认下单" />
        </ElSteps>

        <ElForm v-show="active === 0" label-position="top" class="buy-form">
          <ElFormItem label="地域"><ElSelect v-model="form.region"><ElOption v-for="r in regions" :key="r.regionId" :value="r.regionId" :label="r.regionName" /></ElSelect></ElFormItem>
          <ElFormItem label="日志服务规格">
            <ElSelect v-model="form.spec" :loading="skus.length === 0">
              <ElOption v-for="s in specs" :key="s.code" :value="s.code" :label="s.label" />
            </ElSelect>
          </ElFormItem>
          <p class="form-hint">日志服务为 REGIONAL 产品:采集是地域级的,跨 AZ 存储是副本标志(非放置约束)。</p>
        </ElForm>

        <ElForm v-show="active === 1" label-position="top" class="buy-form">
          <ElFormItem label="存储容量 (GB)"><ElInputNumber v-model="form.storageGb" :min="10" :max="100000" :step="50" /></ElFormItem>
          <ElFormItem label="保留期 (天)"><ElInputNumber v-model="form.retentionDays" :min="1" :max="3650" /></ElFormItem>
          <ElFormItem label="跨可用区存储">
            <ElSwitch v-model="form.crossAz" :disabled="form.edition !== 'pro'" />
            <span class="form-hint-inline">{{ form.crossAz ? "开启:ClickHouse 存储将复制到第二可用区" : "关闭:仅单可用区存储" }}</span>
          </ElFormItem>
        </ElForm>

        <div v-show="active === 2" class="buy-confirm">
          <h2>确认配置</h2>
          <ul class="confirm-list">
            <li>地域:{{ form.region }}</li>
            <li>规格:{{ specs.find(s=>s.code===form.spec)?.label }}</li>
            <li>存储容量:{{ form.storageGb }} GB</li>
            <li>保留期:{{ form.retentionDays }} 天</li>
            <li>跨可用区存储:{{ form.crossAz ? "是" : "否" }}</li>
            <li>计费方式:按量付费</li>
          </ul>
        </div>

        <div class="buy-nav">
          <ElButton v-if="active > 0" @click="prev">上一步</ElButton>
          <ElButton v-if="active < 2" type="primary" @click="next">下一步</ElButton>
          <ElButton v-if="active === 2" type="primary" :loading="submitting" @click="submit">确认下单</ElButton>
        </div>
      </div>

      <aside class="buy-summary">
        <h2>配置摘要</h2>
        <dl class="sum-list">
          <div><dt>规格</dt><dd>{{ specs.find(s=>s.code===form.spec)?.label ?? "—" }}</dd></div>
          <div><dt>存储</dt><dd>{{ form.storageGb }} GB</dd></div>
          <div><dt>保留期</dt><dd>{{ form.retentionDays }} 天</dd></div>
          <div><dt>跨可用区</dt><dd>{{ form.crossAz ? "是" : "否" }}</dd></div>
        </dl>
        <div class="sum-total">
          <span>预估每小时</span>
          <strong v-if="quoting">…</strong>
          <strong v-else-if="perHour !== null">¥{{ perHour.toFixed(4) }}</strong>
          <strong v-else>—</strong>
        </div>
        <p class="sum-hint">按存储容量 + 采集量按量计费;价格由服务端试算</p>
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
.confirm-list { list-style: none; padding: 0; margin: 0; }
.confirm-list li { padding: 6px 0; border-bottom: 1px solid var(--sc-border); font-size: 13px; }
.form-hint { font-size: 12px; color: var(--sc-text-disabled); margin: 4px 0 0; }
.form-hint-inline { font-size: 12px; color: var(--sc-text-secondary); margin-left: 12px; }
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
