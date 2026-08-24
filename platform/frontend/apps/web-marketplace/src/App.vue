<script setup lang="ts">
/**
 * web-marketplace shell (M-10 生态门户). Standalone: shared glass SiteTopbar;
 * Wujie sandbox: shell-less (the console provides the top bar).
 */
import { RouterView } from "vue-router";
import { inWujieSandbox } from "@sc/wujie-bridge";
import { SiteTopbar } from "@sc/ui";

const sandboxed = inWujieSandbox();
</script>

<template>
  <div :class="{ 'mkt-shell': !sandboxed, 'mkt-subapp': sandboxed }">
    <SiteTopbar v-if="!sandboxed" title="辰云市场">
      <template #nav>
        <router-link to="/storefront">商品目录</router-link>
        <router-link to="/publish">发布商品</router-link>
        <router-link to="/review">上架审核</router-link>
        <router-link to="/settlement">订单结算</router-link>
      </template>
      <template #actions>
        <a class="mkt-console" href="https://console.starcloud.cn">返回控制台</a>
      </template>
    </SiteTopbar>
    <main class="mkt-body"><RouterView /></main>
  </div>
</template>

<style>
.mkt-shell { min-height: 100vh; }
.mkt-console { color: var(--sc-color-brand); text-decoration: none; font-size: var(--sc-font-size-md); transition: color var(--sc-transition); }
.mkt-console:hover { color: var(--sc-color-brand-hover); }
.mkt-body { min-height: calc(100vh - var(--sc-topbar-height)); }
.mkt-subapp { min-height: 100vh; }
</style>
