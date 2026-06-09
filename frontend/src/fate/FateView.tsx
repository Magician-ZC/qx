/* 文件说明：角色命运开盒的「四槽主界面」（设计宪法 §4.6 / §3.2 祖魂换皮）。
   四槽：状态卡（她现在怎样）+ 高光卡（一瞥她经历的事）+ 待决策（等你拿主意）+ 回响带（因为你上次…）。
   数据来自 GET /api/fate/feed + GET /api/units，实时增量来自 WS 的 fate_inbox / fate_echo 推送。
   祖魂语气：不出现「命令/控制」字眼；玩家是垂看后人的先祖，给的是家训、托梦、疾呼，不是遥控。*/

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  advanceFateWorld,
  emitClientAnalytics,
  getFateFeed,
  getSessionExecutionInProgress,
  getUnitStatus,
  recordPlayerIntervention,
  resolveFateDecision,
  subscribeSessionStream,
  trackFunnel,
  type FateCard,
} from "../session/api";
import { computeFateCountdown, formatFateCountdown } from "./countdown";
import { ShareCardButton } from "../components/ShareCardButton";
import { highlightCard } from "./shareCard";

type Props = {
  sessionId: string;
  unitId: string;
};

// MoralAlignment 是她的三维数值道德轴（对齐后端 faction.MoralAlignment 的 json tag，各 [0,100]）。
// omitempty：旧单位/旧存档无此字段→全 0（无明显倾向），状态卡按「未显道德倾向」处理、不渲染道德行。
type MoralAlignment = {
  freedom: number;
  order: number;
  chaos: number;
};

type StatusCard = {
  name: string;
  hp: number;
  hunger: number;
  morale: number;
  mood: string;
  biography: string;
  lineage: string;
  // faction：她当前所属阵营 id（freedom/order/chaos）；空串=旧单位无阵营字段，不渲染阵营行。
  faction: string;
  // moral：她的三维道德轴；全 0 视为无明显倾向，不渲染道德行。
  moral: MoralAlignment;
};

// FACTION_NAME_ZH 把阵营 id 译成中文名（对齐后端 faction.Definition.NameZH）。未知 id 回落原串。
const FACTION_NAME_ZH: Record<string, string> = {
  freedom: "自由",
  order: "秩序",
  chaos: "混乱",
};

function factionNameZH(id: string): string {
  const key = id.trim().toLowerCase();
  return FACTION_NAME_ZH[key] ?? id.trim();
}

// readMoral 从单位 JSON 的 moral_alignment 块（后端 faction.MoralAlignment）防御性解出三维道德轴。
// 缺字段/非对象→全 0（无明显倾向）。各维不夹钳（后端已 clamp 到 [0,100]，前端只读展示）。
function readMoral(raw: unknown): MoralAlignment {
  const o = (raw ?? {}) as Record<string, unknown>;
  return {
    freedom: Number(o.freedom ?? 0),
    order: Number(o.order ?? 0),
    chaos: Number(o.chaos ?? 0),
  };
}

// moralIsZero 判定道德轴是否全零（旧单位/无倾向）——对齐后端 MoralAlignment.IsZero。
function moralIsZero(m: MoralAlignment): boolean {
  return m.freedom === 0 && m.order === 0 && m.chaos === 0;
}

// dominantMoral 据三维道德轴用 argmax 取主导维（对齐后端 DominantFaction：平手按 freedom>order>chaos 稳定裁定）。
// 全零返回空串（无明显倾向）。返回的是阵营 id（freedom/order/chaos）。
function dominantMoral(m: MoralAlignment): string {
  if (moralIsZero(m)) return "";
  let dominant = "freedom";
  let best = m.freedom;
  if (m.order > best) {
    best = m.order;
    dominant = "order";
  }
  if (m.chaos > best) {
    dominant = "chaos";
  }
  return dominant;
}

// moralDescribe 用主导维译成一句「心向…」的人话（祖魂语气），如「心向自由」。
function moralDescribe(m: MoralAlignment): string {
  const dom = dominantMoral(m);
  if (!dom) return "";
  return `心向${factionNameZH(dom)}`;
}

function readStatus(unit: Record<string, unknown> | null): StatusCard | null {
  if (!unit) return null;
  const identity = (unit.identity ?? {}) as Record<string, unknown>;
  const status = (unit.status ?? {}) as Record<string, unknown>;
  return {
    name: String(identity.name ?? identity.nickname ?? "她"),
    hp: Number(status.hp ?? 0),
    hunger: Number(status.hunger ?? 0),
    morale: Number(status.morale ?? 0),
    mood: String(status.mood ?? ""),
    biography: String(identity.biography ?? ""),
    lineage: String(identity.lineage ?? ""),
    // faction/moral_alignment 是 unit.Record 的顶层字段（GET /api/units/:id 直接 marshal record）。
    faction: String(unit.faction ?? ""),
    moral: readMoral(unit.moral_alignment),
  };
}

function cardText(payload: Record<string, unknown>): string {
  return String(payload.narrative ?? "她那边，出了点事。");
}

// isFactionSwitchCard 嗅探一张命运卡是否为「阵营切换」事件（后端 FACTION_SWITCH 命运卡，走通用 feed）。
// 后端叙事固定含「渐渐偏离了…认了…」措辞（faction_switch.go 的 cardSummary/provenance）。
// 命运卡无专属 kind/code 字段透传到前端，故按叙事特征串识别——锦上添花的视觉标记，命中即给阵营切换专属调；
// 不命中按普通高光卡渲染，绝不影响功能。
function isFactionSwitchCard(card: FateCard): boolean {
  const text = (card.narrative ?? "").trim();
  return text.includes("渐渐偏离了") || (text.includes("偏离") && text.includes("认了"));
}

// 情境化 choice 按钮的内联样式（叠在 fate.css 的 `.fate-actions button` 基础态之上，仅补「label 叠倾向提示」的纵向排版）。
// 不依赖新 CSS 类（本文件只编辑 FateView.tsx，无法改 fate.css），故用内联样式保证副标渲染正确。
const fateChoiceBtnStyle: React.CSSProperties = {
  display: "flex",
  flexDirection: "column",
  alignItems: "center",
  gap: 3,
  textAlign: "center",
  lineHeight: 1.35,
};
const fateChoiceLabelStyle: React.CSSProperties = {
  fontWeight: 600,
};
const fateChoiceHintStyle: React.CSSProperties = {
  fontSize: 11,
  opacity: 0.72,
  fontWeight: 400,
};

// 「她近来经历的」日常 beat 的内联样式（本文件不可改 fate.css，故内联）：灰底小字时间线，
// 刻意比高光卡更低调——这是她平常活着的一拍，不是值得惊呼的高光，也不是要你拿主意的待决策。
const lifeBeatListStyle: React.CSSProperties = {
  display: "flex",
  flexDirection: "column",
  gap: 6,
  marginTop: 4,
};
const lifeBeatItemStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "flex-start",
  gap: 8,
  padding: "7px 11px",
  borderRadius: 8,
  background: "rgba(120, 90, 50, 0.06)",
  borderLeft: "2px solid rgba(140, 110, 70, 0.3)",
  fontSize: 13,
  lineHeight: 1.7,
  color: "#7a6646",
};
const lifeBeatDotStyle: React.CSSProperties = {
  flex: "none",
  marginTop: 1,
  color: "#a08a60",
  opacity: 0.8,
};

// 「让世界往前走」按钮的内联样式：低调墨色描边按钮，点一下推世界往前一拍（她自己活一段）。
const advanceWorldBtnStyle: React.CSSProperties = {
  display: "inline-flex",
  alignItems: "center",
  gap: 6,
  marginTop: 6,
  padding: "8px 16px",
  border: "1px solid rgba(140, 100, 50, 0.4)",
  borderRadius: 8,
  background: "rgba(255, 252, 246, 0.9)",
  color: "#7a5226",
  fontFamily: "inherit",
  fontSize: 13,
  cursor: "pointer",
};
const advanceWorldBtnDisabledStyle: React.CSSProperties = {
  ...advanceWorldBtnStyle,
  opacity: 0.55,
  cursor: "default",
};

// 「她正在经历」loading 条的内联样式：托梦/推世界后这拍正在世界里执行时的过场提示。
const livingBannerStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  gap: 8,
  padding: "9px 13px",
  borderRadius: 8,
  background: "rgba(160, 110, 50, 0.1)",
  border: "1px solid rgba(160, 110, 50, 0.28)",
  color: "#7a5226",
  fontSize: 13,
  margin: "2px 0 6px",
};

// 高光卡三键反馈的事件名（设计 GDD §8 核心乐趣度量：玩家一点即埋点，供后端算「惊喜命中率 / OOC 率」）。
// expected=意料之中、surprise=有点意外但合理（命中惊喜）、ooc=太离谱（疑似失格）。
const fateReactEventName = {
  expected: "fate_react_expected",
  surprise: "fate_react_surprise",
  ooc: "fate_react_ooc",
} as const;
type FateReactKind = keyof typeof fateReactEventName;

// resolveClassHint 把后端 choice 的 resolve_class（基础后果类）译成一句可见的「倾向/后果提示」副标。
// 后端 buildFateChoices 已用情境化 label 表达「点什么」，这里补一行「点了倾向哪边、会怎样」，
// 让独立 #fate 客户端的待决策从「固定三键」升级为「情境化 Copilot 选项 + 透明后果」。
// 未知 resolve_class（理论上不会出现）返回空串，副标不渲染，绝不阻断点选。
function resolveClassHint(resolveClass: string): string {
  switch (resolveClass.trim()) {
    case "urge":
      return "你出手干预 · 她多半会照办，但代价归你担";
    case "let_her":
      return "放手由她 · 她按自己的心意走，后果她自负";
    case "acknowledge":
      return "只是知悉 · 你不插手，记下这一笔便好";
    default:
      return "";
  }
}

// FateBattle 是命运卡可选携带的「关键战接管上下文」（后端 FateInboxItem.Battle/FateFeedItem.Battle，omitempty）。
// 仅当卡关联一场可由玩家亲自接管的战斗时出现：session_id 指向战棋会话、opponent 是对手描述、takeover 标记可接管。
// 旧后端 / 不关联战斗的卡不带此字段，前端防御性解析后按「无 battle」处理，保持向后兼容、不渲染接管按钮。
type FateBattle = {
  session_id: string;
  opponent: string;
  takeover: boolean;
};

// parseBattle 从一张卡（类型上未声明 battle，运行时可能带）防御性解出关键战接管上下文。
// 仅当 session_id 非空且 takeover 为真时返回可接管的 battle，否则返回 undefined（不渲染接管按钮）。
function parseBattle(card: FateCard): FateBattle | undefined {
  const raw = (card as unknown as Record<string, unknown>).battle;
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return undefined;
  const o = raw as Record<string, unknown>;
  const sessionId = String(o.session_id ?? "").trim();
  const takeover = o.takeover === true;
  if (!sessionId || !takeover) return undefined;
  return { session_id: sessionId, opponent: String(o.opponent ?? "").trim(), takeover };
}

// gotoBattleTakeover 切到 App 战棋「关键战接管视图」：把 hash 置为 battle/<sessionId>，
// 由 Root 路由到 App 并把该 session 载入战棋指挥。注意 hash 不带前导 #（赋值时浏览器自动补）。
function gotoBattleTakeover(sessionId: string): void {
  const id = sessionId.trim();
  if (!id) return;
  window.location.hash = `battle/${encodeURIComponent(id)}`;
}

// parsePayloadChoices 从 WS fate_inbox 原始 payload 里防御性解出 choices 数组（与 feed 的 FateChoiceOut 同形）。
// payload 是不裁字段的透传体，choices 多半缺席；缺席/非法时返回 undefined，待决策卡回落通用三键。
function parsePayloadChoices(raw: unknown): { id: string; label: string; resolve_class: string }[] | undefined {
  if (!Array.isArray(raw)) return undefined;
  const out = raw
    .map((c) => {
      const o = (c ?? {}) as Record<string, unknown>;
      const id = String(o.id ?? "").trim();
      const label = String(o.label ?? "").trim();
      if (!id || !label) return null;
      return { id, label, resolve_class: String(o.resolve_class ?? "").trim() };
    })
    .filter((c): c is { id: string; label: string; resolve_class: string } => c !== null);
  return out.length > 0 ? out : undefined;
}

// fateDedupKey 给一张卡算「WS 与首屏共用」的去重标识：
//   - pending 卡（有 decision_id）→ 直接用 decision_id（同一待决策跨来源唯一）；
//   - 其余卡（highlight/echo）→ 用「route 冒号 narrative」拼接 key。
// route 入参：WS payload 自带 route 字段；feed 卡无显式 route，由调用方按 kind→route 映射传入
//   （kind 与 route 同词表：pending/highlight/echo），从而 WS 与首屏落进同一 seenRef 去重集，
//   消除「同一卡先经首屏 feed、再经 WS 推送」的瞬态重复。
function fateDedupKey(route: string, decisionId: string, narrative: string): string {
  const r = route.trim();
  const id = decisionId.trim();
  if (r === "pending" && id) return id;
  return `${r}:${narrative}`;
}

// fateCardKey 给一张高光卡取稳定标识：优先 decision_id，否则对 narrative 做短哈希（FNV-1a 32bit → base36）。
// 与埋点 props.card 同源，确保去重 Set 与后端聚合用同一标识。
function fateCardKey(card: FateCard): string {
  if (card.decision_id) return card.decision_id;
  const text = (card.narrative ?? "").trim();
  let h = 0x811c9dc5;
  for (let i = 0; i < text.length; i += 1) {
    h ^= text.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return (h >>> 0).toString(36);
}

export function FateView({ sessionId, unitId }: Props) {
  const [status, setStatus] = useState<StatusCard | null>(null);
  const [cards, setCards] = useState<FateCard[]>([]);
  const [resolving, setResolving] = useState<string>("");
  const [interveneText, setInterveneText] = useState("");
  const [toast, setToast] = useState("");
  // living=true：托梦/推世界后，这一拍正在世界里执行（她正去经历）。期间禁用托梦框与推进按钮，
  // 由 runWorldTick 轮询 execution_in_progress 由 true→false（或超时兜底）后解除并刷新 feed/状态卡。
  const [living, setLiving] = useState(false);
  // livingRef 镜像 living，供轮询 setTimeout 闭包内读最新态、并在卸载时停轮询（避免对已卸载组件 setState）。
  const livingRef = useRef(false);
  const seenRef = useRef<Set<string>>(new Set());
  // statusViewedRef 守卫 status_card_viewed 埋点只触发一次（首次拉到角色状态时），避免每次重渲重复上报。
  const statusViewedRef = useRef(false);
  // shareInitiatedRef 守卫 share_initiated 同一卡只上报一次（按 narrative 去重），避免重复点击灌漏斗。
  const shareInitiatedRef = useRef<Set<string>>(new Set());
  // reactedRef 守卫高光卡三键反馈同一卡只记一次（按 fateCardKey 去重）；reactedTick 仅驱动按钮态重渲。
  const reactedRef = useRef<Set<string>>(new Set());
  const [reactedTick, setReactedTick] = useState(0);

  const refresh = useCallback(async () => {
    try {
      const [unit, feed] = await Promise.all([getUnitStatus(unitId), getFateFeed(unitId)]);
      setStatus(readStatus(unit));
      // 把首屏 feed 的每张卡按同形 key 播种进 seenRef，使 WS 推送与首屏共用同一去重集——
      // 否则同一事件「先经 feed 渲染、后经 WS 推来」会因 seenRef 未含首屏卡而重复插入（瞬态重复卡）。
      // feed 卡无显式 route 字段，按 kind→route 映射（kind 与 route 同词表）传入，与 onFateInbox 对齐。
      for (const c of feed) {
        seenRef.current.add(fateDedupKey(c.kind, c.decision_id ?? "", c.narrative ?? ""));
      }
      setCards(feed);
    } catch (err) {
      setToast(`读取命运失败：${err instanceof Error ? err.message : String(err)}`);
    }
  }, [unitId]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  // 组件卸载时落 livingRef=false，让在飞的轮询 setTimeout 闭包看到「已卸载」即自停、不再 setState。
  useEffect(() => {
    return () => {
      livingRef.current = false;
    };
  }, []);

  // runWorldTick 是「托梦后她去经历 / 推世界往前走一拍」的公共收尾循环：
  //   进 living（loading）态 → 轮询 GET /api/sessions/:id 的 execution_in_progress 由 true→false（这拍执行完）
  //   → 刷新 feed + 状态卡 → 解除 living。设 90s 超时兜底（异步执行慢 / 后端未真推进时也终会解除，不会卡死）。
  // 后端契约：托梦 intervene 与 advance 都已自动触发世界推进，前端无需再调 advance-phase，只负责轮询+刷新。
  const runWorldTick = useCallback(async () => {
    setLiving(true);
    livingRef.current = true;
    const startedAt = Date.now();
    const timeoutMs = 90_000;
    const pollIntervalMs = 1500;
    // sawInProgress 记是否曾观测到 in_progress=true：避免「推进还没起来就先读到 false」被误判为已跑完。
    // 但也不强求必现 true（极快的一拍可能在首轮轮询前就已结束）——故超时与「曾 true 后转 false」双出口。
    let sawInProgress = false;
    try {
      // 给后端一点起步时间，再开始轮询（intervene/advance 返回后世界推进多为异步起 goroutine）。
      while (livingRef.current && Date.now() - startedAt < timeoutMs) {
        await new Promise((r) => window.setTimeout(r, pollIntervalMs));
        if (!livingRef.current) return; // 已卸载/被打断
        let inProgress = false;
        try {
          inProgress = await getSessionExecutionInProgress(sessionId);
        } catch {
          // 轮询单次失败（网络抖动）不致命：继续下一轮，由超时兜底。
          continue;
        }
        if (inProgress) {
          sawInProgress = true;
          continue;
        }
        // in_progress=false：若曾见 true（这拍确已从执行翻回闲置）→ 跑完，退出轮询。
        // 若从未见 true，再多等一轮（startedAt 起 >4.5s 仍未见 true 视为这拍极快/未真推进，也退出）。
        if (sawInProgress || Date.now() - startedAt > pollIntervalMs * 3) {
          break;
        }
      }
    } finally {
      // 无论正常跑完/超时/异常，都刷新一次 feed+状态卡并解除 living（仍挂载时）。
      if (livingRef.current) {
        await refresh();
        livingRef.current = false;
        setLiving(false);
      }
    }
  }, [sessionId, refresh]);

  // status_card_viewed：首次拉到角色状态（命运状态卡有内容可看）时上报一次（best-effort，吞错、once 守卫）。
  useEffect(() => {
    if (status && !statusViewedRef.current) {
      statusViewedRef.current = true;
      void emitClientAnalytics("status_card_viewed");
    }
  }, [status]);

  // 实时增量：WS 推来的新卡即时插入（按 narrative 去重，避免与首屏重复）。
  useEffect(() => {
    const unsub = subscribeSessionStream(sessionId, {
      onFateInbox: (payload) => {
        if (String(payload.unit_id ?? "") !== unitId) return;
        const route = String(payload.route ?? "");
        // 用与 refresh() 播种首屏 feed 完全一致的同形 key 去重：pending 卡用 decision_id、其余卡用「route 冒号 narrative」。
        const key = fateDedupKey(route, payload.decision_id ? String(payload.decision_id) : "", cardText(payload));
        if (seenRef.current.has(key)) return;
        seenRef.current.add(key);
        // WS payload 通常不含 expires_at/countdown_hours（后端只推 occurred_at 或更少）；
        // 有就透传，没有则留空，由 computeFateCountdown 按 occurred_at + 48h 兜底。
        // choices 同理：WS 多半不带（情境化选项随 feed 返回），带了就透传以便即时上 Copilot 选项，
        // 不带则待决策卡回落通用三键（首屏 refresh 拉 feed 时仍会拿到完整 choices）。
        const card: FateCard = {
          kind: route === "pending" ? "pending" : "highlight",
          decision_id: payload.decision_id ? String(payload.decision_id) : undefined,
          narrative: cardText(payload),
          occurred_at: payload.occurred_at ? String(payload.occurred_at) : undefined,
          expires_at: payload.expires_at ? String(payload.expires_at) : undefined,
          countdown_hours:
            typeof payload.countdown_hours === "number" ? payload.countdown_hours : undefined,
          choices: parsePayloadChoices(payload.choices),
        };
        // battle 不在 FateCard 类型里（后端 omitempty 透传），WS 带了就挂到运行时对象上，
        // 供 parseBattle 取「关键战接管」上下文；不带则无此键，按无 battle 处理。
        if (payload.battle && typeof payload.battle === "object" && !Array.isArray(payload.battle)) {
          (card as unknown as Record<string, unknown>).battle = payload.battle;
        }
        setCards((prev) => [card, ...prev]);
      },
      onFateEcho: (payload) => {
        if (String(payload.unit_id ?? "") !== unitId) return;
        setCards((prev) => [{ kind: "echo", narrative: cardText(payload) }, ...prev]);
      },
      // 世界推进一拍后她的日常经历：WS 推来即 append 成一条 life_beat 卡（免轮询更跟手）。
      // 与首屏 feed 共用 seenRef 去重：life_beat 无 decision_id，按「life_beat 冒号 narrative」key（与 refresh 播种一致）。
      onFateLifeBeat: (payload) => {
        if (String(payload.unit_id ?? "") !== unitId) return;
        const narrative = cardText(payload);
        const key = fateDedupKey("life_beat", "", narrative);
        if (seenRef.current.has(key)) return;
        seenRef.current.add(key);
        setCards((prev) => [
          {
            kind: "life_beat",
            narrative,
            occurred_at: payload.occurred_at ? String(payload.occurred_at) : undefined,
          },
          ...prev,
        ]);
      },
    });
    return unsub;
  }, [sessionId, unitId]);

  const pending = useMemo(() => cards.filter((c) => c.kind === "pending"), [cards]);
  const highlights = useMemo(() => cards.filter((c) => c.kind === "highlight").slice(0, 3), [cards]);
  const echoes = useMemo(() => cards.filter((c) => c.kind === "echo").slice(0, 4), [cards]);
  // lifeBeats：她近来的日常经历（世界推进一拍后的低调叙事）。feed/WS 都按时间倒序插入，取前若干条展示。
  const lifeBeats = useMemo(() => cards.filter((c) => c.kind === "life_beat").slice(0, 6), [cards]);

  const onResolve = useCallback(
    async (decisionId: string, resolveType: string, label: string) => {
      if (!decisionId) return;
      setResolving(decisionId);
      try {
        await resolveFateDecision(decisionId, sessionId, unitId, resolveType);
        setCards((prev) => prev.filter((c) => c.decision_id !== decisionId));
        setToast(`你${label}。她记下了。`);
      } catch (err) {
        setToast(`没能传达：${err instanceof Error ? err.message : String(err)}`);
      } finally {
        setResolving("");
      }
    },
    [sessionId, unitId],
  );

  const onIntervene = useCallback(async () => {
    const text = interveneText.trim();
    if (!text || living) return;
    try {
      // 后端 intervene 已自动触发世界推进——这里无需再调 advance。成功后进 living 态轮询这拍跑完再刷新。
      await recordPlayerIntervention(sessionId, unitId, text);
      setInterveneText("");
      setToast("你的嘱咐，托梦给了她。她正听着，去经历了…");
      await runWorldTick();
    } catch (err) {
      setToast(`托梦未达：${err instanceof Error ? err.message : String(err)}`);
    }
  }, [interveneText, living, sessionId, unitId, runWorldTick]);

  // onAdvanceWorld 让世界往前走一拍（玩家不托梦也能推她自己活一段）：调 advanceFateWorld 起一拍世界推进，
  // 再走与托梦同一条 living→轮询→刷新收尾循环。advancing=false（已在推进/无可推进）也照常进收尾轮询（很快解除）。
  const onAdvanceWorld = useCallback(async () => {
    if (living) return;
    try {
      const advancing = await advanceFateWorld(sessionId);
      setToast(advancing ? "世界往前走了一拍。她正去经历…" : "世界正自行往前——稍候看看她经历了什么。");
      await runWorldTick();
    } catch (err) {
      setToast(`没能推动世界：${err instanceof Error ? err.message : String(err)}`);
    }
  }, [living, sessionId, runWorldTick]);

  // onShare 把一段命运叙事复制到剪贴板供玩家分享，并埋点 share_initiated（同一段叙事只上报一次）。
  // 埋点与复制均 best-effort：剪贴板不可用时仍给 toast 反馈，绝不阻断 UX。
  const onShare = useCallback(
    (narrative: string) => {
      const text = narrative.trim();
      if (!text) return;
      if (!shareInitiatedRef.current.has(text)) {
        shareInitiatedRef.current.add(text);
        void emitClientAnalytics("share_initiated");
        void trackFunnel("share_initiated", { source: "fate_highlight" });
      }
      try {
        void navigator.clipboard?.writeText(text);
        setToast("她的故事，已经替你抄了下来。去说给别人听吧。");
      } catch {
        setToast("她的故事在这儿——长按或选中，讲给别人听。");
      }
    },
    [],
  );

  // onReact 处理高光卡三键轻反馈（意料之中 / 有点意外但合理 / 太离谱）：一点即埋点，供后端算惊喜命中率/OOC 率。
  // 同卡只记一次（reactedRef 按 fateCardKey 去重）；埋点 best-effort 吞错，绝不阻断 UX。
  const onReact = useCallback((card: FateCard, kind: FateReactKind) => {
    const key = fateCardKey(card);
    if (reactedRef.current.has(key)) return;
    reactedRef.current.add(key);
    void emitClientAnalytics(fateReactEventName[kind], { card: key, source: "fate_highlight" });
    setReactedTick((n) => n + 1);
    setToast("收到。她的命运记下了你的看法。");
  }, []);

  return (
    <div className="fate-root">
      <header className="fate-header">
        <span className="fate-brand">群像 · 命运</span>
        <span className="fate-sub">你是垂看后人的先祖。你能托梦、能疾呼，却不能替她活。</span>
      </header>

      {/* 槽一：状态卡 */}
      <section className="fate-status">
        {status ? (
          <>
            <div className="fate-status-name">{status.name}</div>
            {status.lineage && <div className="fate-status-lineage">{status.lineage}</div>}
            {/* 阵营 + 道德倾向：她现在归属哪片天地、心向何方。
                faction 空（旧单位无字段）则不渲染该行；道德轴全零（无明显倾向）则省略道德描述与三色条。 */}
            <FactionLine faction={status.faction} moral={status.moral} />
            <div className="fate-status-bars">
              <Bar label="气血" value={status.hp} max={100} tone="hp" />
              <Bar label="饥饱" value={status.hunger} max={100} tone="hunger" />
              <Bar label="心气" value={Math.round(status.morale * 100)} max={100} tone="morale" />
            </div>
            {status.biography && <p className="fate-bio">{status.biography}</p>}
          </>
        ) : (
          <div className="fate-status-name">正在感应她的气息…</div>
        )}
      </section>

      {/* 槽四：待决策（最显眼，等你拿主意） */}
      {pending.length > 0 && (
        <section className="fate-pending">
          <div className="fate-slot-title">有件关乎她的事，在等你拿个主意</div>
          <p className="fate-pending-text">{pending[0].narrative}</p>
          <FateCountdownBar card={pending[0]} />
          {/* 关键战手动接管：当这件待决策关联一场可接管的战斗时，给玩家一个「亲自接管此战」的入口，
              点击切到 App 战棋指挥视图（hash=battle/<session_id>）。不带 battle 的待决策卡不渲染此按钮。 */}
          <BattleTakeoverButton card={pending[0]} />
          {/* 情境化 Copilot 选项：后端 buildFateChoices 按事件类型/红线锚/关系生成贴合此刻的 label
              （追讨/求和/认账、刻成传家物/暂且不必、还手/隐忍…），resolve 传该 choice.id，
              后端 resolveFateChoiceClass 再把情境 id 折回基础后果类。每个选项补一行倾向/后果提示。
              feed 未带 choices 时（旧后端 / WS 推送只有 route）回落通用三键，保持向后兼容。 */}
          {pending[0].choices && pending[0].choices.length > 0 ? (
            <div className="fate-actions">
              {pending[0].choices.map((c) => {
                const hint = resolveClassHint(c.resolve_class);
                return (
                  <button
                    key={c.id}
                    style={fateChoiceBtnStyle}
                    title={hint || c.label}
                    disabled={resolving === pending[0].decision_id}
                    onClick={() => onResolve(pending[0].decision_id ?? "", c.id, c.label)}
                  >
                    <span style={fateChoiceLabelStyle}>{c.label}</span>
                    {hint && <span style={fateChoiceHintStyle}>{hint}</span>}
                  </button>
                );
              })}
            </div>
          ) : (
            <div className="fate-actions">
              <button disabled={resolving === pending[0].decision_id} onClick={() => onResolve(pending[0].decision_id ?? "", "let_her", "由她去")}>
                由她去（信她）
              </button>
              <button disabled={resolving === pending[0].decision_id} onClick={() => onResolve(pending[0].decision_id ?? "", "urge", "疾呼拦住")}>
                疾呼拦住她
              </button>
              <button disabled={resolving === pending[0].decision_id} onClick={() => onResolve(pending[0].decision_id ?? "", "acknowledge", "默默看着")}>
                默默看着
              </button>
            </div>
          )}
          {pending.length > 1 && <div className="fate-more">还有 {pending.length - 1} 件事在等你</div>}
        </section>
      )}

      {/* 槽二：高光卡 + 生活 beat（一瞥她经历的事） */}
      <section className="fate-highlights">
        <div className="fate-slot-title">她近来经历的</div>

        {/* 「她正在经历」过场：托梦/推世界后这拍正在世界里执行，给一句祖魂语气的等待提示，期间托梦/推进按钮禁用。 */}
        {living && (
          <div style={livingBannerStyle} aria-live="polite">
            <span aria-hidden="true">✦</span>
            <span>她正听着你的牵挂，去经历了…稍候，看看她这一程遇上了什么。</span>
          </div>
        )}

        {/* 空态：没有高光也没有生活 beat 时，把「让世界往前走走看」做成可点按钮——
            玩家不托梦也能推世界往前一拍（她自己活一段），同样进 living→轮询→刷新。living 中禁用避免重入。 */}
        {highlights.length === 0 && lifeBeats.length === 0 && (
          <div className="fate-empty">
            <span>还很平静。</span>
            <button
              style={living ? advanceWorldBtnDisabledStyle : advanceWorldBtnStyle}
              disabled={living}
              onClick={() => void onAdvanceWorld()}
            >
              {living ? "她正去经历…" : "让世界往前走走看 →"}
            </button>
          </div>
        )}

        {/* 生活 beat 时间线：她近来的日常经历，低调灰底小字、与高光/待决策视觉区分，按时间倒序。 */}
        {lifeBeats.length > 0 && (
          <div style={lifeBeatListStyle}>
            {lifeBeats.map((c, i) => (
              <div style={lifeBeatItemStyle} key={`l${i}`}>
                <span style={lifeBeatDotStyle} aria-hidden="true">
                  ·
                </span>
                <span>{c.narrative}</span>
              </div>
            ))}
          </div>
        )}

        {highlights.map((c, i) => {
          // reactedTick 进表达式仅为让按钮态随 reactedRef 变化重渲（值本身不参与逻辑）。
          const reacted = reactedTick >= 0 && reactedRef.current.has(fateCardKey(c));
          // 阵营切换卡专属视觉标记（锦上添花）：命中则加 -switch 调 + 顶部小徽，提示「她的心渐渐偏离了…」。
          const switched = isFactionSwitchCard(c);
          return (
            <div
              className={`fate-card fate-card-highlight${switched ? " fate-card-switch" : ""}`}
              key={`h${i}`}
            >
              {switched && <span className="fate-switch-badge">⟲ 阵营之变</span>}
              <span className="fate-card-text">{c.narrative}</span>
              {/* 三键轻反馈：这段她的命运，落在你预期里还是吓你一跳？一点即埋点（同卡只记一次）。 */}
              <div className="fate-react-row">
                {reacted ? (
                  <span className="fate-react-done">已反馈，多谢</span>
                ) : (
                  <>
                    <span className="fate-react-label">这段：</span>
                    <button className="fate-react-btn" title="在意料之中" onClick={() => onReact(c, "expected")}>
                      意料之中
                    </button>
                    <button className="fate-react-btn" title="有点意外，但合理" onClick={() => onReact(c, "surprise")}>
                      有点意外但合理
                    </button>
                    <button className="fate-react-btn" title="太离谱了，不像她" onClick={() => onReact(c, "ooc")}>
                      太离谱
                    </button>
                  </>
                )}
              </div>
              <button
                className="fate-share-btn"
                title="把她的故事讲给别人听"
                onClick={() => onShare(c.narrative)}
              >
                讲给别人听
              </button>
              {/* 图片卡分享（文本复制保留在上方）：把这段高光手绘成竖版 PNG，截图传播更易扩散。*/}
              <ShareCardButton
                compact
                card={highlightCard({ title: status?.name ?? "她", narrative: c.narrative })}
                onShared={() => void trackFunnel("share_initiated", { source: "fate_highlight_image" })}
              />
              {/* 高光卡若关联一场可接管的关键战，也露出接管入口（如「她正陷入一场恶战」）。 */}
              <BattleTakeoverButton card={c} />
            </div>
          );
        })}

        {/* 常驻「让世界往前走」入口：即便已有高光/生活 beat，玩家也能不托梦就推世界往前一拍（她自己活一段）。
            空态已自带按钮，这里仅在「有内容可看」时补一个，避免空态重复出现两枚。 */}
        {(highlights.length > 0 || lifeBeats.length > 0) && (
          <button
            style={living ? advanceWorldBtnDisabledStyle : advanceWorldBtnStyle}
            disabled={living}
            onClick={() => void onAdvanceWorld()}
            title="让世界往前走一拍，看看她自己会经历什么"
          >
            {living ? "她正去经历…" : "让世界往前走一拍 →"}
          </button>
        )}
      </section>

      {/* 槽三：回响带（因为你上次…） */}
      {echoes.length > 0 && (
        <section className="fate-echoes">
          <div className="fate-slot-title">回响</div>
          {echoes.map((c, i) => (
            <div className="fate-card fate-card-echo" key={`e${i}`}>
              {c.narrative}
            </div>
          ))}
        </section>
      )}

      {/* 托梦：给她一句嘱咐（可被日后回响引用）。这拍正在经历（living）时禁用，避免重入推进。 */}
      <section className="fate-intervene">
        <input
          value={interveneText}
          disabled={living}
          placeholder={living ? "她正去经历你方才的嘱咐…" : "给她托个梦，留一句嘱咐…（如：别恋战，护住身边人）"}
          onChange={(e) => setInterveneText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") void onIntervene();
          }}
        />
        <button disabled={living} onClick={() => void onIntervene()}>
          {living ? "经历中…" : "托梦"}
        </button>
      </section>

      {toast && (
        <div className="fate-toast" onAnimationEnd={() => setToast("")}>
          {toast}
        </div>
      )}
    </div>
  );
}

// FateCountdownBar 渲染待决策卡的「拿主意倒计时」，每秒实时倒数；过期标灰、剩余 <6h 标红。
// 无任何时间锚（feed 未给 countdown/expires，WS 也无 occurred_at）时不渲染。
function FateCountdownBar({ card }: { card: FateCard }) {
  // tick 仅用于驱动每秒重渲；实际剩余由 computeFateCountdown 按当前时刻实时计算。
  const [, setTick] = useState(0);
  useEffect(() => {
    const timer = window.setInterval(() => setTick((n) => n + 1), 1000);
    return () => window.clearInterval(timer);
  }, []);

  const cd = computeFateCountdown(card);
  if (!cd.available) return null;
  const cls = cd.expired ? "fate-countdown fate-countdown-expired" : cd.urgent ? "fate-countdown fate-countdown-urgent" : "fate-countdown";
  return <div className={cls}>{formatFateCountdown(cd)}</div>;
}

// fateTakeoverBtnStyle 是「接管此战」按钮的内联样式（本文件不可改 fate.css，故内联）：
// 比常规命运按钮更醒目（暖朱底色 + 浅描边），点一下即切到战棋接管视图。
const fateTakeoverBtnStyle: React.CSSProperties = {
  display: "inline-flex",
  alignItems: "center",
  gap: 6,
  marginTop: 8,
  padding: "8px 14px",
  border: "1px solid rgba(168, 58, 40, 0.55)",
  borderRadius: 10,
  background: "rgba(180, 84, 58, 0.14)",
  color: "#a83a28",
  fontFamily: "inherit",
  fontSize: 14,
  fontWeight: 600,
  cursor: "pointer",
};
const fateTakeoverHintStyle: React.CSSProperties = {
  marginTop: 4,
  fontSize: 11,
  opacity: 0.7,
};

// BattleTakeoverButton 在卡关联一场可接管的关键战时渲染「亲自接管此战」入口；否则什么都不渲染。
// 点击切到 App 战棋指挥视图（hash=battle/<session_id>），由玩家亲自指挥这一战。
function BattleTakeoverButton({ card }: { card: FateCard }) {
  const battle = parseBattle(card);
  if (!battle) return null;
  return (
    <div>
      <button
        style={fateTakeoverBtnStyle}
        title={battle.opponent ? `对手：${battle.opponent}` : "亲自接管这一战"}
        onClick={() => gotoBattleTakeover(battle.session_id)}
      >
        ⚔ 亲自接管此战
      </button>
      <div style={fateTakeoverHintStyle}>
        {battle.opponent ? `她正与「${battle.opponent}」对阵。` : "她正陷入一场恶战。"}你可以接过指挥，替她打完这一仗。
      </div>
    </div>
  );
}

// FactionLine 渲染状态卡里的「阵营 + 道德倾向」一行：左侧阵营徽（中文名）、右侧主导倾向描述 +
// 三色道德条（自由/秩序/混乱按各维 [0,100] 占比着色）。
//   - faction 空（旧单位无阵营字段）→ 整行不渲染（返回 null），向后兼容；
//   - 道德轴全零（无明显倾向）→ 渲染阵营徽，但省略「心向…」描述与三色条（无可视化的倾向）。
function FactionLine({ faction, moral }: { faction: string; moral: MoralAlignment }) {
  const fid = faction.trim();
  if (!fid) return null;
  const zero = moralIsZero(moral);
  const describe = zero ? "" : moralDescribe(moral);
  // 三色条按三维数值归一为占比（防全零除零：zero 时不渲染条）。
  const sum = moral.freedom + moral.order + moral.chaos;
  const pct = (v: number) => (sum > 0 ? (v / sum) * 100 : 0);
  // 主导维高亮：与 describe 同口径取主导阵营 id，用于给该徽加 selected 调。
  const dom = dominantMoral(moral);
  return (
    <div className="fate-faction-line">
      <span
        className={`fate-faction-badge fate-faction-badge-${fid}${dom === fid ? " dominant" : ""}`}
        title={`她现在归属：${factionNameZH(fid)}`}
      >
        {factionNameZH(fid)}
      </span>
      {describe && (
        <span className="fate-moral-desc" title={`自由 ${Math.round(moral.freedom)} · 秩序 ${Math.round(moral.order)} · 混乱 ${Math.round(moral.chaos)}`}>
          {describe}
          <span className="fate-moral-nums">
            （自由{Math.round(moral.freedom)}/秩序{Math.round(moral.order)}/混乱{Math.round(moral.chaos)}）
          </span>
        </span>
      )}
      {!zero && (
        <span className="fate-moral-bar" aria-label="道德倾向三色条">
          <span className="fate-moral-seg fate-moral-seg-freedom" style={{ width: `${pct(moral.freedom)}%` }} />
          <span className="fate-moral-seg fate-moral-seg-order" style={{ width: `${pct(moral.order)}%` }} />
          <span className="fate-moral-seg fate-moral-seg-chaos" style={{ width: `${pct(moral.chaos)}%` }} />
        </span>
      )}
    </div>
  );
}

function Bar({ label, value, max, tone }: { label: string; value: number; max: number; tone: string }) {
  const pct = Math.max(0, Math.min(100, (value / max) * 100));
  return (
    <div className={`fate-bar fate-bar-${tone}`}>
      <span className="fate-bar-label">{label}</span>
      <span className="fate-bar-track">
        <span className="fate-bar-fill" style={{ width: `${pct}%` }} />
      </span>
      <span className="fate-bar-value">{Math.round(value)}</span>
    </div>
  );
}
