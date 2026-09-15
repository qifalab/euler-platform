<script setup lang="ts">
/**
 * GlobalSearch — ⌘K command palette (02§7.1).
 * Three grouped legs, all live: sub-apps (local registry match, instant),
 * 我的资源 + 产品 (GET /console/search via console-bff, debounced fan-out to
 * svc-orchestrator + svc-catalog). The backend search is account-scoped and
 * degrades per-leg; a down leg simply renders no hits, never an error page.
 */
import { ref, computed, onMounted, onUnmounted, watch } from "vue";
import { useRouter } from "vue-router";
import { useRegistry } from "./registry";
import { useSDK } from "./sdk";

const router = useRouter();
const registry = useRegistry();
const sdk = useSDK();
const props = defineProps<{ open: boolean }>();
const emit = defineEmits<{ "update:open": [v: boolean] }>();
const query = ref("");
const inputRef = ref<HTMLInputElement>();

interface SearchResource {
  resourceId: string;
  productCode: string;
  region: string;
  state: string;
  specCode: string;
}
interface SearchProduct {
  productCode: string;
  productName: string;
  category: string;
  description: string;
}
interface SearchData {
  query: string;
  resources: SearchResource[];
  products: SearchProduct[];
}

const backend = ref<SearchData | null>(null);
const searching = ref(false);
const searchFailed = ref(false);
let debounceTimer: ReturnType<typeof setTimeout> | null = null;
let searchSeq = 0;

const appHits = computed(() => {
  const q = query.value.trim().toLowerCase();
  if (!q) return registry.apps;
  return registry.apps.filter(
    (a) => a.appTitle.toLowerCase().includes(q) || a.appCode.toLowerCase().includes(q) || a.productCodes.some((p) => p.includes(q)),
  );
});

async function runSearch(q: string) {
  const seq = ++searchSeq;
  searching.value = true;
  searchFailed.value = false;
  try {
    const res = await sdk.get<SearchData>(`/console/search?q=${encodeURIComponent(q)}`);
    if (seq !== searchSeq) return;
    backend.value = res.data;
  } catch {
    if (seq !== searchSeq) return;
    backend.value = null;
    searchFailed.value = true;
  } finally {
    if (seq === searchSeq) searching.value = false;
  }
}

watch(query, (q) => {
  if (debounceTimer) clearTimeout(debounceTimer);
  const trimmed = q.trim();
  if (!trimmed) {
    // Closing the palette clears the query. Firing a request for "" would fan
    // out to the backends for nothing and leave a stale result set to flash on
    // the next open; clear the state instead.
    searchSeq++; // invalidate any in-flight search
    backend.value = null;
    searchFailed.value = false;
    searching.value = false;
    return;
  }
  debounceTimer = setTimeout(() => void runSearch(trimmed), 250);
});

function close() {
  emit("update:open", false);
  query.value = "";
  backend.value = null;
}

function onKey(e: KeyboardEvent) {
  // ⌘K toggles globally only when closed; Escape closes.
  if (e.key === "Escape" && props.open) { e.preventDefault(); close(); return; }
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
    e.preventDefault();
    emit("update:open", !props.open);
    return;
  }
  if (props.open) setTimeout(() => inputRef.value?.focus(), 50);
}
onMounted(() => window.addEventListener("keydown", onKey));
onUnmounted(() => {
  window.removeEventListener("keydown", onKey);
  if (debounceTimer) clearTimeout(debounceTimer);
});

function go(rule: string) { close(); void router.push(rule); }

/** Navigate to the console sub-app owning a productCode (registry match). */
function goProduct(productCode: string) {
  const app = registry.apps.find((a) => a.productCodes.includes(productCode));
  if (app) go(app.activeRules[0]);
}
function goResource(r: SearchResource) { goProduct(r.productCode); }

const stateLabels: Record<string, string> = {
  RUNNING: "运行中", STOPPED: "已停止", CREATING: "创建中", RELEASING: "释放中",
  RELEASED: "已释放", UPGRADING: "变配中",
};
</script>

<template>
  <div v-if="open" class="gs-overlay" @click.self="close">
    <div class="gs-panel sc-glass-strong">
      <input ref="inputRef" v-model="query" class="gs-input" placeholder="搜索产品、资源、文档…" />
      <ul class="gs-list">
        <template v-if="appHits.length">
          <li class="gs-group">应用</li>
          <li v-for="r in appHits" :key="r.appCode" class="gs-item" @click="go(r.activeRules[0])">
            <span class="gs-title">{{ r.appTitle }}</span>
            <span class="gs-code">{{ r.appCode }}</span>
          </li>
        </template>
        <template v-if="backend?.resources?.length">
          <li class="gs-group">我的资源</li>
          <li v-for="r in backend.resources" :key="r.resourceId" class="gs-item" @click="goResource(r)">
            <span class="gs-title">{{ r.resourceId }}</span>
            <span class="gs-code">{{ r.productCode }} · {{ r.region }} · {{ stateLabels[r.state] ?? r.state }}</span>
          </li>
        </template>
        <template v-if="backend?.products?.length">
          <li class="gs-group">产品</li>
          <li v-for="p in backend.products" :key="p.productCode" class="gs-item" @click="goProduct(p.productCode)">
            <span class="gs-title">{{ p.productName }}</span>
            <span class="gs-code">{{ p.productCode }} · {{ p.category }}</span>
          </li>
        </template>
        <li v-if="searching" class="gs-empty">搜索中…</li>
        <li v-else-if="searchFailed" class="gs-empty">资源/产品搜索暂不可用（console-bff 未启动）</li>
        <li
          v-else-if="appHits.length === 0 && !backend?.resources?.length && !backend?.products?.length"
          class="gs-empty"
        >无匹配结果</li>
      </ul>
    </div>
  </div>
</template>

<style scoped>
/* Glass palette: overlay blur + strong-glass panel (tokens .sc-glass-strong). */
.gs-overlay {
  position: fixed; inset: 0;
  background: var(--sc-overlay);
  -webkit-backdrop-filter: blur(4px);
  backdrop-filter: blur(4px);
  z-index: 3000;
  display: flex; justify-content: center; align-items: flex-start;
  padding-top: 120px;
}
.gs-panel { width: 100%; max-width: 560px; border-radius: var(--sc-radius-xl); overflow: hidden; }
.gs-input { width: 100%; border: none; border-bottom: 1px solid var(--sc-border); padding: 16px 20px; font-size: 15px; outline: none; background: transparent; color: var(--sc-text-primary); }
.gs-list { list-style: none; margin: 0; padding: 8px 0; max-height: 320px; overflow-y: auto; }
.gs-group {
  padding: 8px 20px 4px;
  font-size: 11px; font-weight: 600; letter-spacing: 0.05em;
  color: var(--sc-text-secondary); text-transform: uppercase;
}
.gs-item {
  display: flex; justify-content: space-between; align-items: center;
  padding: 10px 20px; margin: 0 var(--sc-spacing-1);
  cursor: pointer; border-radius: var(--sc-radius-sm);
  transition: var(--sc-transition);
}
.gs-item:hover { background: var(--sc-color-brand-soft); }
.gs-title { font-size: 14px; color: var(--sc-text-primary); }
.gs-code { font-size: 12px; color: var(--sc-text-secondary); }
.gs-empty { padding: 16px 20px; color: var(--sc-text-secondary); font-size: 13px; }
</style>
