<script setup lang="ts">
/**
 * web-billing shell (02§1.2 dual-form). Standalone: shared glass SiteTopbar
 * with the billing nav; Wujie sandbox: shell-less (console owns the top bar).
 */
import { RouterView } from "vue-router";
import { inWujieSandbox } from "@eu/wujie-bridge";
import { SiteTopbar } from "@eu/ui";

const sandboxed = inWujieSandbox();
</script>

<template>
  <div :class="{ 'billing-shell': !sandboxed, 'billing-subapp': sandboxed }">
    <SiteTopbar v-if="!sandboxed" title="费用中心">
      <template #nav>
        <router-link to="/bills">账单</router-link>
        <router-link to="/orders">订单</router-link>
        <router-link to="/resource-packs">资源包</router-link>
        <router-link to="/invoices">发票</router-link>
        <router-link to="/cost-analysis">成本分析</router-link>
        <router-link to="/renew">续费管理</router-link>
      </template>
      <template #actions>
        <a class="billing-console" href="https://console.euler.emoera.com">返回控制台</a>
      </template>
    </SiteTopbar>
    <main class="billing-body"><RouterView /></main>
  </div>
</template>

<style>
.billing-shell { min-height: 100vh; }
.billing-console { color: var(--eu-color-brand); text-decoration: none; font-size: var(--eu-font-size-md); transition: color var(--eu-transition); }
.billing-console:hover { color: var(--eu-color-brand-hover); }
.billing-body { min-height: calc(100vh - var(--eu-topbar-height)); }
.billing-subapp { min-height: 100vh; }
</style>
