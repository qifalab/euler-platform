<script lang="ts">
/** One editable attribute of the dialog. */
export interface AttributeField {
  /** Attribute key (e.g. retentionDays). */
  key: string;
  label: string;
  /** "int" (default) renders a number input; "text" a text input. */
  type?: "int" | "text";
  min?: number;
  max?: number;
  step?: number;
  /** Shown when the resource carries no value for the key yet. */
  default?: number | string;
  /** Text fields: reject an empty value. */
  required?: boolean;
  /** Text fields: maximum length in characters. */
  maxLength?: number;
  /** Unit / explanation under the input. */
  hint?: string;
}
</script>

<script setup lang="ts">
/**
 * AttributeEditDialog — edits mutable resource attributes in place (02§7.3):
 *   PATCH /api/v1/orchestrator/resources/{id}/attributes {"attributes": {...}}
 *
 * Fields open on the resource's current `attributes` (falling back to each
 * field's default), are checked client-side against the same bounds the
 * orchestrator enforces, then saved together — related keys (e.g. a scaling
 * policy's min/desired/max) are validated as one set. 400 / 409 answers are
 * shown in place and the dialog stays open; `saved` carries the updated
 * detail so the page can refresh.
 */
import { reactive, ref, watch } from "vue";
import { ElButton, ElDialog, ElForm, ElFormItem, ElInput, ElInputNumber, ElMessage } from "element-plus";
import type { SDK } from "@eu/sdk";
import {
  attributeErrorMessage,
  numberAttr,
  patchResourceAttributes,
  validateIntRange,
  type ResourceAttributes,
} from "./resourceAttributes";

const props = defineProps<{
  modelValue: boolean;
  sdk: Pick<SDK, "patch">;
  resourceId: string;
  title: string;
  fields: AttributeField[];
  /** The resource's current attributes. */
  attributes: ResourceAttributes;
  /** Cross-field rule over the (numeric/text) values; a message or null. */
  validate?: (values: Record<string, number | string | undefined>) => string | null;
  /** Explanation above the form. */
  description?: string;
}>();
const emit = defineEmits<{ "update:modelValue": [boolean]; saved: [unknown] }>();

const nums = reactive<Record<string, number | undefined>>({});
const texts = reactive<Record<string, string>>({});
const formError = ref("");
const saving = ref(false);

const isText = (f: AttributeField) => f.type === "text";

function reset() {
  formError.value = "";
  for (const f of props.fields) {
    if (isText(f)) {
      texts[f.key] = props.attributes[f.key] ?? (f.default !== undefined ? String(f.default) : "");
    } else {
      const d = typeof f.default === "number" ? f.default : undefined;
      nums[f.key] = numberAttr(props.attributes, f.key, d);
    }
  }
}

watch(() => props.modelValue, (open) => { if (open) reset(); }, { immediate: true });

function values(): Record<string, number | string | undefined> {
  const out: Record<string, number | string | undefined> = {};
  for (const f of props.fields) out[f.key] = isText(f) ? texts[f.key] : nums[f.key];
  return out;
}

function check(): string | null {
  for (const f of props.fields) {
    if (isText(f)) {
      const v = (texts[f.key] ?? "").trim();
      if (!v && f.required) return `请填写${f.label}`;
      if (f.maxLength !== undefined && [...v].length > f.maxLength) return `${f.label}最多 ${f.maxLength} 个字符`;
    } else {
      const msg = validateIntRange(
        nums[f.key],
        f.min ?? Number.MIN_SAFE_INTEGER,
        f.max ?? Number.MAX_SAFE_INTEGER,
        f.label,
      );
      if (msg) return msg;
    }
  }
  return props.validate?.(values()) ?? null;
}

function close() {
  emit("update:modelValue", false);
}

async function save() {
  const msg = check();
  formError.value = msg ?? "";
  if (msg) return;
  saving.value = true;
  try {
    const detail = await patchResourceAttributes(props.sdk, props.resourceId, values());
    ElMessage.success("修改已保存");
    emit("saved", detail);
    close();
  } catch (e) {
    formError.value = attributeErrorMessage(e);
  } finally {
    saving.value = false;
  }
}
</script>

<template>
  <ElDialog
    :model-value="modelValue"
    :title="title"
    width="480px"
    append-to-body
    @update:model-value="(v: boolean) => emit('update:modelValue', v)"
  >
    <p v-if="description" class="eu-attr-hint">{{ description }}</p>
    <ElForm label-position="top" @submit.prevent>
      <ElFormItem v-for="f in fields" :key="f.key" :label="f.label">
        <ElInput
          v-if="isText(f)"
          v-model="texts[f.key]"
          :maxlength="f.maxLength"
          :show-word-limit="f.maxLength !== undefined"
        />
        <ElInputNumber
          v-else
          v-model="nums[f.key]"
          :min="f.min"
          :max="f.max"
          :step="f.step ?? 1"
          :precision="0"
        />
        <p v-if="f.hint" class="eu-attr-hint eu-attr-hint-field">{{ f.hint }}</p>
      </ElFormItem>
    </ElForm>
    <p v-if="formError" class="eu-attr-error" role="alert">{{ formError }}</p>
    <template #footer>
      <ElButton @click="close">取消</ElButton>
      <ElButton type="primary" :loading="saving" @click="save">保存</ElButton>
    </template>
  </ElDialog>
</template>

<style>
.eu-attr-hint { font-size: 13px; color: var(--eu-text-secondary); margin: 0 0 12px; }
.eu-attr-hint-field { font-size: 12px; margin: 4px 0 0; width: 100%; }
.eu-attr-error { font-size: 13px; color: var(--eu-color-danger); margin: 0; }
</style>
