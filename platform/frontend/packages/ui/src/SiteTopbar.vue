<script setup lang="ts">
/**
 * SiteTopbar — shared acrylic-glass top bar for the three web sites
 * (web-account / web-billing / web-ticket, Google Cloud Glass theme).
 * Sticky, translucent, backdrop-blurred; brand logo 「辰云」 + vertical
 * divider + site title on the left, `nav` slot for left-side navigation
 * links and `actions` slot for right-side actions. Nav links pick up the
 * brand color when their route is active (router-link-active).
 */
defineProps<{
  /** Site name rendered after the brand divider, e.g. 账号中心. */
  title: string;
}>();
</script>

<template>
  <header class="eu-site-topbar">
    <div class="eu-site-topbar-brand">
      <a class="eu-site-topbar-logo" href="/">辰云</a>
      <span class="eu-site-topbar-divider" aria-hidden="true" />
      <span class="eu-site-topbar-title">{{ title }}</span>
    </div>
    <nav class="eu-site-topbar-nav">
      <slot name="nav" />
    </nav>
    <div class="eu-site-topbar-spacer" />
    <div class="eu-site-topbar-actions">
      <slot name="actions" />
    </div>
  </header>
</template>

<style>
.eu-site-topbar {
  position: sticky;
  top: 0;
  z-index: 100;
  height: var(--eu-topbar-height);
  display: flex;
  align-items: center;
  gap: var(--eu-spacing-6);
  padding: 0 var(--eu-spacing-6);
  background: var(--eu-glass-bg);
  -webkit-backdrop-filter: var(--eu-glass-blur);
  backdrop-filter: var(--eu-glass-blur);
  border-bottom: 1px solid var(--eu-glass-border);
}
.eu-site-topbar-brand {
  display: flex;
  align-items: center;
  gap: var(--eu-spacing-3);
  flex-shrink: 0;
}
.eu-site-topbar-logo {
  font-size: var(--eu-font-size-lg);
  font-weight: 600;
  color: var(--eu-color-brand);
  text-decoration: none;
}
.eu-site-topbar-divider {
  width: 1px;
  height: 16px;
  background: var(--eu-border);
}
.eu-site-topbar-title {
  font-size: var(--eu-font-size-md);
  font-weight: 500;
  color: var(--eu-text-primary);
  white-space: nowrap;
}
.eu-site-topbar-nav {
  display: flex;
  align-items: center;
  gap: var(--eu-spacing-5);
  min-width: 0;
}
.eu-site-topbar-nav a {
  color: var(--eu-text-secondary);
  text-decoration: none;
  font-size: var(--eu-font-size-md);
  white-space: nowrap;
  transition: color var(--eu-transition);
}
.eu-site-topbar-nav a:hover {
  color: var(--eu-color-brand);
}
.eu-site-topbar-nav a.router-link-active {
  color: var(--eu-color-brand);
}
.eu-site-topbar-spacer {
  flex: 1;
}
.eu-site-topbar-actions {
  display: flex;
  align-items: center;
  gap: var(--eu-spacing-3);
  flex-shrink: 0;
}
</style>
