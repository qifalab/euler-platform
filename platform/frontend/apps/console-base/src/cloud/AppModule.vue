<script setup lang="ts">
/// <reference types="vite/client" />
import {
  defineAsyncComponent,
  onBeforeUnmount,
  provide,
  type Component,
} from "vue";
import { appContextKey, createAppContext, type AppScope } from "./apps/shared";
import { useCloud } from "./context";
import CloudState from "./CloudState.vue";
const props = defineProps<{ scope: AppScope }>();
const { notify } = useCloud();
const controller = new AbortController();
provide(
  appContextKey,
  createAppContext(props.scope, notify, controller.signal),
);
const modules = import.meta.glob<{ default: Component }>("./apps/*/App.vue");
const load = modules[`./apps/${props.scope.applicationId}/App.vue`];
const component = load ? defineAsyncComponent(load) : null;
onBeforeUnmount(() => controller.abort());
function reload() {
  window.location.reload();
}
</script>
<template>
  <Suspense v-if="component"
    ><component :is="component" /><template #fallback
      ><CloudState kind="loading" title="正在打开应用" /></template
  ></Suspense>
  <CloudState
    v-else
    kind="error"
    title="应用组件未能加载"
    description="请刷新页面后重试。"
    @retry="reload"
  />
</template>
