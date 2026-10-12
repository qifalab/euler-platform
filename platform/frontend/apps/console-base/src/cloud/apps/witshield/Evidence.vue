<script setup lang="ts">
defineProps<{ value: unknown }>();
const names: Record<string, string> = {
  summary: "说明",
  risk: "风险级别",
  requiresApproval: "需要审批",
  steps: "执行步骤",
  checks: "前置检查",
  impact: "影响",
  rollback: "回滚方式",
  warnings: "注意事项",
  title: "标题",
  type: "动作类型",
  details: "详细信息",
  parameters: "参数",
  packages: "软件包",
  rollbackAfterSeconds: "自动回滚等待秒数",
  address: "目标地址",
  ttlSeconds: "有效期（秒）",
  path: "文件路径",
  mode: "权限/模式",
  pid: "进程 ID",
  executable: "可执行文件",
  startTime: "进程启动标识",
  reason: "原因",
  checksTotal: "全部检查",
  checksCompleted: "完成检查",
  coveragePercent: "检查覆盖率",
  errors: "检查错误",
  score: "安全分数",
  canRollback: "支持回滚",
  allowed: "允许执行",
  action: "决策",
  sourceIp: "来源 IP",
  ban: "是否封禁",
  shouldBan: "是否封禁",
  triggered: "是否触发",
};
</script>
<template>
  <dl
    v-if="value && typeof value === 'object' && !Array.isArray(value)"
    class="ws-evidence"
  >
    <template
      v-for="(item, name) in value as Record<string, unknown>"
      :key="name"
      ><dt>{{ names[name] ?? name }}</dt>
      <dd><Evidence :value="item" /></dd
    ></template>
  </dl>
  <ul v-else-if="Array.isArray(value)">
    <li v-for="(item, index) in value" :key="index">
      <Evidence :value="item" />
    </li>
  </ul>
  <span v-else class="ws-preserve">{{
    value === true ? "是" : value === false ? "否" : (value ?? "—")
  }}</span>
</template>
<style scoped>
.ws-evidence {
  display: grid;
  grid-template-columns: minmax(90px, auto) 1fr;
  gap: 8px 20px;
  margin: 0;
}
.ws-evidence dt {
  color: var(--cloud-muted);
  font-size: 12px;
}
.ws-evidence dd {
  margin: 0;
  min-width: 0;
}
.ws-preserve {
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
ul {
  padding-left: 20px;
  margin: 0;
}
</style>
