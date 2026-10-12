<script setup lang="ts">
import type { History } from "./types";
import { actions, date, statuses } from "./types";
defineProps<{ items?: History[] }>();
</script>
<template>
  <details v-if="items?.length" class="eid-history">
    <summary>查看处理记录（{{ items.length }}）</summary>
    <ol>
      <li v-for="item in items" :key="item.id">
        <strong>{{ actions[item.action] ?? item.action }}</strong
        ><span
          >{{ date(item.createdAt) }} ·
          {{ statuses[item.toStatus] ?? item.toStatus }}</span
        >
      </li>
    </ol>
  </details>
</template>
<style scoped>
.eid-history {
  font-size: 12px;
  margin-top: 12px;
  color: var(--muted);
}
summary {
  cursor: pointer;
}
ol {
  padding-left: 20px;
  display: grid;
  gap: 10px;
}
li strong,
li span {
  display: block;
}
li strong {
  color: var(--text);
  font-weight: 500;
}
</style>
