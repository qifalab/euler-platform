export interface History {
  id: string;
  actorId: string;
  action: string;
  fromStatus: string;
  toStatus: string;
  createdAt: string;
}
export interface Verification {
  id: string;
  actorId: string;
  actorName: string;
  realName: string;
  studentId: string;
  identityType: string;
  identityTitle: string;
  status: string;
  version: number;
  createdAt: string;
  updatedAt: string;
  history?: History[];
}
export interface Club {
  id: string;
  actorId: string;
  actorName: string;
  realName: string;
  email: string;
  status: string;
  trustSchemeId: string;
  trustVerified: boolean;
  version: number;
  createdAt: string;
  updatedAt: string;
  interviewSentAt: string;
  offerSentAt: string;
  confirmedAt: string;
  history?: History[];
}
export interface Member {
  actorId: string;
  name: string;
  email: string;
  emailVerified: boolean;
  bio: string;
  phone: string;
  avatar: string;
  identityLevel: number;
  identityTitle: string;
  createdAt: string;
  updatedAt: string;
}
export interface Notification {
  id: string;
  targetId: string;
  actorId: string;
  kind: string;
  state: string;
  error: string;
  createdAt: string;
  completedAt: string;
}
export interface Page<T> {
  items: T[];
  total: number;
  page: number;
}
export interface ClubOverview extends Page<Club> {
  clubName: string;
  requireTrust: boolean;
  trustSchemeId: string;
  trustVerified: boolean;
  emailVerified: boolean;
  counts?: Record<string, number>;
}
export const identities: Record<string, string> = {
  active: "活跃成员",
  core: "核心成员",
  key: "核心贡献",
  management: "管理层",
  outstanding: "卓越贡献",
  smart_car: "智能车团队",
};
export const statuses: Record<string, string> = {
  pending: "待审核",
  approved: "已通过",
  rejected: "已拒绝",
  interview_sent: "笔试通知已发送",
  offer_sent: "待接受录取",
  offer_confirmed: "已接受录取",
  queued: "等待发送",
  sending: "正在发送／结果待确认",
  sent: "已发送",
  failed: "发送失败",
  uncertain: "结果未知",
};
export const actions: Record<string, string> = {
  "verification.submit": "提交身份申请",
  "verification.edit": "更新申请资料",
  "verification.approved": "通过身份审核",
  "verification.rejected": "拒绝身份申请",
  "club.submit": "提交报名",
  "club.interview": "发送笔试通知",
  "club.resend_interview": "重发笔试通知",
  "club.offer": "发送录取通知",
  "club.resend_offer": "重发录取通知",
  "club.confirm": "接受录取",
  "club.reject": "拒绝报名",
};
export const date = (value: string) =>
  value
    ? new Date(value).toLocaleString("zh-CN", {
        year: "numeric",
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
      })
    : "—";
