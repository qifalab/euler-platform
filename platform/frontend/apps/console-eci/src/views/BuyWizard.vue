<script setup lang="ts">
/**
 * Buy wizard (02§7.4): step form + live price summary on the right.
 *
 * Wired to the real backend chain (same shape as console-ecs, productCode=eueci):
 *  - GET  /api/v1/catalog/skus?productCode=eueci — spec catalogue
 *  - GET  /api/v1/catalog/placement?productCode=eueci — placement contract (M-6)
 *  - POST /api/v1/catalog/quote                  — 询价 (real pricing engine)
 *  - POST /api/v1/orders                         — create order (real orderId)
 *  - POST /api/v1/orders/{id}/pay                — mark PAID
 *  - POST /api/v1/orchestrator/fulfill           — saga → resource RUNNING
 *
 * EUECI is POSTPAID ONLY (no prepaid — a reserved container would just be a
 * VM). The catalogue's per-SECOND pricing rule (M-7.1) returns the per-second
 * unit price as payableAmount; the summary shows the 预估每小时 (×3600) so the
 * user sees a relatable figure. Price is server-trial-computed; the client
 * never invents a unit price. The order id is issued by svc-order.
 */
import { ref, computed, watch, onMounted, onUnmounted } from "vue";
import { useRouter } from "vue-router";
import { ElSteps, ElStep, ElForm, ElFormItem, ElSelect, ElOption, ElInput, ElButton, ElMessage } from "element-plus";
import { createSDK, yuanToMinor } from "@eu/sdk";
import { useCatalogMeta } from "@eu/console-kit";

const router = useRouter();
const sdk = createSDK({ baseURL: "" });
const submitting = ref(false);
const active = ref(0);

const form = ref({
  region: "", zone: "",
  spec: "", cpu: 0, memory: 0,
  image: "registry.euler.emoera.com/library/nginx:1.27",
  command: "",
});

// --- region / zone metadata (catalogue-driven, no hardcoded lists) ---
const { regions, placement, load, zonesOf } = useCatalogMeta("eueci");

// --- spec catalogue (real, from svc-catalog) ---

interface Sku {
  skuCode: string;
  productCode: string;
  chargeType: string; // POSTPAID only for ECI
  specJson: string;
  status: string;
}
interface SkuSpec { cpu: number; mem_gb: number }

const skus = ref<Sku[]>([]);

const specs = computed(() => {
  // ECI has only postpaid SKUs, so there is no prepaid/postpaid grouping —
  // the spec list is the SKU list, labelled by cpu/mem.
  return skus.value.map((s) => {
    const spec = parseSpec(s.specJson);
    return { code: s.skuCode, label: `${spec.cpu}c${spec.mem_gb}g 通用型`, cpu: spec.cpu, mem_gb: spec.mem_gb };
  });
});

function parseSpec(json: string): SkuSpec {
  try {
    const p = JSON.parse(json) as Partial<SkuSpec>;
    return { cpu: p.cpu ?? 0, mem_gb: p.mem_gb ?? 0 };
  } catch {
    return { cpu: 0, mem_gb: 0 };
  }
}

onMounted(async () => {
  try {
    const res = await sdk.get<Sku[]>("/api/v1/catalog/skus?productCode=eueci");
    skus.value = res.data ?? [];
    if (specs.value.length && !form.value.spec) {
      form.value.spec = specs.value[0].code;
      form.value.cpu = specs.value[0].cpu;
      form.value.memory = specs.value[0].mem_gb;
    }
  } catch (e) {
    ElMessage.error(`加载规格目录失败:${(e as Error).message}`);
  }
  // Region / zone metadata: align defaults to the catalogue rows (load()
  // swallows its own errors and just sets `error`; safe to await).
  await load();
  if (!regions.value.some((r) => r.regionId === form.value.region)) {
    form.value.region = regions.value[0]?.regionId ?? "";
  }
  const zones = zonesOf(form.value.region);
  if (!zones.some((z) => z.zoneId === form.value.zone)) {
    form.value.zone = zones[0]?.zoneId ?? "";
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
//
// The catalogue's per-SECOND rule returns the per-second unit price. We show
// the 预估每小时 = perSecond × 3600 so the figure is relatable; the actual
// bill is per-second (the orchestrator meters cpu_core_second/mem_gb_second).

interface QuoteResult {
  payableAmount: string; // per-second yuan, decimal
  durationUnit?: string;
  listAmount?: string;
}
const quote = ref<QuoteResult | null>(null);
const quoting = ref(false);

const perHour = computed(() => {
  if (!quote.value) return null;
  const perSecond = Number(quote.value.payableAmount);
  if (!Number.isFinite(perSecond)) return null;
  return perSecond * 3600;
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
      productCode: "eueci",
      specCode,
      chargeType: "POSTPAID",
      quantity: 1,
      regionId: form.value.region,
      zoneId: form.value.zone, // ZONAL product: quote path enforces the AZ (M-6)
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
  [() => form.value.spec, () => form.value.region, () => form.value.zone],
  () => {
    if (quoteTimer) clearTimeout(quoteTimer);
    quoteTimer = setTimeout(refreshQuote, 300);
  },
);

function next() { if (active.value < 2) active.value++; }
function prev() { if (active.value > 0) active.value--; }

async function submit() {
  const specCode = form.value.spec;
  if (!specCode) { ElMessage.error("请选择实例规格"); active.value = 0; return; }
  if (!form.value.image) { ElMessage.warning("请填写容器镜像"); active.value = 1; return; }
  submitting.value = true;
  try {
    // 1) Quote — the authoritative per-second payable comes from the pricing engine.
    const q = await sdk.post<QuoteResult>("/api/v1/catalog/quote", {
      productCode: "eueci", specCode,
      chargeType: "POSTPAID", quantity: 1,
      regionId: form.value.region, zoneId: form.value.zone,
    });

    // 2) Create the order — svc-order issues the real orderId/orderNo. ECI is
    // postpaid, so the initial amount is the per-second price annualised to one
    // hour (×3600), in 分; actual billing accrues per-second from metering,
    // settled hourly.
    const amountMinor = Math.max(1, yuanToMinor(Number(q.data.payableAmount) * 3600));
    const created = await sdk.post<{ orderId: number; orderNo: string; state: string }>(
      "/api/v1/orders",
      {
        type: "NEW", productCode: "eueci", skuCode: specCode,
        regionId: form.value.region, quantity: 1,
        duration: 1, amountMinor,
      },
    );

    // 3) Pay — transitions the order to PAID (the only provisioning trigger, D8).
    await sdk.post(`/api/v1/orders/${created.data.orderId}/pay`, {
      paymentId: `pay-${created.data.orderId}`,
      paidAmountMinor: amountMinor,
    });

    // 4) Fulfill — the orchestrator runs the saga (order PAID→FULFILLING→COMPLETED,
    // resource CREATING→RUNNING) and returns the new resource id. The
    // productCode=eueci routes it to the DriverK8s binding.
    const res = await sdk.post<{ resourceId: string; state: string; orderState: string }>(
      "/api/v1/orchestrator/fulfill",
      {
        orderId: created.data.orderId, orderNo: created.data.orderNo,
        productCode: "eueci", region: form.value.region,
        specCode, chargeType: "POSTPAID",
        zone: form.value.zone, image: form.value.image,
        command: form.value.command,
        cpu: form.value.cpu, memoryGb: form.value.memory,
      },
    );
    ElMessage.success(`容器实例已创建:${res.data.resourceId.slice(0, 24)}…(状态 ${res.data.state})`);
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
    <h1>创建容器实例</h1>
    <div class="buy-layout">
      <div class="buy-main">
        <ElSteps :active="active" align-center class="buy-steps">
          <ElStep title="基础配置" /><ElStep title="容器与镜像" /><ElStep title="确认下单" />
        </ElSteps>

        <ElForm v-show="active === 0" label-position="top" class="buy-form">
          <ElFormItem label="地域"><ElSelect v-model="form.region"><ElOption v-for="r in regions" :key="r.regionId" :value="r.regionId" :label="r.regionName" /></ElSelect></ElFormItem>
          <ElFormItem v-if="placement === null || placement.zoneRequired" label="可用区">
            <ElSelect v-model="form.zone">
              <ElOption v-for="z in zonesOf(form.region)" :key="z.zoneId" :value="z.zoneId" :label="z.zoneName" />
            </ElSelect>
            <p class="form-hint">容器实例为 ZONAL 产品:调度器将 Pod 绑定到所选可用区内有容量的节点。</p>
          </ElFormItem>
          <ElFormItem label="实例规格">
            <ElSelect v-model="form.spec" :loading="skus.length === 0">
              <ElOption v-for="s in specs" :key="s.code" :value="s.code" :label="s.label" />
            </ElSelect>
          </ElFormItem>
        </ElForm>

        <ElForm v-show="active === 1" label-position="top" class="buy-form">
          <ElFormItem label="容器镜像"><ElInput v-model="form.image" placeholder="registry.euler.emoera.com/library/nginx:1.27" /></ElFormItem>
          <ElFormItem label="启动命令 (可选)"><ElInput v-model="form.command" placeholder="如 nginx -g 'daemon off;'" /></ElFormItem>
          <ElFormItem label="CPU / 内存">
            <span class="readonly-spec">{{ form.cpu }} 核 / {{ form.memory }} GiB</span>
          </ElFormItem>
        </ElForm>

        <div v-show="active === 2" class="buy-confirm">
          <h2>确认配置</h2>
          <ul class="confirm-list">
            <li>地域可用区:{{ form.region }} / {{ form.zone }}</li>
            <li>规格:{{ specs.find(s=>s.code===form.spec)?.label }}</li>
            <li>镜像:{{ form.image }}</li>
            <li>计费方式:按量付费(按秒)</li>
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
          <div><dt>可用区</dt><dd>{{ form.zone }}</dd></div>
          <div><dt>镜像</dt><dd>{{ form.image ? form.image.split("/").pop() : "—" }}</dd></div>
        </dl>
        <div class="sum-total">
          <span>预估每小时</span>
          <strong v-if="quoting">…</strong>
          <strong v-else-if="perHour !== null">¥{{ perHour.toFixed(4) }}</strong>
          <strong v-else>—</strong>
        </div>
        <p class="sum-hint">按秒计费,每小时结算;价格由服务端试算</p>
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
.confirm-list { list-style: none; padding: 0; margin: 0; }
.confirm-list li { padding: 6px 0; border-bottom: 1px solid var(--eu-border); font-size: 13px; }
.form-hint { font-size: 12px; color: var(--eu-text-disabled); margin: 4px 0 0; }
.readonly-spec { font-size: 14px; color: var(--eu-text-secondary); }
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
