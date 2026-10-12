<script setup lang="ts">
import { RouterLink } from "vue-router";
import CloudState from "./CloudState.vue";
</script>
<template>
  <CloudState
    kind="empty"
    title="这个页面已经不在这里"
    description="统一应用云围绕团队、项目和应用组织服务。请从项目总览继续。"
    ><RouterLink class="button button-primary" to="/"
      >返回项目总览</RouterLink
    ></CloudState
  >
</template>
