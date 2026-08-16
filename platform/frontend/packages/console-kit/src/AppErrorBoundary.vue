<script setup lang="ts">
/**
 * AppErrorBoundary — page-level error boundary (02§6.6).
 * Wraps a router view; on a sub-app page error shows the error page with
 * traceId and a feedback entry instead of a white screen.
 */
import { computed, onErrorCaptured, ref } from "vue";

const err = ref<unknown>(null);

const message = computed(() => (err.value instanceof Error ? err.value.message : "未知错误"));
const requestId = computed(() => {
  const e = err.value as { requestId?: string } | null;
  return e?.requestId ?? "—";
});

onErrorCaptured((e) => {
  err.value = e;
  return false; // stop propagation — the shell stays up
});
</script>

<template>
  <div v-if="err" class="sc-error-boundary">
    <h2>页面出错</h2>
    <p class="sc-eb-message">{{ message }}</p>
    <p class="sc-eb-hint">可复制下方 requestId 提交工单,工程师将据此排查。</p>
    <code class="sc-eb-trace">{{ requestId }}</code>
    <button class="sc-eb-action" @click="err = null">重试</button>
  </div>
  <slot v-else />
</template>

<style>
/* Soft-glass error card floating on the mesh background (tokens only). */
.sc-error-boundary {
  max-width: 480px;
  margin: 64px auto;
  padding: 48px 24px;
  text-align: center;
  color: var(--sc-text-primary);
  background: var(--sc-glass-bg-soft);
  -webkit-backdrop-filter: var(--sc-glass-blur-soft);
  backdrop-filter: var(--sc-glass-blur-soft);
  border: 1px solid var(--sc-glass-border);
  border-radius: var(--sc-radius-lg);
  box-shadow: var(--sc-shadow-sm);
}
.sc-eb-message { color: var(--sc-color-danger); }
.sc-eb-hint { color: var(--sc-text-secondary); font-size: var(--sc-font-size-sm); }
.sc-eb-trace { display: inline-block; margin: 12px 0; padding: 4px 12px; background: var(--sc-bg-page); border-radius: var(--sc-radius-sm); }
.sc-eb-action {
  margin-top: 12px; padding: 8px 20px; border: none; border-radius: var(--sc-radius-md);
  background: var(--sc-color-brand); color: var(--sc-text-on-brand); cursor: pointer;
  font-size: var(--sc-font-size-sm);
  transition: var(--sc-transition);
}
.sc-eb-action:hover { background: var(--sc-color-brand-hover); }
</style>
