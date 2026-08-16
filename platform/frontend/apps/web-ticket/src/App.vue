<script setup lang="ts">
/**
 * web-ticket shell (02§1.2 dual-form). Standalone: shared glass SiteTopbar;
 * Wujie sandbox: shell-less (the console provides the top bar).
 */
import { RouterView } from "vue-router";
import { inWujieSandbox } from "@sc/wujie-bridge";
import { SiteTopbar } from "@sc/ui";

const sandboxed = inWujieSandbox();
</script>

<template>
  <div :class="{ 'ticket-shell': !sandboxed, 'ticket-subapp': sandboxed }">
    <SiteTopbar v-if="!sandboxed" title="工单支持">
      <template #nav>
        <router-link to="/list">我的工单</router-link>
        <router-link to="/create">提交工单</router-link>
      </template>
      <template #actions>
        <a class="ticket-console" href="https://console.starcloud.cn">返回控制台</a>
      </template>
    </SiteTopbar>
    <main class="ticket-body"><RouterView /></main>
  </div>
</template>

<style>
.ticket-shell { min-height: 100vh; }
.ticket-console { color: var(--sc-color-brand); text-decoration: none; font-size: var(--sc-font-size-md); transition: color var(--sc-transition); }
.ticket-console:hover { color: var(--sc-color-brand-hover); }
.ticket-body { min-height: calc(100vh - var(--sc-topbar-height)); }
.ticket-subapp { min-height: 100vh; }
</style>
