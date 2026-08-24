<script setup lang="ts">
/**
 * RegionSelector — global region switcher (02§7.1).
 * The base renders this in the top bar; switching broadcasts region:changed
 * via the bridge, and every active sub-app re-fetches. regionId format per
 * 00 附录A (e.g. cn-north-1). The region list is fetched from svc-catalog
 * (useCatalogMeta) — the component never hardcodes geography; the `regions`
 * prop remains as a test/storybook override.
 */
import { computed, onMounted, ref } from "vue";
import { ElSelect, ElOption } from "element-plus";
import { bridge } from "@sc/wujie-bridge";
import { fetchRegions } from "./useCatalogMeta";

const props = defineProps<{
  modelValue: string;
  regions?: Array<{ id: string; label: string }>;
}>();
const emit = defineEmits<{ "update:modelValue": [value: string] }>();

const fetched = ref<Array<{ id: string; label: string }>>([]);
const failed = ref(false);

onMounted(async () => {
  try {
    const rs = await fetchRegions();
    fetched.value = rs.map((r) => ({ id: r.regionId, label: r.regionName }));
  } catch {
    failed.value = true;
  }
});

const regions = computed(() => props.regions ?? fetched.value);

function onChange(id: string) {
  emit("update:modelValue", id);
  bridge.emit("region:changed", { regionId: id });
}
</script>

<template>
  <ElSelect
    :model-value="modelValue"
    size="small"
    style="width: 160px"
    :loading="regions.length === 0 && !failed"
    @update:model-value="onChange"
  >
    <ElOption
      v-for="r in regions"
      :key="r.id"
      :value="r.id"
      :label="r.label"
    />
    <ElOption v-if="failed" value="" label="地域加载失败（svc-catalog 未启动）" disabled />
  </ElSelect>
</template>
