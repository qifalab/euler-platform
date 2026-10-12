<script setup lang="ts">
/**
 * console-storage — OSS Bucket list (EUOSS), the storage category sub-app (02§7.2).
 * Demonstrates the declarative ResourceTable pattern from @eu/console-kit with
 * unified StatusBadge, polling on transitional states, and empty-guide.
 *
 * Bucket creation runs the real 4-step chain (mirror of console-ecs BuyWizard):
 *  - GET  /api/v1/catalog/skus?productCode=euoss — storage-class SKUs (POSTPAID)
 *  - POST /api/v1/catalog/quote                  — 询价 (real pricing engine)
 *  - POST /api/v1/orders                         — create order (real orderId)
 *  - POST /api/v1/orders/{id}/pay                — mark PAID
 *  - POST /api/v1/orchestrator/fulfill           — saga → resource (bucket id)
 * The fulfill endpoint does not accept a bucket name — the bucket id shown in
 * the list IS the ResourceId, so the dialog collects region + storage class
 * only, never a fabricated name.
 */
import { ref, computed, h, onMounted, onUnmounted, watch } from "vue";
import { ElButton, ElDialog, ElForm, ElFormItem, ElSelect, ElOption, ElMessage } from "element-plus";
import {
  ResourceTable, useResourceTable, useCatalogMeta, useOrderCheckout, fulfilledMessage,
  paginate, isTransitionalState, formatTime, attributesOf, resourceDisplayName,
} from "@eu/console-kit";
import { StatusBadge, EmptyGuide, PageHeader } from "@eu/ui";
import { sdk } from "./sdk";
import { yuanToMinor } from "@eu/sdk";
import BucketDetail from "./views/BucketDetail.vue";
import "@eu/tokens/style.css";

// Internal hash routing (no vue-router instance in this sub-app, mirroring the
// console-network reference 02§3.4). The list links to #/euoss/buckets/<id>;
// when the hash matches that shape the detail page takes over. Before this the
// link only changed the URL — BucketDetail existed but was never rendered, so
// every bucket link was a dead end.
const hashRoute = ref(location.hash);
function onHash() { hashRoute.value = location.hash; }
onMounted(() => window.addEventListener("hashchange", onHash));
onUnmounted(() => window.removeEventListener("hashchange", onHash));
const detailBucketId = computed(() => {
  const m = hashRoute.value.match(/euoss\/buckets\/([^/?#]+)/);
  return m ? decodeURIComponent(m[1]) : null;
});
const showDetail = computed(() => detailBucketId.value !== null);

type BucketRow = Record<string, unknown>;

// Real fetcher: console-bff /console/resources, filtered to euoss. The BFF
// returns the shared Resource shape (store.go); map to list columns. Fields
// not surfaced by the resource list (objectCount/size/access) fall back to a
// dash so the existing columns/template keep rendering.
const { rows, loading, error, columns, page, pageSize, total, setPage, refresh } = useResourceTable<BucketRow>({
  api: async (params, signal) => {
    const res = await sdk.get<BucketRow[]>("/console/resources", { signal });
    const items = (res.data ?? [])
      .filter((r) => r.ProductCode === "euoss")
      .map((r) => ({
        bucketName: r.ResourceId,
        // The customer-given `name` attribute (when set) beside the bucket id.
        displayName: resourceDisplayName(String(r.ResourceId ?? ""), attributesOf(r).name),
        status: String(r.State ?? "").charAt(0).toUpperCase() + String(r.State ?? "").slice(1).toLowerCase(),
        storageClass: r.SpecCode,
        region: r.Region,
        objectCount: "—",
        size: "—",
        access: "—",
        createTime: formatTime(r.CreatedAt as string | undefined),
      }));
    // The endpoint returns every resource: page client-side, total = full count.
    return paginate(items, params);
  },
  columns: [
    { key: "bucketName", title: "存储桶名称", link: (r) => `#/euoss/buckets/${r.bucketName}` },
    { key: "status", title: "状态" },
    { key: "storageClass", title: "存储类型" },
    { key: "region", title: "地域" },
    { key: "objectCount", title: "对象数量" },
    { key: "size", title: "存储用量" },
    { key: "access", title: "读写权限" },
    { key: "createTime", title: "创建时间" },
  ],
  polling: { interval: 10_000, when: (r) => r.some((x) => isTransitionalState(x.status)) },
  pageSize: 20,
});

// Wrap StatusBadge as a column render for the status column.
const tableColumns = computed(() =>
  columns.value.map((c) =>
    c.key === "status"
      ? { ...c, render: (row: BucketRow) => h(StatusBadge, { status: String(row.status ?? "") }) }
      : c.key === "bucketName"
        ? { ...c, render: (row: BucketRow) => String(row.displayName ?? row.bucketName ?? "") }
        : c,
  ),
);

/** 后端错误体 message 透出;SDK 兜底文案时退回错误码。 */
function errMsg(e: unknown): string {
  const err = e as { message?: string; code?: string };
  const msg = err?.message ?? "";
  if (!msg || msg === "request failed") return err?.code ?? "请求失败";
  return msg;
}

// --- 创建存储桶弹窗 ---

// euoss 目录元数据:地域来自 svc-catalog(无客户端硬编码地域表)。euoss 为
// REGIONAL 部署(placement.zoneRequired=false),无可用区选择。
const { regions, load } = useCatalogMeta("euoss");

interface EuossSku {
  skuCode: string;
  productCode: string;
  chargeType: string; // PREPAID | POSTPAID
  specJson: string;   // e.g. {"storage_class":"standard"}
  status: string;     // "1" = 上架
}
interface StorageClassOption { skuCode: string; storageClass: string; chargeType: string }

const createVisible = ref(false);
const skuOptions = ref<StorageClassOption[]>([]);
const form = ref({ region: "", sku: "" });

onMounted(async () => {
  void load();
  try {
    const res = await sdk.get<EuossSku[]>("/api/v1/catalog/skus?productCode=euoss");
    skuOptions.value = (res.data ?? [])
      .filter((s) => s.status === "1")
      .map((s) => {
        let storageClass = s.skuCode;
        try {
          storageClass = (JSON.parse(s.specJson) as Partial<{ storage_class: string }>).storage_class ?? s.skuCode;
        } catch { /* 解析失败退回 skuCode */ }
        return { skuCode: s.skuCode, storageClass, chargeType: s.chargeType };
      });
    if (!form.value.sku && skuOptions.value.length) form.value.sku = skuOptions.value[0].skuCode;
  } catch (e) {
    ElMessage.error(`加载存储类型失败:${errMsg(e)}`);
  }
});

function openCreate() {
  createVisible.value = true;
  if (!form.value.region && regions.value.length) form.value.region = regions.value[0].regionId;
  if (!form.value.sku && skuOptions.value.length) form.value.sku = skuOptions.value[0].skuCode;
}

// 地域元数据异步返回后再补默认选中,避免弹窗先开时下拉为空。
watch(regions, (rs) => {
  if (!form.value.region && rs.length) form.value.region = rs[0].regionId;
});

// Create → pay → fulfil, resumable: once svc-order has issued an order, a pay
// or fulfil failure is retried on THAT order (pending) — re-submitting never
// creates and pays a second one.
const { submitting: creating, pending, checkout, resume } = useOrderCheckout({
  sdk,
  onFulfilled: (res) => {
    ElMessage.success(fulfilledMessage("存储桶", res));
    createVisible.value = false;
    void refresh();
  },
});

async function submitCreate() {
  if (!form.value.region || !form.value.sku) { ElMessage.warning("请选择地域和存储类型"); return; }
  const specCode = form.value.sku;
  // The quote rejects a chargeType other than the SKU's own
  // (Catalog.ChargeTypeMismatch): send what the catalogue declares.
  const chargeType = skuOptions.value.find((s) => s.skuCode === specCode)?.chargeType || "POSTPAID";
  const region = form.value.region;
  await checkout(
    async () => {
      // 1) 询价 — 后端定价权威,金额不本地拼算。
      const q = await sdk.post<{ payableAmount: string }>("/api/v1/catalog/quote", {
        productCode: "euoss", specCode, chargeType, quantity: 1, regionId: region,
      });
      // svc-order takes amountMinor in 分 (1 分 = 0.01 元) — same conversion as
      // BuyWizard; the quote's payableAmount is a yuan decimal.
      const amountMinor = yuanToMinor(q.data.payableAmount);
      // 2) Create the order — svc-order issues the real orderId/orderNo.
      const created = await sdk.post<{ orderId: number; orderNo: string }>("/api/v1/orders", {
        type: "NEW", productCode: "euoss", skuCode: specCode, chargeType,
        regionId: region, quantity: 1, duration: 1, amountMinor,
      });
      return created.data;
    },
    // 3) Pay + 4) fulfil (the saga opens the bucket; the ResourceId IS the
    // bucket id), body captured now so a retry repeats what was ordered.
    { productCode: "euoss", region, specCode, chargeType },
  );
}

// EmptyGuide renders its action as a plain <a href> with no click hook, and
// the app has no /euoss/create route — intercept the click on the way in and
// open the create dialog instead of navigating to a dead route.
function onGuideAction(e: MouseEvent) {
  if ((e.target as HTMLElement | null)?.closest(".eu-empty-action")) {
    e.preventDefault();
    openCreate();
  }
}
</script>

<template>
  <section class="oss-app">
    <BucketDetail v-if="showDetail" :key="detailBucketId ?? ''" />
    <template v-else>
    <PageHeader title="对象存储 OSS">
      <template #actions>
        <ElButton type="primary" @click="openCreate">创建存储桶</ElButton>
      </template>
    </PageHeader>

    <!-- A failed load must not read as "暂无存储桶". -->
    <div v-if="error" class="oss-error">
      <p class="oss-error-msg">列表加载失败:{{ (error as Error)?.message ?? String(error) }}</p>
      <ElButton size="small" @click="refresh">重试</ElButton>
    </div>
    <template v-else>
    <ResourceTable
      :rows="rows as Record<string, unknown>[]"
      :columns="tableColumns"
      :loading="loading"
      :total="total"
      :page="page"
      :page-size="pageSize"
      @update:page="setPage"
    >
      <template #empty>
        <div @click.capture="onGuideAction">
          <EmptyGuide
            title="暂无存储桶"
            description="创建您的第一个存储桶,开始使用对象存储服务。"
            action-label="创建存储桶"
            action-href="#/euoss/create"
          />
        </div>
      </template>
    </ResourceTable>
    </template>

    <!-- 建桶弹窗:地域(useCatalogMeta)+ 存储类型(sku 目录)。fulfill 不接收
         桶名,列表中的桶名即 ResourceId,不做名称输入。 -->
    <ElDialog v-model="createVisible" title="创建存储桶" width="480px">
      <ElForm label-position="top">
        <ElFormItem label="地域">
          <ElSelect v-model="form.region" placeholder="请选择地域" style="width: 100%">
            <ElOption v-for="r in regions" :key="r.regionId" :value="r.regionId" :label="r.regionName" />
          </ElSelect>
        </ElFormItem>
        <ElFormItem label="存储类型">
          <ElSelect v-model="form.sku" placeholder="请选择存储类型" style="width: 100%">
            <ElOption v-for="s in skuOptions" :key="s.skuCode" :value="s.skuCode" :label="s.storageClass" />
          </ElSelect>
        </ElFormItem>
      </ElForm>
      <p class="oss-create-hint">按量付费(POSTPAID),创建后立即生效;桶名由系统按资源 ID 生成。</p>
      <template #footer>
        <ElButton @click="createVisible = false">取消</ElButton>
        <ElButton v-if="!pending" type="primary" :loading="creating" @click="submitCreate">创建</ElButton>
        <ElButton v-else type="warning" :loading="creating" @click="resume">{{ pending.stage === "pay" ? "继续支付" : "重试开通" }}(订单 {{ pending.orderNo }})</ElButton>
      </template>
    </ElDialog>
    </template>
  </section>
</template>

<style scoped>
.oss-app { padding: 16px 24px; }
.oss-create-hint { font-size: 12px; color: var(--eu-text-secondary); margin: 0; }
.oss-error {
  padding: 32px; text-align: center;
  border: 1px solid var(--eu-border); border-radius: var(--eu-radius-lg);
}
.oss-error-msg { color: var(--eu-color-danger); font-size: 13px; margin: 0 0 12px; }
</style>
