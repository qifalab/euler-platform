<script setup lang="ts">
import { ref } from "vue";
import { useApp } from "../shared";
import Evidence from "./Evidence.vue";
import { actionNames, when, key, type Prepared } from "./model";
const props = defineProps<{ prepared: Prepared }>();
const emit = defineEmits<{ close: []; approved: [] }>();
const app = useApp();
const checked = ref(false),
  busy = ref(false),
  error = ref("");
async function approve() {
  if (!checked.value || !app.can("manage") || busy.value) return;
  busy.value = true;
  error.value = "";
  try {
    if (Date.parse(props.prepared.approvalExpiresAt) <= Date.now())
      throw new Error("审批已过期，请重新准备操作");
    await app.request(`/actions/${key(props.prepared.action.id)}/approve`, {
      method: "POST",
      body: { approvalNonce: props.prepared.approvalNonce },
    });
    app.notify("审批已提交，等待设备签收与执行");
    emit("approved");
    emit("close");
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    busy.value = false;
  }
}
</script>
<template>
  <div class="ws-overlay" @click.self="!busy && emit('close')">
    <section
      role="dialog"
      aria-modal="true"
      aria-labelledby="approval-heading"
      class="panel ws-dialog"
    >
      <div class="toolbar">
        <h2 id="approval-heading">
          审批 · {{ actionNames[prepared.action.type] ?? prepared.action.type }}
        </h2>
        <button :disabled="busy" @click="emit('close')">关闭</button>
      </div>
      <p>请检查设备、参数、影响与回滚方案。审批后设备才会领取执行命令。</p>
      <p>
        <strong>设备</strong> {{ prepared.action.deviceId }} ·
        <strong>审批到期</strong> {{ when(prepared.approvalExpiresAt) }}
      </p>
      <Evidence :value="prepared.action.preview" />
      <h3>审批参数</h3>
      <Evidence :value="prepared.action.parameters" />
      <p class="status-note">
        SSH 加固需要在操作后使用新的 SSH
        会话确认连通；未确认将由设备自动回滚。执行结果未知时应先核查设备，不重复批准。
      </p>
      <label class="ws-check"
        ><input
          v-model="checked"
          type="checkbox"
        />我已核对目标设备、操作影响及回滚方案</label
      >
      <p v-if="error" role="alert" class="ws-error">{{ error }}</p>
      <div class="actions">
        <button
          class="primary"
          :disabled="!checked || busy || !app.can('manage')"
          @click="approve"
        >
          {{ busy ? "正在审批…" : "批准本次操作" }}</button
        ><button :disabled="busy" @click="emit('close')">暂不执行</button>
      </div>
    </section>
  </div>
</template>
