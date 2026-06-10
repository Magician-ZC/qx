/* 文件说明：前端会话 API 客户端，封装 HTTP/WS 请求、鉴权头注入与流式订阅回调。 */

import type {
  AccountLoginResult,
  AccountUser,
  AuditBundle,
  BillingCharge,
  BillingQuota,
  BillingSKU,
  BloodFeudEntry,
  ComplianceGate,
  ConsentRequest,
  CostDashboardData,
  DialogueMessage,
  BattleUnit,
  DuelRoomStatus,
  DungeonResult,
  EncounterAward,
  Entitlement,
  ExperimentFunnelReport,
  FieldBossResult,
  LeadEvent,
  LeadsFunnelData,
  LLMInteraction,
  ModerationReport,
  NorthStarReport,
  PrivacyEraseOptions,
  PrivacyEraseResult,
  PrivacyPurgeResult,
  ProductFunnelReport,
  SessionLog,
  SessionSnapshot,
  TerrainDefinition,
  WorldBossStrikeResult,
} from "./types";

// 这些类型在 types.ts 定义（单一真相源，严格对齐后端字段），并从 api 模块再导出，
// 让消费方可统一从 './session/api' 取 wrapper 与其返回/载荷类型（World Boss / Ops 看板 / 血仇）。
export type { WorldBossStrikeResult, CostDashboardData, LeadsFunnelData, BloodFeudEntry } from "./types";
// 副本（dungeon）与 Ops 三报表（产品漏斗 / 北极星 / A/B 实验）的返回类型，供面板消费方统一从 './session/api' 取。
export type {
  DungeonResult,
  DungeonFloorResult,
  ProductFunnelReport,
  NorthStarReport,
  ExperimentFunnelReport,
} from "./types";

const API_BASE =
  import.meta.env.VITE_API_BASE_URL ??
  (import.meta.env.DEV ? "http://127.0.0.1:8080" : window.location.origin);
const developerModeStorageKey = "qunxiang.developer.mode.v1";
const accountTokenStorageKey = "qunxiang.account.token.v1";
let sessionRoleToken = "";
// opsToken 是运营态鉴权令牌（X-Ops-Token）：治理/审计/隐私/同意等 ops 端点携带。
// 原型默认无 token，后端对缺头放行；运营态经 setOpsToken 注入后这些端点才带头。
let opsToken = "";
// accountToken 是账户登录令牌（Authorization: Bearer）。billing/compliance 端点强制；
// 建局/advance-phase 软带（已登录才带，让后端归账+合规门控）。从 localStorage 恢复以跨刷新保活。
let accountToken = "";
try {
  accountToken = window.localStorage.getItem(accountTokenStorageKey) ?? "";
} catch {
  accountToken = "";
}
export type DirectiveScope = "doctrine" | "task" | "order";

type SessionStreamHandlers = {
  onSnapshot?: (snapshot: SessionSnapshot, meta: Record<string, unknown>) => void;
  onLog?: (entry: SessionLog) => void;
  onError?: (error: unknown) => void;
  // onFateInbox 收到 fate_inbox 推送的原始 payload（整体透传，不裁字段）。
  // 后端 WS payload 只含 unit_id/route/decision_id/narrative/relevance——**不含 expires_at/countdown_hours/occurred_at**。
  // 故倒计时不能由 WS 直接取：收到推送后应调 getFateFeed(unitID) 拉最新 feed（pending 卡才带 expires_at/countdown_hours）。
  onFateInbox?: (payload: Record<string, unknown>) => void;
  onFateEcho?: (payload: Record<string, unknown>) => void;
  // onFateLifeBeat 收到 fate_life_beat 推送（后端 fate_world.go：世界推进一拍后她的日常经历）。
  // payload 只含 unit_id/narrative/turn（无 decision_id/倒计时）——直接 append 成一条 life_beat 卡，免轮询更跟手。
  onFateLifeBeat?: (payload: Record<string, unknown>) => void;
  // onLlmInteraction 收到 llm_interaction 推送（后端推的是裸 interaction 对象，二层解包后即 LLMInteraction）。
  onLlmInteraction?: (interaction: LLMInteraction) => void;
};

// FateCard 是命运四槽界面的一张卡（高光/待决策/回响）。
// expires_at/countdown_hours 仅 pending 卡随 feed 返回，供前端渲染倒计时；
// 若来源（如 WS fate_inbox）只给 occurred_at，前端可按 occurred_at + 48h 自算。
export type FateCard = {
  // life_beat：世界推进一拍后她的日常经历（区别于值得一看的 highlight / 需拿主意的 pending / 回响 echo），
  // 低调叙事体渲染在「她近来经历的」时间线。后端 fate.go 的 ReasonLifeBeat → kind:"life_beat"，并随 WS fate_life_beat 推送。
  kind: "highlight" | "pending" | "echo" | "life_beat";
  decision_id?: string;
  narrative: string;
  occurred_at?: string;
  expires_at?: string;
  countdown_hours?: number;
  // choices 仅 pending 卡随 feed 返回（后端 buildFateChoices，omitempty）：玩家可选的处理分支，
  // resolve_class 即传给 resolveFateDecision 的 resolveType。
  choices?: { id: string; label: string; resolve_class: string }[];
};

// EliteAward 是一次 elite/PvE 遭遇分到的一件战利品（与后端 encounter.Award 对齐，Go 默认大写键名）。
// 与 types.ts 的 EncounterAward 同构，保留旧名作别名供既有引用沿用。
export type EliteAward = EncounterAward;

// EliteEncounterResult 与后端 session.EliteEncounterResult 对齐（无 json tag，键名为 Go 字段名）。
export type EliteEncounterResult = {
  ThreatID: string;
  Outcome: string; // defeated / fled / down
  Rounds: number;
  DamageDealt: number;
  DamageTaken: number;
  Contribution: number;
  Awards: EliteAward[] | null;
  PenaltyLayer: number; // 失败时实际落地的后果层（0=未触发）
  InboxCard: string; // 祖魂语气的命运收件箱卡
};

export class APIError extends Error {
  session?: SessionSnapshot;
  // HTTP 状态码（如 403 合规拦截 / 401 未授权），供上层精准分支。
  status?: number;
  // 合规门 403 时后端给出的 reason（宵禁/未实名/防沉迷超限），不被吞掉，供上层提示玩家。
  reason?: string;

  constructor(message: string, session?: SessionSnapshot, status?: number, reason?: string) {
    super(message);
    this.name = "APIError";
    this.session = session;
    this.status = status;
    this.reason = reason;
  }
}

function developerDebugEnabled(): boolean {
  try {
    return window.localStorage.getItem(developerModeStorageKey) === "1";
  } catch {
    return false;
  }
}

export type BattleMapSizeID = "small" | "medium" | "large";

// RequestAuthOptions 控制本次请求的鉴权头注入策略（在 RequestInit 之外）。
type RequestAuthOptions = {
  // withOps=true 时携带 X-Ops-Token（取模块级 opsToken，空则不带——后端原型放行）。
  withOps?: boolean;
  // bearer="require" 强制带 Authorization: Bearer（无 token 仍发请求，由后端 401）；
  // bearer="soft" 仅在已登录（accountToken 非空）时带；不传/为 false 时不带。
  bearer?: "require" | "soft";
};

async function request<T>(path: string, init?: RequestInit, auth?: RequestAuthOptions): Promise<T> {
  const headers = new Headers(init?.headers ?? {});
  if (sessionRoleToken.trim() !== "") {
    headers.set("X-Session-Role-Token", sessionRoleToken.trim());
  }
  if (developerDebugEnabled()) {
    headers.set("X-Qunxiang-Debug", "1");
  }
  if (auth?.withOps && opsToken.trim() !== "") {
    headers.set("X-Ops-Token", opsToken.trim());
  }
  // Authorization: Bearer 注入——require 总带（即便空，让后端给 401）；soft 仅已登录才带。
  // 若调用方已在 init.headers 自带 Authorization（如账户 me/logout 显式传 token），不覆盖。
  if (auth?.bearer && !headers.has("Authorization")) {
    if (auth.bearer === "require" || (auth.bearer === "soft" && accountToken.trim() !== "")) {
      headers.set("Authorization", `Bearer ${accountToken.trim()}`);
    }
  }
  let response: Response;
  try {
    response = await fetch(`${API_BASE}${path}`, {
      ...init,
      headers,
    });
  } catch (error) {
    throw new APIError(
      `无法连接后端服务（${API_BASE}）。请确认 backend 已启动并监听 8080 端口。原始错误：${
        error instanceof Error ? error.message : String(error)
      }`,
    );
  }
  const text = await response.text();

  let payload: unknown = null;
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch {
      payload = text;
    }
  }

  if (!response.ok) {
    if (payload && typeof payload === "object") {
      const data = payload as { error?: string; reason?: string; session?: SessionSnapshot };
      // 合规门 403 会带 {error, reason}——reason 透出给上层，绝不吞掉。
      throw new APIError(
        data.error ?? `${response.status} ${response.statusText}`,
        data.session,
        response.status,
        typeof data.reason === "string" ? data.reason : undefined,
      );
    }
    throw new APIError(
      typeof payload === "string" && payload.trim() ? payload : `${response.status} ${response.statusText}`,
      undefined,
      response.status,
    );
  }

  return payload as T;
}

async function unwrapSession(
  path: string,
  init?: RequestInit,
  auth?: RequestAuthOptions,
): Promise<SessionSnapshot> {
  const response = await request<{ session: SessionSnapshot }>(path, init, auth);
  return response.session;
}

// createSinglePlayerSession 请求后端创建单人会话。
export function createSinglePlayerSession(seed = Date.now(), unitCount = 5, mapSize: BattleMapSizeID = "small", fogOfWarEnabled = false, randomEventsEnabled = false): Promise<SessionSnapshot> {
  // 软带 Bearer：已登录则后端归账+合规门控（未实名/宵禁/超限→403 {error,reason}）；未登录匿名放行。
  return unwrapSession(`/api/sessions/single-player?seed=${seed}&unit_count=${unitCount}&map_size=${encodeURIComponent(mapSize)}&fog_of_war=${fogOfWarEnabled ? "true" : "false"}&random_events=${randomEventsEnabled ? "true" : "false"}`, {
	method: "POST",
  }, { bearer: "soft" });
}

export async function createDuelSession(seed = Date.now(), unitCount = 5, mapSize: BattleMapSizeID = "small", fogOfWarEnabled = false, randomEventsEnabled = false, creatorRole: "player" | "enemy" = "player"): Promise<{
  session: SessionSnapshot;
  mode: string;
  room_code: string;
  player_role_token: string;
  enemy_role_token: string;
  commander_faction_id: string;
  room_status?: DuelRoomStatus;
}> {
  return request<{
    session: SessionSnapshot;
    mode: string;
    room_code: string;
    player_role_token: string;
    enemy_role_token: string;
    commander_faction_id: string;
    room_status?: DuelRoomStatus;
}>(`/api/sessions/duel?seed=${seed}&unit_count=${unitCount}&map_size=${encodeURIComponent(mapSize)}&fog_of_war=${fogOfWarEnabled ? "true" : "false"}&random_events=${randomEventsEnabled ? "true" : "false"}&creator_role=${creatorRole}`, { method: "POST" }, { bearer: "soft" });
}

export async function getSession(sessionID: string): Promise<{
  session: SessionSnapshot;
  room_code?: string;
  commander_faction_id?: string;
  room_status?: DuelRoomStatus;
}> {
  return request<{ session: SessionSnapshot; room_code?: string; commander_faction_id?: string; room_status?: DuelRoomStatus }>(
    `/api/sessions/${encodeURIComponent(sessionID)}`,
  );
}

export async function joinDuelByRoomCode(roomCode: string): Promise<{
  session: SessionSnapshot;
  mode: string;
  room_code: string;
  role: "player" | "enemy";
  role_token: string;
  commander_faction_id: string;
  room_status?: DuelRoomStatus;
}> {
  return joinDuelByRoomCodeWithRole(roomCode);
}

export async function joinDuelByRoomCodeWithRole(
  roomCode: string,
  preferredRole?: "player" | "enemy",
): Promise<{
  session: SessionSnapshot;
  mode: string;
  room_code: string;
  role: "player" | "enemy";
  role_token: string;
  commander_faction_id: string;
  room_status?: DuelRoomStatus;
}> {
  return request<{
    session: SessionSnapshot;
    mode: string;
    room_code: string;
    role: "player" | "enemy";
    role_token: string;
    commander_faction_id: string;
    room_status?: DuelRoomStatus;
  }>(`/api/sessions/duel/join`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      room_code: roomCode,
      preferred_role: preferredRole,
    }),
  });
}

// setSessionRoleToken 设置当前会话角色令牌（用于双人房鉴权）。
export function setSessionRoleToken(token: string): void {
  sessionRoleToken = token.trim();
}

// getSessionRoleToken 读取当前会话角色令牌。
export function getSessionRoleToken(): string {
  return sessionRoleToken;
}

// setOpsToken 设置运营态鉴权令牌（X-Ops-Token）。传空清除（回到原型放行态）。
export function setOpsToken(token: string): void {
  opsToken = token.trim();
}

// getOpsToken 读取当前运营态鉴权令牌。
export function getOpsToken(): string {
  return opsToken;
}

// setAccountToken 设置账户登录令牌（Authorization: Bearer），并同步到 localStorage 跨刷新保活。传空清除（登出）。
export function setAccountToken(token: string): void {
  accountToken = token.trim();
  try {
    if (accountToken === "") {
      window.localStorage.removeItem(accountTokenStorageKey);
    } else {
      window.localStorage.setItem(accountTokenStorageKey, accountToken);
    }
  } catch {
    // localStorage 不可用（隐私模式等）时忽略——内存态仍生效。
  }
}

// getAccountToken 读取当前账户登录令牌（已登录非空）。
export function getAccountToken(): string {
  return accountToken;
}

// websocketURL 把 API 地址转换为 WS 订阅地址。
function websocketURL(): string {
  const endpoint = new URL(API_BASE);
  endpoint.protocol = endpoint.protocol === "https:" ? "wss:" : "ws:";
  endpoint.pathname = "/ws";
  endpoint.search = "";
  endpoint.hash = "";
  return endpoint.toString();
}

// asRecord 把 unknown 安全收窄为对象记录类型。
function asRecord(value: unknown): Record<string, unknown> | null {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return null;
  }
  return value as Record<string, unknown>;
}

// subscribeSessionStream 建立会话流订阅，并在断线后自动重连。
export function subscribeSessionStream(sessionID: string, handlers: SessionStreamHandlers): () => void {
  const targetSessionID = sessionID.trim();
  if (!targetSessionID) {
    return () => undefined;
  }

  let socket: WebSocket | null = null;
  let reconnectTimer: number | undefined;
  let heartbeatTimer: number | undefined;
  let closed = false;

  const stopHeartbeat = () => {
    if (heartbeatTimer !== undefined) {
      window.clearInterval(heartbeatTimer);
      heartbeatTimer = undefined;
    }
  };

  const startHeartbeat = (ws: WebSocket) => {
    stopHeartbeat();
    heartbeatTimer = window.setInterval(() => {
      if (ws.readyState !== WebSocket.OPEN) {
        return;
      }
      ws.send(
        JSON.stringify({
          type: "ping",
          payload: { session_id: targetSessionID },
        }),
      );
    }, 60_000);
  };

  const connect = () => {
    if (closed) {
      return;
    }
    const ws = new WebSocket(websocketURL());
    socket = ws;

    ws.onopen = () => {
      ws.send(
        JSON.stringify({
          type: "session_subscribe",
          payload: { session_id: targetSessionID },
        }),
      );
      startHeartbeat(ws);
    };

    ws.onmessage = (event) => {
      let envelope: unknown;
      try {
        envelope = JSON.parse(String(event.data ?? ""));
      } catch {
        return;
      }
      const root = asRecord(envelope);
      if (!root) {
        return;
      }
      const type = typeof root.type === "string" ? root.type : "";
      const wrapped = asRecord(root.payload);
      if (!wrapped) {
        return;
      }
      if (wrapped.session_id !== targetSessionID) {
        return;
      }
      const payload = asRecord(wrapped.payload);
      if (!payload) {
        return;
      }

      if (type === "session_snapshot") {
        const session = asRecord(payload.session);
        if (!session) {
          return;
        }
        handlers.onSnapshot?.(session as SessionSnapshot, payload);
        return;
      }
      if (type === "session_log") {
        handlers.onLog?.(payload as SessionLog);
        return;
      }
      if (type === "fate_inbox") {
        handlers.onFateInbox?.(payload);
        return;
      }
      if (type === "llm_interaction") {
        // 后端推的是裸 interaction 对象，payload 已二层解包即 LLMInteraction。
        handlers.onLlmInteraction?.(payload as LLMInteraction);
        return;
      }
      if (type === "fate_echo") {
        handlers.onFateEcho?.(payload);
        return;
      }
      if (type === "fate_life_beat") {
        handlers.onFateLifeBeat?.(payload);
      }
    };

    ws.onerror = (error) => {
      handlers.onError?.(error);
    };
    ws.onclose = () => {
      stopHeartbeat();
      if (closed) {
        return;
      }
      reconnectTimer = window.setTimeout(connect, 1200);
    };
  };

  connect();

  return () => {
    closed = true;
    stopHeartbeat();
    if (reconnectTimer !== undefined) {
      window.clearTimeout(reconnectTimer);
    }
    if (socket && socket.readyState === WebSocket.OPEN) {
      socket.send(
        JSON.stringify({
          type: "session_unsubscribe",
          payload: { session_id: targetSessionID },
        }),
      );
    }
    socket?.close();
  };
}

// ---- 命运开盒（角色命运 UI）接口 ----

// getFateFeed 取某角色命运四槽首屏卡片（高光/待决策/回响）。
export async function getFateFeed(unitID: string): Promise<FateCard[]> {
  const data = await request<{ feed?: FateCard[] }>(`/api/fate/feed/${encodeURIComponent(unitID)}`);
  return data.feed ?? [];
}

// advanceFateWorld 让命运世界往前走一拍（不依赖玩家指引，她自己活一段）。
// 后端 POST /api/fate/sessions/:sessionId/advance 触发一次世界推进，返回 {advancing}：
//   - true=已开始推进（随后异步执行，前端应进 loading 并轮询 execution_in_progress 由 true→false）；
//   - false=未推进（已在推进中 / 无可推进单位 / 出错——出错时后端把 error 并进 200 体，这里吞掉只取 advancing）。
export async function advanceFateWorld(sessionID: string): Promise<boolean> {
  const data = await request<{ advancing?: boolean; error?: string }>(
    `/api/fate/sessions/${encodeURIComponent(sessionID)}/advance`,
    { method: "POST" },
  );
  return data.advancing ?? false;
}

// moveUnit 玩家在线直接把角色移到目标格（混合模型：上线可玩家操作）。返回移动后坐标；非法（越界/水山）抛错。
export async function moveUnit(sessionID: string, unitID: string, q: number, r: number): Promise<{ q: number; r: number }> {
  const data = await request<{ q?: number; r?: number }>(
    `/api/sessions/${encodeURIComponent(sessionID)}/units/${encodeURIComponent(unitID)}/move`,
    { method: "POST", body: JSON.stringify({ q, r }) },
  );
  return { q: data.q ?? q, r: data.r ?? r };
}

// equipItem 玩家在线给角色从背包穿上某装备。失败抛错（无此物/不可装备等）。
export async function equipItem(sessionID: string, unitID: string, itemID: string): Promise<void> {
  await request(
    `/api/sessions/${encodeURIComponent(sessionID)}/units/${encodeURIComponent(unitID)}/equip`,
    { method: "POST", body: JSON.stringify({ item_id: itemID }) },
  );
}

// MapPOI 对齐后端 session.MapPOI（json tag）：地块特殊资源 / 野外 NPC 身上的事件，画在格子上的徽标。
// consumed=true 表示已被「采完/探完」（前端徽标变淡、动作目录不再给「探一探」）。
export type MapPOI = {
  q: number;
  r: number;
  kind: "resource" | "npc_event" | string;
  type_code: string;
  label_zh: string;
  unit_id?: string;
  consumed?: boolean;
};

// getMapPOIs 拉某会话地图的兴趣点（确定性，命运地图画徽标 + 点击查看）。失败回空数组（不打断观战）。
export async function getMapPOIs(sessionID: string): Promise<MapPOI[]> {
  const data = await request<{ pois?: MapPOI[]; error?: string }>(
    `/api/fate/sessions/${encodeURIComponent(sessionID)}/map-pois`,
  );
  return data.pois ?? [];
}

// getSessionExecutionInProgress 轻量轮询某会话「执行是否进行中」（命运循环判这拍跑完用）。
// 复用 GET /api/sessions/:id 取快照的 execution_in_progress；读不到（404/字段缺）按 false 处理。
export async function getSessionExecutionInProgress(sessionID: string): Promise<boolean> {
  const data = await request<{ session?: { execution_in_progress?: boolean } }>(
    `/api/sessions/${encodeURIComponent(sessionID)}`,
  );
  return Boolean(data.session?.execution_in_progress);
}

// ── 分区大世界导航（三层地图 §8）：世界地图列区域 + 区域间前往/传送 ──

// ZoneSummary 是世界地图上的一个区域块（对齐后端 GET …/zones 的 json tag）。
// faction_id ∈ freedom/order/chaos/neutral；kind ∈ starter/capital/wild；
// reachable=从当前区可前往（border 恒可达；portal 阶段1锁→false）；
// portal_kind ∈ border/portal/""（""=与当前区不通）。
export type ZoneSummary = {
  id: string;
  name: string;
  faction_id: string;
  kind: string;
  level_min: number;
  level_max: number;
  is_current: boolean;
  reachable: boolean;
  portal_kind: string;
};

// getZones 拉某会话的全部区域 + 当前区 id（世界地图渲染源）。
// best-effort：失败（404/网络/字段缺）回 {zones:[], current_zone_id:""}，不打断导航浮层。
export async function getZones(
  sessionID: string,
): Promise<{ zones: ZoneSummary[]; current_zone_id: string }> {
  try {
    const data = await request<{ zones?: ZoneSummary[]; current_zone_id?: string }>(
      `/api/sessions/${encodeURIComponent(sessionID)}/zones`,
    );
    return { zones: data.zones ?? [], current_zone_id: data.current_zone_id ?? "" };
  } catch {
    return { zones: [], current_zone_id: "" };
  }
}

// travelToZone 让某角色前往目标区域（边界过渡 / 已解锁传送门）。
// toCoord 可选落点坐标，**字符串坐标键 "q,r"**（与后端 session/zone.go parseCoordKey 同口径）；
// 省略时由后端选默认入口。后端校验可达/解锁，失败抛中文错误
// （「从这里去不了」/「这道传送门尚未开通」/「她已身在此地」…）。
export async function travelToZone(
  sessionID: string,
  unitID: string,
  toZoneID: string,
  toCoord?: string,
): Promise<void> {
  const body: { to_zone_id: string; to_coord?: string } = { to_zone_id: toZoneID };
  if (toCoord) body.to_coord = toCoord;
  await request<{ ok?: boolean }>(
    `/api/sessions/${encodeURIComponent(sessionID)}/units/${encodeURIComponent(unitID)}/travel`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    },
  );
}

// ── 地块事件系统客户端（开发计划 2026-06-10 §3.7）：点地块→动作目录→直发动作/POI 遭遇/交易 ──

// TileAction 是动作目录里的一个可点条目（available=false 时置灰并展示 reason_zh）。
// 共享大世界：玩家不能在地图上建造/拆除建筑，故无 build/demolish；harvest/forge/upgrade 仅「使用」世界已有的己方设施。
export type TileAction = {
  action: "gather" | "harvest" | "forge" | "upgrade" | "poi_encounter" | "talk" | "trade" | string;
  activity?: string;
  target_unit_id?: string;
  label_zh: string;
  available: boolean;
  reason_zh?: string;
};

// TileAffordances 对齐后端 session.TileAffordances：该格「是什么 + 有什么 + 能做什么」。
export type TileAffordances = {
  q: number;
  r: number;
  terrain: string;
  terrain_zh: string;
  landmark?: string;
  poi?: { kind: string; type_code: string; label_zh: string; consumed: boolean } | null;
  structure?: {
    id: string;
    type: string;
    type_zh: string;
    progress: number;
    required: number;
    complete: boolean;
    harvest_ready: boolean;
    owner_faction: string;
  } | null;
  occupants?: { unit_id: string; name: string; faction_id: string; is_wild: boolean }[];
  unit_on_tile: boolean;
  distance: number;
  actions: TileAction[];
};

// getTileAffordances 拉某格的动作目录（决策零 LLM；用于点格子面板渲染动作按钮）。
export async function getTileAffordances(sessionID: string, unitID: string, q: number, r: number): Promise<TileAffordances> {
  return request<TileAffordances>(
    `/api/sessions/${encodeURIComponent(sessionID)}/tile-affordances?unit_id=${encodeURIComponent(unitID)}&q=${q}&r=${r}`,
  );
}

// TileActionResult 是直发地块动作的结算摘要（中文一句话 + 资源/物品/状态增减明细）。
export type TileActionResult = {
  ok: boolean;
  action: string;
  summary_zh: string;
  effects?: { kind: string; item_id?: string; label_zh: string; delta: number }[];
};

// executeTileAction 直发地块动作（采集/收割/锻造/强化），后端复用既有结算链。
export async function executeTileAction(
  sessionID: string,
  unitID: string,
  payload: { action: string; q: number; r: number; activity?: string; item_id?: string },
): Promise<TileActionResult> {
  return request<TileActionResult>(
    `/api/sessions/${encodeURIComponent(sessionID)}/units/${encodeURIComponent(unitID)}/tile-action`,
    { method: "POST", body: JSON.stringify(payload) },
  );
}

// MerchantGood 是行商货单上的一件商品报价。
export type MerchantGood = {
  item_id: string;
  display_name: string;
  quantity: number;
  buy_price: number;
  sell_price: number;
};

// POIEncounterResult 是触发 POI 遭遇的结算（埋伏→精英战 / 行商→货单 / 求助、奇遇→分支 / 迷途→结识）。
export type POIEncounterResult = {
  ok: boolean;
  kind: "resource" | "ambush" | "merchant" | "help" | "adventure" | "lost" | string;
  type_code: string;
  summary_zh: string;
  consumed: boolean;
  outcome?: {
    wallet_delta?: number;
    hunger_delta?: number;
    morale_delta?: number;
    gained_item_id?: string;
    gained_item_name?: string;
    gained_item_qty?: number;
    relation_zh?: string;
    effect_summary_zh?: string;
    encounter_outcome?: string;
    damage_taken?: number;
    penalty_layer?: number;
    awards?: string[];
  };
  merchant_unit_id?: string;
  merchant_goods?: MerchantGood[];
};

// resolvePOIEncounter 触发该格 POI 遭遇结算（结算后该 POI 标记消耗防重放；行商不消耗、返回货单）。
export async function resolvePOIEncounter(sessionID: string, unitID: string, q: number, r: number): Promise<POIEncounterResult> {
  return request<POIEncounterResult>(
    `/api/sessions/${encodeURIComponent(sessionID)}/units/${encodeURIComponent(unitID)}/poi-encounter`,
    { method: "POST", body: JSON.stringify({ q, r }) },
  );
}

// PlayerTradeResult 是与 NPC 直接买卖的结算（金币/物品走后端既有口径）。
export type PlayerTradeResult = {
  ok: boolean;
  summary_zh: string;
  wallet_after: number;
  items?: { item_id: string; display_name: string; quantity: number }[];
};

// tradeWithUnit 与同格/相邻 NPC 直接买卖（mode=buy 买入 / sell 卖出；quantity 缺省 1）。
export async function tradeWithUnit(
  sessionID: string,
  unitID: string,
  payload: { target_unit_id: string; mode: "buy" | "sell"; item_id: string; quantity?: number },
): Promise<PlayerTradeResult> {
  return request<PlayerTradeResult>(
    `/api/sessions/${encodeURIComponent(sessionID)}/units/${encodeURIComponent(unitID)}/trade`,
    { method: "POST", body: JSON.stringify(payload) },
  );
}

// useItem 玩家在线让角色使用背包消耗品（吃口粮/喝药；恢复恒经后端 Mutator）。失败抛错（中文友好提示）。
export async function useItem(sessionID: string, unitID: string, itemID: string): Promise<void> {
  await request(
    `/api/sessions/${encodeURIComponent(sessionID)}/units/${encodeURIComponent(unitID)}/use-item`,
    { method: "POST", body: JSON.stringify({ item_id: itemID }) },
  );
}

// unequipItem 玩家在线让角色卸下某槽位装备回背包（weapon|armor|shoes|accessory，含重算派生攻防）。
export async function unequipItem(sessionID: string, unitID: string, slot: string): Promise<void> {
  await request(
    `/api/sessions/${encodeURIComponent(sessionID)}/units/${encodeURIComponent(unitID)}/unequip`,
    { method: "POST", body: JSON.stringify({ slot }) },
  );
}

// resolveFateDecision 处理一条待决策（玩家拿主意）。
export async function resolveFateDecision(
  decisionID: string,
  sessionID: string,
  unitID: string,
  resolveType: string,
): Promise<void> {
  await request<{ ok?: boolean }>(`/api/fate/decisions/${encodeURIComponent(decisionID)}/resolve`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ session_id: sessionID, unit_id: unitID, resolve_type: resolveType }),
  });
}

// recordPlayerIntervention 记录一次玩家直接接管（可被回响引用）。
export async function recordPlayerIntervention(
  sessionID: string,
  unitID: string,
  summary: string,
): Promise<string> {
  const data = await request<{ event_id?: string }>(
    `/api/sessions/${encodeURIComponent(sessionID)}/units/${encodeURIComponent(unitID)}/intervene`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ summary }),
    },
  );
  return data.event_id ?? "";
}

// getUnitStatus 读单个角色（命运四槽的状态卡用）。
export async function getUnitStatus(unitID: string): Promise<Record<string, unknown> | null> {
  const data = await request<{ unit?: Record<string, unknown> }>(`/api/units/${encodeURIComponent(unitID)}`);
  return data.unit ?? null;
}

// ── 编年史读侧（chronicle）：对应后端 ChronicleFeed / ChronicleMomentByID，供 ChroniclePanel 依赖注入 ──
// 与后端 json tag 一一对应；结构化类型，与 ChroniclePanel 自声明的 ChronicleFeed/ChronicleView 兼容。
export type ChronicleEntryDTO = {
  id: string;
  session_id: string;
  unit_id?: string;
  turn: number;
  kind: string;
  text: string;
  created_at?: string;
};
export type MomentAnchorDTO = {
  chronicle_id: string;
  unit_id?: string;
  turn: number;
  event_ids?: string[];
};
export type ChronicleViewDTO = { entry: ChronicleEntryDTO; anchor: MomentAnchorDTO };
export type ChronicleFeedDTO = {
  session_id: string;
  unit_id?: string;
  views: ChronicleViewDTO[];
  limit: number;
  offset: number;
  has_more: boolean;
  next_offset?: number;
};

// getChronicleFeed 拉一页编年史（倒序，?limit=&offset= 分页）。unitID 空 → 整局总览；非空 → 该单位传记。
// 会话作用域读，request 自动带会话角色 token（与 feuds/charter 读一致）。
export async function getChronicleFeed(params: {
  sessionID: string;
  unitID?: string;
  limit: number;
  offset: number;
}): Promise<ChronicleFeedDTO> {
  const query = `?limit=${encodeURIComponent(String(params.limit))}&offset=${encodeURIComponent(String(params.offset))}`;
  const base = `/api/sessions/${encodeURIComponent(params.sessionID)}`;
  const path = params.unitID
    ? `${base}/units/${encodeURIComponent(params.unitID)}/chronicle${query}`
    : `${base}/chronicle${query}`;
  const data = await request<{ feed?: ChronicleFeedDTO }>(path);
  return (
    data.feed ?? { session_id: params.sessionID, unit_id: params.unitID, views: [], limit: params.limit, offset: params.offset, has_more: false }
  );
}

// getChronicleMoment 「回到那一刻」单条精确反查（GET …/chronicle/:chronicleId/moment）。找不到返回 null。
export async function getChronicleMoment(params: {
  sessionID: string;
  chronicleID: string;
}): Promise<ChronicleViewDTO | null> {
  const data = await request<{ moment?: ChronicleViewDTO }>(
    `/api/sessions/${encodeURIComponent(params.sessionID)}/chronicle/${encodeURIComponent(params.chronicleID)}/moment`,
  );
  return data.moment ?? null;
}

// bootstrapCharacter 快速创建一个角色（捏人 onboarding 用）。
// withVillage=true 时附带 with_village=1，兑现「她身边已有二十个有名有姓的人」的 onboarding 承诺。
export async function bootstrapCharacter(
  name: string,
  sessionID: string,
  factionID = "player",
  withVillage = false,
): Promise<Record<string, unknown> | null> {
  let qs = `name=${encodeURIComponent(name)}&session_id=${encodeURIComponent(sessionID)}&faction_id=${encodeURIComponent(factionID)}`;
  if (withVillage) {
    qs += `&with_village=1`;
  }
  const data = await request<{ unit?: Record<string, unknown> }>(`/api/units/bootstrap?${qs}`, { method: "POST" });
  return data.unit ?? null;
}

// resolveEliteEncounter 触发一次单人 elite/PvE 遭遇（多回合消耗战→战利品分赃或分级惩罚→祖魂收件箱卡）。
// 这是真实动作：会改动该角色的 HP/士气/钱包并写入命运收件箱。
export async function resolveEliteEncounter(
  sessionID: string,
  unitID: string,
): Promise<EliteEncounterResult> {
  const data = await request<{ encounter?: EliteEncounterResult }>(
    `/api/sessions/${encodeURIComponent(sessionID)}/units/${encodeURIComponent(unitID)}/elite-encounter`,
    { method: "POST" },
  );
  return (
    data.encounter ?? {
      ThreatID: "",
      Outcome: "down",
      Rounds: 0,
      DamageDealt: 0,
      DamageTaken: 0,
      Contribution: 0,
      Awards: null,
      PenaltyLayer: 0,
      InboxCard: "",
    }
  );
}

// setPlayerDirective 向后端提交玩家方针（兼容旧接口）。
export function setPlayerDirective(
  sessionID: string,
  text: string,
  scope: DirectiveScope,
  unitID?: string,
): Promise<SessionSnapshot> {
  return unwrapSession(`/api/sessions/${encodeURIComponent(sessionID)}/directive`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ text, scope, unit_id: unitID }),
  });
}

// setGlobalDirective 提交全局方针文本。
export function setGlobalDirective(sessionID: string, text: string): Promise<SessionSnapshot> {
  return setPlayerDirective(sessionID, text, "doctrine");
}

// setTaskDirective 提交针对指定单位的任务方针。
export function setTaskDirective(
  sessionID: string,
  text: string,
  unitID?: string,
): Promise<SessionSnapshot> {
  return setPlayerDirective(sessionID, text, "task", unitID);
}

// setImmediateOrder 提交高优先级即时命令。
export function setImmediateOrder(
  sessionID: string,
  text: string,
  unitID: string,
): Promise<SessionSnapshot> {
  return setPlayerDirective(sessionID, text, "order", unitID);
}

export async function talkToUnit(
  sessionID: string,
  unitID: string,
  message: string,
): Promise<{ session: SessionSnapshot; reply: DialogueMessage }> {
  return request<{ session: SessionSnapshot; reply: DialogueMessage }>(
    `/api/sessions/${encodeURIComponent(sessionID)}/dialogue`,
    {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ unit_id: unitID, message }),
    },
  );
}

export function confirmOpeningDraft(sessionID: string, units: BattleUnit[]): Promise<SessionSnapshot> {
  return unwrapSession(`/api/sessions/${encodeURIComponent(sessionID)}/opening-draft`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ units }),
  });
}

// advancePhase 请求推进当前阶段并返回最新快照。
// 软带 Bearer：已登录则推进成功后累计防沉迷时长，且受合规门控（403 {error,reason}）；未登录匿名放行。
export function advancePhase(sessionID: string): Promise<SessionSnapshot> {
  return unwrapSession(`/api/sessions/${encodeURIComponent(sessionID)}/advance-phase`, {
    method: "POST",
  }, { bearer: "soft" });
}

export async function listTerrainCatalog(): Promise<TerrainDefinition[]> {
  const response = await request<{ terrains: TerrainDefinition[] }>(`/api/world/terrains`);
  return response.terrains ?? [];
}

export async function submitModerationReport(
  sessionID: string,
  payload: {
    reporter?: string;
    unit_id?: string;
    category: string;
    detail: string;
  },
): Promise<{ session: SessionSnapshot; report: ModerationReport }> {
  return request<{ session: SessionSnapshot; report: ModerationReport }>(
    `/api/sessions/${encodeURIComponent(sessionID)}/reports`,
    {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
      },
      body: JSON.stringify(payload),
    },
  );
}

export async function getAuditBundle(sessionID: string, limit = 80): Promise<AuditBundle> {
  // 审计包是高危只读端点（含完整 LLM prompt），套 X-Ops-Token（原型缺头放行）。
  const result = await request<{ audit: AuditBundle }>(
    `/api/sessions/${encodeURIComponent(sessionID)}/audit?limit=${limit}`,
    undefined,
    { withOps: true },
  );
  return result.audit;
}

export async function eraseSessionPrivateData(
  sessionID: string,
  options: PrivacyEraseOptions = {},
): Promise<{ session: SessionSnapshot; result: PrivacyEraseResult }> {
  return request<{ session: SessionSnapshot; result: PrivacyEraseResult }>(
    `/api/sessions/${encodeURIComponent(sessionID)}/privacy/erase`,
    {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
      },
      body: JSON.stringify(options),
    },
    { withOps: true },
  );
}

export async function purgeExpiredPrivateData(
  retentionDays = 30,
  limit = 200,
): Promise<PrivacyPurgeResult> {
  const response = await request<{ result: PrivacyPurgeResult }>(`/api/privacy/purge`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      retention_days: retentionDays,
      limit,
    }),
  }, { withOps: true });
  return response.result;
}

export async function registerAccount(payload: {
  username: string;
  display_name?: string;
  password: string;
}): Promise<{ user: AccountUser; auth: AccountLoginResult }> {
  const result = await request<{ user: AccountUser; auth: AccountLoginResult }>(`/api/accounts/register`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify(payload),
  });
  // 注册即登录：同步 Bearer 令牌，使后续 billing/compliance/建局软带自动带上。
  if (result.auth?.token) {
    setAccountToken(result.auth.token);
  }
  return result;
}

export async function loginAccount(payload: {
  username: string;
  password: string;
}): Promise<{ user: AccountUser; auth: AccountLoginResult }> {
  const result = await request<{ user: AccountUser; auth: AccountLoginResult }>(`/api/accounts/login`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify(payload),
  });
  // 登录成功：同步 Bearer 令牌（模块级 + localStorage），billing/compliance 端点据此鉴权。
  if (result.auth?.token) {
    setAccountToken(result.auth.token);
  }
  return result;
}

// ---- 账号 + 主世界角色（AuthGate / Root 路由用，全部存/带 localStorage Bearer token）----

// MyCharacter 对齐后端 session.MainWorldCharacter（json tag），是「账号在主世界的持久角色」对外视图。
// has_character=false 表示该账号尚未降生（前端据此进捏人 onboarding）；其余字段 omitempty。
export type MyCharacter = {
  has_character: boolean;
  session_id?: string;
  unit_id?: string;
  name?: string;
  world_id?: string;
  origin?: string;
  // faction/spawn_region/moral_alignment 对齐后端 MainWorldCharacter 的 json tag（阵营开放世界 F1+）：
  // 所属阵营（freedom/order/chaos）/ 出生据点 region / 3 维道德基准（=该阵营道德基准）。均 omitempty。
  faction?: string;
  spawn_region?: string;
  moral_alignment?: { freedom?: number; order?: number; chaos?: number };
  created?: boolean;
};

// MyCharacterInput 是捏人降生入参（对齐后端 MainWorldCharacterInput），全字段可空（后端用占位名兜底）。
export type MyCharacterInput = {
  name?: string;
  origin?: string;
  desire?: string;
  wound?: string;
  redline?: string;
  // faction 是玩家选的阵营（freedom/order/chaos）；空/非法时后端据出身/夙愿启发选（阵营开放世界 F3 捏人入口）。
  faction?: string;
};

// getMe 读当前登录账号；未登录（无 token）或 token 失效（401）一律返回 null（不抛），供 AuthGate 判定登录态。
// 复用既有 getCurrentAccount（显式 Bearer 头），但把未登录/失效收敛为 null，调用方零 try/catch。
export async function getMe(): Promise<AccountUser | null> {
  const token = getAccountToken();
  if (token.trim() === "") {
    return null;
  }
  try {
    return await getCurrentAccount(token);
  } catch (error) {
    // 401（token 失效/被登出）→ 清掉本地令牌并回到未登录态；其它错误（网络等）同样收敛为 null。
    if (error instanceof APIError && error.status === 401) {
      setAccountToken("");
    }
    return null;
  }
}

// getMyCharacter 读当前账号在主世界 world_default 的持久角色（需 Bearer）。无角色 → {has_character:false}。
// 401（token 失效/被登出）→ 镜像 getMe：先清本地 Bearer 再 rethrow，避免持过期 token 反复打 401。
export async function getMyCharacter(): Promise<MyCharacter> {
  try {
    const data = await request<{ character?: MyCharacter }>(
      `/api/me/character`,
      { method: "GET" },
      { bearer: "require" },
    );
    return data.character ?? { has_character: false };
  } catch (error) {
    if (error instanceof APIError && error.status === 401) {
      setAccountToken("");
    }
    throw error;
  }
}

// createMyCharacter 为当前账号在主世界降生一个角色（需 Bearer）。幂等：账号已有角色则返回既有的、绝不重复造人。
// 401 同 getMyCharacter：先清本地 Bearer 再 rethrow（与 getMe 一致），让上层据失效态回登录。
export async function createMyCharacter(input: MyCharacterInput): Promise<MyCharacter> {
  try {
    const data = await request<{ character?: MyCharacter }>(
      `/api/me/character`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(input),
      },
      { bearer: "require" },
    );
    return data.character ?? { has_character: false };
  } catch (error) {
    if (error instanceof APIError && error.status === 401) {
      setAccountToken("");
    }
    throw error;
  }
}

export async function getCurrentAccount(token: string): Promise<AccountUser> {
  const response = await request<{ user: AccountUser }>(`/api/accounts/me`, {
    method: "GET",
    headers: {
      Authorization: `Bearer ${token}`,
    },
  });
  return response.user;
}

export async function logoutAccount(token: string): Promise<boolean> {
  try {
    const response = await request<{ ok: boolean }>(`/api/accounts/logout`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${token}`,
      },
    });
    return response.ok;
  } finally {
    // 无论后端登出是否成功，本地一律清掉 Bearer 令牌（模块级 + localStorage）。
    setAccountToken("");
  }
}

// ---- 治理（运营态，X-Ops-Token）----

// resolveModerationReport 运营处理一条举报（resolve/warn/ban）。返回脱敏会话快照 + 处理后的报告。
// action 缺省走后端默认；report 不存在→404、action 非法→400（经 APIError.status 区分）。
export async function resolveModerationReport(
  sessionID: string,
  reportID: string,
  action?: "resolve" | "warn" | "ban",
  note?: string,
): Promise<{ session: SessionSnapshot; report: ModerationReport }> {
  return request<{ session: SessionSnapshot; report: ModerationReport }>(
    `/api/sessions/${encodeURIComponent(sessionID)}/reports/${encodeURIComponent(reportID)}/resolve`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action, note }),
    },
    { withOps: true },
  );
}

// ---- 跨玩家：异步同意闸 + 跨事件投递 ----

// listPendingConsents 列出某 target 角色待处理的同意请求（其玩家可接受/拒绝）。运营态 X-Ops-Token。
export async function listPendingConsents(unitID: string): Promise<ConsentRequest[]> {
  const data = await request<{ pending?: ConsentRequest[] }>(
    `/api/consent/pending/${encodeURIComponent(unitID)}`,
    undefined,
    { withOps: true },
  );
  return data.pending ?? [];
}

// resolveConsent 处理一条同意请求（accept=true 应用关系效果，false 不应用）。运营态 X-Ops-Token。
export async function resolveConsent(reqID: string, accept: boolean): Promise<ConsentRequest> {
  return request<ConsentRequest>(
    `/api/consent/${encodeURIComponent(reqID)}/resolve`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ accept }),
    },
    { withOps: true },
  );
}

// surfaceCrossEvents 主动把世界总线上牵涉某角色的跨玩家事件投进她的命运收件箱，返回被惊动条数。无鉴权。
export async function surfaceCrossEvents(
  worldID: string,
  unitID: string,
  limit?: number,
): Promise<number> {
  const qs = typeof limit === "number" && limit > 0 ? `?limit=${limit}` : "";
  const data = await request<{ surfaced?: number }>(
    `/api/worlds/${encodeURIComponent(worldID)}/units/${encodeURIComponent(unitID)}/cross-events/surface${qs}`,
    { method: "POST" },
  );
  return data.surfaced ?? 0;
}

// ---- 商业化（Bearer 强制）----

// listBillingSKUs 列出在售 SKU 目录。无鉴权（仅 QUNXIANG_BILLING_ENABLED 开时存在，关→404）。
export async function listBillingSKUs(): Promise<BillingSKU[]> {
  const data = await request<{ skus?: BillingSKU[] }>(`/api/billing/skus`);
  return data.skus ?? [];
}

// purchaseSKU 购买一个 SKU（账户取自 Bearer token，忽略客户端传账户）。返回计费流水。
export async function purchaseSKU(
  skuID: string,
  platform: string,
  receipt: string,
): Promise<BillingCharge> {
  const data = await request<{ charge: BillingCharge }>(
    `/api/billing/purchase`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ sku_id: skuID, platform, receipt }),
    },
    { bearer: "require" },
  );
  return data.charge;
}

// getBillingQuota 查询本账号 LLM 成本配额是否仍允许调用（true=未超额）。accountID 仅占位，实取自 token。
export async function getBillingQuota(accountID: string): Promise<BillingQuota> {
  return request<BillingQuota>(
    `/api/billing/quota/${encodeURIComponent(accountID)}`,
    undefined,
    { bearer: "require" },
  );
}

// listEntitlements 列出本账号已购权益（会员/单品）。accountID 仅占位，实取自 token。
export async function listEntitlements(accountID: string): Promise<Entitlement[]> {
  const data = await request<{ entitlements?: Entitlement[] }>(
    `/api/billing/entitlements/${encodeURIComponent(accountID)}`,
    undefined,
    { bearer: "require" },
  );
  return data.entitlements ?? [];
}

// ---- 合规（Bearer 强制）----

// verifyCompliance 登记实名（姓名+身份证号交后端核验，不落库）与生日（据生日刷新未成年模式）。账户取自 token。
export async function verifyCompliance(payload: {
  birthDate?: string;
  name?: string;
  idNumber?: string;
}): Promise<boolean> {
  const data = await request<{ ok: boolean }>(
    `/api/compliance/verify`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        birth_date: payload.birthDate,
        name: payload.name,
        id_number: payload.idNumber,
      }),
    },
    { bearer: "require" },
  );
  return data.ok;
}

// getComplianceGate 读合规前置门裁决（未实名/宵禁/防沉迷）。账户取自 token，accountID 仅占位。
export async function getComplianceGate(accountID: string): Promise<ComplianceGate> {
  return request<ComplianceGate>(
    `/api/compliance/gate/${encodeURIComponent(accountID)}`,
    undefined,
    { bearer: "require" },
  );
}

// reportPlaySeconds 累计本账号防沉迷在线时长（客户端按心跳/会话时长上报）。账户取自 token。
export async function reportPlaySeconds(seconds: number): Promise<boolean> {
  const data = await request<{ ok: boolean }>(
    `/api/compliance/play-seconds`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ seconds }),
    },
    { bearer: "require" },
  );
  return data.ok;
}

// ---- PvE 组队（无鉴权）----

// resolveFieldBoss 触发一次野外 Boss/组队遭遇（多回合消耗战→按贡献分赃/分级惩罚→祖魂收件箱卡）。
// 真实动作：会改动队员 HP/士气/钱包并写入命运收件箱。注意返回键名为 Go 大写字段名。
export async function resolveFieldBoss(
  sessionID: string,
  unitIDs: string[],
): Promise<FieldBossResult> {
  const data = await request<{ encounter?: FieldBossResult }>(
    `/api/sessions/${encodeURIComponent(sessionID)}/field-boss`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ unit_ids: unitIDs }),
    },
  );
  return (
    data.encounter ?? {
      ThreatID: "",
      Victory: false,
      Rounds: 0,
      Members: null,
    }
  );
}

// runDungeon 跑通一次多层副本（逐层确定性消耗战→通关分赃 / 败北分级惩罚→各队员祖魂收件箱卡）。
// 真实动作：会改参战队员 HP/士气/钱包并写入命运收件箱。注意返回键名为 Go 大写字段名（DungeonResult 无 json tag）。
// FloorResults/Awards 与 Contribution/PenaltyLayer/InboxCards 可能为 null（未通关 / 缺失），消费方需判空。
export async function runDungeon(
  sessionID: string,
  unitIDs: string[],
  floors: number,
): Promise<DungeonResult> {
  const data = await request<{ dungeon?: DungeonResult }>(
    `/api/sessions/${encodeURIComponent(sessionID)}/dungeon`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ unit_ids: unitIDs, floors }),
    },
  );
  return (
    data.dungeon ?? {
      DungeonID: "",
      Floors: 0,
      FloorsClear: 0,
      Outcome: "wiped",
      FloorResults: null,
      Awards: null,
      Contribution: null,
      PenaltyLayer: null,
      InboxCards: null,
    }
  );
}

// ---- 世界 Boss：全世界共享血池的异步协作 PvE（POST，无 ops 守卫）----

// spawnWorldBoss 在某世界投放一头世界 Boss（name 必填、hp 须为正且不超后端上限）。返回 boss ID。
// regionID 可选（分片定位）。真实动作：写 world_bosses 表；world 必须已注册否则 400。
export async function spawnWorldBoss(
  worldID: string,
  name: string,
  hp: number,
  regionID?: string,
): Promise<string> {
  const data = await request<{ boss_id?: string }>(
    `/api/worlds/${encodeURIComponent(worldID)}/bosses`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name, hp, region_id: regionID }),
    },
  );
  return data.boss_id ?? "";
}

// strikeWorldBoss 对一头世界 Boss 出手一次：原子扣血 + 记进世界总线贡献账本；血池清零则由抢到结算闩锁者全员分赃。
// 真实动作：注意返回键名为 Go 大写字段名（WorldBossStrikeResult 无 json tag）。Participants/Awards/BroadcastCard 仅结算者填充。
export async function strikeWorldBoss(
  worldID: string,
  bossID: string,
  attackerID: string,
): Promise<WorldBossStrikeResult> {
  const data = await request<{ strike?: WorldBossStrikeResult }>(
    `/api/worlds/${encodeURIComponent(worldID)}/bosses/${encodeURIComponent(bossID)}/strike`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ attacker_id: attackerID }),
    },
  );
  return (
    data.strike ?? {
      BossID: bossID,
      AttackerID: attackerID,
      Damage: 0,
      HPRemaining: 0,
      Defeated: false,
      SettledByMe: false,
      Participants: 0,
      Awards: null,
      BroadcastCard: "",
    }
  );
}

// ---- 血仇（blood feud）：某角色的世仇清单（GET，会话作用域）----

// listBloodFeuds 列出某角色当前怀有的世仇关系（rivalry 达成仇阈的对外强敌意），按敌意降序。纯读、无副作用。
export async function listBloodFeuds(sessionID: string, unitID: string): Promise<BloodFeudEntry[]> {
  const data = await request<{ feuds?: BloodFeudEntry[] }>(
    `/api/sessions/${encodeURIComponent(sessionID)}/units/${encodeURIComponent(unitID)}/feuds`,
  );
  return data.feuds ?? [];
}

// ---- 离线宪章（offline_charter）：单个单位「玩家不在场时据此自治」的三段长效授权读写（会话作用域）----
// 字段对齐后端 session.OfflineCharter / CharterRedline 的 json tag。request 自动带会话角色 token（与 feuds 读一致）。

// CharterRedlineDTO 对齐后端 session.CharterRedline（id 由后端 NormalizeCharter 补齐/派生）。
export type CharterRedlineDTO = {
  id?: string;
  text: string;
  severity?: string;
};
// OfflineCharterDTO 对齐后端 session.OfflineCharter 三段（全 omitempty，未设立时各段缺省）。
export type OfflineCharterDTO = {
  long_term_goals?: string[];
  redlines?: CharterRedlineDTO[];
  social_mandates?: string[];
};

// getCharter 读某单位现有离线宪章；exists=false 表示从未立约（区分「显式空宪章」与「未设置」）。纯读。
export async function getCharter(
  sessionID: string,
  unitID: string,
): Promise<{ charter: OfflineCharterDTO; exists: boolean }> {
  const data = await request<{ charter?: OfflineCharterDTO; exists?: boolean }>(
    `/api/sessions/${encodeURIComponent(sessionID)}/units/${encodeURIComponent(unitID)}/charter`,
  );
  return { charter: data.charter ?? {}, exists: Boolean(data.exists) };
}

// putCharter 设立/覆盖某单位离线宪章（PUT），返回后端规范化后的宪章（带补齐的红线 id、裁空白条目）。
export async function putCharter(
  sessionID: string,
  unitID: string,
  charter: OfflineCharterDTO,
): Promise<OfflineCharterDTO> {
  const data = await request<{ charter?: OfflineCharterDTO }>(
    `/api/sessions/${encodeURIComponent(sessionID)}/units/${encodeURIComponent(unitID)}/charter`,
    {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(charter),
    },
  );
  return data.charter ?? {};
}

// deleteCharter 撤销某单位整份离线宪章（DELETE，写 CHARTER_UPDATED 留痕）。
export async function deleteCharter(sessionID: string, unitID: string): Promise<void> {
  await request<{ ok?: boolean }>(
    `/api/sessions/${encodeURIComponent(sessionID)}/units/${encodeURIComponent(unitID)}/charter`,
    { method: "DELETE" },
  );
}

// ---- Ops 看板（运营态，X-Ops-Token）----

// fetchCostDashboard 读运营成本/单位经济仪表盘（最近 days 天，默认 30；days<=0 视为全量）。后端裸返回 CostDashboardData。
export async function fetchCostDashboard(days = 30): Promise<CostDashboardData> {
  return request<CostDashboardData>(
    `/api/ops/cost-dashboard?days=${days}`,
    undefined,
    { withOps: true },
  );
}

// fetchLeadsFunnel 读假门转化漏斗（按 kind 计数 + 唯一访客）。后端裸返回 LeadsFunnelData。
export async function fetchLeadsFunnel(): Promise<LeadsFunnelData> {
  return request<LeadsFunnelData>(`/api/ops/leads-funnel`, undefined, { withOps: true });
}

// fetchProductFunnel 读 AARRR 产品漏斗（按事件/阶段计数 + 去重会话）。days 缺省/<=0 视为全量。后端裸返回 ProductFunnelReport（不解包）。
export async function fetchProductFunnel(days?: number): Promise<ProductFunnelReport> {
  const qs = typeof days === "number" ? `?days=${days}` : "";
  return request<ProductFunnelReport>(`/api/ops/product-funnel${qs}`, undefined, { withOps: true });
}

// fetchNorthStar 读北极星指标（收件箱处理率 / 分享 / 付费 / 回访 / 惊喜命中率·OOC 率）。days 缺省/<=0 视为全量。后端裸返回 NorthStarReport（不解包）。
export async function fetchNorthStar(days?: number): Promise<NorthStarReport> {
  const qs = typeof days === "number" ? `?days=${days}` : "";
  return request<NorthStarReport>(`/api/ops/north-star${qs}`, undefined, { withOps: true });
}

// fetchExperiment 读某实验按 ab_bucket 拆分的漏斗（key 回显查询入参、桶名本身编码实验）。days 缺省/<=0 视为全量。后端裸返回 ExperimentFunnelReport（不解包）。
export async function fetchExperiment(key: string, days?: number): Promise<ExperimentFunnelReport> {
  let qs = `?key=${encodeURIComponent(key)}`;
  if (typeof days === "number") {
    qs += `&days=${days}`;
  }
  return request<ExperimentFunnelReport>(`/api/ops/experiment${qs}`, undefined, { withOps: true });
}

// ---- 漏斗埋点（无鉴权，best-effort）----

// emitLead 向 /api/leads 提交一条留资/事件埋点。无鉴权；返回是否成功（best-effort）。
export async function emitLead(payload: LeadEvent): Promise<boolean> {
  const data = await request<{ ok?: boolean }>(`/api/leads`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
  return data.ok ?? false;
}

// trackFunnel 是 emitLead 的轻量埋点包装：自动补 vid（匿名访客 ID，持久化 localStorage），并吞掉所有错误。
// 埋点失败绝不抛——调用方可裸调 `void trackFunnel("cta_click")` 而无需 try/catch。
export function trackFunnel(kind: string, props?: { email?: string; source?: string }): Promise<void> {
  return emitLead({
    kind,
    vid: anonymousVisitorID(),
    email: props?.email,
    source: props?.source,
  })
    .then(() => undefined)
    .catch(() => undefined);
}

// emitClientAnalytics 向 /api/analytics/client 提交一条客户端行为事件（best-effort）。
// 后端白名单 status_card_viewed / share_initiated / fate_react_expected / fate_react_surprise / fate_react_ooc → 落 product_events；非白名单被后端静默丢弃。
// 与 trackFunnel 一样吞掉所有错误——调用方可裸调 `void emitClientAnalytics("status_card_viewed")` 而无需 try/catch。
export function emitClientAnalytics(name: string, props?: Record<string, unknown>): Promise<void> {
  return request<{ ok?: boolean }>(`/api/analytics/client`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    // 带匿名 vid：供后端 A/B 分桶（分桶算法在后端，前端零变体知识）。
    body: JSON.stringify({ name, props, vid: anonymousVisitorID() }),
  })
    .then(() => undefined)
    .catch(() => undefined);
}

// ============ 副本异步分段推进（设计 PvE威胁系统.md §3-5；DungeonSegmentPanel 注入消费） ============
// 与同步 runDungeon 并列：把副本切成「逐段可中断、关键节点暂停问玩家」的异步流。后端 flag QUNXIANG_DUNGEON
// 关时返回 409 ErrDungeonDisabled（APIError.status=409、message 含「未启用」），面板据此识别。
// 注意 /run /resume 返回的 Go 结构体 DungeonSegmentResult 无 json tag → 键名为大写字段名（SegmentID/NextAction/Floor/...）。

// DungeonSegmentNextAction 与后端 session.DungeonNextAction 枚举对齐。
export type DungeonSegmentNextAction =
  | "continue_next_floor"
  | "pause_first_contact"
  | "pause_player_decision"
  | "completed_cleared"
  | "completed_fled"
  | "completed_wiped";

// DungeonSegmentResult 对齐后端 session.DungeonSegmentResult（无 json tag → 大写键名）。
export type DungeonSegmentResult = {
  SegmentID: string;
  NextAction: DungeonSegmentNextAction;
  Floor: number;
  PauseCard: string;
  Outcome: string;
};

// DungeonSegmentStartResult 是 start 端点返回的首段标识（有 json tag → 小写键名）。
export type DungeonSegmentStartResult = {
  segment_id: string;
  floors: number;
  floor: number;
  state: string;
};

// startDungeonAsync 创建一次副本异步推进的首段（不立即推进任何战斗）。flag 关时 409。
export async function startDungeonAsync(
  sessionID: string,
  unitIDs: string[],
  floors: number,
): Promise<DungeonSegmentStartResult> {
  const data = await request<{ segment?: DungeonSegmentStartResult }>(
    `/api/sessions/${encodeURIComponent(sessionID)}/dungeon/segments`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ unit_ids: unitIDs, floors }),
    },
  );
  return data.segment ?? { segment_id: "", floors: 0, floor: 0, state: "" };
}

// runDungeonSegment 推进当前段一层（不暂停则跑到下一关键节点/终局）。
export async function runDungeonSegment(
  sessionID: string,
  segmentID: string,
): Promise<DungeonSegmentResult> {
  const data = await request<{ result?: DungeonSegmentResult }>(
    `/api/sessions/${encodeURIComponent(sessionID)}/dungeon/segments/${encodeURIComponent(segmentID)}/run`,
    { method: "POST" },
  );
  return (
    data.result ?? { SegmentID: segmentID, NextAction: "completed_fled", Floor: 0, PauseCard: "", Outcome: "fled" }
  );
}

// resumeDungeonSegment 玩家回来据选择续跑/见好就收（choice: continue|retreat）。
export async function resumeDungeonSegment(
  sessionID: string,
  segmentID: string,
  choice: "continue" | "retreat",
): Promise<DungeonSegmentResult> {
  const data = await request<{ result?: DungeonSegmentResult }>(
    `/api/sessions/${encodeURIComponent(sessionID)}/dungeon/segments/${encodeURIComponent(segmentID)}/resume`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ choice }),
    },
  );
  return (
    data.result ?? { SegmentID: segmentID, NextAction: "completed_fled", Floor: 0, PauseCard: "", Outcome: "fled" }
  );
}

// ============ Live-Ops 运营端点（GM 注入 / 赛季 / 零和审计），全部 X-Ops-Token（LiveOpsPanel 注入消费） ============

// GmWorldEventInput / GmWorldEventResult 对齐后端 liveops.GMEvent / GMEventResult。
export type GmWorldEventInput = {
  kind: string;
  importance: number;
  actorId?: string;
  targetId?: string;
  regionId?: string;
  payload?: Record<string, unknown>;
};
export type GmWorldEventResult = {
  cross_event_id: string;
  audit_id: string;
  world_tick: number;
};

// LiveopsCreateSeasonInput / LiveopsSeason / LiveopsFinalizeResult 对齐后端 liveops.CreateSeasonInput / Season / FinalizeResult。
export type LiveopsCreateSeasonInput = {
  name: string;
  world_name?: string;
  content_theme_id?: string;
  max_population?: number;
  region_seed?: string;
};
export type LiveopsSeason = {
  id: string;
  world_id: string;
  name: string;
  status: string;
  started_at: string;
  ends_at: string;
  content_theme_id: string;
  created_at: string;
};
export type LiveopsFinalizeResult = {
  season_id: string;
  world_id: string;
  members_total: number;
  archived: number;
  archive_errors: string[];
  sealed: boolean;
};

// LiveopsGroupStat / LiveopsArbitrationAuditReport 对齐后端 liveops.GroupStat / ArbitrationAuditReport。
export type LiveopsGroupStat = { wins: number; losses: number; total: number; win_rate: number };
export type LiveopsArbitrationAuditReport = {
  world_id: string;
  turn_start: number;
  turn_end: number;
  paid: LiveopsGroupStat;
  non_paid: LiveopsGroupStat;
  issue_detected: boolean;
  redline_rate: number;
  sample_sufficient: boolean;
  note: string;
};

// injectWorldEvent 往某活世界注入一条 GM 世界事件（append-only、全量留审计）。运营态 X-Ops-Token。
export async function injectWorldEvent(
  worldID: string,
  input: GmWorldEventInput,
): Promise<GmWorldEventResult> {
  const data = await request<{ result?: GmWorldEventResult }>(
    `/api/ops/worlds/${encodeURIComponent(worldID)}/events`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        kind: input.kind,
        importance: input.importance,
        actor_id: input.actorId,
        target_id: input.targetId,
        region_id: input.regionId,
        payload: input.payload,
      }),
    },
    { withOps: true },
  );
  return data.result ?? { cross_event_id: "", audit_id: "", world_tick: 0 };
}

// createSeason 创建一个赛季（建世界 + 落 seasons）。运营态 X-Ops-Token。
export async function createSeason(input: LiveopsCreateSeasonInput): Promise<LiveopsSeason> {
  const data = await request<{ season?: LiveopsSeason }>(
    `/api/ops/seasons`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(input),
    },
    { withOps: true },
  );
  return (
    data.season ?? {
      id: "",
      world_id: "",
      name: "",
      status: "",
      started_at: "",
      ends_at: "",
      content_theme_id: "",
      created_at: "",
    }
  );
}

// finalizeSeason 收尾一个赛季（存活角色回流名人堂 + 世界封存）。运营态 X-Ops-Token。
export async function finalizeSeason(seasonID: string): Promise<LiveopsFinalizeResult> {
  const data = await request<{ result?: LiveopsFinalizeResult }>(
    `/api/ops/seasons/${encodeURIComponent(seasonID)}/finalize`,
    { method: "POST" },
    { withOps: true },
  );
  return (
    data.result ?? {
      season_id: seasonID,
      world_id: "",
      members_total: 0,
      archived: 0,
      archive_errors: [],
      sealed: false,
    }
  );
}

// fetchArbitrationAudit 扫某世界某回合区间的仲裁结局，按付费态分组算胜率、判 P2W 红线。运营态 X-Ops-Token。
export async function fetchArbitrationAudit(
  worldID: string,
  turnStart: number,
  turnEnd: number,
): Promise<LiveopsArbitrationAuditReport> {
  const params = new URLSearchParams({ turn_start: String(turnStart), turn_end: String(turnEnd) });
  const data = await request<{ report?: LiveopsArbitrationAuditReport }>(
    `/api/ops/worlds/${encodeURIComponent(worldID)}/arbitration-audit?${params.toString()}`,
    undefined,
    { withOps: true },
  );
  return (
    data.report ?? {
      world_id: worldID,
      turn_start: turnStart,
      turn_end: turnEnd,
      paid: { wins: 0, losses: 0, total: 0, win_rate: 0 },
      non_paid: { wins: 0, losses: 0, total: 0, win_rate: 0 },
      issue_detected: false,
      redline_rate: 0.6,
      sample_sufficient: false,
      note: "",
    }
  );
}

// ============ 角色档案（MMORPG 式只读「角色档案」面板 + 账号设置，命运客户端 CharacterSheet/AccountSettings 消费） ============

// UnitRelationView 对齐后端 session.RelationView（json tag 小写）：「她身边的人」的四轴关系。
// 四轴 clamp[-10,10]：trust 信任 / fear 惧 / affection 亲 / rivalry 仇（正=亲/信、负=惧/仇）。
export type UnitRelationView = {
  target_unit_id: string;
  target_name: string;
  trust: number;
  fear: number;
  affection: number;
  rivalry: number;
};

// getUnitRelations 拉某角色身边人的四轴关系（GET /api/units/:id/relations，按强度排序）。纯读、无副作用。
// 后端出错时把 error 并进 200 体（relations 为空数组），这里吞掉只取 relations。失败回空数组（不打断观察）。
export async function getUnitRelations(unitID: string): Promise<UnitRelationView[]> {
  const data = await request<{ relations?: UnitRelationView[]; error?: string }>(
    `/api/units/${encodeURIComponent(unitID)}/relations`,
  );
  return data.relations ?? [];
}

// changePassword 改账号密码（POST /api/accounts/change-password，Bearer 登录态）。
// 成功 → {ok:true}；旧密码错 / 新密码 <6 位 → 后端返 400 {error}，由 request 抛 APIError（带 message）。
// 改密成功后端会吊销全部会话令牌——调用方应据此触发登出（清本地 Bearer + 回登录）。
export async function changePassword(oldPassword: string, newPassword: string): Promise<{ ok: boolean }> {
  const data = await request<{ ok?: boolean }>(
    `/api/accounts/change-password`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ old_password: oldPassword, new_password: newPassword }),
    },
    { bearer: "require" },
  );
  return { ok: Boolean(data.ok) };
}

// ItemCatalogEntry 对齐后端 item.Definition（json tag 小写）：译名/标注/交易报价/装备属性用到的字段，其余忽略。
// price 是目录标价（买入即此价；卖出口径见 sellPriceOf）；attack/defense/move_bonus 是装备穿上后的属性加成。
export type ItemCatalogEntry = {
  id: string;
  display_name: string;
  category?: string;
  slot?: string;
  // 交易报价：price 为买价（与后端目录 price 同口径）；卖价由 sellPriceOf 据此折算（与后端 merchantSellPrice 一致）。
  price?: number;
  // 装备穿上后的属性加成（非装备/无加成为 0）：攻/防/移。
  attack_bonus?: number;
  defense_bonus?: number;
  move_bonus?: number;
};

// sellPriceOf 把目录买价折算成卖价（必须与后端 merchantSellPrice 一致：floor(price*0.8)，至少 1 文）。
// 前端显示的是**预估价**——成交以后端 summary_zh/wallet_after 为准。price 缺失（未拉到目录）返回 0（调用方退显「—」）。
export function sellPriceOf(price?: number): number {
  if (typeof price !== "number" || !Number.isFinite(price) || price <= 0) return 0;
  return Math.max(1, Math.floor(price * 0.8));
}

// getItemCatalogFull 拉物品目录并物化成 id→完整 entry 的 Map（交易报价/装备属性用，比 getItemCatalog 多带 price/加成）。
// 纯读、best-effort；失败回空 Map（调用方据此退显价格「—」、不显属性，绝不打断渲染）。不改既有 getItemCatalog 签名（别处在用）。
export async function getItemCatalogFull(): Promise<Map<string, ItemCatalogEntry>> {
  try {
    const data = await request<{ items?: ItemCatalogEntry[] }>(`/api/items/catalog`);
    const map = new Map<string, ItemCatalogEntry>();
    for (const it of data.items ?? []) {
      if (it && typeof it.id === "string" && it.id !== "") {
        map.set(it.id, it);
      }
    }
    return map;
  } catch {
    return new Map<string, ItemCatalogEntry>();
  }
}

// getItemCatalog 拉物品目录（GET /api/items/catalog）并物化成 id→中文名 的 Map（把 item_id 译成中文名）。
// 译不到的 id 由调用方退显原 id。纯读；失败回空 Map（调用方据此全退原 id，不打断渲染）。
export async function getItemCatalog(): Promise<Map<string, string>> {
  try {
    const data = await request<{ items?: ItemCatalogEntry[] }>(`/api/items/catalog`);
    const map = new Map<string, string>();
    for (const it of data.items ?? []) {
      if (it && typeof it.id === "string" && it.id !== "") {
        map.set(it.id, typeof it.display_name === "string" && it.display_name.trim() !== "" ? it.display_name : it.id);
      }
    }
    return map;
  } catch {
    // best-effort：目录拉取失败回空 Map，调用方全退原 id，不打断角色档案渲染。
    return new Map<string, string>();
  }
}

const visitorIDStorageKey = "qunxiang.visitor.id.v1";

// anonymousVisitorID 读取（或惰性生成并持久化）匿名访客 ID，用于漏斗去重统计。
function anonymousVisitorID(): string {
  try {
    const existing = window.localStorage.getItem(visitorIDStorageKey);
    if (existing && existing.trim() !== "") {
      return existing;
    }
  } catch {
    // localStorage 不可用：退回每次新建（仅本次会话有效）。
  }
  const generated =
    typeof crypto !== "undefined" && typeof crypto.randomUUID === "function"
      ? crypto.randomUUID()
      : `vid-${Date.now()}-${Math.random().toString(36).slice(2)}`;
  try {
    window.localStorage.setItem(visitorIDStorageKey, generated);
  } catch {
    // 忽略持久化失败。
  }
  return generated;
}
