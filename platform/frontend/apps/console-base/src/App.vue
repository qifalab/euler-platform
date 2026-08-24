<script setup lang="ts">
/**
 * Console shell layout (02§7.1).
 * Top bar + product menu + region selector + breadcrumb container + content
 * area. The top bar and content containers are owned by the shell; sub-apps
 * register their menu/breadcrumb via the bridge but never touch this DOM.
 */
import { ref, computed, onMounted, onBeforeUnmount } from "vue";
import { RouterView } from "vue-router";
import { RegionSelector } from "@sc/console-kit";
import { useRegistry } from "./registry";
import { useRegionStore } from "./stores/region";
import { useRouter } from "vue-router";
import GlobalSearch from "./GlobalSearch.vue";
import Breadcrumb from "./Breadcrumb.vue";

const registry = useRegistry();
const router = useRouter();
const regionStore = useRegionStore();
// v-model proxy: reads from the global region store, writes via setRegion
// (which broadcasts region:changed on the Wujie bus).
const regionId = computed({
  get: () => regionStore.regionId,
  set: (id: string) => regionStore.setRegion(id),
});
const menuOpen = ref(false);
const dark = ref(false);
const searchOpen = ref(false);
const menuRef = ref<HTMLElement | null>(null);

// Product menu: click-to-toggle (Google Cloud style). Hover-open conflicted
// with the click toggle (hover opened, then click immediately closed it), so
// we close on outside click / Escape instead.
function onDocClick(e: MouseEvent) {
  if (menuOpen.value && menuRef.value && !menuRef.value.contains(e.target as Node)) {
    menuOpen.value = false;
  }
}
function onDocKey(e: KeyboardEvent) {
  if (e.key === "Escape") menuOpen.value = false;
}
onBeforeUnmount(() => {
  document.removeEventListener("click", onDocClick);
  document.removeEventListener("keydown", onDocKey);
});

// Dark mode: follow system preference on first load, then manual toggle.
// Sets data-theme on :root so the --sc-* token set switches (02§10.1).
onMounted(() => {
  const saved = localStorage.getItem("sc:theme");
  if (saved) dark.value = saved === "dark";
  else dark.value = window.matchMedia?.("(prefers-color-scheme: dark)").matches ?? false;
  applyTheme();
  document.addEventListener("click", onDocClick);
  document.addEventListener("keydown", onDocKey);
});
function applyTheme() {
  document.documentElement.dataset.theme = dark.value ? "dark" : "light";
}
function toggleDark() {
  dark.value = !dark.value;
  localStorage.setItem("sc:theme", dark.value ? "dark" : "light");
  applyTheme();
}
</script>

<template>
  <div class="shell">
    <header class="shell-topbar">
      <span class="shell-logo">辰云</span><span class="shell-logo-sub">控制台</span>

      <button class="shell-search-btn" title="全局搜索 (⌘K)" @click="searchOpen = true">
        <span class="shell-search-hint">搜索…</span>
        <kbd class="shell-kbd">⌘K</kbd>
      </button>

      <div class="shell-product-menu" ref="menuRef">
        <button class="shell-menu-btn" aria-haspopup="menu" :aria-expanded="menuOpen" @click="menuOpen = !menuOpen">产品 ▾</button>
        <div v-show="menuOpen" class="shell-menu-dropdown">
          <a
            v-for="app in registry.apps"
            :key="app.appCode"
            :href="app.activeRules[0]"
            class="shell-menu-item"
            @click.prevent="router.push(app.activeRules[0])"
          >
            {{ app.appTitle }}
          </a>
        </div>
      </div>

      <div class="shell-spacer" />

      <RegionSelector v-model="regionId" />

      <button class="shell-theme-btn" :title="dark ? '切换亮色' : '切换暗色'" @click="toggleDark">
        {{ dark ? "☀" : "☾" }}
      </button>

      <a class="shell-nav-link" href="#" @click.prevent="router.push('/billing')">费用</a>
      <a class="shell-nav-link" href="#" @click.prevent="router.push('/marketplace')">市场</a>
      <a class="shell-nav-link" href="#" @click.prevent="router.push('/explorer')">API 调试</a>
      <a class="shell-nav-link" href="#" @click.prevent="router.push('/ticket')">工单</a>
      <span class="shell-account">账号 ▾</span>
    </header>

    <main class="shell-content">
      <Breadcrumb />
      <RouterView />
    </main>

    <GlobalSearch v-model:open="searchOpen" />
  </div>
</template>

<style>
/*
 * Google Cloud Glass shell. All colors/blur/radius/shadow come from @sc/tokens
 * (--sc-*); the body mesh gradient is provided by tokens and must not be
 * repainted here. The .sub-app-* layout/degrade styles live in SubAppHost.vue
 * (scoped) — do not re-declare them in this global block.
 */
.shell { min-height: 100vh; }
.shell-topbar {
  position: sticky; top: 0;
  height: var(--sc-topbar-height);
  display: flex;
  align-items: center;
  padding: 0 var(--sc-spacing-4);
  gap: var(--sc-spacing-4);
  background: var(--sc-glass-bg);
  -webkit-backdrop-filter: var(--sc-glass-blur);
  backdrop-filter: var(--sc-glass-blur);
  border-bottom: 1px solid var(--sc-border);
  box-shadow: var(--sc-glass-shadow);
}
.shell-logo { font-weight: 600; color: var(--sc-color-brand); }
.shell-logo-sub { font-size: 14px; color: var(--sc-text-secondary); margin-left: var(--sc-spacing-1); margin-right: var(--sc-spacing-2); }
.shell-search-btn {
  display: flex; align-items: center; gap: var(--sc-spacing-2);
  border: 1px solid var(--sc-border);
  background: var(--sc-glass-bg);
  border-radius: 999px;
  padding: 0 var(--sc-spacing-3);
  cursor: pointer; height: 30px;
  transition: var(--sc-transition);
}
.shell-search-btn:hover { background: var(--sc-color-brand-soft); }
.shell-search-hint { font-size: 13px; color: var(--sc-text-secondary); }
.shell-kbd { font-size: 11px; color: var(--sc-text-secondary); background: var(--sc-bg-container); border: 1px solid var(--sc-border); border-radius: 3px; padding: 1px 5px; font-family: monospace; }
.shell-product-menu { position: relative; }
.shell-menu-btn {
  border: none; background: none; cursor: pointer; font-size: 14px;
  color: var(--sc-text-secondary); padding: 6px 8px;
  border-radius: var(--sc-radius-sm);
  transition: var(--sc-transition);
}
.shell-menu-btn:hover { color: var(--sc-color-brand); background: var(--sc-color-brand-soft); }
.shell-menu-dropdown {
  position: absolute; top: 100%; left: 0; min-width: 200px;
  background: var(--sc-glass-bg-strong);
  -webkit-backdrop-filter: var(--sc-glass-blur);
  backdrop-filter: var(--sc-glass-blur);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  box-shadow: var(--sc-glass-shadow);
  padding: var(--sc-spacing-1) 0; z-index: 2000;
}
.shell-menu-item {
  display: block; padding: var(--sc-spacing-2) var(--sc-spacing-4);
  margin: 0 var(--sc-spacing-1);
  text-decoration: none;
  color: var(--sc-text-primary); font-size: 14px;
  border-radius: var(--sc-radius-sm);
  transition: var(--sc-transition);
}
.shell-menu-item:hover { background: var(--sc-color-brand-soft); color: var(--sc-color-brand); }
.shell-spacer { flex: 1; }
.shell-nav-link { color: var(--sc-text-secondary); text-decoration: none; font-size: 14px; transition: var(--sc-transition); }
.shell-nav-link:hover { color: var(--sc-color-brand); }
.shell-theme-btn { border: 1px solid var(--sc-border); background: var(--sc-bg-container); color: var(--sc-text-secondary); border-radius: var(--sc-radius-sm); width: 28px; height: 28px; cursor: pointer; font-size: 14px; line-height: 1; transition: var(--sc-transition); }
.shell-theme-btn:hover { color: var(--sc-color-brand); border-color: var(--sc-color-brand); }
.shell-account { color: var(--sc-text-secondary); font-size: 14px; cursor: pointer; transition: var(--sc-transition); }
.shell-account:hover { color: var(--sc-color-brand); }
.shell-content { padding: 0; }
</style>
