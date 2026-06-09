/* 文件说明：副本「异步分段推进」前端面板（消费后端 StartDungeonAsync / RunDungeonSegment / ResumePausedDungeonSegment /
   DungeonSegmentStatusView，设计 PvE威胁系统.md §3-5）。
   与既有 DungeonPanel.tsx（同步一次跑完）并列：本面板把副本切成「逐段可中断、关键节点暂停问玩家」的异步流：
     - 创建首段（start）→ 逐段推进（step）→ NextAction 决定下一步：
       · continue_next_floor：可自动续下层（面板出「继续」按钮）；
       · pause_first_contact：末层 boss 首触暂停 → 出 PauseCard + 「继续/撤退」按钮；
       · pause_player_decision：濒死撤退抉择 → 出 PauseCard + 「继续/撤退」按钮；
       · completed_*：终局（通关/撤退/团灭），展示终局态。
     - resume(continue/retreat) 续跑或见好就收。
   QUNXIANG_DUNGEON 关时后端报 ErrDungeonDisabled → 据 APIError 透出「副本未启用」。
   依赖注入：本面板不直接 import api.ts（接线由 App.tsx 注入下列回调，见 crossFileNeeds.frontendWiring），以保持组件自包含、
   不与并发改 api.ts 的工作冲突。自包含内联样式，参照 DungeonPanel.tsx / WorldBossPanel.tsx 右侧浮层范式。 */

import { useCallback, useState } from "react";
import { zIndex } from "../zindex-tokens";

// DungeonNextAction 与后端 session.DungeonNextAction 枚举对齐。
export type DungeonNextAction =
  | "continue_next_floor"
  | "pause_first_contact"
  | "pause_player_decision"
  | "completed_cleared"
  | "completed_fled"
  | "completed_wiped";

// DungeonSegmentResult 与后端 session.DungeonSegmentResult 对齐（无 json tag → 键名为 Go 大写字段名）。
export type DungeonSegmentResult = {
  SegmentID: string;
  NextAction: DungeonNextAction;
  Floor: number;
  PauseCard: string;
  Outcome: string;
};

// DungeonSegmentStartResult 是 start 端点返回的首段标识（至少含 segment id + floors）。
export type DungeonSegmentStartResult = {
  segment_id: string;
  floors: number;
  floor: number;
  state: string;
};

// 注入的 API 回调契约（App.tsx 用 api.ts 的对应函数填充）。
type DungeonSegmentAPI = {
  // 创建首段。
  startDungeonAsync: (
    sessionID: string,
    unitIDs: string[],
    floors: number,
  ) => Promise<DungeonSegmentStartResult>;
  // 推进一段（不暂停则一直跑到下一关键节点/终局）。
  runDungeonSegment: (sessionID: string, segmentID: string) => Promise<DungeonSegmentResult>;
  // 玩家回来据选择续跑（choice: "continue" | "retreat"）。
  resumeDungeonSegment: (
    sessionID: string,
    segmentID: string,
    choice: "continue" | "retreat",
  ) => Promise<DungeonSegmentResult>;
};

type Props = {
  // sessionID 当前会话 ID。
  sessionID: string;
  // partyCandidates 本局可组队单位（多选勾选）。
  partyCandidates: { id: string; name: string }[];
  // api 注入的副本分段 API 回调（由 App.tsx 用 api.ts 函数填充）。
  api: DungeonSegmentAPI;
  // onClose 关闭面板。
  onClose: () => void;
};

const FLOOR_MIN = 2;
const FLOOR_MAX = 6;
const FLOOR_DEFAULT = 3;

// isTerminal 判断 NextAction 是否为终局。
function isTerminal(a: DungeonNextAction): boolean {
  return a === "completed_cleared" || a === "completed_fled" || a === "completed_wiped";
}

// isPaused 判断 NextAction 是否为关键节点暂停。
function isPaused(a: DungeonNextAction): boolean {
  return a === "pause_first_contact" || a === "pause_player_decision";
}

// actionLabel 把 NextAction 转中文。
function actionLabel(a: DungeonNextAction): string {
  switch (a) {
    case "continue_next_floor":
      return "可推进下一层";
    case "pause_first_contact":
      return "末层 BOSS 当前——要不要打？";
    case "pause_player_decision":
      return "她撑不住了——要不要撤？";
    case "completed_cleared":
      return "通关";
    case "completed_fled":
      return "撤离（保住已得）";
    case "completed_wiped":
      return "折戟（人还在）";
    default:
      return a;
  }
}

// errText 把错误归一为可展示文案（含「未启用」识别）。
function errText(err: unknown): string {
  const msg = err instanceof Error ? err.message : String(err);
  return msg;
}

export function DungeonSegmentPanel({ sessionID, partyCandidates, api, onClose }: Props) {
  const [selectedIDs, setSelectedIDs] = useState<string[]>([]);
  const [floors, setFloors] = useState<number>(FLOOR_DEFAULT);
  const [segmentID, setSegmentID] = useState<string>("");
  const [last, setLast] = useState<DungeonSegmentResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [log, setLog] = useState<string[]>([]);

  const toggleMember = useCallback((id: string) => {
    setSelectedIDs((prev) => (prev.includes(id) ? prev.filter((x) => x !== id) : [...prev, id]));
  }, []);

  const pushLog = useCallback((line: string) => {
    setLog((prev) => [...prev, line].slice(-12));
  }, []);

  // doStart 创建首段并推进第一段。
  const doStart = useCallback(async () => {
    if (selectedIDs.length === 0) {
      setErr("请至少勾选一名队员。");
      return;
    }
    setBusy(true);
    setErr("");
    setLog([]);
    setLast(null);
    try {
      const started = await api.startDungeonAsync(sessionID, selectedIDs, floors);
      setSegmentID(started.segment_id);
      pushLog(`踏入秘境（共 ${started.floors} 层）。`);
      const res = await api.runDungeonSegment(sessionID, started.segment_id);
      setLast(res);
      pushLog(`第 ${res.Floor} 层：${actionLabel(res.NextAction)}`);
    } catch (e) {
      const msg = errText(e);
      setErr(msg.includes("未启用") ? `副本未启用：${msg}` : `下副本失败：${msg}`);
    } finally {
      setBusy(false);
    }
  }, [api, floors, pushLog, selectedIDs, sessionID]);

  // doStep 在 continue_next_floor 时推进下一段。
  const doStep = useCallback(async () => {
    if (!segmentID) return;
    setBusy(true);
    setErr("");
    try {
      const res = await api.runDungeonSegment(sessionID, segmentID);
      setLast(res);
      pushLog(`第 ${res.Floor} 层：${actionLabel(res.NextAction)}`);
    } catch (e) {
      setErr(`推进失败：${errText(e)}`);
    } finally {
      setBusy(false);
    }
  }, [api, pushLog, segmentID, sessionID]);

  // doResume 在暂停时据玩家选择续跑/撤退。
  const doResume = useCallback(
    async (choice: "continue" | "retreat") => {
      if (!segmentID) return;
      setBusy(true);
      setErr("");
      try {
        const res = await api.resumeDungeonSegment(sessionID, segmentID, choice);
        setLast(res);
        pushLog(choice === "retreat" ? "她见好就收了。" : `继续深入：${actionLabel(res.NextAction)}`);
      } catch (e) {
        setErr(`恢复失败：${errText(e)}`);
      } finally {
        setBusy(false);
      }
    },
    [api, pushLog, segmentID, sessionID],
  );

  const action = last?.NextAction;
  const paused = action ? isPaused(action) : false;
  const terminal = action ? isTerminal(action) : false;
  const canContinueFloor = action === "continue_next_floor";

  return (
    <aside style={panelStyle} role="dialog" aria-label="副本分段推进面板">
      <div style={headerStyle}>
        <div>
          <div style={brandStyle}>副本 · 异步分段</div>
          <div style={subStyle}>逐段推进 · 关键节点暂停问你 · 离线超时见好就收</div>
        </div>
        <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭面板">
          ×
        </button>
      </div>

      {/* ---- 组队（未开始时可选）---- */}
      {!segmentID ? (
        <>
          <div style={slotTitleStyle}>组队（已选 {selectedIDs.length}）</div>
          {partyCandidates.length === 0 ? (
            <div style={{ ...sectionCardStyle, ...mutedStyle }}>本局暂无可组队单位。</div>
          ) : (
            <div style={sectionCardStyle}>
              {partyCandidates.map((c) => {
                const checked = selectedIDs.includes(c.id);
                return (
                  <label key={c.id} style={checked ? memberRowSelectedStyle : memberRowStyle}>
                    <input type="checkbox" checked={checked} onChange={() => toggleMember(c.id)} />
                    <span style={{ color: "#f0ead8" }}>{c.name}</span>
                  </label>
                );
              })}
            </div>
          )}
          <div style={sectionCardStyle}>
            <label style={labelStyle} htmlFor="dgseg-floors">
              层数
            </label>
            <select
              id="dgseg-floors"
              style={selectStyle}
              value={String(floors)}
              onChange={(e) => setFloors(Number.parseInt(e.target.value, 10))}
            >
              {Array.from({ length: FLOOR_MAX - FLOOR_MIN + 1 }, (_, i) => FLOOR_MIN + i).map((n) => (
                <option key={n} value={n}>
                  {n} 层{n === FLOOR_DEFAULT ? "（推荐）" : ""}
                </option>
              ))}
            </select>
            <div style={{ display: "flex", justifyContent: "flex-end", marginTop: 12 }}>
              <button
                type="button"
                style={{ ...primaryBtnStyle, opacity: busy || selectedIDs.length === 0 ? 0.6 : 1 }}
                onClick={() => void doStart()}
                disabled={busy || selectedIDs.length === 0}
              >
                {busy ? "踏入中…" : "踏入秘境"}
              </button>
            </div>
          </div>
        </>
      ) : null}

      {/* ---- 当前段状态 + 暂停卡 + 决策按钮 ---- */}
      {last ? (
        <>
          <div style={slotTitleStyle}>当前进展</div>
          <div style={sectionCardStyle}>
            <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
              <span style={mutedStyle}>第 {last.Floor} 层</span>
              <span style={statePill(action)}>{actionLabel(action!)}</span>
            </div>
          </div>

          {/* 暂停卡（祖魂语气，绝不裸露数值）+ 决策按钮 */}
          {paused && last.PauseCard ? (
            <>
              <div style={inboxCardStyle}>{last.PauseCard}</div>
              <div style={{ display: "flex", gap: 8, marginTop: 8 }}>
                <button
                  type="button"
                  style={{ ...primaryBtnStyle, flex: 1, opacity: busy ? 0.6 : 1 }}
                  onClick={() => void doResume("continue")}
                  disabled={busy}
                >
                  继续深入
                </button>
                <button
                  type="button"
                  style={{ ...secondaryBtnStyle, flex: 1, opacity: busy ? 0.6 : 1 }}
                  onClick={() => void doResume("retreat")}
                  disabled={busy}
                >
                  见好就收
                </button>
              </div>
            </>
          ) : null}

          {/* 可自动推进下一层 */}
          {canContinueFloor ? (
            <div style={{ display: "flex", justifyContent: "flex-end", marginTop: 8 }}>
              <button
                type="button"
                style={{ ...primaryBtnStyle, opacity: busy ? 0.6 : 1 }}
                onClick={() => void doStep()}
                disabled={busy}
              >
                {busy ? "推进中…" : "推进下一层"}
              </button>
            </div>
          ) : null}

          {/* 终局态 */}
          {terminal ? (
            <div style={terminal && action === "completed_wiped" ? toastErrStyle : toastOkStyle}>
              {actionLabel(action!)}
              {last.Outcome ? `（${last.Outcome}）` : ""}
            </div>
          ) : null}
        </>
      ) : null}

      {/* ---- 推进日志 ---- */}
      {log.length > 0 ? (
        <>
          <div style={slotTitleStyle}>足迹</div>
          <div style={sectionCardStyle}>
            {log.map((line, i) => (
              <div key={i} style={{ ...mutedStyle, fontSize: 11, padding: "2px 0" }}>
                {line}
              </div>
            ))}
          </div>
        </>
      ) : null}

      {err ? <div style={toastErrStyle}>{err}</div> : null}
    </aside>
  );
}

// ============ 内联样式（参照 DungeonPanel.tsx 右侧浮层范式） ============

const panelStyle: React.CSSProperties = {
  position: "absolute",
  top: 64,
  right: 12,
  width: 380,
  maxHeight: "calc(100vh - 96px)",
  overflowY: "auto",
  zIndex: zIndex.rightPanel,
  background: "rgba(18, 20, 28, 0.95)",
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
  margin: "14px 0 4px",
  textTransform: "uppercase",
};
const labelStyle: React.CSSProperties = {
  display: "block",
  color: "#cdb98a",
  fontSize: 11,
  letterSpacing: 0.4,
  margin: "10px 0 4px",
};
const selectStyle: React.CSSProperties = {
  width: "100%",
  boxSizing: "border-box",
  background: "rgba(32, 36, 48, 0.9)",
  color: "#e8e2d2",
  border: "1px solid rgba(255,255,255,0.12)",
  borderRadius: 6,
  padding: "7px 8px",
  fontSize: 13,
};
const sectionCardStyle: React.CSSProperties = {
  background: "rgba(32, 36, 48, 0.7)",
  border: "1px solid rgba(255,255,255,0.06)",
  borderRadius: 8,
  padding: "8px 10px",
  margin: "6px 0",
};
const memberRowStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  gap: 8,
  padding: "5px 8px",
  borderRadius: 6,
  cursor: "pointer",
};
const memberRowSelectedStyle: React.CSSProperties = {
  ...memberRowStyle,
  background: "rgba(217, 188, 115, 0.1)",
};
const primaryBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "rgba(217, 188, 115, 0.18)",
  border: "1px solid rgba(217, 188, 115, 0.6)",
  color: "#f2d98f",
  borderRadius: 6,
  padding: "8px 14px",
  fontSize: 13,
  fontWeight: 600,
};
const secondaryBtnStyle: React.CSSProperties = {
  ...primaryBtnStyle,
  background: "rgba(120, 130, 150, 0.15)",
  border: "1px solid rgba(150, 160, 180, 0.5)",
  color: "#cfd6e2",
};
const closeBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "transparent",
  border: "none",
  color: "#9aa0ad",
  fontSize: 18,
  lineHeight: 1,
};
const mutedStyle: React.CSSProperties = { color: "#9aa0ad" };
const inboxCardStyle: React.CSSProperties = {
  ...sectionCardStyle,
  borderColor: "rgba(217, 188, 115, 0.45)",
  background: "rgba(217, 188, 115, 0.08)",
  color: "#f0ead8",
  fontStyle: "italic",
};
const pillBaseStyle: React.CSSProperties = {
  display: "inline-block",
  fontSize: 11,
  padding: "2px 10px",
  borderRadius: 999,
  fontWeight: 600,
};
const toastOkStyle: React.CSSProperties = {
  marginTop: 10,
  padding: "8px 10px",
  borderRadius: 6,
  background: "rgba(111, 181, 130, 0.16)",
  border: "1px solid rgba(111, 181, 130, 0.5)",
  color: "#bfe6c8",
  fontSize: 12,
};
const toastErrStyle: React.CSSProperties = {
  ...toastOkStyle,
  background: "rgba(196, 84, 74, 0.16)",
  border: "1px solid rgba(196, 84, 74, 0.5)",
  color: "#f0b0a6",
};

// statePill 按 NextAction 选徽标配色（暂停=琥珀，团灭=红，通关/继续=绿）。
function statePill(a: DungeonNextAction | undefined): React.CSSProperties {
  if (!a) return pillBaseStyle;
  if (a === "completed_cleared" || a === "continue_next_floor") {
    return {
      ...pillBaseStyle,
      color: "#bfe6c8",
      background: "rgba(111, 181, 130, 0.16)",
      border: "1px solid rgba(111, 181, 130, 0.5)",
    };
  }
  if (a === "completed_wiped") {
    return {
      ...pillBaseStyle,
      color: "#f0b0a6",
      background: "rgba(196, 84, 74, 0.16)",
      border: "1px solid rgba(196, 84, 74, 0.5)",
    };
  }
  return {
    ...pillBaseStyle,
    color: "#e6d3a0",
    background: "rgba(217, 188, 115, 0.16)",
    border: "1px solid rgba(217, 188, 115, 0.4)",
  };
}

export default DungeonSegmentPanel;
