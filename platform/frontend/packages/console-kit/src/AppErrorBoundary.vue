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
  <div v-if="err" class="eu-error-boundary">
    <h2>页面出错</h2>
    <p class="eu-eb-message">{{ message }}</p>
    <p class="eu-eb-hint">可复制下方 requestId 提交工单,工程师将据此排查。</p>
    <code class="eu-eb-trace">{{ requestId }}</code>
    <button class="eu-eb-action" @click="err = null">重试</button>
  </div>
  <slot v-else />
</template>

<style>
/* Soft-glass error card floating on the mesh background (tokens only). */
.eu-error-boundary {
  max-width: 480px;
  margin: 64px auto;
  padding: 48px 24px;
  text-align: center;
  color: var(--eu-text-primary);
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
}
.eu-eb-message { color: var(--eu-color-danger); }
.eu-eb-hint { color: var(--eu-text-secondary); font-size: var(--eu-font-size-sm); }
.eu-eb-trace { display: inline-block; margin: 12px 0; padding: 4px 12px; background: var(--eu-bg-page); border-radius: var(--eu-radius-sm); }
.eu-eb-action {
  margin-top: 12px; padding: 8px 20px; border: none; border-radius: var(--eu-radius-md);
  background: var(--eu-color-brand); color: var(--eu-text-on-brand); cursor: pointer;
  font-size: var(--eu-font-size-sm);
  transition: var(--eu-transition);
}
.eu-eb-action:hover { background: var(--eu-color-brand-hover); }
</style>
