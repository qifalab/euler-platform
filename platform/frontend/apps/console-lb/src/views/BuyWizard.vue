<script setup lang="ts">
/**
 * Buy wizard (02§7.4): step form + live price summary on the right.
 *
 * Wired to the real backend chain (productCode=sclb):
 *  - GET  /api/v1/catalog/skus?productCode=sclb — spec catalogue
 *  - POST /api/v1/catalog/quote                  — 询价 (real pricing engine)
 *  - POST /api/v1/orders                         — create order (real orderId)
 *  - POST /api/v1/orders/{id}/pay                — mark PAID
 *  - POST /api/v1/orchestrator/fulfill           — saga → resource RUNNING
 *
 * SCLB is POSTPAID ONLY (usage-billed: LCU + traffic). SCLB is REGIONAL (a load
 * balancer spans AZs — it is the cross-AZ entry point), so there is NO 可用区
 * zone selector: region only, and the quote request sends no zoneId (the M-6
 * gate requires zoneId only for ZONAL products). The catalogue's hourly pricing
 * rule returns the per-hour unit price as payableAmount directly. Price is
 * server-trial-computed; the client never invents a unit price. The order id is
 * issued by svc-order.
 */
import { ref, computed, watch, onMounted } from "vue";
import { useRouter } from "vue-router";
import { ElSteps, ElStep, ElForm, ElFormItem, ElSelect, ElOption, ElInput, ElInputNumber, ElButton, ElMessage } from "element-plus";
import { createSDK } from "@sc/sdk";
import { useCatalogMeta } from "@sc/console-kit";

const router = useRouter();
const sdk = createSDK({ baseURL: "" });
const submitting = ref(false);
const active = ref(0);

const form = ref({
  region: "",
  spec: "",
  lbType: "", // l4 | l7, derived from the selected SKU's specJson
  listenPort: 443,
  protocol: "HTTPS",
  backends: "10.128.3.17:8080\n10.128.5.22:8080",
});

// --- region metadata (catalogue-driven, no hardcoded lists). SCLB is
// REGIONAL: no zone picker at all, so only the region list is needed. ---
const { regions, load } = useCatalogMeta("sclb");

// --- spec catalogue (real, from svc-catalog) ---

interface Sku {
  skuCode: string;
  productCode: string;
  chargeType: string; // POSTPAID only for SCLB
  specJson: string;
  status: string;
}
interface SkuSpec { type: string; max_qps?: number; max_conn?: number }

const skus = ref<Sku[]>([]);

const specs = computed(() => {
  return skus.value.map((s) => {
    const spec = parseSpec(s.specJson);
    return { code: s.skuCode, label: specLabel(spec), type: spec.type };
  });
});

function parseSpec(json: string): SkuSpec {
  try {
    return JSON.parse(json) as SkuSpec;
  } catch {
    return { type: "" };
  }
}

function specLabel(spec: SkuSpec): string {
  if (spec.type === "l7") return `七层 LB (${spec.max_qps ?? 0} QPS)`;
  if (spec.type === "l4") return `四层 LB (${spec.max_conn ?? 0} 并发连接)`;
  return "负载均衡";
}

onMounted(async () => {
  try {
    const res = await sdk.get<Sku[]>("/api/v1/catalog/skus?productCode=sclb");
    skus.value = res.data ?? [];
    if (specs.value.length && !form.value.spec) {
      form.value.spec = specs.value[0].code;
      form.value.lbType = specs.value[0].type;
      // L4 defaults to TCP, L7 to HTTPS.
      form.value.protocol = specs.value[0].type === "l4" ? "TCP" : "HTTPS";
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

watch(() => form.value.spec, (code) => {
  const entry = specs.value.find((s) => s.code === code);
  if (entry) {
    form.value.lbType = entry.type;
    form.value.protocol = entry.type === "l4" ? "TCP" : "HTTPS";
  }
});

// --- live trial pricing (real, from svc-catalog 询价) ---
//
// SCLB's pricing rule is DurationHour (usage-billed postpaid), so payableAmount
// is the per-hour unit price directly — no per-second conversion needed (unlike
// SCECI). The summary shows the 预估每小时 verbatim.

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
async function refreshQuote() {
  const specCode = form.value.spec;
  if (!specCode) { quote.value = null; return; }
  quoting.value = true;
  try {
    const res = await sdk.post<QuoteResult>("/api/v1/catalog/quote", {
      productCode: "sclb",
      specCode,
      chargeType: "POSTPAID",
      quantity: 1,
      regionId: form.value.region,
      // SCLB is REGIONAL: NO zoneId — the M-6 quote gate requires zoneId only
      // for ZONAL products. A REGIONAL product that sends zoneId is tolerated
      // but not required.
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
  [() => form.value.spec, () => form.value.region],
  () => {
    if (quoteTimer) clearTimeout(quoteTimer);
    quoteTimer = setTimeout(refreshQuote, 300);
  },
);

function next() { if (active.value < 2) active.value++; }
function prev() { if (active.value > 0) active.value--; }

function parseBackends(): string[] {
  return form.value.backends
    .split(/\n|,/)
    .map((s) => s.trim())
    .filter((s) => s.length > 0);
}

async function submit() {
  const specCode = form.value.spec;
  if (!specCode) { ElMessage.error("请选择实例规格"); active.value = 0; return; }
  const backends = parseBackends();
  if (backends.length === 0) { ElMessage.warning("请填写至少一个后端服务器"); active.value = 1; return; }
  submitting.value = true;
  try {
    // 1) Quote — the authoritative per-hour payable comes from the pricing engine.
    const q = await sdk.post<QuoteResult>("/api/v1/catalog/quote", {
      productCode: "sclb", specCode,
      chargeType: "POSTPAID", quantity: 1,
      regionId: form.value.region,
    });

    // 2) Create the order — svc-order issues the real orderId/orderNo. SCLB is
    // usage-billed postpaid; the initial amount is the per-hour unit (rounded
    // to yuan for the order's whole-yuan amountMinor field); actual billing
    // accrues from metering (lcu_hour/traffic_gb), settled hourly.
    const amountMinor = Math.max(1, Math.round(Number(q.data.payableAmount)));
    const created = await sdk.post<{ orderId: number; orderNo: string; state: string }>(
      "/api/v1/orders",
      {
        type: "NEW", productCode: "sclb", skuCode: specCode,
        regionId: form.value.region, quantity: 1,
        duration: 1, amountMinor,
      },
    );

    // 3) Pay — transitions the order to PAID (the only provisioning trigger, D8).
    await sdk.post(`/api/v1/orders/${created.data.orderId}/pay`);

    // 4) Fulfill — the orchestrator runs the saga (order PAID→FULFILLING→COMPLETED,
    // resource CREATING→RUNNING) and returns the new resource id. The
    // productCode=sclb routes it to the DriverK8s binding.
    const res = await sdk.post<{ resourceId: string; state: string; orderState: string }>(
      "/api/v1/orchestrator/fulfill",
      {
        orderId: created.data.orderId, orderNo: created.data.orderNo,
        productCode: "sclb", region: form.value.region,
        specCode, chargeType: "POSTPAID",
        lbType: form.value.lbType,
        listenPort: form.value.listenPort,
        protocol: form.value.protocol,
        backends,
      },
    );
    ElMessage.success(`负载均衡已创建:${res.data.resourceId.slice(0, 24)}…(状态 ${res.data.state})`);
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
    <h1>创建负载均衡</h1>
    <div class="buy-layout">
      <div class="buy-main">
        <ElSteps :active="active" align-center class="buy-steps">
          <ElStep title="基础配置" /><ElStep title="监听器与后端" /><ElStep title="确认下单" />
        </ElSteps>

        <ElForm v-show="active === 0" label-position="top" class="buy-form">
          <ElFormItem label="地域"><ElSelect v-model="form.region"><ElOption v-for="r in regions" :key="r.regionId" :value="r.regionId" :label="r.regionName" /></ElSelect></ElFormItem>
          <ElFormItem label="实例规格">
            <ElSelect v-model="form.spec" :loading="skus.length === 0">
              <ElOption v-for="s in specs" :key="s.code" :value="s.code" :label="s.label" />
            </ElSelect>
            <p class="form-hint">负载均衡为 REGIONAL 产品:跨可用区入口,无需选择可用区。</p>
          </ElFormItem>
          <ElFormItem label="类型">
            <span class="readonly-spec">{{ form.lbType === "l7" ? "七层 (HTTP/HTTPS)" : form.lbType === "l4" ? "四层 (TCP/UDP)" : "—" }}</span>
          </ElFormItem>
        </ElForm>

        <ElForm v-show="active === 1" label-position="top" class="buy-form">
          <ElFormItem label="监听协议">
            <ElSelect v-model="form.protocol">
              <ElOption v-if="form.lbType === 'l7'" value="HTTPS" label="HTTPS" />
              <ElOption v-if="form.lbType === 'l7'" value="HTTP" label="HTTP" />
              <ElOption v-if="form.lbType === 'l4'" value="TCP" label="TCP" />
              <ElOption v-if="form.lbType === 'l4'" value="UDP" label="UDP" />
            </ElSelect>
          </ElFormItem>
          <ElFormItem label="监听端口"><ElInputNumber v-model="form.listenPort" :min="1" :max="65535" /></ElFormItem>
          <ElFormItem label="后端服务器 (每行一个 IP:端口)">
            <ElInput v-model="form.backends" type="textarea" :rows="4" placeholder="10.128.3.17:8080" />
          </ElFormItem>
        </ElForm>

        <div v-show="active === 2" class="buy-confirm">
          <h2>确认配置</h2>
          <ul class="confirm-list">
            <li>地域:{{ form.region }}</li>
            <li>规格:{{ specs.find(s=>s.code===form.spec)?.label }}</li>
            <li>监听:{{ form.protocol }}:{{ form.listenPort }}</li>
            <li>后端:{{ parseBackends().length }} 个</li>
            <li>计费方式:按量付费(LCU + 流量)</li>
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
          <div><dt>监听</dt><dd>{{ form.protocol }}:{{ form.listenPort }}</dd></div>
          <div><dt>后端</dt><dd>{{ parseBackends().length }} 个</dd></div>
        </dl>
        <div class="sum-total">
          <span>预估每小时</span>
          <strong v-if="quoting">…</strong>
          <strong v-else-if="perHour !== null">¥{{ perHour.toFixed(4) }}</strong>
          <strong v-else>—</strong>
        </div>
        <p class="sum-hint">按量计费(LCU+流量),每小时结算;价格由服务端试算</p>
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
.readonly-spec { font-size: 14px; color: var(--sc-text-secondary); }
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
