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
  <header class="eu-page-header">
    <div class="eu-page-header-heading">
      <h1 class="eu-page-header-title">{{ title }}</h1>
      <p v-if="subtitle" class="eu-page-header-subtitle">{{ subtitle }}</p>
    </div>
    <div v-if="$slots.actions" class="eu-page-header-actions">
      <slot name="actions" />
    </div>
  </header>
</template>

<style>
.eu-page-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--eu-spacing-4);
  margin-bottom: var(--eu-spacing-5);
}
.eu-page-header-heading {
  display: flex;
  flex-direction: column;
  gap: var(--eu-spacing-1);
  min-width: 0;
}
.eu-page-header-title {
  margin: 0;
  font-size: 20px;
  font-weight: 500;
  color: var(--eu-text-primary);
}
.eu-page-header-subtitle {
  margin: 0;
  font-size: 13px;
  line-height: 1.5;
  color: var(--eu-text-secondary);
}
.eu-page-header-actions {
  display: flex;
  align-items: center;
  gap: var(--eu-spacing-3);
  flex-shrink: 0;
}
</style>
