<script setup lang="ts">
import {
  computed,
  nextTick,
  onBeforeUnmount,
  onMounted,
  reactive,
  ref,
} from "vue";
import { useApp } from "../shared";

type Room = {
  id: string;
  name: string;
  description: string;
  status: "open" | "closed";
  preventDuplicates: boolean;
  generation: number;
  nextRound: number;
  createdAt: string;
  totalUsers: number;
  currentWinners: number;
  totalRounds: number;
};
type Participant = {
  id: string;
  name: string;
  department: string;
  createdAt: string;
  participated: boolean;
};
type Winner = { id: string; name: string; department: string };
type Draw = {
  id: string;
  roomId: string;
  roomName: string;
  roundNumber: number;
  prizeName: string;
  preventDuplicates: boolean;
  createdAt: string;
  winners: Winner[];
};
type DrawRequest = {
  requestId: string;
  count: number;
  prizeName: string;
  preventDuplicates: boolean;
};
const app = useApp();
const rooms = ref<Room[]>([]),
  selectedID = ref(""),
  selected = ref<Room | null>(null),
  view = ref("rooms"),
  roomTab = ref("draw");
const loading = ref(false),
  busy = ref(false),
  error = ref(""),
  autoRefresh = ref(true),
  lastRefresh = ref(""),
  roomSearch = ref("");
const creating = ref(false),
  roomForm = reactive({
    name: "",
    description: "",
    status: "open",
    preventDuplicates: true,
  });
const participants = ref<Participant[]>([]),
  participantTotal = ref(0),
  available = ref(0),
  participantPage = ref(1),
  participantSize = ref(20),
  participantSearch = ref("");
const personForm = reactive({ name: "", department: "" }),
  batchForm = reactive({ count: 10, startFrom: 1 });
const invite = ref<{ signupURL: string; qrURL: string } | null>(null);
const count = ref(1),
  prize = ref(""),
  preventDuplicates = ref(true),
  drawing = ref(false),
  pendingDraw = ref<DrawRequest | null>(null),
  lastDraw = ref<Draw | null>(null),
  screenCleared = ref(false);
const pendingRecoveryFailed = ref(false);
const pendingByRoom = new Map<string, DrawRequest>();
function pendingKey(roomID: string) {
  return (
    "euler.lottery.pending:" +
    JSON.stringify([
      app.scope.actorId,
      app.scope.tenantId,
      app.scope.projectId,
      app.scope.installationId,
      roomID,
    ])
  );
}
function restorePending(roomID: string): DrawRequest | null {
  const cached = pendingByRoom.get(roomID);
  if (cached) return cached;
  const saved = sessionStorage.getItem(pendingKey(roomID));
  if (!saved) return null;
  const value = JSON.parse(saved) as DrawRequest;
  if (
    !value ||
    typeof value.requestId !== "string" ||
    value.requestId.length < 8 ||
    value.requestId.length > 100 ||
    !Number.isInteger(value.count) ||
    value.count < 1 ||
    value.count > 1000 ||
    typeof value.prizeName !== "string" ||
    value.prizeName.length > 200 ||
    typeof value.preventDuplicates !== "boolean"
  )
    throw new Error(
      "无法恢复未确认的抽奖请求，请先核对本场历史；暂不允许再次抽奖。",
    );
  pendingByRoom.set(roomID, value);
  return value;
}
function rememberPending(roomID: string, value: DrawRequest) {
  // Persist intent before sending. Results and ownership always come from the
  // server; this record merely lets reloads retry the exact outstanding request.
  try {
    sessionStorage.setItem(pendingKey(roomID), JSON.stringify(value));
  } catch {
    throw new Error("无法保存抽奖重试凭据，请允许此页面使用会话存储后再试。");
  }
  pendingByRoom.set(roomID, value);
}
function forgetPending(roomID: string, requestID?: string) {
  if (requestID && pendingByRoom.get(roomID)?.requestId !== requestID) return;
  pendingByRoom.delete(roomID);
  try {
    sessionStorage.removeItem(pendingKey(roomID));
  } catch {
    /* A stale intent can only replay the same server result. */
  }
}
const resultDialog = ref<HTMLDialogElement | null>(null),
  modalDraw = ref<Draw | null>(null),
  confirmDraw = ref(false);
const history = ref<Draw[]>([]),
  historyTotal = ref(0),
  historyPage = ref(1),
  historySize = ref(15),
  historySearch = ref(""),
  historyRoom = ref("");
const visibleRooms = computed(() =>
  rooms.value.filter((r) =>
    `${r.name} ${r.description}`
      .toLowerCase()
      .includes(roomSearch.value.toLowerCase()),
  ),
);
const canWrite = computed(() => app.can("write"));
const eligible = computed(() =>
  preventDuplicates.value ? available.value : (selected.value?.totalUsers ?? 0),
);
const date = (s?: string) => (s ? new Date(s).toLocaleString("zh-CN") : "—");
function fail(e: unknown) {
  error.value = e instanceof Error ? e.message : "操作失败，请稍后重试";
}
async function loadRooms() {
  loading.value = true;
  error.value = "";
  try {
    rooms.value = (await app.request<{ rooms: Room[] }>("/rooms")).rooms;
  } catch (e) {
    fail(e);
  } finally {
    loading.value = false;
  }
}
async function loadParticipants() {
  const id = selectedID.value;
  if (!id) return;
  try {
    const result = await app.request<{
      participants: Participant[];
      total: number;
      available: number;
    }>(
      `/rooms/${id}/participants?page=${participantPage.value}&pageSize=${participantSize.value}&search=${encodeURIComponent(participantSearch.value)}`,
    );
    if (selectedID.value === id) {
      participants.value = result.participants;
      participantTotal.value = result.total;
      available.value = result.available;
    }
  } catch (e) {
    fail(e);
  }
}
async function refreshRoom() {
  const id = selectedID.value;
  if (!id) return;
  try {
    const room = await app.request<Room>(`/rooms/${id}`);
    if (id !== selectedID.value) return;
    selected.value = room;
    const index = rooms.value.findIndex((r) => r.id === id);
    if (index >= 0) rooms.value[index] = room;
    lastRefresh.value = new Date().toISOString();
    await loadParticipants();
  } catch (e) {
    fail(e);
  }
}
async function loadInvitation() {
  const id = selectedID.value;
  if (!id || !canWrite.value) return;
  try {
    const value = await app.request<{ signupURL: string; qrURL: string }>(
      `/rooms/${id}/invitation`,
    );
    if (selectedID.value === id) invite.value = value;
  } catch (e) {
    fail(e);
  }
}
async function loadLastDraw() {
  const id = selectedID.value;
  if (!id) return;
  try {
    const value = await app.request<{ draws: Draw[] }>(
      `/rooms/${id}/draws?pageSize=1`,
    );
    if (selectedID.value === id) lastDraw.value = value.draws[0] ?? null;
  } catch (e) {
    fail(e);
  }
}
async function enter(room: Room) {
  selectedID.value = room.id;
  selected.value = room;
  error.value = "";
  participantPage.value = 1;
  participantSearch.value = "";
  invite.value = null;
  pendingDraw.value = null;
  pendingRecoveryFailed.value = false;
  screenCleared.value = false;
  lastDraw.value = null;
  roomTab.value = "draw";
  view.value = "rooms";
  preventDuplicates.value = room.preventDuplicates;
  try {
    pendingDraw.value = restorePending(room.id);
    if (pendingDraw.value) {
      count.value = pendingDraw.value.count;
      prize.value = pendingDraw.value.prizeName;
      preventDuplicates.value = pendingDraw.value.preventDuplicates;
    }
  } catch (e) {
    pendingRecoveryFailed.value = true;
    fail(e);
  }
  Object.assign(roomForm, {
    name: room.name,
    description: room.description,
    status: room.status,
    preventDuplicates: room.preventDuplicates,
  });
  await Promise.all([refreshRoom(), loadInvitation(), loadLastDraw()]);
}
function newRoom() {
  creating.value = true;
  Object.assign(roomForm, {
    name: "",
    description: "",
    status: "open",
    preventDuplicates: true,
  });
}
async function saveRoom(create = false) {
  busy.value = true;
  error.value = "";
  try {
    const value = await app.request<Room>(
      create ? "/rooms" : `/rooms/${selectedID.value}`,
      { method: create ? "POST" : "PATCH", body: { ...roomForm } },
    );
    creating.value = false;
    await loadRooms();
    await enter(value);
    app.notify(create ? "活动已创建" : "活动设置已保存", "success");
  } catch (e) {
    fail(e);
  } finally {
    busy.value = false;
  }
}
async function addPerson() {
  busy.value = true;
  error.value = "";
  try {
    await app.request(`/rooms/${selectedID.value}/participants`, {
      method: "POST",
      body: { ...personForm },
    });
    personForm.name = "";
    personForm.department = "";
    await refreshRoom();
    app.notify("参与者已添加", "success");
  } catch (e) {
    fail(e);
  } finally {
    busy.value = false;
  }
}
async function generate() {
  busy.value = true;
  error.value = "";
  try {
    const result = await app.request<{
      addedCount: number;
      skippedCount: number;
    }>(`/rooms/${selectedID.value}/participants/batch`, {
      method: "POST",
      body: { ...batchForm },
    });
    await refreshRoom();
    app.notify(
      `已添加 ${result.addedCount} 人，跳过 ${result.skippedCount} 个已有姓名`,
      "success",
    );
  } catch (e) {
    fail(e);
  } finally {
    busy.value = false;
  }
}
async function removePerson(person: Participant) {
  if (
    !window.confirm(
      `将“${person.name}”从后续抽奖名单移除？已有中奖记录会保留。`,
    )
  )
    return;
  busy.value = true;
  error.value = "";
  try {
    await app.request(`/rooms/${selectedID.value}/participants/${person.id}`, {
      method: "DELETE",
    });
    await refreshRoom();
    app.notify("参与者已移除", "success");
  } catch (e) {
    fail(e);
  } finally {
    busy.value = false;
  }
}
async function showResult(draw: Draw) {
  modalDraw.value = draw;
  await nextTick();
  resultDialog.value?.showModal();
}
function closeResult() {
  resultDialog.value?.close();
}
async function draw() {
  if (!selectedID.value || drawing.value || pendingRecoveryFailed.value) return;
  const roomID = selectedID.value,
    roomName = selected.value?.name ?? "活动";
  const retrying = pendingDraw.value !== null;
  confirmDraw.value = false;
  drawing.value = true;
  error.value = "";
  // A retry reuses both the exact request and key. A lost response must never
  // cause a second draw or silently change the requested prize/count.
  const request = pendingDraw.value ?? {
    requestId: crypto.randomUUID(),
    count: count.value,
    prizeName: prize.value,
    preventDuplicates: preventDuplicates.value,
  };
  try {
    rememberPending(roomID, request);
    pendingDraw.value = request;
    const value = await app.request<Draw>(`/rooms/${roomID}/draws`, {
      method: "POST",
      body: request,
    });
    forgetPending(roomID, request.requestId);
    if (selectedID.value === roomID) {
      pendingDraw.value = null;
      lastDraw.value = value;
      screenCleared.value = false;
      await showResult(value);
      if (selectedID.value === roomID) await refreshRoom();
    } else
      app.notify(`${roomName}的抽奖已完成，请进入该活动查看结果`, "success");
  } catch (e) {
    const status = (e as { status?: number })?.status;
    // Revoked permissions or a closed/changed room on a later retry do not
    // prove that the original request failed; retain that uncertain intent.
    if (
      !retrying &&
      status &&
      status < 500 &&
      status !== 408 &&
      status !== 429
    ) {
      forgetPending(roomID, request.requestId);
      if (selectedID.value === roomID) pendingDraw.value = null;
    }
    if (selectedID.value === roomID) fail(e);
    else app.notify(`${roomName}的抽奖结果未确认，请回到该活动重试`, "error");
  } finally {
    drawing.value = false;
  }
}
async function resetRoom() {
  if (
    !selected.value ||
    !window.confirm(
      `为“${selected.value.name}”开启新抽奖周期？当前周期中奖状态和列表将清空，参与者保留，旧周期记录保留用于审计。`,
    )
  )
    return;
  busy.value = true;
  error.value = "";
  const roomID = selectedID.value;
  try {
    await app.request(`/rooms/${roomID}/reset`, {
      method: "POST",
      body: { confirm: roomID },
    });
    forgetPending(roomID);
    if (selectedID.value === roomID) {
      pendingDraw.value = null;
      pendingRecoveryFailed.value = false;
      lastDraw.value = null;
      screenCleared.value = false;
      await refreshRoom();
    }
    app.notify("新周期已开启，所有在场参与者可再次中奖", "success");
  } catch (e) {
    fail(e);
  } finally {
    busy.value = false;
  }
}
async function rotateLink() {
  if (!window.confirm("重新生成报名链接后，旧链接和二维码立即失效。继续？"))
    return;
  busy.value = true;
  try {
    invite.value = await app.request(
      `/rooms/${selectedID.value}/invitation/rotate`,
      { method: "POST", body: {} },
    );
    app.notify("旧报名链接已撤销", "success");
  } catch (e) {
    fail(e);
  } finally {
    busy.value = false;
  }
}
async function copyLink() {
  if (!invite.value) return;
  try {
    await navigator.clipboard.writeText(invite.value.signupURL);
    app.notify("报名链接已复制", "success");
  } catch {
    app.notify("请手动复制报名链接", "error");
  }
}
async function loadHistory() {
  try {
    const value = await app.request<{ draws: Draw[]; total: number }>(
      `/history?page=${historyPage.value}&pageSize=${historySize.value}&roomId=${encodeURIComponent(historyRoom.value)}&search=${encodeURIComponent(historySearch.value)}`,
    );
    history.value = value.draws;
    historyTotal.value = value.total;
  } catch (e) {
    fail(e);
  }
}
function showHistory(roomID = "") {
  historyRoom.value = roomID;
  historyPage.value = 1;
  view.value = "history";
  void loadHistory();
}
function listView() {
  selectedID.value = "";
  selected.value = null;
  view.value = "rooms";
  void loadRooms();
}
function participantTurn(p: number) {
  participantPage.value = p;
  void loadParticipants();
}
function historyTurn(p: number) {
  historyPage.value = p;
  void loadHistory();
}
function settingsTab() {
  if (selected.value)
    Object.assign(roomForm, {
      name: selected.value.name,
      description: selected.value.description,
      status: selected.value.status,
      preventDuplicates: selected.value.preventDuplicates,
    });
  roomTab.value = "settings";
}
let timer: ReturnType<typeof setInterval> | undefined;
onMounted(() => {
  void loadRooms();
  timer = setInterval(() => {
    if (
      autoRefresh.value &&
      selectedID.value &&
      view.value === "rooms" &&
      !document.hidden &&
      !drawing.value
    )
      void refreshRoom();
  }, 5000);
});
onBeforeUnmount(() => clearInterval(timer));
</script>

<template>
  <section class="native-app lottery-app" aria-label="活动抽奖">
    <nav class="app-tabs" aria-label="抽奖导航">
      <button :class="{ active: view === 'rooms' }" @click="listView">
        活动房间</button
      ><button :class="{ active: view === 'history' }" @click="showHistory()">
        中奖历史
      </button>
    </nav>
    <div v-if="error" class="status-note error" role="alert">{{ error }}</div>

    <template v-if="view === 'rooms' && !selectedID">
      <div class="toolbar">
        <div>
          <h2>为每一场相聚，留一点惊喜</h2>
          <p class="muted">报名、名单与现场抽奖，在同一个工作台完成。</p>
        </div>
        <button v-if="canWrite" class="button primary" @click="newRoom">
          ＋ 新建活动
        </button>
      </div>
      <form v-if="creating" class="panel" @submit.prevent="saveRoom(true)">
        <h3>创建抽奖活动</h3>
        <div class="form-grid">
          <label class="field"
            >活动名称<input
              v-model="roomForm.name"
              required
              maxlength="100"
              placeholder="例如：年度聚会 · 幸运抽奖" /></label
          ><label class="field"
            >报名状态<select v-model="roomForm.status">
              <option value="open">开放报名与抽奖</option>
              <option value="closed">暂不开放</option>
            </select></label
          ><label class="field full"
            >活动说明<textarea
              v-model="roomForm.description"
              maxlength="1000"
              placeholder="向参与者介绍本次活动"
            />
          </label>
        </div>
        <div class="actions">
          <button class="button primary" :disabled="busy">
            {{ busy ? "创建中…" : "创建活动" }}</button
          ><button type="button" class="button" @click="creating = false">
            取消
          </button>
        </div>
      </form>
      <div class="toolbar">
        <label class="field search-field"
          >查找活动<input
            v-model="roomSearch"
            type="search"
            placeholder="搜索活动名称或说明" /></label
        ><button class="button" @click="loadRooms">刷新</button>
      </div>
      <div v-if="loading && !rooms.length" class="panel empty" role="status">
        正在加载活动…
      </div>
      <div v-else-if="!visibleRooms.length" class="panel empty">
        <div class="empty-gift">✦</div>
        <h3>
          {{ rooms.length ? "没有匹配的活动" : "第一场活动，从这里开始" }}
        </h3>
        <p>创建活动后即可获得报名二维码，也可手动添加或批量生成参与者。</p>
        <button
          v-if="canWrite && !rooms.length"
          class="button primary"
          @click="newRoom"
        >
          新建活动
        </button>
      </div>
      <div v-else class="room-grid">
        <article
          v-for="room in visibleRooms"
          :key="room.id"
          class="panel room-card"
        >
          <div class="toolbar">
            <span class="room-icon">✦</span
            ><span
              class="badge"
              :class="{ paused: room.status === 'closed' }"
              >{{ room.status === "open" ? "进行中" : "已暂停" }}</span
            >
          </div>
          <h3>{{ room.name }}</h3>
          <p class="room-description muted">
            {{ room.description || "一场属于大家的幸运时刻。" }}
          </p>
          <div class="room-counts">
            <span
              ><strong>{{ room.totalUsers }}</strong
              >参与者</span
            ><span
              ><strong>{{ room.currentWinners }}</strong
              >中奖次数</span
            ><span
              ><strong>{{ room.totalRounds }}</strong
              >轮抽奖</span
            >
          </div>
          <div class="toolbar room-footer">
            <small class="muted">{{ date(room.createdAt) }}</small
            ><button class="button" @click="enter(room)">进入活动 →</button>
          </div>
        </article>
      </div>
    </template>

    <template v-else-if="view === 'rooms' && selected">
      <div class="toolbar">
        <div>
          <button class="back" @click="listView">← 全部活动</button>
          <h2>
            {{ selected.name }}
            <span
              class="badge"
              :class="{ paused: selected.status === 'closed' }"
              >{{ selected.status === "open" ? "进行中" : "已暂停" }}</span
            >
          </h2>
          <p class="muted">{{ selected.description }}</p>
        </div>
        <div class="actions">
          <label class="muted"
            ><input v-model="autoRefresh" type="checkbox" /> 5 秒刷新</label
          ><button class="button" @click="refreshRoom">立即刷新</button
          ><button class="button" @click="showHistory(selectedID)">
            本场历史
          </button>
        </div>
      </div>
      <div class="metric-grid">
        <article class="metric">
          <span>当前参与者</span><strong>{{ selected.totalUsers }}</strong
          ><small>实时更新：{{ date(lastRefresh) }}</small>
        </article>
        <article class="metric pink">
          <span>本周期中奖次数</span
          ><strong>{{ selected.currentWinners }}</strong
          ><small>允许重复中奖时，同一人可计多次</small>
        </article>
        <article class="metric teal">
          <span>已完成轮次</span><strong>{{ selected.totalRounds }}</strong
          ><small>第 {{ selected.generation }} 个抽奖周期</small>
        </article>
      </div>
      <nav class="app-tabs" aria-label="活动工作台">
        <button
          :class="{ active: roomTab === 'draw' }"
          @click="roomTab = 'draw'"
        >
          现场抽奖</button
        ><button
          :class="{ active: roomTab === 'participants' }"
          @click="roomTab = 'participants'"
        >
          参与者</button
        ><button
          v-if="canWrite"
          :class="{ active: roomTab === 'signup' }"
          @click="roomTab = 'signup'"
        >
          报名二维码</button
        ><button
          v-if="canWrite"
          :class="{ active: roomTab === 'settings' }"
          @click="settingsTab"
        >
          活动设置
        </button>
      </nav>

      <div v-if="roomTab === 'draw'" class="draw-layout">
        <div class="panel draw-panel">
          <div class="eyebrow">LET THE LUCK BEGIN</div>
          <h3>下一份幸运，会是谁？</h3>
          <p class="muted">
            第 {{ selected.nextRound }} 轮 · 当前有 {{ eligible }} 人符合条件
          </p>
          <form
            v-if="canWrite"
            @submit.prevent="pendingDraw ? draw() : (confirmDraw = true)"
          >
            <label class="field"
              >奖品名称（可选）<input
                v-model="prize"
                maxlength="100"
                placeholder="例如：一等奖 · 年度惊喜"
                :disabled="drawing || !!pendingDraw" /></label
            ><label class="field"
              >本次中奖人数<input
                v-model.number="count"
                type="number"
                required
                min="1"
                :max="Math.min(1000, Math.max(1, eligible))"
                :disabled="drawing || !!pendingDraw" /></label
            ><label class="switch-label"
              ><input
                v-model="preventDuplicates"
                type="checkbox"
                :disabled="drawing || !!pendingDraw"
              />
              每人只能中奖一次</label
            >
            <p v-if="pendingDraw" class="status-note">
              上次请求结果尚未确认。重试会取回同一次抽奖，不会额外抽取。
            </p>
            <button
              class="draw-button"
              :disabled="
                drawing ||
                busy ||
                pendingRecoveryFailed ||
                (selected.status !== 'open' && !pendingDraw) ||
                (!eligible && !pendingDraw)
              "
            >
              {{
                drawing
                  ? "正在揭晓…"
                  : pendingDraw
                    ? "确认上次抽奖结果"
                    : "✦ 开始抽奖"
              }}
            </button>
          </form>
          <p v-else class="status-note">
            当前为只读权限，可查看名单和中奖结果。
          </p>
          <p v-if="selected.status === 'closed'" class="muted">
            活动已暂停，可在活动设置中重新开启。
          </p>
          <div v-if="confirmDraw" class="draw-confirm" role="alert">
            <h4>准备开始这一轮？</h4>
            <p>
              抽取 {{ count }} 人 · {{ prize || "未设置奖品" }}<br />{{
                preventDuplicates
                  ? "排除本周期已中奖者"
                  : "允许往轮中奖者再次参与"
              }}
            </p>
            <div class="actions">
              <button class="button primary" @click="draw">确认抽奖</button
              ><button class="button" @click="confirmDraw = false">
                再检查一下
              </button>
            </div>
          </div>
        </div>
        <div class="panel">
          <div class="toolbar">
            <h3>本次中奖名单</h3>
            <button
              v-if="lastDraw && !screenCleared"
              class="button"
              @click="screenCleared = true"
            >
              清空展示
            </button>
          </div>
          <div v-if="!lastDraw || screenCleared" class="empty">
            <div class="empty-gift">✧</div>
            <p>
              {{
                screenCleared
                  ? "现场展示已清空，历史记录仍保留。"
                  : "等待第一份幸运诞生。"
              }}
            </p>
            <button
              v-if="screenCleared"
              class="button"
              @click="screenCleared = false"
            >
              恢复展示
            </button>
          </div>
          <template v-else
            ><p class="muted">
              第 {{ lastDraw.roundNumber }} 轮 ·
              {{ lastDraw.prizeName || "幸运奖" }} ·
              {{ date(lastDraw.createdAt) }}
            </p>
            <div class="winner-list">
              <div
                v-for="winner in lastDraw.winners"
                :key="winner.id"
                class="winner-pill"
              >
                <span>✦</span><strong>{{ winner.name }}</strong
                ><small>{{ winner.department || "幸运参与者" }}</small>
              </div>
            </div>
            <button class="button" @click="showResult(lastDraw)">
              再次展示中奖结果
            </button></template
          >
        </div>
      </div>

      <template v-else-if="roomTab === 'participants'">
        <div v-if="canWrite" class="participant-forms">
          <form class="panel" @submit.prevent="addPerson">
            <h3>添加参与者</h3>
            <div class="form-grid">
              <label class="field"
                >姓名<input
                  v-model="personForm.name"
                  required
                  maxlength="100"
                  placeholder="参与者姓名" /></label
              ><label class="field"
                >部门（可选）<input
                  v-model="personForm.department"
                  maxlength="50"
                  placeholder="所属部门"
              /></label>
            </div>
            <button class="button primary" :disabled="busy">添加</button>
          </form>
          <form class="panel" @submit.prevent="generate">
            <h3>批量生成序号</h3>
            <div class="form-grid">
              <label class="field"
                >生成数量<input
                  v-model.number="batchForm.count"
                  type="number"
                  min="1"
                  max="1000"
                  required /></label
              ><label class="field"
                >起始号码<input
                  v-model.number="batchForm.startFrom"
                  type="number"
                  min="1"
                  max="10000000"
                  required
              /></label>
            </div>
            <div class="toolbar">
              <button class="button" :disabled="busy">生成序号</button
              ><small class="muted">生成“用户 N”，自动跳过重名。</small>
            </div>
          </form>
        </div>
        <div class="panel">
          <div class="toolbar">
            <h3>
              参与者名单 <span class="muted">{{ participantTotal }} 人</span>
            </h3>
            <div class="actions">
              <input
                v-model="participantSearch"
                type="search"
                placeholder="搜索姓名或部门"
                aria-label="搜索参与者"
                @change="
                  participantPage = 1;
                  loadParticipants();
                "
              /><select
                v-model.number="participantSize"
                aria-label="每页人数"
                @change="
                  participantPage = 1;
                  loadParticipants();
                "
              >
                <option
                  v-for="size in [10, 20, 50, 100]"
                  :key="size"
                  :value="size"
                >
                  每页 {{ size }} 人
                </option>
              </select>
            </div>
          </div>
          <div v-if="!participants.length" class="empty">
            暂无参与者，请扫码报名或手动添加。
          </div>
          <div v-else class="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>姓名</th>
                  <th>部门</th>
                  <th>报名时间</th>
                  <th>中奖状态</th>
                  <th v-if="canWrite">操作</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="person in participants" :key="person.id">
                  <td>{{ person.name }}</td>
                  <td>{{ person.department || "—" }}</td>
                  <td>{{ date(person.createdAt) }}</td>
                  <td>
                    <span
                      class="badge"
                      :class="{ paused: !person.participated }"
                      >{{ person.participated ? "已中奖" : "未中奖" }}</span
                    >
                  </td>
                  <td v-if="canWrite">
                    <button
                      class="text-danger"
                      :disabled="busy"
                      @click="removePerson(person)"
                    >
                      移除
                    </button>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <div class="pagination">
            <button
              class="button"
              :disabled="participantPage <= 1"
              @click="participantTurn(participantPage - 1)"
            >
              上一页</button
            ><span
              >{{ participantPage }} /
              {{
                Math.max(1, Math.ceil(participantTotal / participantSize))
              }}</span
            ><button
              class="button"
              :disabled="participantPage * participantSize >= participantTotal"
              @click="participantTurn(participantPage + 1)"
            >
              下一页
            </button>
          </div>
        </div>
      </template>

      <div
        v-else-if="roomTab === 'signup' && canWrite"
        class="panel signup-panel"
      >
        <div class="signup-copy">
          <div class="eyebrow">INVITE EVERYONE</div>
          <h3>扫码报名，一起等好运</h3>
          <p>
            参与者填写姓名和部门即可报名，无需进入管理控制台。链接只对本场活动有效。
          </p>
          <span
            class="badge"
            :class="{ paused: selected.status === 'closed' }"
            >{{
              selected.status === "open" ? "报名通道已开启" : "报名已暂停"
            }}</span
          ><label v-if="invite" class="field"
            >报名链接<input
              :value="invite.signupURL"
              readonly
              @focus="($event.target as HTMLInputElement).select()"
          /></label>
          <div class="actions">
            <button
              class="button primary"
              :disabled="!invite"
              @click="copyLink"
            >
              复制报名链接</button
            ><a
              v-if="invite"
              class="button"
              :href="invite.signupURL"
              target="_blank"
              rel="noopener noreferrer"
              >查看报名页 ↗</a
            >
          </div>
          <button
            v-if="app.can('manage')"
            class="text-danger rotate"
            :disabled="busy"
            @click="rotateLink"
          >
            撤销旧链接并重新生成
          </button>
        </div>
        <div class="qr-frame">
          <img
            v-if="invite"
            :src="invite.qrURL"
            alt="活动报名二维码"
            width="280"
            height="280"
          />
          <p v-else>正在生成二维码…</p>
          <strong>{{ selected.name }}</strong
          ><small>手机扫码即可报名</small>
        </div>
      </div>

      <template v-else-if="roomTab === 'settings' && canWrite"
        ><form class="panel" @submit.prevent="saveRoom(false)">
          <h3>活动设置</h3>
          <div class="form-grid">
            <label class="field"
              >活动名称<input
                v-model="roomForm.name"
                required
                maxlength="100" /></label
            ><label class="field"
              >活动状态<select v-model="roomForm.status">
                <option value="open">开放报名与抽奖</option>
                <option value="closed">暂停报名与抽奖</option>
              </select></label
            ><label class="field full"
              >活动说明<textarea
                v-model="roomForm.description"
                maxlength="1000"
              />
            </label>
          </div>
          <label class="switch-label"
            ><input v-model="roomForm.preventDuplicates" type="checkbox" />
            默认每人只能中奖一次</label
          ><button class="button primary" :disabled="busy">保存设置</button>
        </form>
        <article v-if="app.can('manage')" class="panel reset-panel">
          <h3>开启新的抽奖周期</h3>
          <p class="muted">
            保留当前参与者，清空当前周期的中奖状态和显示列表。旧记录保留用于审计；此操作只影响本场活动。
          </p>
          <button
            class="button danger"
            :disabled="busy || drawing"
            @click="resetRoom"
          >
            确认并重置本场活动
          </button>
        </article></template
      >
    </template>

    <template v-else-if="view === 'history'">
      <div class="toolbar">
        <div>
          <h2>每一份幸运，都有记录</h2>
          <p class="muted">
            按活动与轮次查看本项目当前抽奖周期的完整中奖历史。
          </p>
        </div>
        <button class="button" @click="loadHistory">刷新记录</button>
      </div>
      <div class="panel">
        <div class="toolbar">
          <div class="actions">
            <select
              v-model="historyRoom"
              aria-label="活动筛选"
              @change="
                historyPage = 1;
                loadHistory();
              "
            >
              <option value="">全部活动</option>
              <option v-for="room in rooms" :key="room.id" :value="room.id">
                {{ room.name }}
              </option></select
            ><input
              v-model="historySearch"
              type="search"
              placeholder="搜索活动、奖品、姓名或部门"
              aria-label="搜索中奖历史"
              @change="
                historyPage = 1;
                loadHistory();
              "
            />
          </div>
          <span class="muted">共 {{ historyTotal }} 轮</span>
        </div>
        <div v-if="!history.length" class="empty">
          还没有符合条件的中奖记录。
        </div>
        <div v-else class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>活动 / 轮次</th>
                <th>奖品</th>
                <th>中奖名单</th>
                <th>时间</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="record in history" :key="record.id">
                <td>
                  <strong>{{ record.roomName }}</strong>
                  <p class="muted">
                    第 {{ record.roundNumber }} 轮 ·
                    {{ record.winners.length }} 人
                  </p>
                </td>
                <td>{{ record.prizeName || "幸运奖" }}</td>
                <td>
                  <div class="history-winners">
                    <span
                      v-for="winner in record.winners"
                      :key="winner.id"
                      class="badge"
                      >{{ winner.name
                      }}{{
                        winner.department ? ` · ${winner.department}` : ""
                      }}</span
                    >
                  </div>
                </td>
                <td>{{ date(record.createdAt) }}</td>
                <td>
                  <button class="button" @click="showResult(record)">
                    展示
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <div class="pagination">
          <select
            v-model.number="historySize"
            aria-label="每页轮数"
            @change="
              historyPage = 1;
              loadHistory();
            "
          >
            <option v-for="size in [15, 30, 50, 100]" :key="size" :value="size">
              {{ size }} 轮 / 页
            </option></select
          ><button
            class="button"
            :disabled="historyPage <= 1"
            @click="historyTurn(historyPage - 1)"
          >
            上一页</button
          ><span
            >{{ historyPage }} /
            {{ Math.max(1, Math.ceil(historyTotal / historySize)) }}</span
          ><button
            class="button"
            :disabled="historyPage * historySize >= historyTotal"
            @click="historyTurn(historyPage + 1)"
          >
            下一页
          </button>
        </div>
      </div>
    </template>

    <dialog
      ref="resultDialog"
      class="result-dialog"
      aria-labelledby="winner-title"
    >
      <div v-if="modalDraw" :key="modalDraw.id" class="celebration">
        <div class="confetti" aria-hidden="true">
          <i v-for="n in 24" :key="n" :style="{ '--i': n }">✦</i>
        </div>
        <button
          class="close-result"
          aria-label="关闭中奖展示"
          @click="closeResult"
        >
          ×
        </button>
        <div class="eyebrow">CONGRATULATIONS</div>
        <h2 id="winner-title">幸运，属于你们</h2>
        <p>{{ modalDraw.roomName }} · 第 {{ modalDraw.roundNumber }} 轮</p>
        <div class="prize-name">{{ modalDraw.prizeName || "幸运奖" }}</div>
        <div class="winner-list">
          <div
            v-for="winner in modalDraw.winners"
            :key="winner.id"
            class="winner-pill"
          >
            <span>✦</span><strong>{{ winner.name }}</strong
            ><small>{{ winner.department || "恭喜中奖" }}</small>
          </div>
        </div>
        <button class="button primary" @click="closeResult">
          收下这份幸运
        </button>
      </div>
    </dialog>
  </section>
</template>

<style scoped>
.lottery-app .panel {
  padding: 24px;
}
h2,
h3 {
  margin: 0 0 10px;
}
h2 {
  font-size: 23px;
}
.muted {
  color: var(--muted);
  font-size: 13px;
  line-height: 1.75;
}
.search-field {
  min-width: 280px;
}
.room-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(290px, 1fr));
  gap: 20px;
}
.room-card {
  border-top: 3px solid #b17be7;
}
.room-icon {
  font-size: 28px;
  color: #a86cda;
  background: #f6ecfc;
  width: 48px;
  height: 48px;
  text-align: center;
  line-height: 48px;
  border-radius: 14px;
}
.room-card h3 {
  margin: 22px 0 8px;
}
.room-description {
  min-height: 42px;
}
.room-counts {
  display: flex;
  justify-content: space-between;
  gap: 10px;
  margin: 22px 0;
}
.room-counts span {
  display: grid;
  gap: 8px;
  font-size: 11px;
  color: var(--muted);
}
.room-counts strong {
  font-size: 24px;
  color: var(--text);
  font-weight: 650;
}
.room-footer {
  border-top: 1px solid var(--border);
  padding-top: 16px;
}
.badge.paused {
  background: #fff2d9;
  color: #98671b;
}
.empty-gift {
  font-size: 50px;
  color: #b17be7;
}
.back {
  padding: 0;
  margin-bottom: 14px;
  border: 0;
  background: transparent;
  color: var(--muted);
  cursor: pointer;
  font: inherit;
  font-size: 12px;
}
.metric-grid {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 16px;
}
.metric {
  padding: 22px 24px;
  border: 1px solid var(--border);
  border-radius: 14px;
  background: var(--surface, #fff);
  display: grid;
  gap: 12px;
  color: #7854ce;
}
.metric span {
  font-size: 12px;
  color: var(--muted);
}
.metric strong {
  font-size: 34px;
  letter-spacing: -1px;
}
.metric small {
  font-size: 11px;
  color: var(--muted);
}
.metric.pink {
  color: #ce548b;
}
.metric.teal {
  color: #199589;
}
.draw-layout {
  display: grid;
  grid-template-columns: minmax(280px, 0.9fr) minmax(300px, 1.1fr);
  gap: 20px;
}
.draw-panel {
  background: linear-gradient(130deg, #faf6ff, var(--surface, #fff));
  position: relative;
}
.eyebrow {
  font-size: 10px;
  color: #9c65b8;
  letter-spacing: 2px;
  font-weight: 700;
  margin: 4px 0 20px;
}
.draw-panel h3 {
  font-size: 23px;
}
.draw-panel .field {
  margin: 18px 0;
}
.draw-button {
  border: 0;
  border-radius: 12px;
  padding: 17px 24px;
  width: 100%;
  margin-top: 22px;
  background: linear-gradient(100deg, #8659c8, #c65b9a);
  color: #fff;
  font: inherit;
  font-size: 18px;
  font-weight: 600;
  cursor: pointer;
  box-shadow: 0 8px 20px #9955bb22;
}
.draw-button:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
.switch-label {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 13px;
  margin: 18px 0 24px;
}
.draw-confirm {
  margin-top: 20px;
  padding: 18px;
  border: 1px solid #bf91d9;
  border-radius: 12px;
  background: var(--surface, #fff);
  font-size: 13px;
}
.draw-confirm h4 {
  margin: 0;
}
.winner-list {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
  margin: 24px 0;
  max-height: 440px;
  overflow: auto;
}
.winner-pill {
  display: grid;
  gap: 8px;
  text-align: center;
  border: 1px solid #eadcf4;
  background: #fcf8ff;
  border-radius: 14px;
  padding: 20px;
  min-width: 120px;
  flex: 1;
  max-width: 210px;
  color: #744798;
  overflow-wrap: anywhere;
}
.winner-pill span {
  font-size: 24px;
  color: #c2995e;
}
.winner-pill strong {
  font-size: 20px;
}
.winner-pill small {
  font-size: 11px;
  color: #937fa2;
}
.participant-forms {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 20px;
}
.participant-forms .button {
  margin-top: 18px;
}
.text-danger {
  color: #bc4b69;
  background: none;
  border: 0;
  font: inherit;
  font-size: 12px;
  cursor: pointer;
  padding: 6px;
}
.signup-panel {
  display: grid;
  grid-template-columns: 1fr 320px;
  align-items: center;
  gap: 40px;
}
.signup-copy p {
  font-size: 14px;
  line-height: 1.9;
  max-width: 500px;
}
.signup-copy .field {
  margin: 25px 0 16px;
}
.qr-frame {
  text-align: center;
  display: grid;
  justify-items: center;
  gap: 10px;
  background: #fff;
  border: 1px solid #e8dff1;
  border-radius: 20px;
  padding: 20px;
  color: #463c57;
}
.qr-frame img {
  max-width: 100%;
  height: auto;
}
.qr-frame small {
  color: #938799;
  font-size: 12px;
}
.rotate {
  margin-top: 22px;
}
.reset-panel {
  border: 1px solid #e4bbc5;
}
.error {
  color: #b52c4b;
  background: #fff1f3;
}
.history-winners {
  display: flex;
  flex-wrap: wrap;
  gap: 7px;
  max-width: 460px;
}
.result-dialog {
  padding: 0;
  border: 1px solid #ead8f6;
  border-radius: 24px;
  max-width: 820px;
  width: calc(100% - 32px);
  max-height: 92vh;
  color: #49345c;
  background: #fff9ff;
}
.result-dialog::backdrop {
  background: #221331b0;
  backdrop-filter: blur(5px);
}
.celebration {
  padding: 50px 40px;
  text-align: center;
  position: relative;
  overflow: hidden;
}
.celebration h2 {
  font-size: 36px;
  letter-spacing: 2px;
  margin: 20px 0;
}
.celebration > p {
  font-size: 13px;
  color: #927d9f;
}
.celebration .winner-list {
  justify-content: center;
  position: relative;
  z-index: 1;
}
.celebration .winner-pill {
  animation: reveal 0.6s ease-out;
  background: #fff;
  box-shadow: 0 10px 20px #aa66bb0c;
}
.prize-name {
  font-size: 22px;
  font-weight: 600;
  color: #be8b41;
  margin: 24px 0;
}
.close-result {
  position: absolute;
  right: 18px;
  top: 12px;
  background: none;
  border: 0;
  font-size: 30px;
  color: #927d9f;
  cursor: pointer;
  z-index: 2;
}
.confetti {
  position: absolute;
  inset: 0;
  pointer-events: none;
  overflow: hidden;
}
.confetti i {
  position: absolute;
  left: calc(var(--i) * 4%);
  top: -40px;
  font-style: normal;
  color: #d795c5;
  animation: fall 3s ease-out both;
  animation-delay: calc(var(--i) * 0.045s);
}
.confetti i:nth-child(3n) {
  color: #d2ad63;
}
.confetti i:nth-child(2n) {
  color: #b5a0e5;
}
.actions {
  margin-top: 4px;
}
select,
input[type="search"] {
  font: inherit;
  padding: 9px 12px;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--surface, #fff);
  color: inherit;
}
.form-grid {
  margin: 18px 0;
}
.status-note {
  font-size: 13px;
}
@keyframes reveal {
  from {
    opacity: 0;
    transform: translateY(18px) scale(0.95);
  }
  to {
    opacity: 1;
    transform: none;
  }
}
@keyframes fall {
  to {
    transform: translateY(520px) rotate(190deg);
    opacity: 0;
  }
}
@media (max-width: 900px) {
  .draw-layout,
  .participant-forms {
    grid-template-columns: 1fr;
  }
  .signup-panel {
    grid-template-columns: 1fr;
  }
  .qr-frame {
    justify-self: center;
  }
  .metric-grid {
    grid-template-columns: 1fr 1fr;
  }
  .metric:last-child {
    grid-column: 1/-1;
  }
}
@media (max-width: 560px) {
  .lottery-app .panel {
    padding: 18px;
  }
  .metric-grid {
    grid-template-columns: 1fr;
  }
  .metric:last-child {
    grid-column: auto;
  }
  .search-field {
    min-width: 0;
    width: 100%;
  }
  .celebration {
    padding: 40px 18px;
  }
  .celebration h2 {
    font-size: 27px;
  }
  .winner-pill {
    padding: 15px;
    min-width: 100px;
  }
  .draw-layout {
    display: block;
  }
  .draw-layout > .panel {
    margin-bottom: 20px;
  }
}
@media (prefers-reduced-motion: reduce) {
  .confetti {
    display: none;
  }
  .celebration .winner-pill {
    animation: none;
  }
}
@media (prefers-color-scheme: dark) {
  .draw-panel {
    background: var(--surface, #221e2b);
  }
  .error {
    background: #4a2030;
  }
  .badge.paused {
    background: #49371a;
  }
}
</style>
