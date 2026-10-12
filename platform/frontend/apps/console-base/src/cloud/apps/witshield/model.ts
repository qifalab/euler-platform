// Euler-owned presentation, using the Apache-2.0 WitShield business protocol.
export type {
  IncidentDetail,
  SecurityIncident,
  PolicyGrant,
  SensorHealth,
  AIInvestigationPolicy,
  AIInvestigationUsage,
  SystemHealth,
  ScanSchedule,
} from "./upstream-types";
export interface Device {
  id: string;
  name: string;
  hostname: string;
  os: string;
  arch: string;
  agentVersion: string;
  observerOnly: boolean;
  status: string;
  lastSeenAt?: string;
  enrolledAt: string;
}
export interface Finding {
  id: string;
  deviceId: string;
  title: string;
  description: string;
  evidence?: string;
  remediation?: string;
  category: string;
  severity: string;
  status: string;
  lastSeenAt: string;
}
export interface Report {
  id: string;
  deviceId: string;
  score: number;
  startedAt: string;
  completedAt: string;
  summary?: Record<string, unknown>;
  findings?: Finding[];
}
export interface Action {
  id: string;
  deviceId: string;
  findingId?: string;
  type: string;
  parameters: Record<string, unknown>;
  preview: Record<string, unknown>;
  status: string;
  createdAt: string;
  updatedAt: string;
  confirmBy?: string;
  error?: string;
  rollbackPayload?: unknown;
  approvedBy?: string;
}
export interface Prepared {
  action: Action;
  approvalNonce: string;
  approvalExpiresAt: string;
  notice?: string;
}
export interface Audit {
  id: string | number;
  actor: string;
  event: string;
  actionId?: string;
  deviceId?: string;
  summary?: string;
  details?: unknown;
  createdAt: string;
}
export interface Token {
  id: string;
  name: string;
  hint: string;
  uses: number;
  maxUses: number;
  expiresAt: string;
  revokedAt?: string;
}
export interface Defense {
  enabled: boolean;
  emergencyStop: boolean;
  autoBan: boolean;
  failureThreshold: number;
  window: string;
  banDuration: string;
  maxBansPerHour: number;
  allowlist: string[];
}
export const actionNames: Record<string, string> = {
  package_security_upgrade: "安全软件包升级",
  ssh_password_hardening: "SSH 密码登录加固",
  temporary_ip_ban: "临时封禁 IP",
  file_permission_repair: "修复文件权限",
  temporary_process_suspend: "临时暂停进程",
};
export const statusNames: Record<string, string> = {
  ok: "正常",
  starting: "启动中",
  stale: "更新超时",
  completed: "已完成",
  running: "运行中",
  queued: "已排队",
  proposed: "待审方案",
  ignored: "已忽略",
  online: "在线",
  offline: "离线",
  pending: "待连接",
  revoked: "已撤销",
  open: "待处理",
  resolved: "已解决",
  dismissed: "已忽略",
  investigating: "调查中",
  awaiting_approval: "待审批",
  responding: "响应中",
  monitoring: "监控中",
  draft: "待审批",
  approved: "已批准",
  executing: "执行中",
  awaiting_confirmation: "等待 SSH 连通确认",
  confirming: "正在确认",
  succeeded: "已成功",
  failed: "失败",
  rolling_back: "回滚中",
  rolled_back: "已回滚",
  cancelled: "已取消",
  indeterminate: "执行结果未知",
  active: "正常",
  degraded: "降级",
  unavailable: "不可用",
  optional: "可选",
  critical: "严重",
  high: "高风险",
  medium: "中风险",
  low: "低风险",
  info: "提示",
};
export const label = (value: string) => statusNames[value] ?? value;
export function when(value?: string) {
  if (!value || value.startsWith("0001-")) return "尚无记录";
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? value : d.toLocaleString("zh-CN");
}
export const key = (value: string) => encodeURIComponent(value);
export const split = (value: string) =>
  value
    .split(/[\n,，]/)
    .map((x) => x.trim())
    .filter(Boolean);
