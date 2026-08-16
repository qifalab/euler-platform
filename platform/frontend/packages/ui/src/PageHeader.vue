<script setup lang="ts">
/**
 * PageHeader — unified list-page heading (Google Cloud Glass theme).
 * Title (20px/500) + optional subtitle (13px secondary) on the left,
 * action buttons via the `actions` slot on the right. No background —
 * the page mesh (body) stays visible underneath.
 */
defineProps<{
  /** Page title, required. */
  title: string;
  /** Optional one-line description under the title. */
  subtitle?: string;
}>();
</script>

<template>
  <header class="sc-page-header">
    <div class="sc-page-header-heading">
      <h1 class="sc-page-header-title">{{ title }}</h1>
      <p v-if="subtitle" class="sc-page-header-subtitle">{{ subtitle }}</p>
    </div>
    <div v-if="$slots.actions" class="sc-page-header-actions">
      <slot name="actions" />
    </div>
  </header>
</template>

<style>
.sc-page-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--sc-spacing-4);
  margin-bottom: var(--sc-spacing-5);
}
.sc-page-header-heading {
  display: flex;
  flex-direction: column;
  gap: var(--sc-spacing-1);
  min-width: 0;
}
.sc-page-header-title {
  margin: 0;
  font-size: 20px;
  font-weight: 500;
  color: var(--sc-text-primary);
}
.sc-page-header-subtitle {
  margin: 0;
  font-size: 13px;
  line-height: 1.5;
  color: var(--sc-text-secondary);
}
.sc-page-header-actions {
  display: flex;
  align-items: center;
  gap: var(--sc-spacing-3);
  flex-shrink: 0;
}
</style>
