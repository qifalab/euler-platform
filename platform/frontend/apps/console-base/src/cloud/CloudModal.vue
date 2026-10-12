<script setup lang="ts">
import { ref, watch, nextTick, onBeforeUnmount, getCurrentInstance } from "vue";
import CloudIcon from "./CloudIcon.vue";
const props = defineProps<{
  modelValue: boolean;
  title: string;
  description?: string;
}>();
const emit = defineEmits<{ "update:modelValue": [value: boolean] }>();
const dialog = ref<HTMLDialogElement>();
const titleId = `cloud-modal-${getCurrentInstance()?.uid}`;
let previous: HTMLElement | null = null;
function close() {
  emit("update:modelValue", false);
}
watch(
  () => props.modelValue,
  async (open) => {
    await nextTick();
    if (open && !dialog.value?.open) {
      previous = document.activeElement as HTMLElement;
      dialog.value?.showModal();
    } else if (!open && dialog.value?.open) {
      dialog.value.close();
      previous?.focus();
    }
  },
  { immediate: true },
);
onBeforeUnmount(() => dialog.value?.close());
</script>
<template>
  <dialog
    ref="dialog"
    class="cloud-modal"
    :aria-labelledby="titleId"
    @cancel.prevent="close"
    @click="
      (event) => {
        if (event.target === dialog) close();
      }
    "
  >
    <div class="modal-heading">
      <div>
        <h2 :id="titleId">{{ title }}</h2>
        <p v-if="description" class="muted">{{ description }}</p>
      </div>
      <button class="icon-button" aria-label="关闭对话框" @click="close">
        <CloudIcon name="close" />
      </button>
    </div>
    <slot />
  </dialog>
</template>
