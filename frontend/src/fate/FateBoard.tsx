/* 文件说明：命运地图舞台（混合模型：观战为主 + 在线直驱）——把战棋的 PixiBoard 搬进命运客户端作她日常生活的主舞台。
   她随世界推进在六边形格子上移动；玩家是垂看的先祖，但**上线时可直接操作她**：
   - 点格子弹「这是什么地方 + 这里有谁」浮卡，附「让她去这里」直驱移动按钮；
   - 浮卡同时 best-effort 拉该格**动作目录**（getTileAffordances，失败静默回退只读展示——旧后端 404 时面板不能坏），
     渲染采集/收割/锻造/探遭遇/交谈/交易等动作按钮：不可用置灰带 reason_zh 小字；
     （共享大世界：玩家不能在共用地图上建造/拆除建筑，故无建造/拆除；收割/锻造仅「使用」世界已有的己方设施。）
   - 直发动作（gather/harvest/forge/upgrade）→ executeTileAction，结算摘要+明细内嵌小卡展示；
   - POI 遭遇（poi_encounter）→ resolvePOIEncounter；撞上行商则展开「行商货单」交易小面板（买/卖对照 sell_price）；
   - 普通 NPC 交易复用同款小面板但只开「卖」侧（基准卖价后端结算，前端不显示预估价）；
   - 交谈：同阵营单位复用战棋 /dialogue 链路（talkToUnit）弹简易输入框；据点 NPC/野外散人不在该链路
     鉴权范围内（router.go 校验 commander faction），降级把「与TA交谈」预填进指引草稿。
   结果叙事同时经 WS fate_life_beat 冒进命运 feed（父层）；面板内嵌小卡是即时反馈、可「×」关掉。
   数据来自 GET /api/sessions/:id 的整块快照（getSession），与 FateView 文字命运卡同源同一会话。
   刷新节奏：自身挂载即拉一次 + 每隔若干秒轻量轮询；另接受 refreshSignal——父层在「世界往前走一拍」执行完后
   bump 该值，FateBoard 即重拉快照，board 随她移动重渲（PixiBoard 数据变化自动重渲）。
   祖魂语气/宣纸墨色：本文件不可改 fate.css/styles.css，故浮卡/动作区/交易卡均用内联样式贴合 .fate-* 墨色调。*/

import { Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  executeTileAction,
  getItemCatalog,
  getMapPOIs,
  getSession,
  getTileAffordances,
  moveUnit,
  resolvePOIEncounter,
  talkToUnit,
  tradeWithUnit,
} from "../session/api";
import type { MapPOI, MerchantGood, POIEncounterResult, TileAction, TileAffordances } from "../session/api";
import type { BattleUnit, InventoryItem, SessionSnapshot } from "../session/types";

// LazyPixiBoard 与 App.tsx 同款懒加载 PixiBoard：让 Pixi 战场代码留在独立 chunk（不并进命运首屏主包），
// 既复用 App 的代码分割收益，也消除「同一模块被静态+动态双导入」的打包合并告警。
const LazyPixiBoard = lazy(() => import("../game/PixiBoard").then((m) => ({ default: m.PixiBoard })));

type Props = {
  sessionId: string;
  // unitId：主角的单位 ID，用于在浮卡里标注「这就是她」+ 动作目录/直驱动作以她为行动者。
  unitId: string;
  // refreshSignal：父层每推世界往前一拍并执行完后 bump 此值，FateBoard 据此重拉快照让 board 随她移动重渲。
  // 不传则只靠自身轮询刷新。
  refreshSignal?: number;
  // onGuidanceSuggested：点地图格子/人时，把一句祖魂语气的「指向型指引草稿」上抛父层（父层预填进 FateView 指引框）。
  onGuidanceSuggested?: (text: string) => void;
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

// 城镇类地形（点击展示「这里住着谁」名单）。
const TOWN_TERRAINS = new Set(["city", "village"]);

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

// TradePanel 是交易小面板：行商带货单（可买可卖）；普通 NPC 无货单只开「卖」侧。
type TradePanel = {
  targetUnitId: string;
  targetName: string;
  goods: MerchantGood[];
  isMerchant: boolean;
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

// outcomeLines 把 POI 遭遇 outcome 摘要成中文行（钱囊/饱腹/心绪/得物/受创/胜负/关系/战获）。
function outcomeLines(outcome: POIEncounterResult["outcome"], catalog: Map<string, string>): string[] {
  if (!outcome) return [];
  const lines: string[] = [];
  if (outcome.wallet_delta) lines.push(`钱囊 ${signedNum(outcome.wallet_delta)} 文`);
  if (outcome.hunger_delta) lines.push(`饱腹 ${signedNum(outcome.hunger_delta)}`);
  if (outcome.morale_delta) lines.push(`心绪 ${signedNum(outcome.morale_delta)}`);
  const gainedName =
    outcome.gained_item_name ||
    (outcome.gained_item_id ? catalog.get(outcome.gained_item_id) || outcome.gained_item_id : "");
  if (gainedName) lines.push(`得「${gainedName}」×${outcome.gained_item_qty ?? 1}`);
  if (outcome.damage_taken && outcome.damage_taken > 0) lines.push(`受创 -${outcome.damage_taken}`);
  if (outcome.encounter_outcome) lines.push(ENCOUNTER_OUTCOME_ZH[outcome.encounter_outcome] ?? outcome.encounter_outcome);
  if (outcome.penalty_layer && outcome.penalty_layer > 0) lines.push(`受挫层级 D${outcome.penalty_layer}`);
  if (outcome.relation_zh) lines.push(outcome.relation_zh);
  if (outcome.effect_summary_zh) lines.push(outcome.effect_summary_zh);
  if (outcome.awards && outcome.awards.length > 0) lines.push(`战获：${outcome.awards.join("、")}`);
  return lines;
}

export function FateBoard({ sessionId, unitId, refreshSignal, onGuidanceSuggested }: Props) {
  const [snap, setSnap] = useState<SessionSnapshot | null>(null);
  // pois：地图兴趣点（地块资源 / 野外 NPC 事件），画在格子上的徽标 + 点击查看。
  const [pois, setPois] = useState<MapPOI[]>([]);
  // selected：当前点选的格子（用于 PixiBoard 高亮 + 浮卡定位锚点）。
  const [selected, setSelected] = useState<{ q: number; r: number } | null>(null);
  // tile：点格子弹出的「这是什么地方 + 这里有谁」面板的本地拼装内容；null=不展示。
  const [tile, setTile] = useState<TilePanel | null>(null);
  // mountedRef 守卫异步拉取返回时组件已卸载就不再 setState。
  const mountedRef = useRef(true);

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
  // itemCatalog：物品 id→中文名（卖给行商区译名用），挂载时 best-effort 拉一次（失败回空 Map 退原 id）。
  const [itemCatalog, setItemCatalog] = useState<Map<string, string>>(() => new Map());

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  useEffect(() => {
    void getItemCatalog().then((map) => {
      if (mountedRef.current) setItemCatalog(map);
    });
  }, []);

  const refresh = useCallback(async () => {
    try {
      const res = await getSession(sessionId);
      if (mountedRef.current) setSnap(res.session);
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

  // herBackpack：她的行囊（卖给行商区数据源）。快照里 player_units 中 id===unitId 的 inventory.backpack。
  const herBackpack = useMemo<InventoryItem[]>(() => {
    const her = snap?.player_units?.find((u) => u.id === unitId);
    return her?.inventory?.backpack ?? [];
  }, [snap, unitId]);

  // itemNameOf：行囊物品译名（custom_name 优先，其次物品目录，再退原 id）。
  const itemNameOf = useCallback(
    (it: InventoryItem): string => {
      if (it.custom_name) return it.custom_name;
      return itemCatalog.get(it.item_id) || it.item_id || "未知物品";
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
      // 普通 NPC 交易（非行商）：无货单，只开「卖」侧（基准卖价后端结算，前端不显示预估价）。
      if (a.action === "trade") {
        const targetId = (a.target_unit_id ?? "").trim();
        if (!targetId) return;
        setTalk(null);
        setActionResult(null);
        setTrade({ targetUnitId: targetId, targetName: unitNameOf(targetId, "对方"), goods: [], isMerchant: false });
        return;
      }
      setActionBusy(true);
      try {
        if (a.action === "poi_encounter") {
          const res = await resolvePOIEncounter(sessionId, unitId, tile.q, tile.r);
          setActionResult({ summary: res.summary_zh, lines: outcomeLines(res.outcome, itemCatalog) });
          // 行商：带货单展开「行商货单」交易小面板（可买可卖）。
          if (res.kind === "merchant" && res.merchant_unit_id && (res.merchant_goods?.length ?? 0) > 0) {
            setTalk(null);
            setTrade({
              targetUnitId: res.merchant_unit_id,
              targetName: unitNameOf(res.merchant_unit_id, "行商"),
              goods: res.merchant_goods ?? [],
              isMerchant: true,
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
    [tile, snap, sessionId, unitId, itemCatalog, refresh, fetchAffordances, unitNameOf, onGuidanceSuggested],
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
                  goods: cur.goods
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
          {/* 交易小面板：行商带货单（买/卖对照 sell_price）；普通 NPC 只开卖侧（基准价后端结算不预估）。 */}
          {trade && (
            <div style={resultCardStyle} aria-label="交易">
              <button style={subCloseStyle} aria-label="收起交易" onClick={() => setTrade(null)}>
                ×
              </button>
              <div style={{ fontSize: 13, color: "#6b4a22" }}>🪙 与「{trade.targetName}」交易</div>
              {trade.isMerchant && trade.goods.length > 0 && (
                <div style={{ marginTop: 4 }}>
                  <div style={{ fontSize: 11, color: "#8a7556" }}>行商货单</div>
                  {trade.goods.map((g) => (
                    <div key={g.item_id} style={tradeRowStyle}>
                      <span>
                        {g.display_name} ×{g.quantity} · {g.buy_price}文
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
                  ))}
                </div>
              )}
              <div style={{ marginTop: 6 }}>
                <div style={{ fontSize: 11, color: "#8a7556" }}>
                  {trade.isMerchant ? "卖给行商" : "把行囊里的东西卖给TA"}
                </div>
                {herBackpack.length === 0 ? (
                  <div style={{ fontSize: 12, color: "#8a7556", marginTop: 2 }}>她的行囊空空如也。</div>
                ) : (
                  herBackpack.map((it, i) => {
                    // 行商只收货单上标了 sell_price 的物件（其余按钮禁用「不收」）；普通 NPC 全可卖、不显示预估价。
                    const good = trade.isMerchant ? trade.goods.find((g) => g.item_id === it.item_id) : undefined;
                    const sellPrice = good && good.sell_price > 0 ? good.sell_price : 0;
                    const sellable = !trade.isMerchant || sellPrice > 0;
                    return (
                      <div key={`${it.item_id}-${i}`} style={tradeRowStyle}>
                        <span>
                          {itemNameOf(it)} ×{it.quantity}
                          {sellPrice > 0 && <span style={{ color: "#8a7556" }}> · 可卖{sellPrice}文</span>}
                        </span>
                        <button
                          type="button"
                          style={tradeBtnStyle(busyAll || !sellable)}
                          disabled={busyAll || !sellable}
                          onClick={() => void onTrade("sell", it.item_id)}
                        >
                          {sellable ? "卖出" : "不收"}
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
