<script setup lang="ts">
/**
 * EmptyGuide — empty-state component (02§7.5).
 * Every empty state must give a next step: no resources → create entry + quick-start doc link;
 * search no results → suggest a new query.
 */
defineProps<{
  title?: string;
  description?: string;
  actionLabel?: string;
  actionHref?: string;
}>();
</script>

<template>
  <div class="sc-empty-guide">
    <div class="sc-empty-icon" aria-hidden="true">
      <svg viewBox="0 0 64 64" width="56" height="56">
        <rect x="8" y="14" width="48" height="36" rx="4" fill="none" stroke="currentColor" stroke-width="2" opacity="0.35" />
        <path d="M8 24h48" stroke="currentColor" stroke-width="2" opacity="0.35" />
        <circle cx="18" cy="19" r="1.5" fill="currentColor" opacity="0.35" />
        <circle cx="24" cy="19" r="1.5" fill="currentColor" opacity="0.35" />
      </svg>
    </div>
    <p class="sc-empty-title">{{ title ?? "暂无资源" }}</p>
    <p class="sc-empty-desc">{{ description ?? "创建您的第一个资源开始使用。" }}</p>
    <a v-if="actionLabel && actionHref" class="sc-empty-action" :href="actionHref">
      {{ actionLabel }}
    </a>
  </div>
</template>

<style>
.sc-empty-guide {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  padding: 48px 16px;
  color: var(--sc-text-secondary);
}
.sc-empty-icon { color: var(--sc-text-disabled); }
.sc-empty-title { margin: 8px 0 0; font-size: var(--sc-font-size-md); color: var(--sc-text-primary); }
.sc-empty-desc { margin: 0; font-size: var(--sc-font-size-sm); }
.sc-empty-action {
  margin-top: 12px;
  display: inline-block;
  padding: 8px 20px;
  border-radius: var(--sc-radius-md);
  background: var(--sc-color-brand);
  color: var(--sc-text-on-brand);
  text-decoration: none;
  font-size: var(--sc-font-size-sm);
}
.sc-empty-action:hover { background: var(--sc-color-brand-hover); }
</style>
