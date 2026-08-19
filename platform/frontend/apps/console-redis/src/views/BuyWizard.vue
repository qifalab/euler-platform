<script setup lang="ts">
/**
 * Buy wizard (02§7.4): step form + live price summary on the right.
 *
 * Wired to the real backend chain (same shape as console-ecs, productCode=scredis):
 *  - GET  /api/v1/catalog/skus?productCode=scredis — spec catalogue
 *  - GET  /api/v1/catalog/placement?productCode=scredis — placement contract (M-6)
 *  - POST /api/v1/catalog/quote                  — 询价 (real pricing engine)
 *  - POST /api/v1/orders                         — create order (real orderId)
 *  - POST /api/v1/orders/{id}/pay                — mark PAID
 *  - POST /api/v1/orchestrator/fulfill           — saga → resource RUNNING
 *
 * SCREDIS offers BOTH prepaid (包年包月) and postpaid (按量) — managed DBs offer
 * both, like scrds. The catalogue lists prepaid + postpaid variants of each
 * spec as separate SKUs; the wizard groups them and flips the quote between
 * MONTH (prepaid) and HOUR (postpaid) by the selected charge type. scredis is
 * ZONAL, so the AZ picker is present and the quote enforces the zone (M-6).
 * Price is server-trial-computed; the client never invents a unit price.
 */
import { ref, computed, watch, onMounted } from "vue";
import { useRouter } from "vue-router";
import { ElSteps, ElStep, ElForm, ElFormItem, ElSelect, ElOption, ElInput, ElInputNumber, ElRadioGroup, ElRadio, ElSwitch, ElButton, ElMessage } from "element-plus";
import { createSDK } from "@sc/sdk";
import { useCatalogMeta } from "@sc/console-kit";

const router = useRouter();
const sdk = createSDK({ baseURL: "" });
const submitting = ref(false);
const active = ref(0);

const form = ref({
  region: "", zone: "",
  spec: "", memGb: 0,
  vpcId: "", subnetId: "",
  ha: true,
  chargeType: "prepaid", period: 1,
});

// --- region / zone metadata (catalogue-driven, no hardcoded lists) ---
const { regions, placement, load, zonesOf } = useCatalogMeta("scredis");

// --- VPC options: the account's real scvpc resources (console-bff) ---
interface ConsoleResource { ResourceId: string; ProductCode: string; Region: string; State: string }
const vpcs = ref<ConsoleResource[]>([]);

// --- spec catalogue (real, from svc-catalog) ---

interface Sku {
  skuCode: string;
  productCode: string;
  chargeType: string; // PREPAID | POSTPAID
  specJson: string;   // e.g. {"engine":"redis","version":"7.0","mem_gb":1,"ha":true}
  status: string;
}
interface SkuSpec { mem_gb: number }

const skus = ref<Sku[]>([]);

// Derive a displayable spec per spec group: the catalogue lists prepaid and
// postpaid variants of the same spec as separate SKUs. Group by parsed mem_gb.
const specs = computed(() => {
  const bySpec = new Map<string, { code: string; label: string; mem_gb: number; prepaid: string; postpaid: string }>();
  for (const s of skus.value) {
    const spec = parseSpec(s.specJson);
    const key = `${spec.mem_gb}g`;
    const entry = bySpec.get(key) ?? { code: "", label: `Redis ${spec.mem_gb}G 标准型`, mem_gb: spec.mem_gb, prepaid: "", postpaid: "" };
    if (s.chargeType === "PREPAID" && !entry.prepaid) {
      entry.prepaid = s.skuCode;
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
    return { mem_gb: p.mem_gb ?? 0 };
  } catch {
    return { mem_gb: 0 };
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
    const res = await sdk.get<Sku[]>("/api/v1/catalog/skus?productCode=scredis");
    skus.value = res.data ?? [];
    if (specs.value.length && !form.value.spec) {
      form.value.spec = specs.value[0].code;
      form.value.memGb = specs.value[0].mem_gb;
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
  // VPC options: live scvpc resources of this account (never a fake id).
  try {
    const res = await sdk.get<ConsoleResource[]>("/console/resources");
    vpcs.value = (res.data ?? []).filter((r) => r.ProductCode === "scvpc" && r.State !== "RELEASED");
  } catch {
    vpcs.value = [];
  }
  alignVpcToRegion();
});

function alignVpcToRegion() {
  const inRegion = vpcs.value.filter((v) => v.Region === form.value.region);
  const pool = inRegion.length ? inRegion : vpcs.value;
  if (!pool.some((v) => v.ResourceId === form.value.vpcId)) {
    form.value.vpcId = pool[0]?.ResourceId ?? "";
  }
}

// Region switch: keep the zone and VPC valid for the newly-chosen region.
watch(() => form.value.region, () => {
  const zones = zonesOf(form.value.region);
  if (!zones.some((z) => z.zoneId === form.value.zone)) {
    form.value.zone = zones[0]?.zoneId ?? "";
  }
  alignVpcToRegion();
});

watch(() => form.value.spec, (code) => {
  const entry = specs.value.find((s) => s.code === code);
  if (entry) { form.value.memGb = entry.mem_gb; }
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
  const per = Number(quote.value.payableAmount);
  if (!Number.isFinite(per)) return null;
  // Prepaid: catalogue quotes per-month payable × duration. Postpaid: per-hour.
  return form.value.chargeType === "prepaid" ? per * form.value.period : per;
});

let quoteTimer: ReturnType<typeof setTimeout> | null = null;
async function refreshQuote() {
  const specCode = skuForCurrent();
  if (!specCode) { quote.value = null; return; }
  quoting.value = true;
  try {
    const res = await sdk.post<QuoteResult>("/api/v1/catalog/quote", {
      productCode: "scredis",
      specCode,
      chargeType: form.value.chargeType === "prepaid" ? "PREPAID" : "POSTPAID",
      duration: form.value.chargeType === "prepaid" ? form.value.period : 1,
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
  [() => form.value.spec, () => form.value.chargeType, () => form.value.period, () => form.value.region, () => form.value.zone],
  () => {
    if (quoteTimer) clearTimeout(quoteTimer);
    quoteTimer = setTimeout(refreshQuote, 300);
  },
);

function next() { if (active.value < 2) active.value++; }
function prev() { if (active.value > 0) active.value--; }

async function submit() {
  const specCode = skuForCurrent();
  if (!specCode) { ElMessage.error("请选择实例规格"); active.value = 0; return; }
  if (!form.value.vpcId) { ElMessage.warning("请选择专有网络"); active.value = 1; return; }
  submitting.value = true;
  try {
    // 1) Quote — the authoritative payable comes from the pricing engine.
    const q = await sdk.post<QuoteResult>("/api/v1/catalog/quote", {
      productCode: "scredis", specCode,
      chargeType: form.value.chargeType === "prepaid" ? "PREPAID" : "POSTPAID",
      duration: form.value.chargeType === "prepaid" ? form.value.period : 1,
      quantity: 1, regionId: form.value.region, zoneId: form.value.zone,
    });
    // svc-order treats amountMinor as a yuan integer (store.go:104 parses it as
    // a whole-yuan Amount), so round the catalogue's payableAmount to yuan.
    const amountMinor = Math.round(Number(q.data.payableAmount) * (form.value.chargeType === "prepaid" ? form.value.period : 1));

    // 2) Create the order — svc-order issues the real orderId/orderNo.
    const created = await sdk.post<{ orderId: number; orderNo: string; state: string }>(
      "/api/v1/orders",
      {
        type: "NEW", productCode: "scredis", skuCode: specCode,
        regionId: form.value.region, quantity: 1,
        duration: form.value.chargeType === "prepaid" ? form.value.period : 1,
        amountMinor,
      },
    );

    // 3) Pay — transitions the order to PAID (the only provisioning trigger, D8).
    await sdk.post(`/api/v1/orders/${created.data.orderId}/pay`);

    // 4) Fulfill — the orchestrator runs the saga (order PAID→FULFILLING→COMPLETED,
    // resource CREATING→RUNNING) and returns the new resource id. The
    // productCode=scredis routes it to the DriverK8s binding.
    const res = await sdk.post<{ resourceId: string; state: string; orderState: string }>(
      "/api/v1/orchestrator/fulfill",
      {
        orderId: created.data.orderId, orderNo: created.data.orderNo,
        productCode: "scredis", region: form.value.region,
        specCode, chargeType: form.value.chargeType.toUpperCase(),
        zone: form.value.zone, vpcId: form.value.vpcId, subnetId: form.value.subnetId,
        ha: form.value.ha,
      },
    );
    ElMessage.success(`Redis 实例已创建:${res.data.resourceId.slice(0, 24)}…(状态 ${res.data.state})`);
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
    <h1>创建 Redis 实例</h1>
    <div class="buy-layout">
      <div class="buy-main">
        <ElSteps :active="active" align-center class="buy-steps">
          <ElStep title="基础配置" /><ElStep title="网络与高可用" /><ElStep title="确认下单" />
        </ElSteps>

        <ElForm v-show="active === 0" label-position="top" class="buy-form">
          <ElFormItem label="地域"><ElSelect v-model="form.region"><ElOption v-for="r in regions" :key="r.regionId" :value="r.regionId" :label="r.regionName" /></ElSelect></ElFormItem>
          <ElFormItem v-if="placement === null || placement.zoneRequired" label="可用区">
            <ElSelect v-model="form.zone">
              <ElOption v-for="z in zonesOf(form.region)" :key="z.zoneId" :value="z.zoneId" :label="z.zoneName" />
            </ElSelect>
            <p class="form-hint">托管 Redis 为 ZONAL 产品:主节点落所选可用区,HA 副本落对侧可用区。</p>
          </ElFormItem>
          <ElFormItem label="实例规格">
            <ElSelect v-model="form.spec" :loading="skus.length === 0">
              <ElOption v-for="s in specs" :key="s.code" :value="s.code" :label="s.label" />
            </ElSelect>
          </ElFormItem>
        </ElForm>

        <ElForm v-show="active === 1" label-position="top" class="buy-form">
          <ElFormItem label="专有网络">
            <ElSelect v-model="form.vpcId" :loading="vpcs.length === 0" placeholder="该账号暂无 VPC，请先创建">
              <ElOption v-for="v in vpcs" :key="v.ResourceId" :value="v.ResourceId" :label="v.ResourceId" />
            </ElSelect>
          </ElFormItem>
          <ElFormItem label="交换机子网"><ElInput v-model="form.subnetId" placeholder="请输入子网 ID，如 subnet-xxx" /></ElFormItem>
          <ElFormItem label="跨可用区高可用">
            <ElSwitch v-model="form.ha" />
            <p class="form-hint">开启后主备跨可用区部署,副本可在单 AZ 故障时接管(M-6.2a 拓扑复用)。</p>
          </ElFormItem>
        </ElForm>

        <div v-show="active === 2" class="buy-confirm">
          <h2>确认配置</h2>
          <ul class="confirm-list">
            <li>地域可用区:{{ form.region }} / {{ form.zone }}</li>
            <li>规格:{{ specs.find(s=>s.code===form.spec)?.label }}</li>
            <li>专有网络:{{ form.vpcId }} / {{ form.subnetId }}</li>
            <li>跨可用区高可用:{{ form.ha ? "开启" : "关闭" }}</li>
          </ul>
          <ElRadioGroup v-model="form.chargeType" class="confirm-charge">
            <ElRadio value="prepaid">包年包月</ElRadio>
            <ElRadio value="postpaid">按量付费</ElRadio>
          </ElRadioGroup>
          <ElFormItem v-if="form.chargeType === 'prepaid'" label="购买时长(月)"><ElInputNumber v-model="form.period" :min="1" :max="36" /></ElFormItem>
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
          <div><dt>高可用</dt><dd>{{ form.ha ? "主备跨可用区" : "单可用区" }}</dd></div>
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
