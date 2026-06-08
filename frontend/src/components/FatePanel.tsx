/* 文件说明：命运四槽侧栏面板（设计宪法 §4.6），接进主指挥客户端 App.tsx。
   把独立 #fate 路由的「四槽」体验做成一个可折叠的浮层面板：对当前指挥阵营的某个单位
   调 getFateFeed 渲染——状态卡（她现在怎样）/ 高光（一瞥她经历的事）/ 待决策（等你拿主意，
   let_her/urge/acknowledge 三按钮）/ 回响带（因为你上次…）。实时增量来自会话流的
   fate_inbox / fate_echo 推送（本面板自管订阅，scoped 到当前选中单位，App 无需额外接线）。
   祖魂语气：不出现「命令/控制」字眼；玩家是垂看后人的先祖。自包含内联样式，不依赖 fate.css。*/

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  getFateFeed,
  getUnitStatus,
  resolveFateDecision,
  subscribeSessionStream,
  type FateCard,
} from "../session/api";
import { computeFateCountdown, formatFateCountdown } from "../fate/countdown";

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

const panelStyle: React.CSSProperties = {
  position: "absolute",
  top: 64,
  right: 12,
  width: 320,
  maxHeight: "calc(100vh - 96px)",
  overflowY: "auto",
  zIndex: 40,
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

  return (
    <aside style={panelStyle} role="dialog" aria-label="命运面板">
      <div style={headerStyle}>
        <div>
          <div style={brandStyle}>群像 · 命运</div>
          <div style={subStyle}>你是垂看后人的先祖，能托梦能疾呼，却不能替她活。</div>
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
        highlights.map((c, i) => (
          <div key={`h${i}`} style={sectionCardStyle}>
            {c.narrative}
          </div>
        ))
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
