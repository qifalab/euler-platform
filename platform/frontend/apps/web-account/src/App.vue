<script setup lang="ts">
/**
 * web-account shell. Standalone: renders its own glass top bar (account.*
 * subdomain) via the shared SiteTopbar. Wujie sandbox: renders shell-less
 * (the console provides the top bar).
 */
import { RouterView } from "vue-router";
import { inWujieSandbox } from "@sc/wujie-bridge";
import { SiteTopbar } from "@sc/ui";
import { useAccountAuth } from "./stores/auth";

const auth = useAccountAuth();
const sandboxed = inWujieSandbox();
</script>

<template>
  <div :class="{ 'account-shell': !sandboxed, 'account-subapp': sandboxed }">
    <SiteTopbar v-if="!sandboxed" title="账号中心">
      <template #nav>
        <router-link v-if="auth.isAuthenticated" to="/profile">账号信息</router-link>
        <router-link v-if="auth.isAuthenticated" to="/ram/users">RAM 用户</router-link>
        <router-link v-if="auth.isAuthenticated" to="/ak">AccessKey</router-link>
      </template>
      <template #actions>
        <a class="account-console" href="https://console.starcloud.cn">返回控制台</a>
      </template>
    </SiteTopbar>
    <main class="account-body">
      <RouterView />
    </main>
  </div>
</template>

<style>
.account-shell { min-height: 100vh; }
.account-console { color: var(--sc-color-brand); text-decoration: none; font-size: var(--sc-font-size-md); transition: color var(--sc-transition); }
.account-console:hover { color: var(--sc-color-brand-hover); }
.account-body { min-height: calc(100vh - var(--sc-topbar-height)); }
.account-subapp { min-height: 100vh; }
</style>
