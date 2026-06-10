/* 文件说明：命运地图舞台（混合模型：观战为主 + 在线直驱）——把战棋的 PixiBoard 搬进命运客户端作她日常生活的主舞台。
   她随世界推进在六边形格子上移动；玩家是垂看的先祖，但**上线时可直接操作她**：
   - 点格子弹「这是什么地方 + 这里有谁」浮卡，附「让她去这里」直驱移动按钮；
   - 浮卡同时 best-effort 拉该格**动作目录**（getTileAffordances，失败静默回退只读展示——旧后端 404 时面板不能坏），
     渲染采集/收割/锻造/探遭遇/交谈/交易等动作按钮：不可用置灰带 reason_zh 小字；
     （共享大世界：玩家不能在共用地图上建造/拆除建筑，故无建造/拆除；收割/锻造仅「使用」世界已有的己方设施。）
   - 直发动作（gather/harvest/forge/upgrade）→ executeTileAction，结算摘要+明细内嵌小卡展示；
   - POI 遭遇（poi_encounter）→ resolvePOIEncounter；撞上行商则展开交易小面板（买侧=行商货单，卖侧=她的行囊）；
   - 任何 NPC 交易都买+卖双向对称（Bug1/Bug2）：买侧读对方背包查目录得买价（price）；卖侧读她行囊、每件预显
     预估卖价 floor(price*0.8)（与后端 merchantSellPrice 一致）。买价/卖价均为预估，成交以后端权威结算为准；
   - 交谈：同阵营单位复用战棋 /dialogue 链路（talkToUnit）弹简易输入框；据点 NPC/野外散人不在该链路
     鉴权范围内（router.go 校验 commander faction），降级把「与TA交谈」预填进指引草稿。
   结果叙事同时经 WS fate_life_beat 冒进命运 feed（父层）；面板内嵌小卡是即时反馈、可「×」关掉。
   数据来自 GET /api/sessions/:id 的整块快照（getSession），与 FateView 文字命运卡同源同一会话。
   刷新节奏：自身挂载即拉一次 + 每隔若干秒轻量轮询；另接受 refreshSignal——父层在「世界往前走一拍」执行完后
   bump 该值，FateBoard 即重拉快照，board 随她移动重渲（PixiBoard 数据变化自动重渲）。
   祖魂语气/宣纸墨色：本文件不可改 fate.css/styles.css，故浮卡/动作区/交易卡均用内联样式贴合 .fate-* 墨色调。*/

import { Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  challengeZoneBoss,
  enterZoneDungeon,
  executeTileAction,
  getItemCatalogFull,
  getMapPOIs,
  getSession,
  getTileAffordances,
  getZones,
  moveUnit,
  resolvePOIEncounter,
  sellPriceOf,
  talkToUnit,
  tradeWithUnit,
} from "../session/api";
import type {
  DungeonResult,
  EliteEncounterResult,
  ItemCatalogEntry,
  MapPOI,
  MerchantGood,
  POIEncounterResult,
  TileAction,
  TileAffordances,
  ZoneSummary,
} from "../session/api";
import type { BattleUnit, InventoryItem, SessionSnapshot } from "../session/types";

// LazyPixiBoard 与 App.tsx 同款懒加载 PixiBoard：让 Pixi 战场代码留在独立 chunk（不并进命运首屏主包），
// 既复用 App 的代码分割收益，也消除「同一模块被静态+动态双导入」的打包合并告警。
const LazyPixiBoard = lazy(() => import("../game/PixiBoard").then((m) => ({ default: m.PixiBoard })));

type Props = {
  sessionId: string;
  // unitId：主角的单位 ID，用于在浮卡里标注「这就是她」+ 动作目录/直驱动作以她为行动者。
  unitId: string;
  // refreshSignal：父层每推世界往前一拍并执行完后 bump 此值，FateBoard 据此重拉快照让 board 随她移动重渲。
  // 不传则只靠自身轮询刷新。父层「世界地图前往新区」成功后也 bump 此值，使 board 重拉切到新区地图（state.map 已投影）。
  refreshSignal?: number;
  // onGuidanceSuggested：点地图格子/人时，把一句祖魂语气的「指向型指引草稿」上抛父层（父层预填进 FateView 指引框）。
  onGuidanceSuggested?: (text: string) => void;
  // onSnapshot：每次快照更新（挂载首拉 / 轮询 / refreshSignal 重拉 / 动作结算后重拉）后，把最新整快照上抛父层。
  // 让父层把同一份快照喂给小地图 Minimap（避免再起一条独立 getSession 轮询，两者同源不漂移）。可选。
  onSnapshot?: (snap: SessionSnapshot) => void;
};

// FACTION_NAME_ZH 把阵营 id 译成中文名（与 FateView 同口径，未知 id 回落原串）。
const FACTION_NAME_ZH: Record<string, string> = {
  freedom: "自由",
  order: "秩序",
  chaos: "混乱",
};

function factionNameZH(id: string): string {
  const key = (id ?? "").trim().toLowerCase();
  return FACTION_NAME_ZH[key] ?? (id ?? "").trim();
}

// TERRAIN_NAME_ZH 把地形代码译成中文（点格子时展示「这是什么地方」）。未知回落原码。
const TERRAIN_NAME_ZH: Record<string, string> = {
  plains: "平原",
  forest: "森林",
  mountain: "山地",
  river: "河流",
  river_valley: "河谷",
  grassland: "草原",
  desert: "荒漠",
  swamp: "沼泽",
  ruins: "废墟",
  village: "村庄",
  city: "城市",
  snowfield: "雪原",
  road: "道路",
};

function terrainNameZH(code: string): string {
  return TERRAIN_NAME_ZH[(code ?? "").trim().toLowerCase()] ?? (code ?? "").trim() ?? "未知之地";
}

// 城镇类地形（点击展示「这里住着谁」名单 + 主城区副本入口锚在此类地形）。
const TOWN_TERRAINS = new Set(["city", "village"]);

// ── 分区大世界阶段2 §3/§4：区域 boss / 副本入口的前端坐标推断 ──
// 后端不在 /zones（ZoneSummary）或会话快照里暴露 BossCoord/DungeonCoord——它们只在 world.Zone 上。
// 但 wireZoneContent（worldgen.go）是**确定性、无随机**的：boss 坐标恒为当前区地图中心格 (⌊W/2⌋,⌊H/2⌋)，
// 仅 capital/wild 区有；副本入口锚在 capital 区的城镇格。故前端可据「getZones 拿当前区 kind/faction/level + 快照 map 尺寸」
// 等价复算，无需后端新端点；后端 ChallengeZoneBoss/EnterZoneDungeon 仍是站位/归属/防刷的唯一权威（前端推断只决定按钮露不露）。

// ZONE_BOSS_NAMES 与后端 worldgen.go 的 bossNamesByFaction 表逐字对齐（capital 用第 0 个、wild 用第 1 个）。
// 仅用于在按钮上预显 boss 名（成交叙事仍以后端结算为准）；阵营/kind 查不到时回退通用名。
const ZONE_BOSS_NAMES: Record<string, [string, string]> = {
  freedom: ["晨曦平原之主·赤鬣兽王", "自由荒野的噬骨魔狼"],
  order: ["铁律城郊的钢甲傀儡", "秩序荒野的肃刑巨像"],
  chaos: ["裂隙城郊的混沌触手", "混乱荒野的虚空噬主"],
};

// 有区域 boss 的区域类型（与后端 wireZoneContent 一致：capital/wild 才填 boss；副本入口仅 capital）。
const BOSS_ZONE_KINDS = new Set(["capital", "wild"]);

// ZONE_BOSS_LEVEL_GUARD_GAP 与后端 zoneBossLevelGuardGap 对齐：boss 高出主角 ≥5 级即「此地凶险」（软门）。
// 前端据此把按钮标红 + 预显凶险提示（后端 ChallengeZoneBoss 是唯一权威，点下去会返回中文拒绝消息）。
const ZONE_BOSS_LEVEL_GUARD_GAP = 5;

// playerLevelOf 从快照取主角等级（player_units 中 id===unitId 的 stats.growth.level）。
// 缺字段（旧后端/未填）兜底 Lv1——与后端 pregame 初始 Level=1 一致。
function playerLevelOf(snap: SessionSnapshot | null, unitId: string): number {
  const her = snap?.player_units?.find((u) => u.id === unitId);
  const lvl = her?.stats?.growth?.level;
  return typeof lvl === "number" && lvl > 0 ? lvl : 1;
}

// zoneBossNameFor 按阵营 + kind 取区域 boss 名（与后端 bossNameFor 同口径，未知回退通用名）。
function zoneBossNameFor(factionID: string, kind: string): string {
  const names = ZONE_BOSS_NAMES[(factionID ?? "").trim().toLowerCase()];
  if (!names) return "盘踞此地的霸主";
  return kind === "wild" ? names[1] : names[0];
}

// ZoneContentInfo 是从 getZones 当前区 + 快照地图尺寸推断出的「本区 boss / 副本」露出信息。
type ZoneContentInfo = {
  zoneID: string;
  // bossCoord：区域 boss 坐标（地图中心格）；null=本区无 boss（neutral/starter 区，或无地图尺寸）。
  bossCoord: { q: number; r: number } | null;
  bossName: string;
  bossLevel: number;
  // hasDungeon：本区是否有副本入口（capital 区且城镇可下副本）；副本入口锚在城镇格（city/village）。
  hasDungeon: boolean;
  // bossDefeated：本区 boss 是否已被讨平（服务端权威 ZoneSummary.boss_defeated）——使置灰态跨刷新/跨设备持久。
  bossDefeated: boolean;
};

// BOARD_POLL_MS：观战自身轮询间隔。她在执行阶段被唤醒移动后，board 至多滞后这一拍即追平。
// 取较慢节奏（避免高频拉整快照增成本）；父层 refreshSignal 才是「这拍刚跑完，立刻看她在哪」的精确刷新。
const BOARD_POLL_MS = 8000;

// 「这是谁」浮卡的内联样式（墨色宣纸调，叠在 PixiBoard 之上）。本文件不可改 css，故内联。
const whoCardStyle: React.CSSProperties = {
  position: "absolute",
  bottom: 16,
  left: 16,
  zIndex: 8,
  maxWidth: 280,
  maxHeight: "44vh",
  overflowY: "auto",
  padding: "12px 16px",
  borderRadius: 10,
  background: "rgba(250, 244, 232, 0.96)",
  border: "1px solid rgba(120, 90, 50, 0.4)",
  boxShadow: "0 6px 20px rgba(60, 44, 27, 0.22)",
  color: "#4a3417",
  fontFamily: '"Noto Serif SC", "Songti SC", serif',
  fontSize: 13,
  lineHeight: 1.7,
};
const whoCardNameStyle: React.CSSProperties = {
  fontSize: 16,
  color: "#6b4a22",
  marginBottom: 2,
};
const whoCardLineStyle: React.CSSProperties = {
  fontSize: 12,
  color: "#8a7556",
};
const whoCardCloseStyle: React.CSSProperties = {
  position: "absolute",
  top: 6,
  right: 8,
  border: "none",
  background: "transparent",
  color: "#a08a60",
  fontSize: 16,
  cursor: "pointer",
  lineHeight: 1,
  padding: 2,
};

// 动作目录滚动容器：动作列表自身限高滚动，避免把下方住户名单挤没（whoCard 整体已有 maxHeight）。
const actionListStyle: React.CSSProperties = {
  marginTop: 8,
  maxHeight: 150,
  overflowY: "auto",
  display: "flex",
  flexDirection: "column",
  gap: 5,
  paddingRight: 2,
};

// actionBtnStyle 动作按钮两态（可用/置灰）内联样式（墨色调）。
function actionBtnStyle(disabled: boolean): React.CSSProperties {
  return {
    display: "block",
    width: "100%",
    textAlign: "left",
    padding: "5px 10px",
    borderRadius: 8,
    border: "1px solid rgba(140, 100, 50, 0.4)",
    background: disabled ? "rgba(220, 210, 195, 0.55)" : "rgba(196, 132, 58, 0.14)",
    color: disabled ? "#a99b82" : "#7a5226",
    fontFamily: "inherit",
    fontSize: 13,
    cursor: disabled ? "default" : "pointer",
  };
}

// 内嵌小卡（动作结果/交易/交谈共用）：比浮卡再深一档的墨色衬底，自带右上「×」。
const resultCardStyle: React.CSSProperties = {
  position: "relative",
  marginTop: 8,
  padding: "8px 24px 8px 10px",
  borderRadius: 8,
  background: "rgba(120, 90, 50, 0.08)",
  border: "1px solid rgba(120, 90, 50, 0.25)",
  fontSize: 12,
  color: "#4a3417",
  lineHeight: 1.6,
};
const subCloseStyle: React.CSSProperties = {
  position: "absolute",
  top: 4,
  right: 6,
  border: "none",
  background: "transparent",
  color: "#a08a60",
  fontSize: 14,
  cursor: "pointer",
  lineHeight: 1,
  padding: 2,
};

// 交易小面板的一行（商品/行囊物品 + 买/卖按钮）。
const tradeRowStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  justifyContent: "space-between",
  gap: 6,
  borderTop: "1px solid rgba(120, 90, 50, 0.14)",
  paddingTop: 4,
  marginTop: 4,
};

// tradeBtnStyle 买/卖小按钮两态内联样式。
function tradeBtnStyle(disabled: boolean): React.CSSProperties {
  return {
    flexShrink: 0,
    padding: "2px 10px",
    borderRadius: 999,
    border: "1px solid rgba(140, 100, 50, 0.4)",
    background: disabled ? "rgba(220, 210, 195, 0.55)" : "rgba(196, 132, 58, 0.16)",
    color: disabled ? "#a99b82" : "#7a5226",
    fontFamily: "inherit",
    fontSize: 12,
    cursor: disabled ? "default" : "pointer",
  };
}

// 交谈输入框（textarea）内联样式。
const talkInputStyle: React.CSSProperties = {
  width: "100%",
  boxSizing: "border-box",
  marginTop: 6,
  marginBottom: 4,
  padding: "5px 8px",
  borderRadius: 6,
  border: "1px solid rgba(140, 100, 50, 0.35)",
  background: "rgba(255, 252, 245, 0.9)",
  color: "#4a3417",
  fontFamily: "inherit",
  fontSize: 12,
  lineHeight: 1.5,
  resize: "vertical",
};

// 舞台容器内联样式：相对定位以承载浮卡。**确定高度由 .fate-board-stage（fate.css）提供**——不能用 auto 高度
// （minHeight），否则与 PixiBoard 的 resizeTo:container 形成「容器高=canvas 高」反馈循环致地图无限拉长。
const boardWrapStyle: React.CSSProperties = {
  position: "relative",
  width: "100%",
  borderRadius: 12,
  overflow: "hidden",
  border: "1px solid rgba(120, 90, 50, 0.28)",
  boxShadow: "0 4px 18px rgba(120, 90, 50, 0.1)",
};

// 懒加载 PixiBoard 期间的占位（墨色宣纸调）。
const boardLoadingStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  justifyContent: "center",
  width: "100%",
  minHeight: 360,
  color: "#8a7556",
  fontFamily: '"Noto Serif SC", "Songti SC", serif',
  fontSize: 14,
  letterSpacing: "0.08em",
};

// allUnitsOf 把一份快照里四类单位（玩家/敌方/据点 NPC/野外散人）汇成可查列表，
// 供点格子时定位「这格上站的是谁」与按 unit_id 反查名字。
function allUnitsOf(snap: SessionSnapshot | null): BattleUnit[] {
  if (!snap) return [];
  return [
    ...(snap.player_units ?? []),
    ...(snap.enemy_units ?? []),
    ...(snap.ambient_units ?? []),
    ...(snap.wild_units ?? []),
  ];
}

// TileOccupant 是格子上一个角色的简介（名字 + 称谓 + 阵营 + 是否就是她）。
type TileOccupant = {
  name: string;
  lineage: string;
  faction: string;
  isHer: boolean;
};

// TilePanel 是点格子弹出的「这是什么地方 + 这里有谁 + 有什么 + 能做什么」面板的本地拼装部分。
type TilePanel = {
  q: number;
  r: number;
  terrain: string; // 中文地形名
  landmark: string; // 地标（如有）
  isTown: boolean; // 城镇（城市/村庄）——展示「镇上的人」名单
  occupants: TileOccupant[];
  pois: { kind: string; label: string }[]; // 这格的兴趣点（资源/事件）
};

// ActionResultCard 是动作结算的内嵌即时反馈（中文一句话 + 增减明细行），可「×」关掉。
type ActionResultCard = {
  summary: string;
  lines: string[];
};

// BuyGood 是交易面板买侧的一件可买物（统一行商货单 MerchantGood 与「读 NPC 背包 + 查目录」两条来源）。
// buy_price：买价（=目录 price；查不到目录时为 0，调用方退显「—」、买按钮仍可点，成交以后端为准）。
type BuyGood = {
  item_id: string;
  display_name: string;
  quantity: number;
  buy_price: number;
};

// TradePanel 是交易小面板：买侧（对方背包/行商货单）+ 卖侧（她的行囊）双向对称。
// 任何 NPC 交易都给买+卖两侧——买侧空（对方背包无可出手物）时显示「对方没有要出手的东西」。
type TradePanel = {
  targetUnitId: string;
  targetName: string;
  // buyGoods：对方可出手的货（行商=货单；普通 NPC=读其背包查目录）。空数组=对方没有要出手的东西。
  buyGoods: BuyGood[];
};

// TalkPanel 是与同阵营单位交谈的简易输入框目标。
type TalkPanel = {
  targetUnitId: string;
  targetName: string;
};

// signedNum 把增减数值格式化成带符号明细（+3 / -2）。
function signedNum(n: number): string {
  return n > 0 ? `+${n}` : `${n}`;
}

// ENCOUNTER_OUTCOME_ZH 把遭遇结果代码译成中文短语（未知回落原串）。
const ENCOUNTER_OUTCOME_ZH: Record<string, string> = {
  victory: "得胜而归",
  defeat: "不敌败退",
  escaped: "脱身而走",
  retreat: "且战且退",
};

// effectLines 把直发动作的 effects 明细转成中文行（label ±delta）。
function effectLines(effects: { kind: string; item_id?: string; label_zh: string; delta: number }[] | undefined): string[] {
  return (effects ?? []).map((e) => `${e.label_zh} ${signedNum(e.delta)}`);
}

// catalogNameOf 从完整目录里取某物的中文名（查不到退原 id）。
function catalogNameOf(catalog: Map<string, ItemCatalogEntry>, itemId: string): string {
  return catalog.get(itemId)?.display_name || itemId;
}

// outcomeLines 把 POI 遭遇 outcome 摘要成中文行（钱囊/饱腹/心绪/得物/受创/胜负/关系/战获）。
function outcomeLines(outcome: POIEncounterResult["outcome"], catalog: Map<string, ItemCatalogEntry>): string[] {
  if (!outcome) return [];
  const lines: string[] = [];
  if (outcome.wallet_delta) lines.push(`钱囊 ${signedNum(outcome.wallet_delta)} 文`);
  if (outcome.hunger_delta) lines.push(`饱腹 ${signedNum(outcome.hunger_delta)}`);
  if (outcome.morale_delta) lines.push(`心绪 ${signedNum(outcome.morale_delta)}`);
  const gainedName =
    outcome.gained_item_name ||
    (outcome.gained_item_id ? catalogNameOf(catalog, outcome.gained_item_id) : "");
  if (gainedName) lines.push(`得「${gainedName}」×${outcome.gained_item_qty ?? 1}`);
  if (outcome.damage_taken && outcome.damage_taken > 0) lines.push(`受创 -${outcome.damage_taken}`);
  if (outcome.encounter_outcome) lines.push(ENCOUNTER_OUTCOME_ZH[outcome.encounter_outcome] ?? outcome.encounter_outcome);
  if (outcome.penalty_layer && outcome.penalty_layer > 0) lines.push(`受挫层级 D${outcome.penalty_layer}`);
  if (outcome.relation_zh) lines.push(outcome.relation_zh);
  if (outcome.effect_summary_zh) lines.push(outcome.effect_summary_zh);
  if (outcome.awards && outcome.awards.length > 0) lines.push(`战获：${outcome.awards.join("、")}`);
  return lines;
}

// ELITE_OUTCOME_ZH 把 elite/区域 boss 结局代码译成中文（未知回落原串）。
const ELITE_OUTCOME_ZH: Record<string, string> = {
  defeated: "讨平了它",
  fled: "且战且退，脱身而走",
  down: "力竭倒地，败下阵来",
};

// DUNGEON_OUTCOME_ZH 把副本整体结局译成中文（未知回落原串）。
const DUNGEON_OUTCOME_ZH: Record<string, string> = {
  cleared: "通关而归",
  fled: "见好就收，半途折返",
  wiped: "折戟沉沙，败退而出",
};

// eliteResultCard 把区域 boss 结算（EliteEncounterResult，Go 大写键名）转成内嵌结果卡（一句话 + 明细行）。
function eliteResultCard(name: string, res: EliteEncounterResult): ActionResultCard {
  const outcomeZH = ELITE_OUTCOME_ZH[res.Outcome] ?? res.Outcome;
  const lines: string[] = [`鏖战 ${res.Rounds} 回合`];
  if (res.DamageDealt > 0) lines.push(`予敌 ${res.DamageDealt} 伤`);
  if (res.DamageTaken > 0) lines.push(`受创 -${res.DamageTaken}`);
  if (res.PenaltyLayer > 0) lines.push(`受挫层级 D${res.PenaltyLayer}`);
  if (res.Awards && res.Awards.length > 0) {
    lines.push(`战获：${res.Awards.map((a) => `${a.ItemID}×${a.Quantity}`).join("、")}`);
  }
  if (res.InboxCard) lines.push(res.InboxCard);
  return { summary: `与「${name}」一战：${outcomeZH}`, lines };
}

// dungeonResultCard 把副本结算（DungeonResult，Go 大写键名）转成内嵌结果卡（一句话 + 逐层/分赃明细）。
function dungeonResultCard(res: DungeonResult, unitId: string): ActionResultCard {
  const outcomeZH = DUNGEON_OUTCOME_ZH[res.Outcome] ?? res.Outcome;
  const lines: string[] = [`闯过 ${res.FloorsClear}/${res.Floors} 层`];
  // 分赃：只挑落到主角名下的（按 UnitID 匹配），避免把全队战获堆给她。
  const myAwards = (res.Awards ?? []).filter((a) => a.UnitID === unitId);
  if (myAwards.length > 0) {
    lines.push(`战获：${myAwards.map((a) => `${a.ItemID}×${a.Quantity}`).join("、")}`);
  }
  const myPenalty = res.PenaltyLayer?.[unitId];
  if (typeof myPenalty === "number" && myPenalty > 0) lines.push(`受挫层级 D${myPenalty}`);
  const myCard = res.InboxCards?.[unitId];
  if (myCard) lines.push(myCard);
  return { summary: `探秘境：${outcomeZH}`, lines };
}

export function FateBoard({ sessionId, unitId, refreshSignal, onGuidanceSuggested, onSnapshot }: Props) {
  const [snap, setSnap] = useState<SessionSnapshot | null>(null);
  // pois：地图兴趣点（地块资源 / 野外 NPC 事件），画在格子上的徽标 + 点击查看。
  const [pois, setPois] = useState<MapPOI[]>([]);
  // selected：当前点选的格子（用于 PixiBoard 高亮 + 浮卡定位锚点）。
  const [selected, setSelected] = useState<{ q: number; r: number } | null>(null);
  // tile：点格子弹出的「这是什么地方 + 这里有谁」面板的本地拼装内容；null=不展示。
  const [tile, setTile] = useState<TilePanel | null>(null);
  // mountedRef 守卫异步拉取返回时组件已卸载就不再 setState。
  const mountedRef = useRef(true);
  // onSnapshotRef 持最新的 onSnapshot 回调：让 refresh/onTalkSend 在 setSnap 处统一上抛快照，
  // 而不必把 onSnapshot 列进 refresh 的依赖（避免父层每渲染换函数引用就重建 refresh、重置轮询）。
  const onSnapshotRef = useRef(onSnapshot);
  onSnapshotRef.current = onSnapshot;

  // ── 动作面板状态（开发计划 2026-06-10 §3.7：TilePanel 只读 → 动作面板）──
  // affordances：该格动作目录（best-effort；null=拉取失败/未拉到→回退只读展示，旧后端 404 时面板不能坏）。
  const [affordances, setAffordances] = useState<TileAffordances | null>(null);
  const [affLoading, setAffLoading] = useState(false);
  // affSeqRef：快速连点不同格子时丢弃过期的 affordances 响应。
  const affSeqRef = useRef(0);
  // actionBusy：任一动作（直发/遭遇/交易/交谈）进行中——复用 moveBusy 模式，动作区全列表禁点。
  const [actionBusy, setActionBusy] = useState(false);
  // actionResult：动作结算的内嵌即时反馈卡（结果叙事同时经 WS fate_life_beat 冒进父层命运 feed）。
  const [actionResult, setActionResult] = useState<ActionResultCard | null>(null);
  // trade：交易小面板（行商货单 / 普通 NPC 卖侧）。
  const [trade, setTrade] = useState<TradePanel | null>(null);
  // talk：交谈输入框（仅同阵营单位走 /dialogue 链路）。
  const [talk, setTalk] = useState<TalkPanel | null>(null);
  const [talkDraft, setTalkDraft] = useState("");
  const [talkReply, setTalkReply] = useState("");
  // itemCatalog：物品 id→完整目录条目（译名 + 买价 price + 卖价折算用），挂载时 best-effort 拉一次
  // （失败回空 Map：译名退原 id、价格退显「—」）。买卖双向都要查 price，故拉完整目录而非仅译名表。
  const [itemCatalog, setItemCatalog] = useState<Map<string, ItemCatalogEntry>>(() => new Map());
  // zones/currentZoneID：本会话区域摘要 + 当前区 id（best-effort）。用于推断当前区 boss 坐标/副本入口（露出按钮用）。
  // 后端不在快照/affordances 里暴露 boss/dungeon 坐标，故另拉 getZones 拿当前区 kind/faction/level，再据快照地图尺寸等价复算。
  const [zones, setZones] = useState<ZoneSummary[]>([]);
  const [currentZoneID, setCurrentZoneID] = useState("");
  // bossDone：本次挂载内已讨平的区域 id 集合（前端即时反馈把按钮置「已讨平」；后端 DefeatedBosses 是权威防刷）。
  const [bossDone, setBossDone] = useState<Set<string>>(() => new Set());

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  useEffect(() => {
    void getItemCatalogFull().then((map) => {
      if (mountedRef.current) setItemCatalog(map);
    });
  }, []);

  // 拉区域摘要（best-effort，失败回空→不露 boss/副本按钮）。挂载即拉 + refreshSignal 变化时重拉
  // （父层「前往新区」成功后 bump refreshSignal，使当前区随之切换，boss/副本入口跟着换区）。
  useEffect(() => {
    void getZones(sessionId).then((res) => {
      if (mountedRef.current) {
        setZones(res.zones);
        setCurrentZoneID(res.current_zone_id);
      }
    });
  }, [sessionId, refreshSignal]);

  const refresh = useCallback(async () => {
    try {
      const res = await getSession(sessionId);
      if (mountedRef.current) {
        setSnap(res.session);
        onSnapshotRef.current?.(res.session);
      }
    } catch {
      // 观战快照拉取失败不致命（轮询/下次 refreshSignal 会再试），保持上一帧、不打断她的舞台。
    }
    try {
      const ps = await getMapPOIs(sessionId);
      if (mountedRef.current) setPois(ps);
    } catch {
      // POI 拉取失败：保持上一帧 POI，不影响地图主体。
    }
  }, [sessionId]);

  // 挂载即拉一次；父层 refreshSignal 变化（世界往前走一拍执行完）时重拉，让 board 随她移动追平。
  useEffect(() => {
    void refresh();
  }, [refresh, refreshSignal]);

  // 自身轻量轮询：即便玩家不指引、父层不 bump，她若在执行阶段自己移动了，board 也会至多滞后一拍追平。
  useEffect(() => {
    const timer = window.setInterval(() => void refresh(), BOARD_POLL_MS);
    return () => window.clearInterval(timer);
  }, [refresh]);

  // fetchAffordances：best-effort 拉该格动作目录。失败静默置 null（面板回退只读展示，绝不报错打断）。
  const fetchAffordances = useCallback(
    async (q: number, r: number) => {
      const seq = ++affSeqRef.current;
      setAffLoading(true);
      try {
        const res = await getTileAffordances(sessionId, unitId, q, r);
        if (mountedRef.current && affSeqRef.current === seq) setAffordances(res);
      } catch {
        // 旧后端进程没有该路由（404）/网络失败：静默回退只读展示。
        if (mountedRef.current && affSeqRef.current === seq) setAffordances(null);
      } finally {
        if (mountedRef.current && affSeqRef.current === seq) setAffLoading(false);
      }
    },
    [sessionId, unitId],
  );

  // 点格子：本地拼装只读信息（地形/地标/占据者/POI 徽标）+ 并行 best-effort 拉动作目录。
  const onTileClick = useCallback(
    (q: number, r: number) => {
      const mapTile = snap?.map?.tiles?.find((t) => t.coord.q === q && t.coord.r === r);
      const terrainCode = (mapTile?.terrain ?? "").toLowerCase();
      const occupants: TileOccupant[] = allUnitsOf(snap)
        .filter((u) => u.status.position_q === q && u.status.position_r === r)
        .map((u) => ({
          name: u.identity?.name || u.identity?.nickname || "无名之人",
          lineage: u.identity?.lineage ?? "",
          faction: u.faction_id ?? "",
          isHer: u.id === unitId,
        }));
      const tilePois = pois.filter((p) => p.q === q && p.r === r).map((p) => ({ kind: p.kind, label: p.label_zh }));
      const landmark = mapTile?.landmark ?? "";
      setSelected({ q, r });
      setTile({
        q,
        r,
        terrain: terrainNameZH(terrainCode),
        landmark,
        isTown: TOWN_TERRAINS.has(terrainCode),
        occupants,
        pois: tilePois,
      });
      // 换格子时清空上一格的动作面板余烬（结果卡/交易/交谈），再拉新目录。
      setAffordances(null);
      setActionResult(null);
      setTrade(null);
      setTalk(null);
      setTalkDraft("");
      setTalkReply("");
      void fetchAffordances(q, r);
      // 指向型指引草稿：点中具名的人→「留意」她；点中地标/POI/地形→「去那看看」。上抛父层预填进指引框。
      if (onGuidanceSuggested) {
        const namedOther = occupants.find((o) => !o.isHer);
        let draft = "";
        if (namedOther) {
          draft = `留意「${namedOther.name}」`;
        } else if (tilePois.length > 0) {
          draft = `去探一探那处${tilePois[0].label}`;
        } else if (landmark) {
          draft = `去${landmark}那边看看`;
        } else {
          draft = `往${terrainNameZH(terrainCode)}那边走走`;
        }
        onGuidanceSuggested(draft);
      }
    },
    [snap, unitId, pois, fetchAffordances, onGuidanceSuggested],
  );

  // moveBusy：玩家「让她去这里」直接移动进行中（防重复点）。
  const [moveBusy, setMoveBusy] = useState(false);
  // busyAll：移动或任一动作进行中——浮卡上所有可点项统一禁点，防并发直驱写。
  const busyAll = moveBusy || actionBusy;

  // onMoveHere：玩家在线直接把她移到点选的格子（混合模型：上线可操作）。成功后重拉快照让 board 追平。
  const onMoveHere = useCallback(
    async (q: number, r: number) => {
      setMoveBusy(true);
      try {
        await moveUnit(sessionId, unitId, q, r);
        await refresh();
        setTile(null);
      } catch (e) {
        // 移动失败（越界/水山阻挡）：保留面板，提示走 alert 兜底（命运墨色调下不便弹 toast，从简）。
        window.alert(e instanceof Error ? e.message : "她去不了那里");
      } finally {
        setMoveBusy(false);
      }
    },
    [sessionId, unitId, refresh],
  );

  // unitNameOf：按 unit_id 反查名字（优先动作目录的 occupants，回落整快照四类单位）。
  const unitNameOf = useCallback(
    (targetUnitId: string, fallback: string): string => {
      const occ = affordances?.occupants?.find((o) => o.unit_id === targetUnitId);
      if (occ?.name) return occ.name;
      const u = allUnitsOf(snap).find((x) => x.id === targetUnitId);
      return u?.identity?.name || u?.identity?.nickname || fallback;
    },
    [affordances, snap],
  );

  // herBackpack：她的行囊（卖侧数据源）。快照里 player_units 中 id===unitId 的 inventory.backpack。
  const herBackpack = useMemo<InventoryItem[]>(() => {
    const her = snap?.player_units?.find((u) => u.id === unitId);
    return her?.inventory?.backpack ?? [];
  }, [snap, unitId]);

  // zoneContent：据当前区 ZoneSummary（kind/faction/level）+ 快照地图尺寸，等价复算本区 boss 坐标/副本入口
  // （与后端 wireZoneContent 同口径）。当前区未知 / 中立新手区 → boss 坐标 null、无副本（不露按钮）。
  const zoneContent = useMemo<ZoneContentInfo | null>(() => {
    const zone = zones.find((z) => z.id === currentZoneID && z.is_current) ?? zones.find((z) => z.is_current);
    if (!zone) return null;
    const kind = (zone.kind ?? "").trim().toLowerCase();
    const width = snap?.map?.width ?? 0;
    const height = snap?.map?.height ?? 0;
    // boss：capital/wild 区才有，坐标=地图中心格（⌊W/2⌋,⌊H/2⌋，与后端一致）。无地图尺寸则不露。
    const hasBoss = BOSS_ZONE_KINDS.has(kind) && width > 0 && height > 0;
    return {
      zoneID: zone.id,
      bossCoord: hasBoss ? { q: Math.floor(width / 2), r: Math.floor(height / 2) } : null,
      bossName: zoneBossNameFor(zone.faction_id, kind),
      bossLevel: zone.level_max,
      // 副本入口仅 capital 区（city/village 城镇格可下副本）。
      hasDungeon: kind === "capital",
      // 权威已讨平态（跨刷新持久）；旧后端无此字段 → undefined → 按未讨平处理。
      bossDefeated: Boolean(zone.boss_defeated),
    };
  }, [zones, currentZoneID, snap?.map?.width, snap?.map?.height]);

  // bossAlreadyDefeated：本区 boss 是否已被讨平。取「服务端权威 boss_defeated（跨刷新/跨设备持久）」
  // ∪「本次挂载内本地即时态 bossDone（刚讨平、getZones 尚未重拉时也立刻置灰）」——两者皆有则置灰。
  const bossAlreadyDefeated = useMemo(
    () => Boolean(zoneContent && (zoneContent.bossDefeated || bossDone.has(zoneContent.zoneID))),
    [zoneContent, bossDone],
  );

  // bossPerilous：本区 boss 是否「此地凶险」——boss 等级高出主角 ≥ZONE_BOSS_LEVEL_GUARD_GAP 级（与后端软门同口径）。
  // 前端据此把挑战按钮标红 + 预显凶险提示；后端 ChallengeZoneBoss 仍是唯一权威（点下去会被中文拒绝）。
  const bossPerilous = useMemo(() => {
    if (!zoneContent) return false;
    const playerLevel = playerLevelOf(snap, unitId);
    return zoneContent.bossLevel - playerLevel >= ZONE_BOSS_LEVEL_GUARD_GAP;
  }, [zoneContent, snap, unitId]);

  // tileIsBoss：当前点选的 tile 是否就是本区 boss 坐标格（露「挑战区域 boss」按钮的判据）。
  const tileIsBoss = useMemo(() => {
    if (!tile || !zoneContent?.bossCoord) return false;
    return tile.q === zoneContent.bossCoord.q && tile.r === zoneContent.bossCoord.r;
  }, [tile, zoneContent]);

  // tileIsDungeon：当前点选的 tile 是否是本区可下副本的城镇格（capital 区 + city/village 地形）。
  // 用快照地形判（与后端「副本入口锚在城镇」一致）；后端再校验主角是否真站在城镇格。
  const tileIsDungeon = useMemo(() => {
    if (!tile || !zoneContent?.hasDungeon) return false;
    const mapTile = snap?.map?.tiles?.find((t) => t.coord.q === tile.q && t.coord.r === tile.r);
    return TOWN_TERRAINS.has((mapTile?.terrain ?? "").toLowerCase());
  }, [tile, zoneContent, snap?.map?.tiles]);

  // onChallengeBoss：玩家在线让她挑战当前区域 boss（真实动作：改 HP/士气/钱包 + 落收件箱）。
  // 复用 actionBusy 模式；结算内嵌展示，成功（讨平）后本地记入 bossDone 把按钮置「已讨平」；任意结局后 refresh() 追平 HP/士气。
  const onChallengeBoss = useCallback(async () => {
    if (!zoneContent) return;
    const name = zoneContent.bossName;
    setActionBusy(true);
    try {
      const res = await challengeZoneBoss(sessionId, unitId);
      setActionResult(eliteResultCard(name, res));
      if (res.Outcome === "defeated" && mountedRef.current) {
        setBossDone((cur) => new Set(cur).add(zoneContent.zoneID));
      }
      await refresh();
    } catch (e) {
      window.alert(e instanceof Error ? e.message : "这一战没打成");
    } finally {
      if (mountedRef.current) setActionBusy(false);
    }
  }, [zoneContent, sessionId, unitId, refresh]);

  // onEnterDungeon：玩家在线让她进入本区城镇副本（floors 不传，由后端按区域等级派生）。
  // 副本 flag 关时后端 409（message 含「未启用」）——alert 兜底提示。结算内嵌展示，结束后 refresh() 追平。
  const onEnterDungeon = useCallback(async () => {
    setActionBusy(true);
    try {
      const res = await enterZoneDungeon(sessionId, unitId);
      setActionResult(dungeonResultCard(res, unitId));
      await refresh();
    } catch (e) {
      window.alert(e instanceof Error ? e.message : "这趟秘境没探成");
    } finally {
      if (mountedRef.current) setActionBusy(false);
    }
  }, [sessionId, unitId, refresh]);

  // buildBuyGoods：组装某 NPC 的「可买货单」（Bug1 买侧）——读对方背包（ambient_units / wild_units 里 target 那个
  // 单位的 inventory.backpack），每件查目录得买价（price）与中文名。查不到目录的（罕见）也列出、买价记 0
  // （UI 退显「—」，买按钮仍可点，成交以后端为准）。大多数 NPC 背包空 → 返回空数组 → 面板显示「对方没有要出手的东西」。
  const buildBuyGoods = useCallback(
    (targetUnitId: string): BuyGood[] => {
      const npc =
        snap?.ambient_units?.find((u) => u.id === targetUnitId) ??
        snap?.wild_units?.find((u) => u.id === targetUnitId);
      const backpack = npc?.inventory?.backpack ?? [];
      return backpack
        .filter((it) => it.item_id && it.quantity > 0)
        .map((it) => {
          const entry = itemCatalog.get(it.item_id);
          return {
            item_id: it.item_id,
            display_name: it.custom_name || entry?.display_name || it.item_id,
            quantity: it.quantity,
            buy_price: typeof entry?.price === "number" ? entry.price : 0,
          };
        });
    },
    [snap, itemCatalog],
  );

  // itemNameOf：行囊物品译名（custom_name 优先，其次物品目录，再退原 id）。
  const itemNameOf = useCallback(
    (it: InventoryItem): string => {
      if (it.custom_name) return it.custom_name;
      return itemCatalog.get(it.item_id)?.display_name || it.item_id || "未知物品";
    },
    [itemCatalog],
  );

  // onAction：动作目录按钮分发。直发动作/POI 遭遇走后端结算并内嵌展示结果；交谈/交易打开各自小面板。
  const onAction = useCallback(
    async (a: TileAction) => {
      if (!tile) return;
      // 交谈：后端 POST /api/sessions/:id/dialogue（talkToUnit）仅鉴权放行**指挥阵营自家单位**
      // （router.go 在 PlayerUnits/EnemyUnits 里查 faction 并比对 commander faction）。
      // 同阵营单位（player_units 里能找到）→ 复用该链路弹输入框；
      // 据点 NPC / 野外散人不在覆盖内 → 降级把「与TA交谈」预填进指引草稿，让她自治时找TA搭话。
      // TODO：缺「她（玩家单位）→ 任意同格 NPC」的对话端点；待后端补 fate 作用域 dialogue 路由后接上。
      if (a.action === "talk") {
        const targetId = (a.target_unit_id ?? "").trim();
        const name = targetId ? unitNameOf(targetId, "那人") : "那人";
        const inPlayerUnits = Boolean(targetId && snap?.player_units?.some((u) => u.id === targetId));
        if (inPlayerUnits) {
          setTrade(null);
          setActionResult(null);
          setTalkDraft("");
          setTalkReply("");
          setTalk({ targetUnitId: targetId, targetName: name });
        } else {
          onGuidanceSuggested?.(`跟「${name}」说说话，听听TA的来历`);
        }
        return;
      }
      // 普通 NPC 交易（Bug1）：买+卖双向对称。买侧读对方背包查目录得货单（buildBuyGoods），
      // 卖侧用她的行囊（herBackpack，渲染处取）。买侧空 → 面板显示「对方没有要出手的东西」。
      if (a.action === "trade") {
        const targetId = (a.target_unit_id ?? "").trim();
        if (!targetId) return;
        setTalk(null);
        setActionResult(null);
        setTrade({
          targetUnitId: targetId,
          targetName: unitNameOf(targetId, "对方"),
          buyGoods: buildBuyGoods(targetId),
        });
        return;
      }
      setActionBusy(true);
      try {
        if (a.action === "poi_encounter") {
          const res = await resolvePOIEncounter(sessionId, unitId, tile.q, tile.r);
          setActionResult({ summary: res.summary_zh, lines: outcomeLines(res.outcome, itemCatalog) });
          // 行商：带货单展开交易小面板（可买可卖）。行商货单已铺货，把 merchant_goods（带 buy_price）映成
          // 统一买侧 BuyGood；与普通 NPC 读背包同一渲染路径（卖侧仍用她的行囊）。
          if (res.kind === "merchant" && res.merchant_unit_id && (res.merchant_goods?.length ?? 0) > 0) {
            setTalk(null);
            const goods: MerchantGood[] = res.merchant_goods ?? [];
            setTrade({
              targetUnitId: res.merchant_unit_id,
              targetName: unitNameOf(res.merchant_unit_id, "行商"),
              buyGoods: goods.map((g) => ({
                item_id: g.item_id,
                display_name: g.display_name,
                quantity: g.quantity,
                buy_price: g.buy_price,
              })),
            });
          }
        } else {
          // gather/harvest/forge/upgrade：直发地块动作，后端复用既有结算链（forge/upgrade 的目标装备由后端取目录默认）。
          const res = await executeTileAction(sessionId, unitId, {
            action: a.action,
            q: tile.q,
            r: tile.r,
            activity: a.activity,
          });
          setActionResult({ summary: res.summary_zh, lines: effectLines(res.effects) });
        }
        // 结算后重拉快照与 POI（钱包/背包/consumed 徽标追平），并重拉动作目录（可用性变化）。
        await refresh();
        void fetchAffordances(tile.q, tile.r);
      } catch (e) {
        window.alert(e instanceof Error ? e.message : "这事没办成");
      } finally {
        setActionBusy(false);
      }
    },
    [tile, snap, sessionId, unitId, itemCatalog, refresh, fetchAffordances, unitNameOf, buildBuyGoods, onGuidanceSuggested],
  );

  // onTrade：与行商/NPC 买卖一件（quantity 恒 1，后端权威结算）。买成后货单本地扣减作即时反馈。
  const onTrade = useCallback(
    async (mode: "buy" | "sell", itemId: string) => {
      if (!trade) return;
      setActionBusy(true);
      try {
        const res = await tradeWithUnit(sessionId, unitId, {
          target_unit_id: trade.targetUnitId,
          mode,
          item_id: itemId,
          quantity: 1,
        });
        setActionResult({ summary: res.summary_zh, lines: [`钱囊余 ${res.wallet_after} 文`] });
        if (mode === "buy") {
          setTrade((cur) =>
            cur
              ? {
                  ...cur,
                  buyGoods: cur.buyGoods
                    .map((g) => (g.item_id === itemId ? { ...g, quantity: g.quantity - 1 } : g))
                    .filter((g) => g.quantity > 0),
                }
              : cur,
          );
        }
        await refresh();
      } catch (e) {
        window.alert(e instanceof Error ? e.message : "买卖没谈成");
      } finally {
        setActionBusy(false);
      }
    },
    [trade, sessionId, unitId, refresh],
  );

  // onTalkSend：把话经 /dialogue 链路递给同阵营单位，对方回应叙事展示在面板内。
  const onTalkSend = useCallback(async () => {
    if (!talk) return;
    const text = talkDraft.trim();
    if (!text) return;
    setActionBusy(true);
    setTalkReply("她正与对方搭话…");
    try {
      const res = await talkToUnit(sessionId, talk.targetUnitId, text);
      if (mountedRef.current) {
        setSnap(res.session);
        onSnapshotRef.current?.(res.session);
        setTalkReply(`${res.reply.speaker || talk.targetName}：${res.reply.message}`);
        setTalkDraft("");
      }
    } catch (e) {
      if (mountedRef.current) setTalkReply("");
      window.alert(e instanceof Error ? e.message : "这话没说上");
    } finally {
      if (mountedRef.current) setActionBusy(false);
    }
  }, [talk, talkDraft, sessionId]);

  // commanderFactionID：按她所属阵营给玩家暖色（snap 里有 player_faction_id 即用，缺则回落 "player"）。
  const commanderFactionID = useMemo(() => snap?.player_faction_id || "player", [snap?.player_faction_id]);

  return (
    <div className="fate-board-stage" style={boardWrapStyle} aria-label="她的命运地图">
      <Suspense fallback={<div style={boardLoadingStyle}>正在铺开她脚下的天地…</div>}>
        <LazyPixiBoard
          session={snap}
          commanderFactionID={commanderFactionID}
          fogPerspectiveUnitID=""
          selectedTileCoord={selected}
          onTileClick={onTileClick}
          spectator
          zoom={1.3}
          focusUnitID={unitId}
          pois={pois.map((p) => ({ q: p.q, r: p.r, kind: p.kind, label: p.label_zh, consumed: p.consumed }))}
        />
      </Suspense>
      {tile && (
        <div style={whoCardStyle} role="dialog" aria-label="这是什么地方">
          <button style={whoCardCloseStyle} aria-label="收起" onClick={() => setTile(null)}>
            ×
          </button>
          <div style={whoCardNameStyle}>
            {tile.terrain}
            {tile.landmark && (
              <span style={{ fontSize: 12, color: "#8a7556", marginLeft: 6 }}>· {tile.landmark}</span>
            )}
          </div>
          <div style={whoCardLineStyle}>
            坐标（{tile.q}, {tile.r}）
          </div>
          {/* 本格设施信息（A9）：世界里已存在的建筑（NPC 自治建造产生）——展示类型 + 修建进度/成熟状态。
              玩家不能自建（共享大世界），但完工的己方农田/铁匠铺会在动作目录里给出收割/锻造按钮。 */}
          {affordances && affordances.q === tile.q && affordances.r === tile.r && affordances.structure && (
            <div style={{ ...whoCardLineStyle, marginTop: 6, color: "#6b4a22" }}>
              🏗 {affordances.structure.type_zh}
              {!affordances.structure.complete
                ? `（修建中 ${affordances.structure.progress}/${affordances.structure.required}）`
                : affordances.structure.harvest_ready
                  ? "（已成熟，可收割）"
                  : "（已建成）"}
            </div>
          )}
          {/* 玩家在线操作：让她直接走到这里（混合模型——你可指挥她，世界也自治推进）。 */}
          <button
            type="button"
            disabled={busyAll}
            onClick={() => void onMoveHere(tile.q, tile.r)}
            style={{
              marginTop: 8,
              padding: "6px 12px",
              borderRadius: 8,
              border: "1px solid rgba(140, 100, 50, 0.5)",
              background: busyAll ? "rgba(220,210,195,0.7)" : "rgba(196, 132, 58, 0.16)",
              color: "#7a5226",
              fontFamily: "inherit",
              fontSize: 13,
              cursor: busyAll ? "default" : "pointer",
            }}
          >
            {moveBusy ? "她正动身…" : "🚶 让她去这里"}
          </button>
          {/* 区域 boss 挑战（阶段2 §3/§4）：点中本区 boss 坐标格（地图中心）才露。已讨平置灰；
              未讨平时按「等级护栏」分难度色——bossPerilous(boss 高出主角 ≥5 级)标暗红「此地凶险」，否则常态。
              坐标由 getZones 当前区 kind + 快照地图尺寸前端等价复算（后端不暴露 BossCoord）；后端再校验站位(≤1)/等级护栏/防刷，是唯一权威。 */}
          {tileIsBoss && zoneContent && (
            <>
              <button
                type="button"
                disabled={busyAll || bossAlreadyDefeated}
                onClick={() => void onChallengeBoss()}
                style={{
                  marginTop: 8,
                  padding: "6px 12px",
                  borderRadius: 8,
                  border: `1px solid ${bossAlreadyDefeated ? "rgba(150, 70, 50, 0.5)" : bossPerilous ? "rgba(120, 30, 20, 0.7)" : "rgba(150, 70, 50, 0.5)"}`,
                  background: busyAll || bossAlreadyDefeated ? "rgba(220,210,195,0.7)" : bossPerilous ? "rgba(120, 28, 20, 0.18)" : "rgba(168, 58, 40, 0.14)",
                  color: bossAlreadyDefeated ? "#a99b82" : bossPerilous ? "#7a1c14" : "#8a3422",
                  fontFamily: "inherit",
                  fontSize: 13,
                  cursor: busyAll || bossAlreadyDefeated ? "default" : "pointer",
                  display: "block",
                  width: "100%",
                  textAlign: "left",
                }}
              >
                {bossAlreadyDefeated
                  ? `✔ 「${zoneContent.bossName}」已讨平`
                  : actionBusy
                    ? "鏖战中…"
                    : `${bossPerilous ? "☠ " : "⚔ "}挑战区域boss「${zoneContent.bossName}」(Lv${zoneContent.bossLevel})`}
              </button>
              {/* 凶险预警（等级护栏前置提示，设计 §3）：未讨平且 boss 高出主角 ≥5 级时显「此地凶险」。
                  这是软提示，按钮仍可点；点下去后端会返回中文拒绝消息（走 onChallengeBoss 的 alert 兜底）。 */}
              {!bossAlreadyDefeated && bossPerilous && (
                <div
                  style={{
                    marginTop: 4,
                    fontSize: 12,
                    color: "#9a2c1e",
                    fontStyle: "italic",
                    lineHeight: 1.4,
                  }}
                >
                  此地凶险——她（Lv{playerLevelOf(snap, unitId)}）还远未到能撼动这头霸主的时候，先去低等级带历练吧。
                </div>
              )}
            </>
          )}
          {/* 区域副本进入（阶段2 §4）：点中本区城镇格（capital 区 city/village）才露。floors 由后端按区域等级派生。
              副本 flag 关时后端 409（「未启用」），alert 兜底。后端校验主角是否真站在城镇格，是唯一权威。 */}
          {tileIsDungeon && (
            <button
              type="button"
              disabled={busyAll}
              onClick={() => void onEnterDungeon()}
              style={{
                marginTop: 8,
                padding: "6px 12px",
                borderRadius: 8,
                border: "1px solid rgba(120, 90, 50, 0.5)",
                background: busyAll ? "rgba(220,210,195,0.7)" : "rgba(132, 96, 58, 0.16)",
                color: "#6b4a22",
                fontFamily: "inherit",
                fontSize: 13,
                cursor: busyAll ? "default" : "pointer",
                display: "block",
                width: "100%",
                textAlign: "left",
              }}
            >
              {actionBusy ? "探秘境中…" : "🏰 进入副本"}
            </button>
          )}
          {/* 动作目录：拉取中给一行占位；拉到即渲染按钮列表（不可用置灰带 reason_zh）；失败静默回退只读。
              渲染前校验目录坐标与当前面板格一致——动作完成后的尾随重拉捕获的是旧格闭包，若玩家此间点了
              新格，旧格目录可能后到并压过新格（seq 只防「旧响应覆盖新响应」，防不了「旧格的尾随请求」）。 */}
          {affLoading && <div style={{ ...whoCardLineStyle, marginTop: 8 }}>她正打量这块地方…</div>}
          {!affLoading && affordances && affordances.q === tile.q && affordances.r === tile.r && affordances.actions.length > 0 && (
            <div style={actionListStyle} aria-label="她在这里能做的事">
              {affordances.actions.map((a, i) => (
                <div key={`${a.action}-${a.activity ?? ""}-${a.target_unit_id ?? ""}-${i}`}>
                  <button
                    type="button"
                    style={actionBtnStyle(!a.available || busyAll)}
                    disabled={!a.available || busyAll}
                    onClick={() => void onAction(a)}
                  >
                    {a.label_zh}
                  </button>
                  {!a.available && a.reason_zh && (
                    <div style={{ fontSize: 11, color: "#a99b82", padding: "1px 2px 0" }}>{a.reason_zh}</div>
                  )}
                </div>
              ))}
            </div>
          )}
          {/* 动作结果内嵌小卡：即时反馈（同一叙事会经 WS fate_life_beat 冒进父层命运 feed），可「×」关掉。 */}
          {actionResult && (
            <div style={resultCardStyle} role="status" aria-label="动作结果">
              <button style={subCloseStyle} aria-label="收起结果" onClick={() => setActionResult(null)}>
                ×
              </button>
              <div>{actionResult.summary}</div>
              {actionResult.lines.length > 0 && (
                <div style={{ marginTop: 2, color: "#6b4a22" }}>
                  {actionResult.lines.map((ln, i) => (
                    <div key={i}>· {ln}</div>
                  ))}
                </div>
              )}
            </div>
          )}
          {/* 交易小面板（Bug1+Bug2）：任何 NPC 都买+卖双向对称。
              买侧（对方背包/行商货单）：每件显示买价 `price 文`（查不到目录退「—」）。
              卖侧（她的行囊）：每件显示预估卖价 floor(price*0.8)（≥1，与后端 merchantSellPrice 一致）。
              买价/卖价均为**预估**——成交以后端 summary_zh/wallet_after 为准。 */}
          {trade && (
            <div style={resultCardStyle} aria-label="交易">
              <button style={subCloseStyle} aria-label="收起交易" onClick={() => setTrade(null)}>
                ×
              </button>
              <div style={{ fontSize: 13, color: "#6b4a22" }}>🪙 与「{trade.targetName}」交易</div>
              {/* 买侧：对方要出手的东西（行商货单 / 普通 NPC 背包）。空 → 显示「对方没有要出手的东西」。 */}
              <div style={{ marginTop: 4 }}>
                <div style={{ fontSize: 11, color: "#8a7556" }}>买入（TA 出手的东西）</div>
                {trade.buyGoods.length === 0 ? (
                  <div style={{ fontSize: 12, color: "#8a7556", marginTop: 2 }}>对方没有要出手的东西。</div>
                ) : (
                  trade.buyGoods.map((g) => (
                    <div key={g.item_id} style={tradeRowStyle}>
                      <span>
                        {g.display_name} ×{g.quantity}
                        <span style={{ color: "#8a7556" }}> · {g.buy_price > 0 ? `${g.buy_price}文` : "—"}</span>
                      </span>
                      <button
                        type="button"
                        style={tradeBtnStyle(busyAll)}
                        disabled={busyAll}
                        onClick={() => void onTrade("buy", g.item_id)}
                      >
                        买下
                      </button>
                    </div>
                  ))
                )}
              </div>
              {/* 卖侧：她的行囊。每件显示预估卖价（Bug2：卖前即可见，不必卖出去才知道）。 */}
              <div style={{ marginTop: 6 }}>
                <div style={{ fontSize: 11, color: "#8a7556" }}>卖出（把行囊里的东西卖给 TA）</div>
                {herBackpack.length === 0 ? (
                  <div style={{ fontSize: 12, color: "#8a7556", marginTop: 2 }}>她的行囊空空如也。</div>
                ) : (
                  herBackpack.map((it, i) => {
                    // 预估卖价 = floor(price*0.8)（≥1），从目录查 price 折算；查不到目录退显「—」。
                    const sellPrice = sellPriceOf(itemCatalog.get(it.item_id)?.price);
                    return (
                      <div key={`${it.item_id}-${i}`} style={tradeRowStyle}>
                        <span>
                          {itemNameOf(it)} ×{it.quantity}
                          <span style={{ color: "#8a7556" }}> · {sellPrice > 0 ? `可卖${sellPrice}文` : "—"}</span>
                        </span>
                        <button
                          type="button"
                          style={tradeBtnStyle(busyAll)}
                          disabled={busyAll}
                          onClick={() => void onTrade("sell", it.item_id)}
                        >
                          卖出
                        </button>
                      </div>
                    );
                  })
                )}
              </div>
            </div>
          )}
          {/* 交谈输入框：同阵营单位走 /dialogue 链路；回应叙事就地展示。 */}
          {talk && (
            <div style={resultCardStyle} aria-label="交谈">
              <button
                style={subCloseStyle}
                aria-label="收起交谈"
                onClick={() => {
                  setTalk(null);
                  setTalkReply("");
                }}
              >
                ×
              </button>
              <div style={{ fontSize: 13, color: "#6b4a22" }}>💬 与「{talk.targetName}」交谈</div>
              <textarea
                value={talkDraft}
                onChange={(e) => setTalkDraft(e.target.value)}
                rows={2}
                placeholder="想对TA说点什么…"
                aria-label="交谈内容"
                style={talkInputStyle}
              />
              <button
                type="button"
                style={tradeBtnStyle(busyAll || talkDraft.trim() === "")}
                disabled={busyAll || talkDraft.trim() === ""}
                onClick={() => void onTalkSend()}
              >
                {actionBusy ? "传话中…" : "发送"}
              </button>
              {talkReply && <div style={{ marginTop: 6, fontStyle: "italic" }}>{talkReply}</div>}
            </div>
          )}
          {tile.pois.length > 0 && (
            <div style={{ marginTop: 8, display: "flex", flexWrap: "wrap", gap: 6 }}>
              {tile.pois.map((p, i) => (
                <span
                  key={i}
                  style={{
                    fontSize: 12,
                    padding: "2px 8px",
                    borderRadius: 999,
                    background: p.kind === "resource" ? "rgba(217, 188, 115, 0.3)" : "rgba(198, 109, 72, 0.22)",
                    border: "1px solid rgba(120, 90, 50, 0.3)",
                    color: "#6b4a22",
                  }}
                >
                  {p.kind === "resource" ? "💎 " : "❗ "}
                  {p.label}
                </span>
              ))}
            </div>
          )}
          {tile.occupants.length === 0 ? (
            <div style={{ ...whoCardLineStyle, marginTop: 8 }}>
              {tile.isTown ? "镇上此刻无人露面。" : "此地此刻空无一人。"}
            </div>
          ) : (
            <div style={{ marginTop: 10, display: "flex", flexDirection: "column", gap: 6 }}>
              <div style={{ fontSize: 12, color: "#6b4a22" }}>
                {tile.isTown ? `镇上的人（${tile.occupants.length}）` : `这里的人（${tile.occupants.length}）`}
              </div>
              {tile.occupants.map((o, i) => (
                <div key={i} style={{ borderTop: "1px solid rgba(120, 90, 50, 0.16)", paddingTop: 5 }}>
                  <div style={{ fontSize: 14, color: "#4a3417" }}>
                    {o.name}
                    {o.isHer && <span style={{ fontSize: 11, color: "#a83a28", marginLeft: 6 }}>· 就是她</span>}
                  </div>
                  {(o.lineage || o.faction) && (
                    <div style={whoCardLineStyle}>
                      {o.lineage}
                      {o.lineage && o.faction ? " · " : ""}
                      {o.faction ? `心向${factionNameZH(o.faction)}` : ""}
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
