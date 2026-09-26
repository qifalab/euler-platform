export type Role = "owner" | "admin" | "member" | "viewer";
export interface Session {
  authenticated: boolean;
  user?: { id: string; displayName: string; provider: string; subject: string };
  csrfToken?: string;
  loginAvailable: boolean;
  loginError?: string;
  platformAdmin?: boolean;
}
export interface Tenant {
  id: string;
  name: string;
  role: Role;
  createdAt: string;
}
export interface Project {
  id: string;
  tenantId: string;
  name: string;
  role: Role;
  createdAt: string;
}
export interface Member {
  userId: string;
  displayName: string;
  role: Role;
  joinedAt?: string;
}
export interface Invitation {
  id: string;
  role: Role;
  expiresAt: string;
  createdAt?: string;
}
export interface Application {
  id: string;
  name: string;
  category: string;
  description: string;
  color: string;
  homepage: string;
  repository: string;
  capabilities: string[];
  connectionMode: string;
  limitations: string[];
}
export interface Installation {
  id: string;
  tenantId: string;
  projectId: string;
  applicationId: string;
  status: "enabled" | "disabled";
  createdAt: string;
  connection?: { baseUrl: string; configured: boolean; updatedAt: string };
}
export interface Summary {
  state: "ready" | "unconfigured" | "unavailable" | "unsupported";
  message: string;
  metrics?: { label: string; value: string | number }[];
  verification?: { status: string; identityType?: string };
  consoleUrl?: string;
  checkedAt?: string;
}
export interface Resource {
  id: string;
  name: string;
  type: string;
  status?: string;
  url?: string;
}
export interface AuditEvent {
  id: string;
  actorId: string;
  action: string;
  targetId: string;
  projectId?: string;
  createdAt: string;
  summary: string;
}
export interface List<T> {
  items: T[];
}
