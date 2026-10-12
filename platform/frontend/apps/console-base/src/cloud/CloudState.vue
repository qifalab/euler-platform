<script setup lang="ts">
import CloudIcon from "./CloudIcon.vue";
defineProps<{
  kind: "loading" | "error" | "empty";
  title: string;
  description?: string;
}>();
defineEmits<{ retry: [] }>();
</script>
<template>
  <div
    class="cloud-state"
    :class="kind"
    :role="kind === 'error' ? 'alert' : 'status'"
    :aria-busy="kind === 'loading'"
  >
    <span v-if="kind === 'loading'" class="spinner" /><span
      v-else
      class="state-icon"
      ><CloudIcon :name="kind === 'error' ? 'alert' : 'layers'" :size="26"
    /></span>
    <h3>{{ title }}</h3>
    <p v-if="description">{{ description }}</p>
    <button v-if="kind === 'error'" class="button" @click="$emit('retry')">
      <CloudIcon name="refresh" :size="16" />重试</button
    ><slot />
  </div>
</template>
