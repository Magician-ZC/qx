/* 文件说明：命运四槽侧栏面板（设计宪法 §4.6），接进主指挥客户端 App.tsx。
   把独立 #fate 路由的「四槽」体验做成一个可折叠的浮层面板：对当前指挥阵营的某个单位
   调 getFateFeed 渲染——状态卡（她现在怎样）/ 高光（一瞥她经历的事）/ 待决策（等你拿主意，
   let_her/urge/acknowledge 三按钮）/ 回响带（因为你上次…）。实时增量来自会话流的
   fate_inbox / fate_echo 推送（本面板自管订阅，scoped 到当前选中单位，App 无需额外接线）。
   祖魂语气：不出现「命令/控制」字眼；玩家是垂看后人的先祖。自包含内联样式，不依赖 fate.css。*/

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  emitClientAnalytics,
  getFateFeed,
  getUnitStatus,
  resolveFateDecision,
  subscribeSessionStream,
  trackFunnel,
  type FateCard,
} from "../session/api";
import { computeFateCountdown, formatFateCountdown } from "../fate/countdown";
import { ShareCardButton } from "./ShareCardButton";
import { highlightCard } from "../fate/shareCard";
import { zIndex } from "../zindex-tokens";

type FateUnitOption = {
  id: string;
  name: string;
};

type Props = {
  sessionId: string;
  units: FateUnitOption[];
  // initialUnitID 是面板首选聚焦的单位（通常为 App 当前选中的单位）。
  initialUnitID?: string | null;
  onClose: () => void;
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

// 高光卡三键反馈的事件名（设计 GDD §8 核心乐趣度量：玩家一点即埋点，供后端算「惊喜命中率 / OOC 率」）。
// expected=意料之中、surprise=有点意外但合理（命中惊喜）、ooc=太离谱（疑似失格）。
const fateReactEventName = {
  expected: "fate_react_expected",
  surprise: "fate_react_surprise",
  ooc: "fate_react_ooc",
} as const;
type FateReactKind = keyof typeof fateReactEventName;

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

const panelStyle: React.CSSProperties = {
  position: "absolute",
  top: 64,
  right: 12,
  width: 320,
  maxHeight: "calc(100vh - 96px)",
  overflowY: "auto",
  zIndex: zIndex.rightPanel,
  background: "rgba(18, 20, 28, 0.94)",
  border: "1px solid rgba(217, 188, 115, 0.35)",
  borderRadius: 10,
  boxShadow: "0 8px 28px rgba(0,0,0,0.45)",
  color: "#e8e2d2",
  padding: 12,
  fontSize: 13,
};

const headerStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  justifyContent: "space-between",
  marginBottom: 8,
};

const brandStyle: React.CSSProperties = { color: "#f2d98f", fontWeight: 700, fontSize: 14 };
const subStyle: React.CSSProperties = { color: "#9aa0ad", fontSize: 11, marginTop: 2 };
const slotTitleStyle: React.CSSProperties = {
  color: "#cdb98a",
  fontSize: 11,
  letterSpacing: 0.5,
  margin: "10px 0 4px",
  textTransform: "uppercase",
};
const sectionCardStyle: React.CSSProperties = {
  background: "rgba(32, 36, 48, 0.7)",
  border: "1px solid rgba(255,255,255,0.06)",
  borderRadius: 8,
  padding: "8px 10px",
  margin: "4px 0",
};
const pendingCardStyle: React.CSSProperties = {
  ...sectionCardStyle,
  border: "1px solid rgba(217, 188, 115, 0.5)",
  background: "rgba(40, 34, 18, 0.7)",
};
const echoCardStyle: React.CSSProperties = {
  ...sectionCardStyle,
  borderLeft: "3px solid #6f8db5",
};
const actionRowStyle: React.CSSProperties = { display: "flex", gap: 6, marginTop: 8, flexWrap: "wrap" };
// 高光卡「分享」小按钮（贴右下角，弱化样式，不抢叙事正文风头）。
const shareBtnStyle: React.CSSProperties = {
  marginTop: 6,
  cursor: "pointer",
  background: "transparent",
  border: "1px solid rgba(217, 188, 115, 0.35)",
  color: "#cdb98a",
  borderRadius: 6,
  padding: "3px 8px",
  fontSize: 11,
};
// 高光卡「三键轻反馈」一行（贴叙事下方，比分享更轻，是埋点入口非操作）。
const reactRowStyle: React.CSSProperties = {
  display: "flex",
  gap: 5,
  marginTop: 7,
  flexWrap: "wrap",
  alignItems: "center",
};
const reactBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "rgba(255,255,255,0.04)",
  border: "1px solid rgba(255,255,255,0.12)",
  color: "#aeb4c0",
  borderRadius: 999,
  padding: "2px 9px",
  fontSize: 11,
};
const reactDoneStyle: React.CSSProperties = {
  color: "#9aa0ad",
  fontSize: 11,
};
const btnStyle: React.CSSProperties = {
  flex: "1 1 auto",
  cursor: "pointer",
  background: "rgba(217, 188, 115, 0.14)",
  border: "1px solid rgba(217, 188, 115, 0.5)",
  color: "#f2d98f",
  borderRadius: 6,
  padding: "5px 6px",
  fontSize: 12,
};
const closeBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "transparent",
  border: "none",
  color: "#9aa0ad",
  fontSize: 18,
  lineHeight: 1,
};
const selectStyle: React.CSSProperties = {
  width: "100%",
  margin: "6px 0",
  background: "rgba(32, 36, 48, 0.9)",
  color: "#e8e2d2",
  border: "1px solid rgba(255,255,255,0.12)",
  borderRadius: 6,
  padding: "5px 6px",
  fontSize: 12,
};
const toastStyle: React.CSSProperties = {
  marginTop: 8,
  padding: "6px 8px",
  borderRadius: 6,
  background: "rgba(111, 141, 181, 0.18)",
  border: "1px solid rgba(111, 141, 181, 0.45)",
  color: "#cdd7e6",
  fontSize: 12,
};
// 待决策倒计时条（拿主意还剩多久）——自包含内联样式，不依赖 fate.css。
const countdownBaseStyle: React.CSSProperties = {
  display: "inline-flex",
  alignItems: "center",
  gap: 5,
  margin: "8px 0 2px",
  padding: "3px 10px",
  borderRadius: 999,
  fontSize: 11,
  letterSpacing: 0.3,
  color: "#e0c789",
  background: "rgba(217, 188, 115, 0.12)",
  border: "1px solid rgba(217, 188, 115, 0.3)",
};
const countdownUrgentStyle: React.CSSProperties = {
  ...countdownBaseStyle,
  color: "#ff9a8a",
  background: "rgba(180, 84, 58, 0.18)",
  border: "1px solid rgba(180, 84, 58, 0.5)",
};
const countdownExpiredStyle: React.CSSProperties = {
  ...countdownBaseStyle,
  color: "#9aa0ad",
  background: "rgba(120, 120, 120, 0.12)",
  border: "1px solid rgba(255,255,255,0.1)",
};

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
  const style = cd.expired ? countdownExpiredStyle : cd.urgent ? countdownUrgentStyle : countdownBaseStyle;
  return (
    <div style={style}>
      <span aria-hidden>{cd.expired ? "⌛" : "⏳"}</span>
      {formatFateCountdown(cd)}
    </div>
  );
}

function Bar({ label, value, max }: { label: string; value: number; max: number }) {
  const pct = Math.max(0, Math.min(100, (value / max) * 100));
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 6, margin: "3px 0" }}>
      <span style={{ width: 32, color: "#9aa0ad", fontSize: 11 }}>{label}</span>
      <span style={{ flex: 1, height: 6, background: "rgba(255,255,255,0.08)", borderRadius: 3, overflow: "hidden" }}>
        <span style={{ display: "block", height: "100%", width: `${pct}%`, background: "#d9bc73" }} />
      </span>
      <span style={{ width: 28, textAlign: "right", color: "#cbd1da", fontSize: 11 }}>{Math.round(value)}</span>
    </div>
  );
}

// FatePanel 是接进 App 的命运四槽浮层。
export function FatePanel({ sessionId, units, initialUnitID, onClose }: Props) {
  const firstUnitID = useMemo(() => {
    if (initialUnitID && units.some((u) => u.id === initialUnitID)) {
      return initialUnitID;
    }
    return units[0]?.id ?? "";
  }, [initialUnitID, units]);

  const [unitID, setUnitID] = useState<string>(firstUnitID);
  const [status, setStatus] = useState<StatusCard | null>(null);
  const [cards, setCards] = useState<FateCard[]>([]);
  const [resolving, setResolving] = useState<string>("");
  const [toast, setToast] = useState("");
  const seenRef = useRef<Set<string>>(new Set());
  // shareInitiatedRef 守卫 share_initiated 同一卡只上报一次（按 narrative 去重），避免重复点击灌漏斗。
  const shareInitiatedRef = useRef<Set<string>>(new Set());
  // reactedRef 守卫高光卡三键反馈同一卡只记一次（按 fateCardKey 去重）；reactedTick 仅驱动按钮态重渲。
  const reactedRef = useRef<Set<string>>(new Set());
  const [reactedTick, setReactedTick] = useState(0);

  // 外部选中单位变化时，跟随聚焦（仅当面板尚未手动切换过其它单位时不强切；这里简单跟随首选）。
  useEffect(() => {
    if (firstUnitID && !units.some((u) => u.id === unitID)) {
      setUnitID(firstUnitID);
    }
  }, [firstUnitID, unitID, units]);

  const refresh = useCallback(async () => {
    if (!unitID) {
      setStatus(null);
      setCards([]);
      return;
    }
    try {
      const [unit, feed] = await Promise.all([getUnitStatus(unitID), getFateFeed(unitID)]);
      setStatus(readStatus(unit));
      setCards(feed);
    } catch (err) {
      setToast(`读取命运失败：${err instanceof Error ? err.message : String(err)}`);
    }
  }, [unitID]);

  useEffect(() => {
    seenRef.current = new Set();
    void refresh();
  }, [refresh]);

  // 实时增量：WS 推来的新卡即时插入（按 route+narrative 去重，避免与首屏重复）。
  useEffect(() => {
    if (!sessionId || !unitID) {
      return undefined;
    }
    const unsub = subscribeSessionStream(sessionId, {
      onFateInbox: (payload) => {
        if (String(payload.unit_id ?? "") !== unitID) return;
        const key = `${String(payload.route ?? "")}:${cardText(payload)}`;
        if (seenRef.current.has(key)) return;
        seenRef.current.add(key);
        const route = String(payload.route ?? "");
        // WS payload 通常不含 expires_at/countdown_hours（后端只推 occurred_at 或更少）；
        // 有就透传，没有则留空，由 computeFateCountdown 按 occurred_at + 48h 兜底。
        const card: FateCard = {
          kind: route === "pending" ? "pending" : "highlight",
          decision_id: payload.decision_id ? String(payload.decision_id) : undefined,
          narrative: cardText(payload),
          occurred_at: payload.occurred_at ? String(payload.occurred_at) : undefined,
          expires_at: payload.expires_at ? String(payload.expires_at) : undefined,
          countdown_hours:
            typeof payload.countdown_hours === "number" ? payload.countdown_hours : undefined,
        };
        setCards((prev) => [card, ...prev]);
      },
      onFateEcho: (payload) => {
        if (String(payload.unit_id ?? "") !== unitID) return;
        setCards((prev) => [{ kind: "echo", narrative: cardText(payload) }, ...prev]);
      },
    });
    return unsub;
  }, [sessionId, unitID]);

  const pending = useMemo(() => cards.filter((c) => c.kind === "pending"), [cards]);
  const highlights = useMemo(() => cards.filter((c) => c.kind === "highlight").slice(0, 3), [cards]);
  const echoes = useMemo(() => cards.filter((c) => c.kind === "echo").slice(0, 4), [cards]);

  const onResolve = useCallback(
    async (decisionId: string, resolveType: string, label: string) => {
      if (!decisionId) return;
      setResolving(decisionId);
      try {
        await resolveFateDecision(decisionId, sessionId, unitID, resolveType);
        setCards((prev) => prev.filter((c) => c.decision_id !== decisionId));
        setToast(`你${label}。她记下了。`);
      } catch (err) {
        setToast(`没能传达：${err instanceof Error ? err.message : String(err)}`);
      } finally {
        setResolving("");
      }
    },
    [sessionId, unitID],
  );

  // onShare 把一段命运叙事复制到剪贴板供玩家分享，并埋点 share_initiated（同一段叙事只上报一次）。
  // 埋点与复制均 best-effort：剪贴板不可用时仍给 toast 反馈，绝不阻断 UX。
  const onShare = useCallback((narrative: string) => {
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
  }, []);

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
    <aside style={panelStyle} role="dialog" aria-label="命运面板">
      <div style={headerStyle}>
        <div>
          <div style={brandStyle}>一念 · 命运</div>
          <div style={subStyle}>你是垂看后人的先祖，能指引能疾呼，却不能替她活。</div>
        </div>
        <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭命运面板">
          ×
        </button>
      </div>

      {units.length > 1 ? (
        <select
          style={selectStyle}
          value={unitID}
          onChange={(e) => setUnitID(e.target.value)}
          aria-label="选择关注的角色"
        >
          {units.map((u) => (
            <option key={u.id} value={u.id}>
              {u.name}
            </option>
          ))}
        </select>
      ) : null}

      {/* 槽一：状态卡 */}
      <div style={sectionCardStyle}>
        {status ? (
          <>
            <div style={{ fontWeight: 600, color: "#f0ead8" }}>{status.name}</div>
            {status.lineage ? <div style={{ color: "#9aa0ad", fontSize: 11 }}>{status.lineage}</div> : null}
            <div style={{ marginTop: 6 }}>
              <Bar label="气血" value={status.hp} max={100} />
              <Bar label="饥饱" value={status.hunger} max={100} />
              <Bar label="心气" value={Math.round(status.morale * 100)} max={100} />
            </div>
            {status.biography ? (
              <p style={{ margin: "6px 0 0", color: "#c2c7d0", fontSize: 12 }}>{status.biography}</p>
            ) : null}
          </>
        ) : (
          <div style={{ color: "#9aa0ad" }}>{unitID ? "正在感应她的气息…" : "选一个你的人，看看她的命运。"}</div>
        )}
      </div>

      {/* 槽四：待决策（最显眼，等你拿主意） */}
      {pending.length > 0 ? (
        <>
          <div style={slotTitleStyle}>有件关乎她的事，在等你拿个主意</div>
          <div style={pendingCardStyle}>
            <p style={{ margin: 0 }}>{pending[0].narrative}</p>
            <FateCountdownBar card={pending[0]} />
            <div style={actionRowStyle}>
              {/* 情境化 choices：后端 buildFateChoices 按事件类型/红线/关系生成贴合选项（label/id），
                  resolveType 传 choice.id，后端 resolveFateChoiceClass 会把情境 id 折回基础后果类。
                  feed 未带 choices 时回落硬编码三键（向后兼容 WS 推送/旧后端）。 */}
              {pending[0].choices && pending[0].choices.length > 0 ? (
                pending[0].choices.map((c) => (
                  <button
                    key={c.id}
                    type="button"
                    style={btnStyle}
                    disabled={resolving === pending[0].decision_id}
                    onClick={() => void onResolve(pending[0].decision_id ?? "", c.id, c.label)}
                  >
                    {c.label}
                  </button>
                ))
              ) : (
                <>
                  <button
                    type="button"
                    style={btnStyle}
                    disabled={resolving === pending[0].decision_id}
                    onClick={() => void onResolve(pending[0].decision_id ?? "", "let_her", "由她去")}
                  >
                    由她去
                  </button>
                  <button
                    type="button"
                    style={btnStyle}
                    disabled={resolving === pending[0].decision_id}
                    onClick={() => void onResolve(pending[0].decision_id ?? "", "urge", "疾呼拦住")}
                  >
                    疾呼拦住
                  </button>
                  <button
                    type="button"
                    style={btnStyle}
                    disabled={resolving === pending[0].decision_id}
                    onClick={() => void onResolve(pending[0].decision_id ?? "", "acknowledge", "默默看着")}
                  >
                    默默看着
                  </button>
                </>
              )}
            </div>
            {pending.length > 1 ? (
              <div style={{ color: "#9aa0ad", fontSize: 11, marginTop: 6 }}>还有 {pending.length - 1} 件事在等你</div>
            ) : null}
          </div>
        </>
      ) : null}

      {/* 槽二：高光卡 */}
      <div style={slotTitleStyle}>她近来经历的</div>
      {highlights.length === 0 ? (
        <div style={{ ...sectionCardStyle, color: "#9aa0ad" }}>还很平静。让世界往前走走看。</div>
      ) : (
        highlights.map((c, i) => {
          // reactedTick 进表达式仅为让按钮态随 reactedRef 变化重渲（值本身不参与逻辑）。
          const reacted = reactedTick >= 0 && reactedRef.current.has(fateCardKey(c));
          return (
            <div key={`h${i}`} style={sectionCardStyle}>
              <div>{c.narrative}</div>
              {/* 三键轻反馈：这段她的命运，落在你预期里还是吓你一跳？一点即埋点（同卡只记一次）。 */}
              <div style={reactRowStyle}>
                {reacted ? (
                  <span style={reactDoneStyle}>已反馈，多谢</span>
                ) : (
                  <>
                    <span style={{ color: "#7e8493", fontSize: 11 }}>这段：</span>
                    <button type="button" style={reactBtnStyle} title="在意料之中" onClick={() => onReact(c, "expected")}>
                      意料之中
                    </button>
                    <button type="button" style={reactBtnStyle} title="有点意外，但合理" onClick={() => onReact(c, "surprise")}>
                      有点意外但合理
                    </button>
                    <button type="button" style={reactBtnStyle} title="太离谱了，不像她" onClick={() => onReact(c, "ooc")}>
                      太离谱
                    </button>
                  </>
                )}
              </div>
              <button
                type="button"
                style={shareBtnStyle}
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
        })
      )}

      {/* 槽三：回响带 */}
      {echoes.length > 0 ? (
        <>
          <div style={slotTitleStyle}>回响</div>
          {echoes.map((c, i) => (
            <div key={`e${i}`} style={echoCardStyle}>
              {c.narrative}
            </div>
          ))}
        </>
      ) : null}

      {toast ? <div style={toastStyle}>{toast}</div> : null}
    </aside>
  );
}

export default FatePanel;
