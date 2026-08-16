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
  <header class="sc-site-topbar">
    <div class="sc-site-topbar-brand">
      <a class="sc-site-topbar-logo" href="/">辰云</a>
      <span class="sc-site-topbar-divider" aria-hidden="true" />
      <span class="sc-site-topbar-title">{{ title }}</span>
    </div>
    <nav class="sc-site-topbar-nav">
      <slot name="nav" />
    </nav>
    <div class="sc-site-topbar-spacer" />
    <div class="sc-site-topbar-actions">
      <slot name="actions" />
    </div>
  </header>
</template>

<style>
.sc-site-topbar {
  position: sticky;
  top: 0;
  z-index: 100;
  height: var(--sc-topbar-height);
  display: flex;
  align-items: center;
  gap: var(--sc-spacing-6);
  padding: 0 var(--sc-spacing-6);
  background: var(--sc-glass-bg);
  -webkit-backdrop-filter: var(--sc-glass-blur);
  backdrop-filter: var(--sc-glass-blur);
  border-bottom: 1px solid var(--sc-glass-border);
}
.sc-site-topbar-brand {
  display: flex;
  align-items: center;
  gap: var(--sc-spacing-3);
  flex-shrink: 0;
}
.sc-site-topbar-logo {
  font-size: var(--sc-font-size-lg);
  font-weight: 600;
  color: var(--sc-color-brand);
  text-decoration: none;
}
.sc-site-topbar-divider {
  width: 1px;
  height: 16px;
  background: var(--sc-border);
}
.sc-site-topbar-title {
  font-size: var(--sc-font-size-md);
  font-weight: 500;
  color: var(--sc-text-primary);
  white-space: nowrap;
}
.sc-site-topbar-nav {
  display: flex;
  align-items: center;
  gap: var(--sc-spacing-5);
  min-width: 0;
}
.sc-site-topbar-nav a {
  color: var(--sc-text-secondary);
  text-decoration: none;
  font-size: var(--sc-font-size-md);
  white-space: nowrap;
  transition: color var(--sc-transition);
}
.sc-site-topbar-nav a:hover {
  color: var(--sc-color-brand);
}
.sc-site-topbar-nav a.router-link-active {
  color: var(--sc-color-brand);
}
.sc-site-topbar-spacer {
  flex: 1;
}
.sc-site-topbar-actions {
  display: flex;
  align-items: center;
  gap: var(--sc-spacing-3);
  flex-shrink: 0;
}
</style>
