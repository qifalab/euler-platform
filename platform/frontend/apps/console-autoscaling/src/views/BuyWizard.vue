<script setup lang="ts">
/**
 * Buy wizard (02§7.4): step form + live price summary on the right.
 *
 * Wired to the real backend chain (same shape as console-ecs/eci, productCode=scas):
 *  - GET  /api/v1/catalog/skus?productCode=scas — spec catalogue
 *  - POST /api/v1/catalog/quote                  — 询价 (real pricing engine)
 *  - POST /api/v1/orders                         — create order (real orderId)
 *  - POST /api/v1/orders/{id}/pay                — mark PAID
 *  - POST /api/v1/orchestrator/fulfill           — saga → resource RUNNING
 *
 * SCAS is POSTPAID ONLY. It is REGIONAL (a scaling group spans a region's
 * compute, not pinned to one AZ), so there is NO zone picker — unlike the
 * ZONAL products (scecs/sceci), the quote does not carry a zoneId. The
 * catalogue's per-HOUR pricing rule returns the per-hour management fee as
 * payableAmount; the managed ECS/ECI instances bill separately under their own
 * products. Price is server-trial-computed; the client never invents a price.
 */
import { ref, computed, watch, onMounted } from "vue";
import { useRouter } from "vue-router";
import { ElSteps, ElStep, ElForm, ElFormItem, ElSelect, ElOption, ElInputNumber, ElRadioGroup, ElRadio, ElButton, ElMessage } from "element-plus";
import { createSDK } from "@sc/sdk";

const router = useRouter();
const sdk = createSDK({ baseURL: "" });
const submitting = ref(false);
const active = ref(0);

const form = ref({
  region: "cn-north-1",
  spec: "",
  managedType: "eci",
  minReplicas: 1,
  maxReplicas: 10,
  desiredReplicas: 3,
  cpuThreshold: 0.8,
  cooldownSeconds: 300,
});

// --- spec catalogue (real, from svc-catalog) ---

interface Sku {
  skuCode: string;
  productCode: string;
  chargeType: string;
  specJson: string;
  status: string;
}
interface SkuSpec { managed_type?: string }

const skus = ref<Sku[]>([]);

const specs = computed(() => {
  // Each SKU's specJson carries the managed_type (ecs/eci). Label by it.
  return skus.value.map((s) => {
    const spec = parseSpec(s.specJson);
    const label = spec.managed_type === "eci" ? "管理 ECI(弹性容器实例)" : "管理 ECS(云服务器)";
    return { code: s.skuCode, label, managed_type: spec.managed_type ?? "ecs" };
  });
});

function parseSpec(json: string): SkuSpec {
  try {
    return JSON.parse(json) as SkuSpec;
  } catch {
    return {};
  }
}

onMounted(async () => {
  try {
    const res = await sdk.get<Sku[]>("/api/v1/catalog/skus?productCode=scas");
    skus.value = res.data ?? [];
    if (specs.value.length && !form.value.spec) {
      form.value.spec = specs.value[0].code;
      form.value.managedType = specs.value[0].managed_type;
    }
  } catch (e) {
    ElMessage.error(`加载规格目录失败:${(e as Error).message}`);
  }
});

watch(() => form.value.spec, (code) => {
  const entry = specs.value.find((s) => s.code === code);
  if (entry) form.value.managedType = entry.managed_type;
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
  if (!Number.isFinite(v)) return null;
  return v;
});

let quoteTimer: ReturnType<typeof setTimeout> | null = null;
async function refreshQuote() {
  const specCode = form.value.spec;
  if (!specCode) { quote.value = null; return; }
  quoting.value = true;
  try {
    const res = await sdk.post<QuoteResult>("/api/v1/catalog/quote", {
      productCode: "scas",
      specCode,
      chargeType: "POSTPAID",
      quantity: 1,
      regionId: form.value.region,
      // No zoneId: SCAS is REGIONAL, the quote path does not require one (M-6).
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

async function submit() {
  const specCode = form.value.spec;
  if (!specCode) { ElMessage.error("请选择规格"); active.value = 0; return; }
  if (form.value.minReplicas > form.value.maxReplicas) {
    ElMessage.warning("最小副本数不能大于最大副本数"); active.value = 1; return;
  }
  submitting.value = true;
  try {
    // 1) Quote — the authoritative per-hour management fee from the pricing engine.
    const q = await sdk.post<QuoteResult>("/api/v1/catalog/quote", {
      productCode: "scas", specCode,
      chargeType: "POSTPAID", quantity: 1,
      regionId: form.value.region,
    });

    // 2) Create the order — svc-order issues the real orderId/orderNo. The
    // scaling-group management fee is per-hour; the managed instances bill
    // separately under their own products.
    const amountMinor = Math.max(1, Math.round(Number(q.data.payableAmount)));
    const created = await sdk.post<{ orderId: number; orderNo: string; state: string }>(
      "/api/v1/orders",
      {
        type: "NEW", productCode: "scas", skuCode: specCode,
        regionId: form.value.region, quantity: 1,
        duration: 1, amountMinor,
      },
    );

    // 3) Pay — transitions the order to PAID (the only provisioning trigger, D8).
    await sdk.post(`/api/v1/orders/${created.data.orderId}/pay`);

    // 4) Fulfill — the orchestrator runs the saga (order PAID→FULFILLING→COMPLETED,
    // resource CREATING→RUNNING) and returns the new resource id. productCode=scas
    // routes it to the DriverK8s binding (the policy layer over HPA/VPA/CA).
    const res = await sdk.post<{ resourceId: string; state: string; orderState: string }>(
      "/api/v1/orchestrator/fulfill",
      {
        orderId: created.data.orderId, orderNo: created.data.orderNo,
        productCode: "scas", region: form.value.region,
        specCode, chargeType: "POSTPAID",
        managedType: form.value.managedType,
        minReplicas: form.value.minReplicas, maxReplicas: form.value.maxReplicas,
        desiredReplicas: form.value.desiredReplicas, cpuThreshold: form.value.cpuThreshold,
        cooldownSeconds: form.value.cooldownSeconds,
      },
    );
    ElMessage.success(`伸缩组已创建:${res.data.resourceId.slice(0, 24)}…(状态 ${res.data.state})`);
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
    <h1>创建伸缩组</h1>
    <div class="buy-layout">
      <div class="buy-main">
        <ElSteps :active="active" align-center class="buy-steps">
          <ElStep title="基础配置" /><ElStep title="伸缩策略" /><ElStep title="确认下单" />
        </ElSteps>

        <ElForm v-show="active === 0" label-position="top" class="buy-form">
          <ElFormItem label="地域"><ElSelect v-model="form.region"><ElOption value="cn-north-1" label="华北 1(北京)" /></ElSelect></ElFormItem>
          <ElFormItem label="规格">
            <ElSelect v-model="form.spec" :loading="skus.length === 0">
              <ElOption v-for="s in specs" :key="s.code" :value="s.code" :label="s.label" />
            </ElSelect>
            <p class="form-hint">伸缩组为 REGIONAL 产品:跨可用区管理计算资源,无需选择可用区。</p>
          </ElFormItem>
        </ElForm>

        <ElForm v-show="active === 1" label-position="top" class="buy-form">
          <ElFormItem label="管理对象">
            <ElRadioGroup v-model="form.managedType">
              <ElRadio value="eci">ECI(弹性容器实例)</ElRadio>
              <ElRadio value="ecs">ECS(云服务器)</ElRadio>
            </ElRadioGroup>
            <p class="form-hint">被管理的 ECS/ECI 实例按各自产品计费;伸缩组仅收管理费。</p>
          </ElFormItem>
          <ElFormItem label="最小副本数"><ElInputNumber v-model="form.minReplicas" :min="0" :max="1000" /></ElFormItem>
          <ElFormItem label="最大副本数"><ElInputNumber v-model="form.maxReplicas" :min="1" :max="10000" /></ElFormItem>
          <ElFormItem label="期望副本数"><ElInputNumber v-model="form.desiredReplicas" :min="0" :max="10000" /></ElFormItem>
          <ElFormItem label="CPU 扩容阈值"><ElInputNumber v-model="form.cpuThreshold" :min="0" :max="1" :step="0.1" :precision="2" /></ElFormItem>
          <ElFormItem label="冷却时间(秒)"><ElInputNumber v-model="form.cooldownSeconds" :min="0" :max="3600" /></ElFormItem>
        </ElForm>

        <div v-show="active === 2" class="buy-confirm">
          <h2>确认配置</h2>
          <ul class="confirm-list">
            <li>地域:{{ form.region }}</li>
            <li>规格:{{ specs.find(s=>s.code===form.spec)?.label }}</li>
            <li>管理对象:{{ form.managedType === "eci" ? "ECI(弹性容器实例)" : "ECS(云服务器)" }}</li>
            <li>副本数:{{ form.minReplicas }} ~ {{ form.maxReplicas }}(期望 {{ form.desiredReplicas }})</li>
            <li>CPU 扩容阈值:{{ form.cpuThreshold }}</li>
            <li>冷却时间:{{ form.cooldownSeconds }} 秒</li>
            <li>计费方式:按量付费(管理费/小时)</li>
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
          <div><dt>管理对象</dt><dd>{{ form.managedType === "eci" ? "ECI" : "ECS" }}</dd></div>
          <div><dt>副本范围</dt><dd>{{ form.minReplicas }} ~ {{ form.maxReplicas }}</dd></div>
        </dl>
        <div class="sum-total">
          <span>预估每小时</span>
          <strong v-if="quoting">…</strong>
          <strong v-else-if="perHour !== null">¥{{ perHour.toFixed(4) }}</strong>
          <strong v-else>—</strong>
        </div>
        <p class="sum-hint">伸缩组管理费/小时;被管理实例各自计费</p>
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
