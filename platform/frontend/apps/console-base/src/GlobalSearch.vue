<script setup lang="ts">
/**
 * GlobalSearch — ⌘K command palette (02§7.1).
 * Cross-product search: matches against the sub-app registry (title/code).
 * In production this hits GET /api/v1/search/global?q= aggregated across
 * resources/products/docs; phase-1 filters the local registry.
 */
import { ref, computed, onMounted, onUnmounted } from "vue";
import { useRouter } from "vue-router";
import { useRegistry } from "./registry";

const router = useRouter();
const registry = useRegistry();
const props = defineProps<{ open: boolean }>();
const emit = defineEmits<{ "update:open": [v: boolean] }>();
const query = ref("");
const inputRef = ref<HTMLInputElement>();

const results = computed(() => {
  const q = query.value.trim().toLowerCase();
  if (!q) return registry.apps;
  return registry.apps.filter(
    (a) => a.appTitle.toLowerCase().includes(q) || a.appCode.toLowerCase().includes(q) || a.productCodes.some((p) => p.includes(q)),
  );
});

function close() { emit("update:open", false); query.value = ""; }

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
onUnmounted(() => window.removeEventListener("keydown", onKey));

function go(rule: string) { close(); router.push(rule); }
</script>

<template>
  <div v-if="open" class="gs-overlay" @click.self="close">
    <div class="gs-panel sc-glass-strong">
      <input ref="inputRef" v-model="query" class="gs-input" placeholder="搜索产品、资源、文档…" />
      <ul class="gs-list">
        <li v-for="r in results" :key="r.appCode" class="gs-item" @click="go(r.activeRules[0])">
          <span class="gs-title">{{ r.appTitle }}</span>
          <span class="gs-code">{{ r.appCode }}</span>
        </li>
        <li v-if="results.length === 0" class="gs-empty">无匹配结果</li>
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
