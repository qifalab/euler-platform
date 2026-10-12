<script setup lang="ts">
/**
 * web-marketplace shell (M-10 生态门户). Standalone: shared glass SiteTopbar;
 * Wujie sandbox: shell-less (the console provides the top bar).
 */
import { ref } from "vue";
import { RouterView } from "vue-router";
import { inWujieSandbox } from "@eu/wujie-bridge";
import { SiteTopbar } from "@eu/ui";
import { isOperator } from "./marketplace";

const sandboxed = inWujieSandbox();
// 上架审核 / 订单结算 are operator-only; partners and tenants never see them.
const operator = ref(false);
void isOperator().then((v) => { operator.value = v; });
</script>

<template>
  <div :class="{ 'mkt-shell': !sandboxed, 'mkt-subapp': sandboxed }">
    <SiteTopbar v-if="!sandboxed" title="辰云市场">
      <template #nav>
        <router-link to="/storefront">商品目录</router-link>
        <router-link to="/publish">发布商品</router-link>
        <router-link v-if="operator" to="/review">上架审核</router-link>
        <router-link v-if="operator" to="/settlement">订单结算</router-link>
      </template>
      <template #actions>
        <a class="mkt-console" href="https://console.euler.emoera.com">返回控制台</a>
      </template>
    </SiteTopbar>
    <main class="mkt-body"><RouterView /></main>
  </div>
</template>

<style>
.mkt-shell { min-height: 100vh; }
.mkt-console { color: var(--eu-color-brand); text-decoration: none; font-size: var(--eu-font-size-md); transition: color var(--eu-transition); }
.mkt-console:hover { color: var(--eu-color-brand-hover); }
.mkt-body { min-height: calc(100vh - var(--eu-topbar-height)); }
.mkt-subapp { min-height: 100vh; }
</style>
