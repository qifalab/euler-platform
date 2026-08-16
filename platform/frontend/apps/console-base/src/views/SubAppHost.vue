<script setup lang="ts">
/**
 * SubAppHost — renders the Wujie container for the active sub-app.
 * Resolves the route to a registry entry, then mounts its Wujie sandbox.
 * On failure shows the degradation panel (02§6.6): version + retry + open-standalone.
 */
import { ref, watch, onMounted, onUnmounted } from "vue";
import { startApp, destroyApp } from "wujie";
import { useRoute } from "vue-router";
import { useRegistry, type RegistryApp } from "../registry";

const route = useRoute();
const registry = useRegistry();
const containerRef = ref<HTMLDivElement>();
const error = ref<string | null>(null);
const current = ref<RegistryApp | undefined>();

async function mount(force = false) {
  const app = registry.resolve(route.path);
  if (!app) {
    error.value = `未找到 ${route.path} 对应的子应用`;
    current.value = undefined;
    return;
  }
  // Same sub-app → internal (level-2+) routing is handled by Wujie URL sync;
  // restarting here would tear down the whole sandbox on every route change.
  if (!force && current.value?.appCode === app.appCode && !error.value) return;
  error.value = null;
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

function retry() {
  void mount(true);
}

onMounted(() => mount());
watch(() => route.path, () => mount());

onUnmounted(() => {
  // Tear down the sandbox for non-keep-alive apps; alive apps keep their
  // instance and simply re-attach on next visit.
  if (current.value && !current.value.keepAlive) {
    destroyApp(current.value.appCode);
  }
  current.value = undefined;
});
</script>

<template>
  <div class="sub-app-host">
    <div v-if="error" class="sub-app-degrade">
      <p class="sub-app-degrade-title">子应用「{{ current?.appTitle ?? "" }}」加载失败</p>
      <p class="sub-app-degrade-msg">{{ error }}</p>
      <button class="sub-app-degrade-retry" type="button" @click="retry">重试加载</button>
      <a class="sub-app-degrade-link" :href="current?.entryUrl" target="_blank">在新窗口独立打开 →</a>
    </div>
    <!-- v-show (not v-else/v-if): the container DOM must survive an error so
         retry can re-mount into the same element. -->
    <div
      v-show="!error"
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
.sub-app-degrade-retry {
  display: inline-block; margin: 0 12px 12px 0; padding: 6px 16px;
  border: none; border-radius: var(--sc-radius-md); cursor: pointer;
  background: var(--sc-color-brand); color: var(--sc-text-on-brand); font-size: 13px;
  transition: background var(--sc-transition);
}
.sub-app-degrade-retry:hover { background: var(--sc-color-brand-hover); }
.sub-app-degrade-link { color: var(--sc-color-brand); text-decoration: none; transition: var(--sc-transition); }
.sub-app-degrade-link:hover { color: var(--sc-color-brand-hover); }
</style>
