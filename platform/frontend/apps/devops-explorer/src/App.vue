<script setup lang="ts">
/** devops-explorer shell — RouterView host (02§3.4). */
import { RouterView } from "vue-router";
import { inWujieSandbox } from "@eu/wujie-bridge";
import { SiteTopbar } from "@eu/ui";

const sandboxed = inWujieSandbox();
</script>

<template>
  <div :class="{ 'explorer-shell': !sandboxed, 'explorer-subapp': sandboxed }">
    <SiteTopbar v-if="!sandboxed" title="OpenAPI Explorer">
      <template #nav>
        <router-link to="/explorer">在线调试</router-link>
        <router-link to="/actions">API 目录</router-link>
        <router-link to="/community">开发者社区</router-link>
      </template>
      <template #actions>
        <a class="explorer-console" href="https://console.euler.emoera.com">返回控制台</a>
      </template>
    </SiteTopbar>
    <main class="explorer-body"><RouterView /></main>
  </div>
</template>

<style>
.explorer-shell { min-height: 100vh; }
.explorer-console { color: var(--eu-color-brand); text-decoration: none; font-size: var(--eu-font-size-md); transition: color var(--eu-transition); }
.explorer-console:hover { color: var(--eu-color-brand-hover); }
.explorer-body { min-height: calc(100vh - var(--eu-topbar-height)); }
.explorer-subapp { min-height: 100vh; }
</style>
