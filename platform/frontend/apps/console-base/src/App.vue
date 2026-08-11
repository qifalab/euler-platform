<template>
  <div class="console-shell">
    <header class="shell-topbar">
      <span class="shell-logo">辰云控制台</span>
      <nav class="shell-nav">
        <a
          v-for="app in apps"
          :key="app.appCode"
          :href="'#' + app.activeRules[0]"
          class="shell-nav-item"
        >
          {{ app.appTitle }}
        </a>
      </nav>
    </header>

    <main class="shell-body">
      <!-- Sub-app containers are appended by the registry bootstrap in main.js.
           The top bar + nav above are owned by the base (shell) only. -->
    </main>
  </div>
</template>

<script>
export default {
  name: "ConsoleBase",
  props: {
    apps: { type: Array, default: () => [] },
  },
};
</script>

<style>
:root {
  --sc-topbar-height: 56px;
  --sc-sider-width: 208px;
  --sc-bg-page: #f5f7fa;
  --sc-bg-container: #ffffff;
  --sc-color-brand: #2b7de9;
  --sc-text-primary: #1f2329;
  --sc-text-secondary: #646a73;
}
* { box-sizing: border-box; }
body { margin: 0; font-family: -apple-system, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif; color: var(--sc-text-primary); background: var(--sc-bg-page); }
.console-shell { min-height: 100vh; }
.shell-topbar { height: var(--sc-topbar-height); background: var(--sc-bg-container); border-bottom: 1px solid #e5e6eb; display: flex; align-items: center; padding: 0 16px; gap: 24px; }
.shell-logo { font-weight: 600; color: var(--sc-color-brand); }
.shell-nav { display: flex; gap: 16px; }
.shell-nav-item { color: var(--sc-text-secondary); text-decoration: none; font-size: 14px; }
.shell-nav-item:hover { color: var(--sc-color-brand); }
.shell-body { padding: 16px; }
.sub-app-container { margin-top: 12px; }
.sub-app-placeholder { background: var(--sc-bg-container); border: 1px dashed #c9cdd4; border-radius: 6px; padding: 16px; color: var(--sc-text-secondary); }
</style>
