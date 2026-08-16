<script setup lang="ts">
/**
 * RegionSelector — global region switcher (02§7.1).
 * The base renders this in the top bar; switching broadcasts region:changed
 * via the bridge, and every active sub-app re-fetches. regionId format per
 * 00 附录A (e.g. cn-north-1).
 */
import { ElSelect, ElOption } from "element-plus";
import { bridge } from "@sc/wujie-bridge";

const props = defineProps<{
  modelValue: string;
  regions?: Array<{ id: string; label: string }>;
}>();
const emit = defineEmits<{ "update:modelValue": [value: string] }>();

const defaultRegions = [
  { id: "cn-north-1", label: "华北 1（北京）" },
  { id: "cn-east-1", label: "华东 1（杭州）" },
  { id: "cn-south-1", label: "华南 1（深圳）" },
];
const regions = props.regions ?? defaultRegions;

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
    @update:model-value="onChange"
  >
    <ElOption
      v-for="r in regions"
      :key="r.id"
      :value="r.id"
      :label="r.label"
    />
  </ElSelect>
</template>
