/* 文件说明：独立 GM 管理后台（#admin）的专属 API 客户端，与游戏客户端的 session/api.ts 完全解耦。
   - 自持 ops-token（X-Ops-Token）：从 localStorage 恢复、setAdminOpsToken 持久化；所有请求自动带头。
   - 所有 GM 后台端点都套后端 opsTokenGuard（QUNXIANG_OPS_TOKEN）：未配 token 后端放行，配了需正确 X-Ops-Token 否则 403。
   - 端点分两类：①已落地（ops 看板/GM 注入/赛季/零和审计/世界列表/世界 Boss/村庄播种走 bootstrap）；
     ②尚待后端落地（/api/admin/flags 运行时开关、worlds-detail region/人口、region 威胁度）——
     这些 wrapper 已按约定路径/载荷写好，后端接线后即生效（见 AdminApp 顶部 crossFileNeeds 注释）。
   不 import session/api.ts（零并发冲突）；类型自声明，对齐后端 json tag / liveops 结构。 */

// API_BASE 与 session/api.ts 同口径：优先 VITE_API_BASE_URL，dev 默认本地 8080，生产用同源。
const API_BASE =
  import.meta.env.VITE_API_BASE_URL ??
  (import.meta.env.DEV ? "http://127.0.0.1:8080" : window.location.origin);

// adminOpsTokenStorageKey 独立于游戏客户端的 ops token（运营人员在 #admin 单独登录）。
const adminOpsTokenStorageKey = "qunxiang.admin.ops.token.v1";

// 模块级 ops token，从 localStorage 恢复以跨刷新保活。
let adminOpsToken = "";
try {
  adminOpsToken = window.localStorage.getItem(adminOpsTokenStorageKey) ?? "";
} catch {
  adminOpsToken = "";
}

// AdminAPIError 携带 HTTP 状态码，供上层区分 401/403（需填/换 ops token）与其它失败。
export class AdminAPIError extends Error {
  status?: number;
  constructor(message: string, status?: number) {
    super(message);
    this.name = "AdminAPIError";
    this.status = status;
  }
}

// setAdminOpsToken 设置并持久化运营令牌（传空清除=登出）。
export function setAdminOpsToken(token: string): void {
  adminOpsToken = token.trim();
  try {
    if (adminOpsToken === "") {
      window.localStorage.removeItem(adminOpsTokenStorageKey);
    } else {
      window.localStorage.setItem(adminOpsTokenStorageKey, adminOpsToken);
    }
  } catch {
    // localStorage 不可用（隐私模式）时忽略——内存态仍生效。
  }
}

// getAdminOpsToken 读取当前运营令牌（已登录非空）。
export function getAdminOpsToken(): string {
  return adminOpsToken;
}

// hasAdminOpsToken 是登录门判定：是否已填过 ops token。
export function hasAdminOpsToken(): boolean {
  return adminOpsToken.trim() !== "";
}

// request 是统一请求器：注入 X-Ops-Token + Content-Type，非 2xx 抛 AdminAPIError（带 status）。
async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers ?? {});
  if (adminOpsToken.trim() !== "") {
    headers.set("X-Ops-Token", adminOpsToken.trim());
  }
  if (init?.body && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  let response: Response;
  try {
    response = await fetch(`${API_BASE}${path}`, { ...init, headers });
  } catch (error) {
    throw new AdminAPIError(
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
    const msg =
      payload && typeof payload === "object" && typeof (payload as { error?: string }).error === "string"
        ? (payload as { error: string }).error
        : typeof payload === "string" && payload.trim()
          ? payload
          : `${response.status} ${response.statusText}`;
    throw new AdminAPIError(msg, response.status);
  }
  return payload as T;
}

// ============ ① 运行时 flag 开关（头牌；后端 featureflags.SnapshotEffective 已就绪，HTTP 路由 /api/admin/flags 待接线） ============

// AdminFlag 对齐后端 featureflags.EffectiveFlag（嵌入 FlagSpec，**均无 json tag → Go 大写键名**）。
//   - Name/Description/DefaultOn/Values：flag 静态规格（Values 非空=多档字符串型，空=布尔型）。
//   - OverrideSet/OverrideValue：是否设了运行时 override 及其原始值。
//   - EnvValue：os.Getenv 原始值（GM 没动手时的底值）。
//   - Effective：EnvOrOverride 实际生效的原始字符串值。
// 后端预期直接序列化 SnapshotEffective() 数组，故消费方按大写键名取。
export type AdminFlag = {
  Name: string;
  Description: string;
  DefaultOn: boolean;
  Values: string[] | null;
  OverrideSet: boolean;
  OverrideValue: string;
  EnvValue: string;
  Effective: string;
};

// flagTruthy 复刻后端布尔解析约定（true/1/yes/on 不分大小写视为开），用于把 Effective 字符串判定为开/关。
export function flagTruthy(value: string): boolean {
  switch (value.trim().toLowerCase()) {
    case "true":
    case "1":
    case "yes":
    case "on":
      return true;
    default:
      return false;
  }
}

// flagIsMultiValue 判定一个 flag 是否多档字符串型（Values 非空），否则按布尔开关渲染。
export function flagIsMultiValue(flag: AdminFlag): boolean {
  return Array.isArray(flag.Values) && flag.Values.length > 0;
}

// listAdminFlags 拉所有可运营游戏 flag 的当前生效态（GET /api/admin/flags，直接序列化 SnapshotEffective()）。
export async function listAdminFlags(): Promise<AdminFlag[]> {
  const data = await request<{ flags?: AdminFlag[] }>(`/api/admin/flags`);
  return data.flags ?? [];
}

// setAdminFlagOverride 运行时覆盖某 flag（POST /api/admin/flags，body {name, value}）。
// value 是原始字符串：布尔型传 "on"/"off"，多档型传具体档名（如 "per_session"）。返回覆盖后的最新态。
export async function setAdminFlagOverride(name: string, value: string): Promise<AdminFlag | null> {
  const data = await request<{ flag?: AdminFlag }>(`/api/admin/flags`, {
    method: "POST",
    body: JSON.stringify({ name, value }),
  });
  return data.flag ?? null;
}

// clearAdminFlagOverride 清除运行时覆盖、回落 env 默认（DELETE /api/admin/flags?name=）。返回回落后的最新态。
export async function clearAdminFlagOverride(name: string): Promise<AdminFlag | null> {
  const data = await request<{ flag?: AdminFlag }>(
    `/api/admin/flags?name=${encodeURIComponent(name)}`,
    { method: "DELETE" },
  );
  return data.flag ?? null;
}

// ============ ①b 可运营数值配置（runtimeconfig；后端 runtimeconfig.SnapshotEffective + /api/admin/config 路由已接线） ============

// AdminConfigItem 对齐后端 runtimeconfig.EffectiveParam（嵌入 ParamSpec 与 EffectiveParam 自有字段**均无 json tag**
// → 全部序列化为 Go 大写键名，与 AdminFlag 同口径）。
//   - Name/Namespace/Type/Default/Min/Max/Values/Description/HotReload：参数静态规格（来自嵌入的 ParamSpec，大写键名）。
//     · Type ∈ {bool,int,float,enum,string}；Min/Max 仅数值型有值（*float64，nil → null）；Values 仅 enum 非空。
//   - OverrideSet/OverrideValue/Effective：运行时态（EffectiveParam 自有字段，无 tag → **大写** OverrideSet/OverrideValue/Effective）。
// 后端直接序列化 SnapshotEffective() 数组，故消费方按上述全大写键名取。
export type AdminConfigItem = {
  Name: string;
  Namespace: string;
  Type: string;
  Default: string;
  Min: number | null;
  Max: number | null;
  Values: string[] | null;
  Description: string;
  HotReload: boolean;
  OverrideSet: boolean;
  OverrideValue: string;
  Effective: string;
};

// listAdminConfig 拉所有可运营数值/枚举参数的当前生效态（GET /api/admin/config，直接序列化 SnapshotEffective()）。
export async function listAdminConfig(): Promise<AdminConfigItem[]> {
  const data = await request<{ params?: AdminConfigItem[] }>(`/api/admin/config`);
  return data.params ?? [];
}

// setAdminConfig 运行时覆盖某参数（POST /api/admin/config，body {name, value}）。value 是原始字符串
// （bool 传 "on"/"off"，int/float 传数值串，enum 传档名，string 传原文）。返回覆盖后的最新态。
export async function setAdminConfig(name: string, value: string): Promise<AdminConfigItem | null> {
  const data = await request<{ param?: AdminConfigItem }>(`/api/admin/config`, {
    method: "POST",
    body: JSON.stringify({ name, value }),
  });
  return data.param ?? null;
}

// clearAdminConfig 清除运行时覆盖、回落注册默认（DELETE /api/admin/config?name=）。返回回落后的最新态。
export async function clearAdminConfig(name: string): Promise<AdminConfigItem | null> {
  const data = await request<{ param?: AdminConfigItem }>(
    `/api/admin/config?name=${encodeURIComponent(name)}`,
    { method: "DELETE" },
  );
  return data.param ?? null;
}

// ============ ①c 操作者与审计（runtimeconfig/ops 操作者表；HTTP 路由 /api/admin/operators · /audit · /whoami 待接线） ============

// OpsOperator 对齐后端操作者记录（json tag）：名/角色/创建时间。token 仅创建时一次性返回，列表不含。
export type OpsOperator = { name: string; role: string; created_at: string };

// OpsAuditRow 对齐后端 ops 审计行（json tag）：谁/什么角色/做了什么动作/作用对象/何时。
export type OpsAuditRow = {
  operator: string;
  role: string;
  action: string;
  target: string;
  created_at: string;
};

// listOperators 列出全部操作者（GET /api/admin/operators）。
export async function listOperators(): Promise<OpsOperator[]> {
  const data = await request<{ operators?: OpsOperator[] }>(`/api/admin/operators`);
  return data.operators ?? [];
}

// upsertOperator 新增/更新一名操作者（POST /api/admin/operators，body {name, role, token}）。token 仅此次提交，
// 后端落库（哈希）后不再回显——前端提交后须提示运营自行保存。
export async function upsertOperator(name: string, role: string, token: string): Promise<void> {
  await request<unknown>(`/api/admin/operators`, {
    method: "POST",
    body: JSON.stringify({ name, role, token }),
  });
}

// deleteOperator 删除一名操作者（DELETE /api/admin/operators?name=）。
export async function deleteOperator(name: string): Promise<void> {
  await request<unknown>(`/api/admin/operators?name=${encodeURIComponent(name)}`, {
    method: "DELETE",
  });
}

// listOpsAudit 拉最近的操作审计（GET /api/admin/audit?limit=）。
export async function listOpsAudit(limit = 50): Promise<OpsAuditRow[]> {
  const data = await request<{ audit?: OpsAuditRow[] }>(`/api/admin/audit?limit=${limit}`);
  return data.audit ?? [];
}

// whoami 读当前 ops-token 对应的操作者身份（GET /api/admin/whoami）。
export async function whoami(): Promise<{ name: string; role: string }> {
  const data = await request<{ name?: string; role?: string }>(`/api/admin/whoami`);
  return { name: data.name ?? "", role: data.role ?? "" };
}

// ============ ② 世界配置（列表已落地；region 详情/威胁度待后端落地） ============

// AdminWorld 对齐后端 world.World（基本列表 GET /api/worlds，Go 默认大写 json 键名，无 tag）。
export type AdminWorld = {
  ID: string;
  Name: string;
  Status: string;
  Tick: number;
  MaxPopulation: number;
  RegionSeed: string;
  CreatedAt: string;
};

// AdminRegionDetail 对齐后端 session.RegionDetail（json tag）：region 的活跃度档 / 威胁等级 / 逻辑时钟。
export type AdminRegionDetail = {
  id: string;
  world_id: string;
  activity_tier: string;
  threat_level: number;
  last_tick: number;
};

// AdminWorldDetail 对齐后端 session.WorldDetail（json tag，扁平结构含 region 概览与人口数）。
export type AdminWorldDetail = {
  id: string;
  name: string;
  status: string;
  tick: number;
  max_population: number;
  population: number; // 已接入成员数（world_members）
  regions: AdminRegionDetail[] | null;
};

// listWorlds 列出活跃世界（GET /api/worlds，已落地基本列表，无 region/人口）。
export async function listWorlds(): Promise<AdminWorld[]> {
  const data = await request<{ worlds?: AdminWorld[] }>(`/api/worlds`);
  return data.worlds ?? [];
}

// listWorldsDetail 列出世界 + region/人口详情（GET /api/admin/worlds-detail；后端 session.ListWorldsDetail
// 域层已就绪，HTTP 路由待接线）。后端未接线时调用方回退到 listWorlds（AdminApp 已做）。
export async function listWorldsDetail(): Promise<AdminWorldDetail[]> {
  const data = await request<{ worlds?: AdminWorldDetail[] }>(`/api/admin/worlds-detail`);
  return data.worlds ?? [];
}

// setRegionThreat 把某世界某 region 的威胁等级绝对置位到 level（POST /api/admin/worlds/:worldId/regions/:regionId/threat；
// 后端 session.SetRegionThreatLevel 域层已就绪，HTTP 路由待接线）。返回置位后的新威胁等级。
export async function setRegionThreat(
  worldId: string,
  regionId: string,
  threatLevel: number,
): Promise<number> {
  const data = await request<{ threat_level?: number }>(
    `/api/admin/worlds/${encodeURIComponent(worldId)}/regions/${encodeURIComponent(regionId)}/threat`,
    { method: "POST", body: JSON.stringify({ level: threatLevel }) },
  );
  return data.threat_level ?? threatLevel;
}

// SeedVillageResult 是村庄播种回执（后端 session.SeedWorldVillage 返回新增村民数）。
export type SeedVillageResult = { seeded: number };

// seedVillage 在某世界触发一次村庄播种（POST /api/admin/worlds/:worldId/seed-village；
// 后端 session.SeedWorldVillage 域层已就绪，HTTP 路由待接线）。
export async function seedVillage(
  worldId: string,
  sessionId?: string,
  factionId?: string,
  seed?: number,
): Promise<SeedVillageResult> {
  const data = await request<{ result?: SeedVillageResult; seeded?: number }>(
    `/api/admin/worlds/${encodeURIComponent(worldId)}/seed-village`,
    { method: "POST", body: JSON.stringify({ session_id: sessionId, faction_id: factionId, seed }) },
  );
  return data.result ?? { seeded: data.seeded ?? 0 };
}

// ============ ②b 阵营概览（GM 只读，三阵营开放世界 F3；后端 session.ListFactionsDetail，GET /api/admin/factions） ============

// AdminFactionTriaxis 对齐后端 session.Triaxis（json tag）：道德基准三维（freedom/order/chaos，各 [0,100]）。
export type AdminFactionTriaxis = {
  freedom: number;
  order: number;
  chaos: number;
};

// AdminFactionDetail 对齐后端 session.FactionDetail（json tag）：阵营标识 + 中文名 + 信条 + 基准 + 据点 + 人口。
export type AdminFactionDetail = {
  id: string; // 阵营 ID（freedom/order/chaos）
  name_zh: string; // 中文名（自由/秩序/混乱）
  moral_creed: string; // 道德信条
  baseline: AdminFactionTriaxis; // 道德基准
  spawn_points: string[] | null; // 出生据点 region ID 集合
  population: number; // 当前人口（属该阵营的 units 计数，best-effort）
};

// listFactionsDetail 拉三阵营概览（GET /api/admin/factions；后端 session.ListFactionsDetail 已就绪）。
// 后端未接线（404）时调用方回退到空列表（FactionPanel 已做友好降级提示）。
export async function listFactionsDetail(): Promise<AdminFactionDetail[]> {
  const data = await request<{ factions?: AdminFactionDetail[] }>(`/api/admin/factions`);
  return data.factions ?? [];
}

// ============ ③ GM 世界事件注入（已落地：POST /api/ops/worlds/:worldId/events） ============

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

// injectWorldEvent 往某活世界注入一条 GM 世界事件（append-only、全量留审计）。
export async function injectWorldEvent(
  worldId: string,
  input: GmWorldEventInput,
): Promise<GmWorldEventResult> {
  const data = await request<{ result?: GmWorldEventResult }>(
    `/api/ops/worlds/${encodeURIComponent(worldId)}/events`,
    {
      method: "POST",
      body: JSON.stringify({
        kind: input.kind,
        importance: input.importance,
        actor_id: input.actorId,
        target_id: input.targetId,
        region_id: input.regionId,
        payload: input.payload,
      }),
    },
  );
  return data.result ?? { cross_event_id: "", audit_id: "", world_tick: 0 };
}

// ============ ④ 赛季（已落地：POST /api/ops/seasons / :id/finalize） ============

export type CreateSeasonInput = {
  name: string;
  world_name?: string;
  content_theme_id?: string;
  max_population?: number;
  region_seed?: string;
};
export type Season = {
  id: string;
  world_id: string;
  name: string;
  status: string;
  started_at: string;
  ends_at: string;
  content_theme_id: string;
  created_at: string;
};
export type FinalizeResult = {
  season_id: string;
  world_id: string;
  members_total: number;
  archived: number;
  archive_errors: string[];
  sealed: boolean;
};

// createSeason 创建一个赛季（建世界 + 落 seasons）。
export async function createSeason(input: CreateSeasonInput): Promise<Season> {
  const data = await request<{ season?: Season }>(`/api/ops/seasons`, {
    method: "POST",
    body: JSON.stringify(input),
  });
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

// finalizeSeason 收尾一个赛季（存活角色回流名人堂 + 世界封存）。
export async function finalizeSeason(seasonId: string): Promise<FinalizeResult> {
  const data = await request<{ result?: FinalizeResult }>(
    `/api/ops/seasons/${encodeURIComponent(seasonId)}/finalize`,
    { method: "POST" },
  );
  return (
    data.result ?? {
      season_id: seasonId,
      world_id: "",
      members_total: 0,
      archived: 0,
      archive_errors: [],
      sealed: false,
    }
  );
}

// listSeasons 列出赛季（GET /api/ops/seasons，待后端落地）。
// 后端未落地时 AdminApp 仅展示本会话内刚创建的赛季（本地态）。
export async function listSeasons(): Promise<Season[]> {
  const data = await request<{ seasons?: Season[] }>(`/api/ops/seasons`);
  return data.seasons ?? [];
}

// ============ ④b 内容运营（母题库 / 翻译模板 / SKU 目录；后端 /api/admin/content-themes ·
//                /api/admin/translation-templates · /api/admin/skus · /api/billing/skus 已接线） ============

// ContentTheme 对齐后端 season_content_themes（json tag，全小写键名）：
//   - id：母题 ID（POST 时留空则后端新建）。
//   - season_id：归属赛季 ID。
//   - decisive_event_ids / title_ids / landmark_names：本季的关键事件 / 头衔 / 地标名集合（字符串数组）。
//   - created_at：创建时间（只读）。
export type ContentTheme = {
  id: string;
  season_id: string;
  decisive_event_ids: string[];
  title_ids: string[];
  landmark_names: string[];
  created_at: string;
};

// TranslationTemplate 对齐后端 translation_templates（json tag，全小写键名）：
//   - id：模板 ID（后端按 reason_code+anchor_kind upsert，写后即时生效）。
//   - reason_code / anchor_kind：联合主键——事件 reason code + 关系锚类型。
//   - narrative_template：叙事模板文案。
//   - force_pending：是否强制走待决策（高光卡/收件箱）而非自治。
//   - priority：优先级（越大越靠前）。
//   - updated_at：更新时间（只读）。
export type TranslationTemplate = {
  id: string;
  reason_code: string;
  anchor_kind: string;
  narrative_template: string;
  force_pending: boolean;
  priority: number;
  updated_at: string;
};

// SKU 对齐后端 billing_skus（json tag，全小写键名）：
//   - id：SKU ID（POST 时留空则后端新建）。
//   - kind：类型（如订阅 / 一次性 / 内购档）。
//   - name：展示名。
//   - price_cents：定价（分）。
//   - period：周期（如 month / once）。
//   - active：是否上架。
//   - created_at：创建时间（只读）。
export type SKU = {
  id: string;
  kind: string;
  name: string;
  price_cents: number;
  period: string;
  active: boolean;
  created_at: string;
};

// listContentThemes 列出所有内容母题（GET /api/admin/content-themes）。
export async function listContentThemes(): Promise<ContentTheme[]> {
  const data = await request<{ themes?: ContentTheme[] }>(`/api/admin/content-themes`);
  return data.themes ?? [];
}

// upsertContentTheme 新增/更新一条母题（POST /api/admin/content-themes，id 空则后端新建）。返回（新）id。
export async function upsertContentTheme(theme: ContentTheme): Promise<string> {
  const data = await request<{ id?: string }>(`/api/admin/content-themes`, {
    method: "POST",
    body: JSON.stringify(theme),
  });
  return data.id ?? theme.id;
}

// deleteContentTheme 删除一条母题（DELETE /api/admin/content-themes?id=）。
export async function deleteContentTheme(id: string): Promise<void> {
  await request<unknown>(`/api/admin/content-themes?id=${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

// listTranslationTemplates 列出所有翻译模板（GET /api/admin/translation-templates）。
export async function listTranslationTemplates(): Promise<TranslationTemplate[]> {
  const data = await request<{ templates?: TranslationTemplate[] }>(`/api/admin/translation-templates`);
  return data.templates ?? [];
}

// upsertTranslationTemplate 新增/更新一条翻译模板（POST /api/admin/translation-templates，
// 按 reason_code+anchor_kind upsert，写后即时生效）。
export async function upsertTranslationTemplate(tpl: TranslationTemplate): Promise<void> {
  await request<unknown>(`/api/admin/translation-templates`, {
    method: "POST",
    body: JSON.stringify(tpl),
  });
}

// deleteTranslationTemplate 删除一条翻译模板（DELETE /api/admin/translation-templates?reason_code=&anchor_kind=）。
export async function deleteTranslationTemplate(reasonCode: string, anchorKind: string): Promise<void> {
  await request<unknown>(
    `/api/admin/translation-templates?reason_code=${encodeURIComponent(reasonCode)}&anchor_kind=${encodeURIComponent(anchorKind)}`,
    { method: "DELETE" },
  );
}

// listSKUs 列出计费 SKU 目录（GET /api/billing/skus，公开端点）。兼容后端返回 {skus:[]} 或裸数组两种形态。
// billing 关闭时该 GET 可能 404/503——调用方据抛错降级提示（ContentPanel 已做）。
export async function listSKUs(): Promise<SKU[]> {
  const data = await request<{ skus?: SKU[] } | SKU[]>(`/api/billing/skus`);
  if (Array.isArray(data)) {
    return data;
  }
  return data.skus ?? [];
}

// upsertSKU 新增/更新一条 SKU（POST /api/admin/skus，id 空则后端新建）。billing 关闭时后端返 503。返回（新）id。
export async function upsertSKU(sku: SKU): Promise<string> {
  const data = await request<{ id?: string }>(`/api/admin/skus`, {
    method: "POST",
    body: JSON.stringify(sku),
  });
  return data.id ?? sku.id;
}

// ============ ⑤ 零和审计（已落地：GET /api/ops/worlds/:worldId/arbitration-audit） ============

export type GroupStat = { wins: number; losses: number; total: number; win_rate: number };
export type ArbitrationAuditReport = {
  world_id: string;
  turn_start: number;
  turn_end: number;
  paid: GroupStat;
  non_paid: GroupStat;
  issue_detected: boolean;
  redline_rate: number;
  sample_sufficient: boolean;
  note: string;
};

// fetchArbitrationAudit 扫某世界某回合区间的仲裁结局，按付费态分组算胜率、判 P2W 红线。
export async function fetchArbitrationAudit(
  worldId: string,
  turnStart: number,
  turnEnd: number,
): Promise<ArbitrationAuditReport> {
  const params = new URLSearchParams({ turn_start: String(turnStart), turn_end: String(turnEnd) });
  const data = await request<{ report?: ArbitrationAuditReport }>(
    `/api/ops/worlds/${encodeURIComponent(worldId)}/arbitration-audit?${params.toString()}`,
  );
  return (
    data.report ?? {
      world_id: worldId,
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

// ============ ⑥ 监控嵌入（已落地：ops 看板三报 + 成本） ============

// ProviderCost / CostDashboardData 对齐后端 CostDashboardData（json tag）。
export type ProviderCost = {
  provider: string;
  calls: number;
  cost_usd: number;
  total_tokens: number;
  fallback_hits: number;
};
export type CostDashboardData = {
  generated_at?: string;
  total_cost_usd: number;
  total_interactions: number;
  total_tokens: number;
  fallback_count: number;
  fallback_rate: number;
  cost_per_session_usd: number;
  distinct_sessions: number;
  units_total: number;
  by_provider?: Record<string, ProviderCost>;
  units_by_life_state?: Record<string, number>;
};

// NorthStarReport 对齐后端北极星报表（json tag）。
export type NorthStarReport = {
  generated_at?: string;
  surprise_hit_rate: number;
  ooc_rate: number;
  inbox_process_rate: number;
  share_initiated: number;
  purchases: number;
  return_visits: number;
  sessions_created: number;
  characters_created: number;
  decision_pending: number;
  decision_resolved: number;
  fate_react_expected: number;
  fate_react_surprise: number;
  fate_react_ooc: number;
};

// ProductFunnelReport 对齐后端 AARRR 漏斗（json tag）。
export type ProductFunnelReport = {
  generated_at?: string;
  distinct_sessions: number;
  by_stage?: Record<string, number>;
  by_event?: Record<string, number>;
};

// fetchCostDashboard 读运营成本/单位经济仪表盘（最近 days 天，默认 30；days<=0 视为全量）。
export async function fetchCostDashboard(days = 30): Promise<CostDashboardData> {
  return request<CostDashboardData>(`/api/ops/cost-dashboard?days=${days}`);
}

// fetchNorthStar 读北极星指标。days 缺省/<=0 视为全量。
export async function fetchNorthStar(days?: number): Promise<NorthStarReport> {
  const qs = typeof days === "number" ? `?days=${days}` : "";
  return request<NorthStarReport>(`/api/ops/north-star${qs}`);
}

// fetchProductFunnel 读 AARRR 产品漏斗。days 缺省/<=0 视为全量。
export async function fetchProductFunnel(days?: number): Promise<ProductFunnelReport> {
  const qs = typeof days === "number" ? `?days=${days}` : "";
  return request<ProductFunnelReport>(`/api/ops/product-funnel${qs}`);
}

// ============ ⑦ 客户管理（账户/角色/权益/合规；后端 /api/admin/clients 已接线，读端 operator 级·写端 admin 级） ============

// AdminClient 对齐后端 AdminUser（json tag，全小写键名）：账户标识 + 展示名 + 封禁态 + 创建时间。
//   - 列表行与详情里的 account 同型。banned=true 表示该账户已被封禁。
export type AdminClient = {
  id: string;
  username: string;
  display_name: string;
  banned: boolean;
  created_at: string;
};

// ClientCharacter 对齐后端 CharacterSummary（json tag）：该账户名下一个角色（一局命运）的概览。
//   - session_id/world_id：归属会话与世界；turn：当前回合；hero_name：主角名；life_state：生命状态（如存活/陨落）。
export type ClientCharacter = {
  session_id: string;
  world_id: string;
  turn: number;
  hero_name: string;
  life_state: string;
  created_at: string;
  updated_at: string;
};

// ClientEntitlement 对齐后端 Entitlement（json tag）：该账户的一项充值权益（SKU 授予态）。
//   - account_id/sku_id：归属账户与 SKU；status：权益状态（如 active/expired/revoked）；granted_at/expires_at：起止时间。
export type ClientEntitlement = {
  account_id: string;
  sku_id: string;
  status: string;
  granted_at: string;
  expires_at: string;
};

// ClientCompliance 对齐后端 ComplianceStatus（json tag）：该账户的实名/防沉迷合规态。
//   - realname_verified：是否实名；minor_mode：是否未成年模式；day_bucket：当日时段桶；daily_play_seconds：当日已玩秒数。
export type ClientCompliance = {
  account_id: string;
  birth_date: string;
  realname_verified: boolean;
  minor_mode: boolean;
  day_bucket: string;
  daily_play_seconds: number;
};

// ClientDetail 对齐后端 GET /api/admin/clients/:id 返回体：账号 + 角色摘要 + 权益 + 合规四块。
export type ClientDetail = {
  account: AdminClient;
  characters: ClientCharacter[];
  entitlements: ClientEntitlement[];
  compliance: ClientCompliance | null;
};

// listClients 模糊检索客户（GET /api/admin/clients?q=&limit=，q 匹配 username/display_name 或 id 精确，limit 默认 100）。
// 读端是 operator 级（opsWriter）——未配 ops 鉴权时可能 503，调用方据抛错降级提示（ClientPanel 已做）。
export async function listClients(q = "", limit = 100): Promise<AdminClient[]> {
  const params = new URLSearchParams();
  if (q.trim() !== "") params.set("q", q.trim());
  params.set("limit", String(limit));
  const data = await request<{ clients?: AdminClient[] }>(`/api/admin/clients?${params.toString()}`);
  return data.clients ?? [];
}

// getClientDetail 拉单个客户详情（GET /api/admin/clients/:id）：账号 + 角色 + 权益 + 合规。
export async function getClientDetail(id: string): Promise<ClientDetail> {
  const data = await request<ClientDetail>(`/api/admin/clients/${encodeURIComponent(id)}`);
  return {
    account: data.account ?? { id, username: "", display_name: "", banned: false, created_at: "" },
    characters: data.characters ?? [],
    entitlements: data.entitlements ?? [],
    compliance: data.compliance ?? null,
  };
}

// setClientBanned 封禁/解封一个账户（POST /api/admin/clients/:id/ban，body {banned}，admin 级）。返回封禁后的最新态。
export async function setClientBanned(id: string, banned: boolean): Promise<boolean> {
  const data = await request<{ ok?: boolean; banned?: boolean }>(
    `/api/admin/clients/${encodeURIComponent(id)}/ban`,
    { method: "POST", body: JSON.stringify({ banned }) },
  );
  return data.banned ?? banned;
}

// eraseClientData 按账户不可逆擦除其全部会话数据（POST /api/admin/clients/:id/erase，admin 级）。返回被擦除的会话数。
export async function eraseClientData(id: string): Promise<number> {
  const data = await request<{ erased_sessions?: number }>(
    `/api/admin/clients/${encodeURIComponent(id)}/erase`,
    { method: "POST" },
  );
  return data.erased_sessions ?? 0;
}

// refundClient 撤销该账户权益/退款（POST /api/admin/clients/:id/refund，body {sku_id?}，admin 级）。
// 不传 sku_id 表示撤销全部权益。billing 关闭时后端返 503。返回被撤销的权益条数。
export async function refundClient(id: string, skuId?: string): Promise<number> {
  const body = skuId && skuId.trim() !== "" ? JSON.stringify({ sku_id: skuId.trim() }) : JSON.stringify({});
  const data = await request<{ revoked?: number }>(
    `/api/admin/clients/${encodeURIComponent(id)}/refund`,
    { method: "POST", body },
  );
  return data.revoked ?? 0;
}

// errText 把错误归一为可展示文案，鉴权类错误（401/403）额外提示填/换 X-Ops-Token。
export function errText(err: unknown): string {
  if (err instanceof AdminAPIError) {
    const parts = [err.message];
    if (typeof err.status === "number") parts.push(`(HTTP ${err.status})`);
    if (err.status === 401 || err.status === 403) parts.push("— ops-token 可能无效或缺失");
    return parts.join(" ");
  }
  return err instanceof Error ? err.message : String(err);
}
