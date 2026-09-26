<script setup lang="ts">
import { computed, onMounted, onBeforeUnmount, ref } from "vue";
import { api, errorMessage, formatDate, roleLabels } from "./api";
import { useCloud } from "./context";
import type { Invitation, List, Member, Role } from "./types";
import CloudIcon from "./CloudIcon.vue";
import CloudState from "./CloudState.vue";
import CloudModal from "./CloudModal.vue";
const {
  state,
  tenant,
  project,
  canManageTeam,
  canManageProject,
  tenantPath,
  projectPath,
  notify,
} = useCloud();
const members = ref<Member[]>([]);
const projectMembers = ref<Member[]>([]);
const invitations = ref<Invitation[]>([]);
const loading = ref(true);
const error = ref("");
const busy = ref(false);
const actionError = ref("");
const tab = ref("team");
const inviteOpen = ref(false);
const grantOpen = ref(false);
const inviteRole = ref<Role>("member");
const grantUser = ref("");
const grantRole = ref<Role>("member");
const invitationLink = ref("");
const invitationExpiry = ref("");
const confirmation = ref<{
  title: string;
  description: string;
  path: string;
  method: string;
  body?: unknown;
} | null>(null);
const confirmOpen = computed({
  get: () => Boolean(confirmation.value),
  set: (value) => {
    if (!value) confirmation.value = null;
  },
});
const roleOptions: Role[] = ["admin", "member", "viewer"];
let controller: AbortController | undefined;
async function load() {
  controller?.abort();
  controller = new AbortController();
  const signal = controller.signal;
  loading.value = true;
  error.value = "";
  try {
    const results = await Promise.all([
      api<List<Member>>(tenantPath("/members"), { signal }),
      state.projectId
        ? api<List<Member>>(projectPath("/members"), { signal })
        : Promise.resolve({ items: [] }),
      canManageTeam.value
        ? api<List<Invitation>>(tenantPath("/invitations"), { signal })
        : Promise.resolve({ items: [] }),
    ]);
    if (signal.aborted) return;
    members.value = results[0].items;
    projectMembers.value = results[1].items;
    invitations.value = results[2].items;
  } catch (err) {
    if (!signal.aborted) error.value = errorMessage(err);
  } finally {
    if (!signal.aborted) loading.value = false;
  }
}
async function createInvitation() {
  busy.value = true;
  actionError.value = "";
  try {
    const result = await api<{ id: string; token: string; expiresAt: string }>(
      tenantPath("/invitations"),
      { method: "POST", body: { role: inviteRole.value } },
    );
    invitationLink.value = `${window.location.origin}/join?token=${encodeURIComponent(result.token)}`;
    invitationExpiry.value = result.expiresAt;
    await load();
  } catch (err) {
    actionError.value = errorMessage(err);
  } finally {
    busy.value = false;
  }
}
async function copyInvitation() {
  try {
    await navigator.clipboard.writeText(invitationLink.value);
    notify("邀请链接已复制。");
  } catch {
    actionError.value = "无法自动复制，请选中下方链接手动复制。";
  }
}
async function grant() {
  if (!grantUser.value) return;
  busy.value = true;
  actionError.value = "";
  try {
    await api(projectPath("/members"), {
      method: "POST",
      body: { userId: grantUser.value, role: grantRole.value },
    });
    grantOpen.value = false;
    notify("项目授权已更新。");
    await load();
  } catch (err) {
    actionError.value = errorMessage(err);
  } finally {
    busy.value = false;
  }
}
function requestRole(member: Member, event: Event) {
  const select = event.target as HTMLSelectElement;
  const role = select.value;
  select.value = member.role;
  if (role === member.role) return;
  actionError.value = "";
  confirmation.value = {
    title: "变更团队角色",
    description: `将 ${member.displayName} 的团队角色改为${roleLabels[role]}。项目访问范围可能随之变化。`,
    path: tenantPath(`/members/${encodeURIComponent(member.userId)}`),
    method: "PATCH",
    body: { role },
  };
}
function remove(member: Member, scope: string) {
  actionError.value = "";
  confirmation.value = {
    title: scope === "team" ? "移除团队成员" : "移除项目授权",
    description: `确认移除 ${member.displayName} 的${scope === "team" ? "团队成员身份及相关项目访问权限" : "项目访问权限"}？`,
    path:
      scope === "team"
        ? tenantPath(`/members/${encodeURIComponent(member.userId)}`)
        : projectPath(`/members/${encodeURIComponent(member.userId)}`),
    method: "DELETE",
  };
}
async function confirm() {
  if (!confirmation.value) return;
  busy.value = true;
  actionError.value = "";
  try {
    const item = confirmation.value;
    await api(item.path, { method: item.method, body: item.body });
    confirmation.value = null;
    notify("变更已保存。");
    await load();
  } catch (err) {
    actionError.value = errorMessage(err);
  } finally {
    busy.value = false;
  }
}
function canEdit(member: Member) {
  return (
    canManageTeam.value &&
    (member.role !== "owner" || tenant.value?.role === "owner")
  );
}
onMounted(load);
onBeforeUnmount(() => {
  controller?.abort();
  invitationLink.value = "";
});
</script>
<template>
  <section class="page-heading">
    <div>
      <span class="eyebrow">PEOPLE & ACCESS</span>
      <h1>把合适的人，放在一起</h1>
      <p>管理 {{ tenant?.name }} 的成员与当前项目的独立授权。</p>
    </div>
    <button
      v-if="canManageTeam"
      class="button button-primary"
      @click="
        invitationLink = '';
        actionError = '';
        inviteOpen = true;
      "
    >
      <CloudIcon name="plus" :size="18" />邀请成员
    </button>
  </section>
  <div class="info-note compact">
    <CloudIcon name="shield" :size="19" />
    <p>
      团队成员不等于项目成员。团队所有者和管理员可管理全部项目，其他成员需要单独获得项目授权。
    </p>
  </div>
  <section class="panel">
    <div class="panel-heading">
      <div class="filter-tabs">
        <button :class="{ active: tab === 'team' }" @click="tab = 'team'">
          团队成员 <span>{{ members.length }}</span></button
        ><button
          :class="{ active: tab === 'project' }"
          :disabled="!state.projectId"
          @click="tab = 'project'"
        >
          项目授权 <span>{{ projectMembers.length }}</span></button
        ><button
          v-if="canManageTeam"
          :class="{ active: tab === 'invitations' }"
          @click="tab = 'invitations'"
        >
          有效邀请 <span>{{ invitations.length }}</span>
        </button>
      </div>
      <button
        v-if="tab === 'project' && canManageProject"
        class="button"
        @click="
          grantUser = '';
          actionError = '';
          grantOpen = true;
        "
      >
        <CloudIcon name="plus" :size="16" />添加项目成员
      </button>
    </div>
    <CloudState
      v-if="loading"
      kind="loading"
      title="正在读取成员权限"
    /><CloudState
      v-else-if="error"
      kind="error"
      title="成员信息加载失败"
      :description="error"
      @retry="load"
    />
    <template v-else-if="tab === 'team'"
      ><div v-if="members.length" class="table-wrap">
        <table class="data-table">
          <thead>
            <tr>
              <th>成员</th>
              <th>团队角色</th>
              <th>加入时间</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="member in members" :key="member.userId">
              <td>
                <div class="member-cell">
                  <span class="avatar">{{
                    member.displayName?.slice(0, 1) ?? "成"
                  }}</span>
                  <div>
                    <strong
                      >{{ member.displayName }}
                      <span
                        v-if="member.userId === state.session?.user?.id"
                        class="muted"
                        >（你）</span
                      ></strong
                    ><small>{{ member.userId }}</small>
                  </div>
                </div>
              </td>
              <td>
                <select
                  v-if="canEdit(member)"
                  class="inline-select"
                  :value="member.role"
                  :aria-label="`${member.displayName}的团队角色`"
                  @change="requestRole(member, $event)"
                >
                  <option v-if="tenant?.role === 'owner'" value="owner">
                    所有者
                  </option>
                  <option v-for="role in roleOptions" :key="role" :value="role">
                    {{ roleLabels[role] }}
                  </option></select
                ><span v-else class="badge neutral">{{
                  roleLabels[member.role]
                }}</span>
              </td>
              <td class="nowrap">{{ formatDate(member.joinedAt) }}</td>
              <td>
                <button
                  v-if="canEdit(member)"
                  class="text-link danger-text"
                  @click="remove(member, 'team')"
                >
                  移除</button
                ><span v-else class="muted">—</span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <CloudState v-else kind="empty" title="暂时没有团队成员"
    /></template>
    <template v-else-if="tab === 'project'"
      ><div class="section-caption">
        {{ project?.name }} · 以下为单独授予的项目角色
      </div>
      <div v-if="projectMembers.length" class="table-wrap">
        <table class="data-table">
          <thead>
            <tr>
              <th>成员</th>
              <th>项目角色</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="member in projectMembers" :key="member.userId">
              <td>
                <div class="member-cell">
                  <span class="avatar">{{
                    member.displayName?.slice(0, 1) ?? "成"
                  }}</span>
                  <div>
                    <strong>{{
                      member.displayName ||
                      members.find((item) => item.userId === member.userId)
                        ?.displayName ||
                      member.userId
                    }}</strong
                    ><small>{{ member.userId }}</small>
                  </div>
                </div>
              </td>
              <td>
                <span class="badge neutral">{{ roleLabels[member.role] }}</span>
              </td>
              <td>
                <div v-if="canManageProject" class="row">
                  <button
                    class="text-link"
                    @click="
                      grantUser = member.userId;
                      grantRole = member.role;
                      actionError = '';
                      grantOpen = true;
                    "
                  >
                    编辑</button
                  ><button
                    class="text-link danger-text"
                    @click="remove(member, 'project')"
                  >
                    移除
                  </button>
                </div>
                <span v-else class="muted">—</span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <CloudState
        v-else
        kind="empty"
        title="还没有单独授权的项目成员"
        description="团队所有者和管理员已有管理权限。你可以为其他团队成员授予项目角色。"
    /></template>
    <template v-else
      ><div v-if="invitations.length" class="table-wrap">
        <table class="data-table">
          <thead>
            <tr>
              <th>邀请</th>
              <th>授予角色</th>
              <th>到期时间</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="invite in invitations" :key="invite.id">
              <td>
                <code>{{ invite.id }}</code>
              </td>
              <td>{{ roleLabels[invite.role] }}</td>
              <td>{{ formatDate(invite.expiresAt) }}</td>
              <td>
                <button
                  class="text-link danger-text"
                  @click="
                    actionError = '';
                    confirmation = {
                      title: '撤销邀请',
                      description:
                        '撤销后，持有该链接的人将无法通过它加入团队。',
                      path: tenantPath(
                        `/invitations/${encodeURIComponent(invite.id)}`,
                      ),
                      method: 'DELETE',
                    };
                  "
                >
                  撤销
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <CloudState
        v-else
        kind="empty"
        title="暂无有效邀请"
        description="创建限时邀请链接后，自行分享给你信任的成员。"
    /></template>
  </section>
  <CloudModal
    v-model="inviteOpen"
    title="邀请新的协作者"
    description="邀请持有者可以加入当前团队。链接限时、仅可使用一次，请勿公开分享。"
    ><form
      v-if="!invitationLink"
      class="stack"
      @submit.prevent="createInvitation"
    >
      <label class="field"
        >团队角色<select v-model="inviteRole">
          <option v-for="role in roleOptions" :key="role" :value="role">
            {{ roleLabels[role] }}
          </option>
        </select></label
      >
      <p v-if="actionError" class="form-error" role="alert">
        {{ actionError }}
      </p>
      <div class="modal-actions">
        <button type="button" class="button" @click="inviteOpen = false">
          取消</button
        ><button class="button button-primary" :disabled="busy">
          {{ busy ? "正在创建…" : "生成邀请链接" }}
        </button>
      </div>
    </form>
    <div v-else class="stack">
      <div class="info-note compact">
        <CloudIcon name="check" />
        <p>邀请已创建。此链接只在本次显示，关闭前请妥善复制。</p>
      </div>
      <label class="field"
        >一次性邀请链接<input
          :value="invitationLink"
          readonly
          @focus="($event.target as HTMLInputElement).select()"
      /></label>
      <p class="muted">有效期至 {{ formatDate(invitationExpiry) }}</p>
      <p v-if="actionError" class="form-error" role="alert">
        {{ actionError }}
      </p>
      <div class="modal-actions">
        <button class="button button-primary" @click="copyInvitation">
          <CloudIcon name="copy" :size="16" />复制链接
        </button>
      </div>
    </div></CloudModal
  >
  <CloudModal
    v-model="grantOpen"
    title="授予项目访问权限"
    :description="`授权仅适用于 ${project?.name ?? '当前项目'}，不会改变团队角色。`"
    ><form class="stack" @submit.prevent="grant">
      <label class="field"
        >团队成员<select v-model="grantUser" required>
          <option value="" disabled>选择一名团队成员</option>
          <option
            v-for="member in members"
            :key="member.userId"
            :value="member.userId"
          >
            {{ member.displayName }} · {{ roleLabels[member.role] }}
          </option>
        </select></label
      ><label class="field"
        >项目角色<select v-model="grantRole">
          <option v-for="role in roleOptions" :key="role" :value="role">
            {{ roleLabels[role] }}
          </option>
        </select></label
      >
      <p v-if="actionError" class="form-error" role="alert">
        {{ actionError }}
      </p>
      <div class="modal-actions">
        <button type="button" class="button" @click="grantOpen = false">
          取消</button
        ><button class="button button-primary" :disabled="busy || !grantUser">
          {{ busy ? "正在保存…" : "保存授权" }}
        </button>
      </div>
    </form></CloudModal
  >
  <CloudModal
    v-model="confirmOpen"
    :title="confirmation?.title ?? '确认操作'"
    :description="confirmation?.description"
    ><p v-if="actionError" class="form-error" role="alert">{{ actionError }}</p>
    <div class="modal-actions">
      <button class="button" :disabled="busy" @click="confirmation = null">
        取消</button
      ><button class="button button-danger" :disabled="busy" @click="confirm">
        {{ busy ? "正在处理…" : "确认变更" }}
      </button>
    </div></CloudModal
  >
</template>
