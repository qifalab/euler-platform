<script setup lang="ts">
/**
 * 修改备份策略 — PATCH /api/v1/orchestrator/resources/{id}/attributes with the
 * eubackup policy attributes `schedule` ("every Nh" | "daily HH:MM") and
 * `retentionDays` (1–3650). Opens on the policy's current attributes; 400
 * Resource.InvalidAttribute / AttributeImmutable and 409 (policy not live) are
 * shown in place. `saved` carries the updated resource detail.
 */
import { ref, watch } from "vue";
import { ElButton, ElDialog, ElForm, ElFormItem, ElInputNumber, ElMessage, ElRadio, ElRadioGroup, ElTimePicker } from "element-plus";
import { attributeErrorMessage, numberAttr, patchResourceAttributes, type ResourceAttributes } from "@eu/console-kit";
import { sdk } from "../sdk";
import {
  RETENTION_MAX,
  RETENTION_MIN,
  SCHEDULE_MAX_HOURS,
  clockOf,
  formatSchedule,
  parseSchedule,
  retentionError,
  scheduleError,
  type BackupSchedule,
} from "../backupPolicy";

const props = defineProps<{ modelValue: boolean; resourceId: string; attributes: ResourceAttributes }>();
const emit = defineEmits<{ "update:modelValue": [boolean]; saved: [unknown] }>();

const mode = ref<"every" | "daily">("every");
const hours = ref<number | undefined>(24);
const clock = ref("02:00");
const retention = ref<number | undefined>(30);
const formError = ref("");
const saving = ref(false);

function reset() {
  formError.value = "";
  const s = parseSchedule(props.attributes.schedule ?? "");
  mode.value = s?.kind ?? "every";
  hours.value = s?.kind === "every" ? s.hours : 24;
  clock.value = s ? clockOf(s) : "02:00";
  retention.value = numberAttr(props.attributes, "retentionDays", 30);
}
watch(() => props.modelValue, (open) => { if (open) reset(); }, { immediate: true });

function schedule(): BackupSchedule | null {
  if (mode.value === "every") return { kind: "every", hours: hours.value ?? Number.NaN };
  const m = /^([0-9]{2}):([0-9]{2})$/.exec(clock.value ?? "");
  return m ? { kind: "daily", hour: Number(m[1]), minute: Number(m[2]) } : null;
}

function close() {
  emit("update:modelValue", false);
}

async function save() {
  const s = schedule();
  const msg = (s ? scheduleError(s) : "请选择每天的备份时间") ?? retentionError(retention.value);
  formError.value = msg ?? "";
  if (msg || !s) return;
  saving.value = true;
  try {
    const detail = await patchResourceAttributes(sdk, props.resourceId, {
      schedule: formatSchedule(s),
      retentionDays: retention.value,
    });
    ElMessage.success("备份策略已更新");
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
    title="修改备份策略"
    width="480px"
    append-to-body
    @update:model-value="(v: boolean) => emit('update:modelValue', v)"
  >
    <ElForm label-position="top" @submit.prevent>
      <ElFormItem label="备份周期">
        <ElRadioGroup v-model="mode">
          <ElRadio value="every">按间隔</ElRadio>
          <ElRadio value="daily">每天定时</ElRadio>
        </ElRadioGroup>
      </ElFormItem>
      <ElFormItem v-if="mode === 'every'" label="备份间隔 (小时)">
        <ElInputNumber v-model="hours" :min="1" :max="SCHEDULE_MAX_HOURS" :precision="0" />
      </ElFormItem>
      <ElFormItem v-else label="每天备份时间">
        <ElTimePicker v-model="clock" format="HH:mm" value-format="HH:mm" :clearable="false" />
        <p class="bp-hint">按北京时间(UTC+08:00)执行。</p>
      </ElFormItem>
      <ElFormItem label="保留期 (天)">
        <ElInputNumber v-model="retention" :min="RETENTION_MIN" :max="RETENTION_MAX" :precision="0" />
        <p class="bp-hint">新保留期对之后的快照生效;已有快照按创建时的保留期到期清理。</p>
      </ElFormItem>
    </ElForm>
    <p v-if="formError" class="bp-error" role="alert">{{ formError }}</p>
    <template #footer>
      <ElButton @click="close">取消</ElButton>
      <ElButton type="primary" :loading="saving" @click="save">保存</ElButton>
    </template>
  </ElDialog>
</template>

<style scoped>
.bp-hint { font-size: 12px; color: var(--eu-text-disabled); margin: 4px 0 0; width: 100%; }
.bp-error { font-size: 13px; color: var(--eu-color-danger); margin: 0; }
</style>
