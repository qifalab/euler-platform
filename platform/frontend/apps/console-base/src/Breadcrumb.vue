<script setup lang="ts">
/**
 * Breadcrumb (02§7.1). The shell renders the breadcrumb container; the path
 * derives from the current route (first segment → registry app title, then the
 * sub-app's own title via route meta). Sub-apps register deeper crumbs via the
 * bridge in the real architecture; phase-1 derives from the route.
 */
import { computed } from "vue";
import { useRoute } from "vue-router";
import { useRegistry } from "./registry";

const route = useRoute();
const registry = useRegistry();

const crumbs = computed(() => {
  const segs = route.path.split("/").filter(Boolean);
  if (segs.length === 0) return [{ label: "总览", path: "/" }];
  const app = registry.resolve(route.path);
  const head = app?.appTitle ?? segs[0];
  return [{ label: head, path: "/" + segs[0] }];
});
</script>

<template>
  <nav class="breadcrumb">
    <template v-for="(c, i) in crumbs" :key="i">
      <span v-if="i > 0" class="bc-sep">/</span>
      <span class="bc-item">{{ c.label }}</span>
    </template>
  </nav>
</template>

<style scoped>
/* Transparent: the breadcrumb floats on the body mesh gradient (tokens). */
.breadcrumb { display: flex; align-items: center; gap: 6px; padding: 10px 24px; font-size: 13px; color: var(--sc-text-secondary); background: transparent; }
.bc-sep { color: var(--sc-text-disabled); }
.bc-item { color: var(--sc-text-primary); }
</style>
