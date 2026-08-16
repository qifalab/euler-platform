<script setup lang="ts">
import { useRouter } from "vue-router";
const router = useRouter();
</script>

<template>
  <div class="not-found">
    <h1>404</h1>
    <p>页面不存在或您没有访问权限。</p>
    <a href="/" @click.prevent="router.push('/')">返回总览</a>
  </div>
</template>

<style scoped>
.not-found { padding: 64px 24px; text-align: center; }
.not-found h1 { font-size: 48px; margin: 0 0 8px; color: var(--sc-text-disabled); }
.not-found p { color: var(--sc-text-secondary); }
.not-found a { color: var(--sc-color-brand); }
</style>
