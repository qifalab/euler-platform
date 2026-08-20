<script setup lang="ts">
/** 发布商品 — POST /api/v1/marketplace/listings (伙伴提交,进入 PENDING_APPROVAL). */
import { ref } from "vue";
import { useRouter } from "vue-router";
import { ElMessage } from "element-plus";
import { PageHeader } from "@sc/ui";
import { sdk, type ListingDTO } from "@/marketplace";

const router = useRouter();
const form = ref({ name: "", category: "IMAGE", openapiUrl: "", partnerRate: 30 });
const submitting = ref(false);

async function submit() {
  if (!form.value.name) { ElMessage.warning("请填写商品名称"); return; }
  if (!form.value.openapiUrl) { ElMessage.warning("请填写履约 OpenAPI 地址"); return; }
  const rate = form.value.partnerRate;
  if (rate < 0 || rate > 100) { ElMessage.warning("分成比例需在 0–100% 之间"); return; }
  submitting.value = true;
  try {
    await sdk.post<ListingDTO>("/api/v1/marketplace/listings", {
      name: form.value.name,
      category: form.value.category,
      openapiUrl: form.value.openapiUrl,
      partnerRateBps: Math.round(rate * 100), // % → bps (10000 bps = 100%)
    });
    ElMessage.success("已提交,等待平台审核");
    router.push("/review");
  } catch (e) {
    ElMessage.error(`提交失败:${(e as Error).message}`);
  } finally {
    submitting.value = false;
  }
}
</script>

<template>
  <div class="publish">
    <PageHeader title="发布商品" />
    <p class="pb-hint">发布后进入「上架审核」队列;三方产品按回调伙伴 OpenAPI 方式履约,平台不代运营伙伴产品。</p>
    <form class="pb-form" @submit.prevent="submit">
      <label class="pb-field"><span>商品名称</span>
        <input v-model="form.name" placeholder="如:伙伴 MySQL 镜像 8.0" />
      </label>
      <label class="pb-field"><span>商品类别</span>
        <select v-model="form.category">
          <option value="IMAGE">镜像</option>
          <option value="SAAS">SaaS</option>
          <option value="SERVICE">服务</option>
        </select>
      </label>
      <label class="pb-field"><span>履约 OpenAPI 地址</span>
        <input v-model="form.openapiUrl" placeholder="https://partner.example/api" />
      </label>
      <label class="pb-field"><span>伙伴分成比例(%)</span>
        <input v-model.number="form.partnerRate" type="number" min="0" max="100" step="0.5" />
      </label>
      <button class="pb-submit" type="submit" :disabled="submitting">{{ submitting ? "提交中…" : "提交审核" }}</button>
    </form>
  </div>
</template>

<style scoped>
.publish { padding: var(--sc-spacing-6); max-width: 720px; }
.pb-hint { color: var(--sc-text-secondary); font-size: 13px; margin: 0 0 var(--sc-spacing-4); }
.pb-form {
  display: flex; flex-direction: column; gap: var(--sc-spacing-4);
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  box-shadow: var(--sc-shadow-sm);
  padding: var(--sc-spacing-6);
}
.pb-field { display: flex; flex-direction: column; gap: 6px; font-size: 13px; color: var(--sc-text-secondary); }
.pb-field input, .pb-field select {
  padding: 10px 12px; background: var(--sc-bg-container); border: 1px solid var(--sc-border);
  border-radius: var(--sc-radius-md); font-size: 14px; color: var(--sc-text-primary);
  outline: none; font-family: inherit;
  transition: border-color var(--sc-transition), box-shadow var(--sc-transition);
}
.pb-field input:focus, .pb-field select:focus {
  border-color: var(--sc-color-brand); box-shadow: 0 0 0 3px var(--sc-color-brand-soft);
}
.pb-submit {
  align-self: flex-start; padding: 11px 24px; border: none; border-radius: var(--sc-radius-md);
  background: var(--sc-color-brand); color: var(--sc-color-on-brand); font-size: 14px; cursor: pointer;
  transition: background var(--sc-transition);
}
.pb-submit:hover { background: var(--sc-color-brand-hover); }
.pb-submit:disabled { opacity: 0.6; cursor: not-allowed; }
</style>
