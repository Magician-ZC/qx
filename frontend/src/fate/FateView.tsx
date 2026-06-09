/* 文件说明：角色命运开盒的「四槽主界面」（设计宪法 §4.6 / §3.2 祖魂换皮）。
   四槽：状态卡（她现在怎样）+ 高光卡（一瞥她经历的事）+ 待决策（等你拿主意）+ 回响带（因为你上次…）。
   数据来自 GET /api/fate/feed + GET /api/units，实时增量来自 WS 的 fate_inbox / fate_echo 推送。
   祖魂语气：不出现「命令/控制」字眼；玩家是垂看后人的先祖，给的是家训、托梦、疾呼，不是遥控。*/

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  emitClientAnalytics,
  getFateFeed,
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

type StatusCard = {
  name: string;
  hp: number;
  hunger: number;
  morale: number;
  mood: string;
  biography: string;
  lineage: string;
};

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
  };
}

function cardText(payload: Record<string, unknown>): string {
  return String(payload.narrative ?? "她那边，出了点事。");
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
        setCards((prev) => [card, ...prev]);
      },
      onFateEcho: (payload) => {
        if (String(payload.unit_id ?? "") !== unitId) return;
        setCards((prev) => [{ kind: "echo", narrative: cardText(payload) }, ...prev]);
      },
    });
    return unsub;
  }, [sessionId, unitId]);

  const pending = useMemo(() => cards.filter((c) => c.kind === "pending"), [cards]);
  const highlights = useMemo(() => cards.filter((c) => c.kind === "highlight").slice(0, 3), [cards]);
  const echoes = useMemo(() => cards.filter((c) => c.kind === "echo").slice(0, 4), [cards]);

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
    if (!text) return;
    try {
      await recordPlayerIntervention(sessionId, unitId, text);
      setInterveneText("");
      setToast("你的嘱咐，托梦给了她。");
    } catch (err) {
      setToast(`托梦未达：${err instanceof Error ? err.message : String(err)}`);
    }
  }, [interveneText, sessionId, unitId]);

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

      {/* 槽二：高光卡（一瞥她经历的事） */}
      <section className="fate-highlights">
        <div className="fate-slot-title">她近来经历的</div>
        {highlights.length === 0 && <div className="fate-empty">还很平静。让世界往前走走看。</div>}
        {highlights.map((c, i) => {
          // reactedTick 进表达式仅为让按钮态随 reactedRef 变化重渲（值本身不参与逻辑）。
          const reacted = reactedTick >= 0 && reactedRef.current.has(fateCardKey(c));
          return (
            <div className="fate-card fate-card-highlight" key={`h${i}`}>
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
            </div>
          );
        })}
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

      {/* 托梦：给她一句嘱咐（可被日后回响引用） */}
      <section className="fate-intervene">
        <input
          value={interveneText}
          placeholder="给她托个梦，留一句嘱咐…（如：别恋战，护住身边人）"
          onChange={(e) => setInterveneText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") void onIntervene();
          }}
        />
        <button onClick={() => void onIntervene()}>托梦</button>
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
