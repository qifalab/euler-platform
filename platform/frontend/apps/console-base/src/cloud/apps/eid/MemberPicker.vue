<script setup lang="ts">
import { onMounted, ref } from "vue";
import { useApp } from "../shared";
import type { Member, Page } from "./types";
const props = defineProps<{ modelValue: string[] }>();
const emit = defineEmits<{ "update:modelValue": [value: string[]] }>();
const app = useApp(),
  people = ref<Member[]>([]),
  page = ref(1),
  total = ref(0),
  search = ref(""),
  loading = ref(false),
  error = ref("");
const names = ref<Record<string, string>>({});
async function load(next = 1) {
  if (loading.value) return;
  loading.value = true;
  error.value = "";
  try {
    const out = await app.request<Page<Member>>(
      `/admin/members?page=${next}&pageSize=25&search=${encodeURIComponent(search.value)}`,
    );
    people.value = out.items;
    page.value = next;
    total.value = out.total;
    for (const person of out.items) names.value[person.actorId] = person.name;
  } catch (e) {
    error.value = e instanceof Error ? e.message : "无法加载会员";
  } finally {
    loading.value = false;
  }
}
function select(id: string, checked: boolean) {
  emit(
    "update:modelValue",
    checked
      ? [...new Set([...props.modelValue, id])]
      : props.modelValue.filter((value) => value !== id),
  );
}
onMounted(() => load());
</script>
<template>
  <fieldset class="member-picker field full">
    <legend>允许查询的会员</legend>
    <p class="muted">
      已选 {{ modelValue.length }} / 100 位，搜索或翻页会保留选择。
    </p>
    <div class="toolbar">
      <input
        v-model="search"
        aria-label="搜索授权会员"
        placeholder="姓名或欧拉成员编号"
        @keydown.enter.prevent="load(1)"
      /><button
        type="button"
        class="button"
        :disabled="loading"
        @click="load(1)"
      >
        搜索会员
      </button>
    </div>
    <p v-if="error" role="alert">{{ error }}</p>
    <p v-if="loading" role="status">正在加载会员…</p>
    <div class="member-options">
      <label
        v-for="person in people"
        :key="person.actorId"
        class="member-option"
        ><input
          type="checkbox"
          :value="person.actorId"
          :checked="modelValue.includes(person.actorId)"
          :disabled="
            !modelValue.includes(person.actorId) && modelValue.length >= 100
          "
          @change="
            select(person.actorId, ($event.target as HTMLInputElement).checked)
          "
        /><span
          >{{ person.name }} <small>{{ person.actorId }}</small></span
        ></label
      >
      <p v-if="!loading && !people.length" class="muted">
        未找到会员。成员使用本应用后会建立会员档案。
      </p>
    </div>
    <div class="pagination">
      <button
        type="button"
        class="button"
        :disabled="loading || page <= 1"
        @click="load(page - 1)"
      >
        上一页会员</button
      ><span>第 {{ page }} 页 · 共 {{ total }} 位</span
      ><button
        type="button"
        class="button"
        :disabled="loading || page * 25 >= total"
        @click="load(page + 1)"
      >
        下一页会员
      </button>
    </div>
    <div v-if="modelValue.length" class="selected-members">
      <span v-for="id in modelValue" :key="id"
        >{{ names[id] || id }}
        <button
          type="button"
          :aria-label="`移除 ${names[id] || id}`"
          @click="select(id, false)"
        >
          ×
        </button></span
      >
    </div>
  </fieldset>
</template>
<style scoped>
.member-picker {
  min-width: 0;
  border: 1px solid var(--cloud-border);
  border-radius: 12px;
  padding: 16px;
}
.member-picker legend {
  padding: 0 5px;
}
.member-picker .toolbar input {
  flex: 1;
  min-width: 0;
}
.member-options {
  display: grid;
  gap: 10px;
  margin: 12px 0;
  max-height: 250px;
  overflow: auto;
}
.member-option {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 13px;
}
.member-option input {
  width: auto;
}
.member-option small {
  display: block;
  font-size: 11px;
  color: var(--cloud-muted);
  overflow-wrap: anywhere;
}
.selected-members {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}
.selected-members > span {
  background: var(--cloud-panel);
  border: 1px solid var(--cloud-border);
  border-radius: 8px;
  padding: 4px 8px;
  font-size: 12px;
}
.selected-members button {
  border: 0;
  background: none;
  color: inherit;
  cursor: pointer;
  padding: 2px 5px;
}
.muted {
  font-size: 12px;
  color: var(--cloud-muted);
}
</style>
