<script setup lang="ts">
/** 商品目录 — GET /api/v1/marketplace/listings?category= (仅 APPROVED,对客目录). */
import { ref, onMounted } from "vue";
import { useRouter } from "vue-router";
import { ElButton } from "element-plus";
import { PageHeader } from "@sc/ui";
import { fetchListings, categoryLabel, bpsPercent, CATEGORIES, type ListingDTO } from "@/marketplace";

const router = useRouter();
const listings = ref<ListingDTO[]>([]);
const category = ref("");
const loading = ref(true);
const error = ref<string | null>(null);

async function load() {
  loading.value = true;
  error.value = null;
  try {
    listings.value = await fetchListings(undefined, category.value || undefined);
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}
function pick(c: string) { category.value = c; void load(); }

onMounted(() => void load());
</script>

<template>
  <div class="storefront">
    <PageHeader title="商品目录">
      <template #actions>
        <ElButton type="primary" @click="router.push('/publish')">发布商品</ElButton>
      </template>
    </PageHeader>
    <div class="sf-tabs">
      <button
        v-for="c in CATEGORIES" :key="c.value"
        class="sf-tab" :class="{ active: category === c.value }"
        @click="pick(c.value)"
      >{{ c.label }}</button>
    </div>
    <p v-if="error" class="sf-error">加载失败:{{ error }}(请确认 svc-marketplace 在 :9212)</p>
    <p v-else-if="loading" class="sf-loading">加载中…</p>
    <div v-else-if="listings.length" class="sf-cards">
      <div v-for="l in listings" :key="l.listingId" class="sf-card">
        <div class="sf-card-head">
          <span class="sf-cat">{{ categoryLabel(l.category) }}</span>
          <span class="sf-rate">伙伴分成 {{ bpsPercent(l.partnerRateBps) }}</span>
        </div>
        <h3 class="sf-name">{{ l.name }}</h3>
        <p class="sf-url">{{ l.openapiUrl }}</p>
        <div class="sf-card-foot">
          <span class="sf-id">ID {{ l.listingId }}</span>
          <ElButton size="small" @click="router.push(`/settlement?listingId=${l.listingId}`)">购买结算</ElButton>
        </div>
      </div>
    </div>
    <p v-else class="sf-empty">暂无已上架商品——伙伴在「发布商品」提交、平台在「上架审核」通过后,商品才会出现在这里。</p>
  </div>
</template>

<style scoped>
.storefront { padding: var(--sc-spacing-6); max-width: 1200px; }
.sf-tabs { display: flex; gap: var(--sc-spacing-2); margin-bottom: var(--sc-spacing-5); }
.sf-tab {
  padding: 8px 16px; border: 1px solid var(--sc-border); border-radius: var(--sc-radius-md);
  background: var(--sc-glass-bg-soft); color: var(--sc-text-secondary); font-size: 13px; cursor: pointer;
  transition: all var(--sc-transition);
}
.sf-tab:hover { color: var(--sc-color-brand); border-color: var(--sc-color-brand); }
.sf-tab.active { background: var(--sc-color-brand); border-color: var(--sc-color-brand); color: var(--sc-color-on-brand); }
.sf-cards { display: grid; grid-template-columns: repeat(auto-fill, minmax(320px, 1fr)); gap: var(--sc-spacing-4); }
.sf-card {
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  box-shadow: var(--sc-shadow-sm);
  padding: var(--sc-spacing-5);
  display: flex; flex-direction: column; gap: var(--sc-spacing-2);
  transition: box-shadow var(--sc-transition), transform var(--sc-transition);
}
.sf-card:hover { box-shadow: var(--sc-shadow-md); transform: translateY(-2px); }
.sf-card-head { display: flex; justify-content: space-between; align-items: center; }
.sf-cat { font-size: 12px; padding: 2px 8px; border-radius: var(--sc-radius-sm); color: var(--sc-color-brand); background: var(--sc-color-brand-soft); }
.sf-rate { font-size: 12px; color: var(--sc-text-secondary); }
.sf-name { margin: 0; font-size: 16px; font-weight: 500; color: var(--sc-text-primary); }
.sf-url { margin: 0; font-size: 12px; color: var(--sc-text-secondary); word-break: break-all; }
.sf-card-foot { display: flex; justify-content: space-between; align-items: center; margin-top: var(--sc-spacing-2); }
.sf-id { font-size: 12px; color: var(--sc-text-secondary); }
.sf-error, .sf-loading, .sf-empty { color: var(--sc-text-secondary); padding: var(--sc-spacing-6); }
.sf-error { color: var(--sc-color-danger); }
</style>
