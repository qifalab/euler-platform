<script setup lang="ts">
/**
 * StatusBadge — globally unified resource status badge (02§7.2).
 * Running=green, Stopped=grey, Expired/Locked=orange, Error=red,
 * Creating/Pending=blue. Colors come from design tokens, never hard-coded.
 */
import { computed } from "vue";

const props = defineProps<{
  status: string;
  label?: string;
}>();

const STATUS_KIND: Record<string, "success" | "info" | "warning" | "danger" | "primary"> = {
  running: "success",
  active: "success",
  stopped: "info",
  inactive: "info",
  expired: "warning",
  locked: "warning",
  error: "danger",
  failed: "danger",
  creating: "primary",
  pending: "primary",
  starting: "primary",
};

const kind = computed(() => STATUS_KIND[props.status.toLowerCase()] ?? "info");
const KIND_VAR: Record<string, string> = {
  success: "var(--eu-color-success)",
  info: "var(--eu-text-secondary)",
  warning: "var(--eu-color-warning)",
  danger: "var(--eu-color-danger)",
  primary: "var(--eu-color-brand)",
};
</script>

<template>
  <span
    class="eu-status-badge"
    :style="{ '--eu-badge-color': KIND_VAR[kind] }"
    :data-kind="kind"
  >
    <i class="eu-status-dot" />
    {{ label ?? status }}
  </span>
</template>

<style>
.eu-status-badge {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: var(--eu-font-size-sm);
  color: var(--eu-badge-color, var(--eu-text-secondary));
}
.eu-status-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--eu-badge-color, var(--eu-text-secondary));
}
</style>
