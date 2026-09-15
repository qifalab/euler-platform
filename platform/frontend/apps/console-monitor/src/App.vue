<script setup lang="ts">
/** console-monitor shell — RouterView host + in-app view tabs (SCMON, 02§7.2). */
import { RouterView, RouterLink, useRoute } from "vue-router";
import { computed, onMounted, onUnmounted, ref } from "vue";
import MonitorDashboard from "./views/MonitorDashboard.vue";

// The rule list links to #/scmon/dashboard/<ruleId> — a hash link, so the
// browser never leaves the page. vue-router (history mode) cannot serve that
// shape, so the dashboard is mounted here when the hash matches; without this
// the link was a dead end (MonitorDashboard was never rendered anywhere).
const hashRoute = ref(location.hash);
function onHash() { hashRoute.value = location.hash; }
onMounted(() => window.addEventListener("hashchange", onHash));
onUnmounted(() => window.removeEventListener("hashchange", onHash));
const detailRuleId = computed(() => {
  const m = hashRoute.value.match(/scmon\/dashboard\/([^/?#]+)/);
  return m ? decodeURIComponent(m[1]) : null;
});
/** A tab click leaves the detail view: clear the hash so RouterView shows. */
function leaveDetail() {
  if (detailRuleId.value) location.hash = "";
}

const route = useRoute();
const tabs = [
  { to: "/rules", label: "监控规则" },
  { to: "/alerts", label: "告警中心" },
  { to: "/anomaly", label: "异常检测" },
  { to: "/stability", label: "稳定性平台" },
  { to: "/regions", label: "地域与容灾" },
];
const active = computed(() => tabs.find((t) => route.path.startsWith(t.to))?.to ?? "/rules");
</script>

<template>
  <section class="mon-app">
    <nav class="mon-tabs">
      <RouterLink
        v-for="t in tabs" :key="t.to" :to="t.to"
        class="mon-tab" :class="{ active: active === t.to }"
        @click="leaveDetail"
      >{{ t.label }}</RouterLink>
    </nav>
    <MonitorDashboard v-if="detailRuleId" :key="detailRuleId" />
    <RouterView v-else />
  </section>
</template>

<style scoped>
.mon-app { padding: 16px 24px; }
.mon-tabs { display: flex; gap: 4px; border-bottom: 1px solid var(--sc-border); margin-bottom: 16px; }
.mon-tab {
  padding: 8px 16px; font-size: 14px; color: var(--sc-text-secondary);
  text-decoration: none; border-bottom: 2px solid transparent; margin-bottom: -1px;
  transition: color var(--sc-transition), border-color var(--sc-transition);
}
.mon-tab:hover { color: var(--sc-color-brand); }
.mon-tab.active { color: var(--sc-color-brand); border-bottom-color: var(--sc-color-brand); font-weight: 500; }
</style>
