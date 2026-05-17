/* 文件说明：前端会话 API 客户端，封装 HTTP/WS 请求、鉴权头注入与流式订阅回调。 */

import type {
  AccountLoginResult,
  AccountUser,
  AuditBundle,
  DialogueMessage,
  BattleUnit,
  DuelRoomStatus,
  ModerationReport,
  PrivacyEraseOptions,
  PrivacyEraseResult,
  PrivacyPurgeResult,
  SessionLog,
  SessionSnapshot,
  TerrainDefinition,
} from "./types";

const API_BASE =
  import.meta.env.VITE_API_BASE_URL ??
  (import.meta.env.DEV ? "http://127.0.0.1:8080" : window.location.origin);
const developerModeStorageKey = "qunxiang.developer.mode.v1";
let sessionRoleToken = "";
export type DirectiveScope = "doctrine" | "task" | "order";

type SessionStreamHandlers = {
  onSnapshot?: (snapshot: SessionSnapshot, meta: Record<string, unknown>) => void;
  onLog?: (entry: SessionLog) => void;
  onError?: (error: unknown) => void;
};

export class APIError extends Error {
  session?: SessionSnapshot;

  constructor(message: string, session?: SessionSnapshot) {
    super(message);
    this.name = "APIError";
    this.session = session;
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

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers ?? {});
  if (sessionRoleToken.trim() !== "") {
    headers.set("X-Session-Role-Token", sessionRoleToken.trim());
  }
  if (developerDebugEnabled()) {
    headers.set("X-Qunxiang-Debug", "1");
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
      const data = payload as { error?: string; session?: SessionSnapshot };
      throw new APIError(data.error ?? `${response.status} ${response.statusText}`, data.session);
    }
    throw new APIError(
      typeof payload === "string" && payload.trim() ? payload : `${response.status} ${response.statusText}`,
    );
  }

  return payload as T;
}

async function unwrapSession(path: string, init?: RequestInit): Promise<SessionSnapshot> {
  const response = await request<{ session: SessionSnapshot }>(path, init);
  return response.session;
}

// createSinglePlayerSession 请求后端创建单人会话。
export function createSinglePlayerSession(seed = Date.now(), unitCount = 5, mapSize: BattleMapSizeID = "small", fogOfWarEnabled = false, randomEventsEnabled = false): Promise<SessionSnapshot> {
  return unwrapSession(`/api/sessions/single-player?seed=${seed}&unit_count=${unitCount}&map_size=${encodeURIComponent(mapSize)}&fog_of_war=${fogOfWarEnabled ? "true" : "false"}&random_events=${randomEventsEnabled ? "true" : "false"}`, {
	method: "POST",
  });
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
}>(`/api/sessions/duel?seed=${seed}&unit_count=${unitCount}&map_size=${encodeURIComponent(mapSize)}&fog_of_war=${fogOfWarEnabled ? "true" : "false"}&random_events=${randomEventsEnabled ? "true" : "false"}&creator_role=${creatorRole}`, { method: "POST" });
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
export function advancePhase(sessionID: string): Promise<SessionSnapshot> {
  return unwrapSession(`/api/sessions/${encodeURIComponent(sessionID)}/advance-phase`, {
    method: "POST",
  });
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
  const result = await request<{ audit: AuditBundle }>(
    `/api/sessions/${encodeURIComponent(sessionID)}/audit?limit=${limit}`,
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
  });
  return response.result;
}

export async function registerAccount(payload: {
  username: string;
  display_name?: string;
  password: string;
}): Promise<{ user: AccountUser; auth: AccountLoginResult }> {
  return request<{ user: AccountUser; auth: AccountLoginResult }>(`/api/accounts/register`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify(payload),
  });
}

export async function loginAccount(payload: {
  username: string;
  password: string;
}): Promise<{ user: AccountUser; auth: AccountLoginResult }> {
  return request<{ user: AccountUser; auth: AccountLoginResult }>(`/api/accounts/login`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify(payload),
  });
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
  const response = await request<{ ok: boolean }>(`/api/accounts/logout`, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${token}`,
    },
  });
  return response.ok;
}
