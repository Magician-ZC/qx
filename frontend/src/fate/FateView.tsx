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

  const refresh = useCallback(async () => {
    try {
      const [unit, feed] = await Promise.all([getUnitStatus(unitId), getFateFeed(unitId)]);
      setStatus(readStatus(unit));
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
        const key = `${payload.route}:${cardText(payload)}`;
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
          {pending.length > 1 && <div className="fate-more">还有 {pending.length - 1} 件事在等你</div>}
        </section>
      )}

      {/* 槽二：高光卡（一瞥她经历的事） */}
      <section className="fate-highlights">
        <div className="fate-slot-title">她近来经历的</div>
        {highlights.length === 0 && <div className="fate-empty">还很平静。让世界往前走走看。</div>}
        {highlights.map((c, i) => (
          <div className="fate-card fate-card-highlight" key={`h${i}`}>
            <span className="fate-card-text">{c.narrative}</span>
            <button
              className="fate-share-btn"
              title="把她的故事讲给别人听"
              onClick={() => onShare(c.narrative)}
            >
              讲给别人听
            </button>
          </div>
        ))}
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
