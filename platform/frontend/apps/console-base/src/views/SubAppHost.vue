<script setup lang="ts">
/**
 * SubAppHost — renders the Wujie container for the active sub-app.
 * Resolves the route to a registry entry, then mounts its Wujie sandbox.
 * On failure shows the degradation panel (02§6.6): version + retry + open-standalone.
 */
import { ref, watch, onMounted } from "vue";
import { startApp } from "wujie";
import { useRoute } from "vue-router";
import { useRegistry, type RegistryApp } from "../registry";

const route = useRoute();
const registry = useRegistry();
const containerRef = ref<HTMLDivElement>();
const error = ref<string | null>(null);
const current = ref<RegistryApp | undefined>();

async function mount() {
  error.value = null;
  const app = registry.resolve(route.path);
  if (!app) {
    error.value = `未找到 ${route.path} 对应的子应用`;
    return;
  }
  current.value = app;
  const el = containerRef.value;
  if (!el) return;
  try {
    await startApp({
      name: app.appCode,
      url: app.entryUrl,
      el,
      alive: app.keepAlive,
      sync: true, // URL two-way sync (02§2.2)
    });
  } catch (e) {
    error.value = (e as Error).message ?? "子应用加载失败";
  }
}

onMounted(mount);
watch(() => route.path, mount);
</script>

<template>
  <div class="sub-app-host">
    <div v-if="error" class="sub-app-degrade">
      <p class="sub-app-degrade-title">子应用「{{ current?.appTitle ?? "" }}」加载失败</p>
      <p class="sub-app-degrade-msg">{{ error }}</p>
      <a class="sub-app-degrade-link" :href="current?.entryUrl" target="_blank">在新窗口独立打开 →</a>
    </div>
    <div
      v-else
      ref="containerRef"
      class="sub-app-container"
      :data-app="current?.appCode"
    />
  </div>
</template>

<style scoped>
/*
 * Transparent host so the tokens body mesh shows through; the degrade panel
 * is a soft-glass card. These rules are the single source of truth for the
 * .sub-app-* classes (the duplicate global copy in App.vue was removed).
 */
.sub-app-host { min-height: calc(100vh - var(--sc-topbar-height)); background: transparent; }
.sub-app-container { min-height: calc(100vh - var(--sc-topbar-height)); background: transparent; }
.sub-app-degrade {
  max-width: 480px; margin: 64px auto; padding: 48px 24px;
  text-align: center; color: var(--sc-text-primary);
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  box-shadow: var(--sc-shadow-sm);
}
.sub-app-degrade-title { font-size: 16px; margin: 0 0 8px; }
.sub-app-degrade-msg { color: var(--sc-color-danger); font-size: 14px; margin: 0 0 16px; }
.sub-app-degrade-link { color: var(--sc-color-brand); text-decoration: none; transition: var(--sc-transition); }
.sub-app-degrade-link:hover { color: var(--sc-color-brand-hover); }
</style>
