/* 文件说明：任务面板浮层（分区大世界阶段3 §5「任务系统」的命运客户端露出）。
   全屏遮罩 overlay，墨色宣纸调（全内联样式，不碰 fate.css/styles.css，仿 WorldMap 范式）。
   挂载即并发拉「可接任务（当前区或指定区）」+「进行中任务」，分三段渲染：
     1) 可接任务：标题 + 叙事 + 目标 + 奖励 +「接取」按钮（acceptQuest→刷新）。
     2) 进行中：标题 + 各目标进度 N/M（available/active，目标未全满）。
     3) 可交付：进行中里 objective 全满（state=completed 或本地判全满）的「交付」按钮（turnInQuest→发奖→刷新）。
   busy 态防重复点；接取/交付后重拉两侧列表。遮罩空白 / Esc 关闭（busy 时不关，避免打断反馈）。
   剧情/标题由后端按角色画像 LLM 动态生成，本面板纯展示，不依赖任何外部 CSS。 */

import { useCallback, useEffect, useMemo, useState } from "react";
import { zIndex } from "../zindex-tokens";
import {
  acceptQuest,
  getActiveQuests,
  getAvailableQuests,
  turnInQuest,
  type Quest,
  type QuestObjective,
  type QuestReward,
} from "../session/api";

type Props = {
  sessionId: string;
  unitId: string;
  // zoneId 可选：限定拉某区的可接任务（缺省由后端按当前区生成）。城镇 NPC 入口可传该镇所在区。
  zoneId?: string;
  onClose: () => void;
  // onChanged 接取/交付成功后回调（让父级刷新快照——任务进度也喂自治、解锁传送会改可达区）。
  onChanged?: () => void;
};

// 任务类型中文 + 配色（slay 讨伐暗红 / collect 收集金 / explore 探索暖蓝 / 其它灰）。
const QUEST_TYPE_META: Record<string, { label: string; color: string }> = {
  slay: { label: "讨伐", color: "#a3433f" },
  collect: { label: "收集", color: "#c79a3a" },
  explore: { label: "探索", color: "#3f7fb0" },
  escort: { label: "护送", color: "#6b8e5a" },
  story: { label: "剧情", color: "#7a5cae" },
};

function questTypeMeta(type: string) {
  return QUEST_TYPE_META[type] ?? { label: type || "任务", color: "#7a7268" };
}

// objectiveText 把一条目标翻成一句中文进度行（kind 决定动词，target 为对象，N/M 为进度）。
function objectiveText(o: QuestObjective): string {
  const progress = `${o.current}/${o.required}`;
  switch (o.kind) {
    case "defeat_boss":
      return `讨平此地霸主（${progress}）`;
    case "collect_item":
      return `采集「${o.target}」×${o.required}（${progress}）`;
    case "reach_zone":
      return `抵达指定之地（${progress}）`;
    default:
      return `${o.target || "目标"}（${progress}）`;
  }
}

// objectiveDone 判定单条目标是否达成。
function objectiveDone(o: QuestObjective): boolean {
  return o.current >= o.required;
}

// allObjectivesDone 判定一桩任务的全部目标是否达成（空目标视为未达成，与后端口径一致）。
function allObjectivesDone(q: Quest): boolean {
  if (!q.objectives || q.objectives.length === 0) {
    return false;
  }
  return q.objectives.every(objectiveDone);
}

// rewardText 把奖励拼成一句中文摘要（钱包/经验/物品/解锁传送，缺省项跳过）。
function rewardText(r: QuestReward): string {
  const parts: string[] = [];
  if (r.wallet > 0) parts.push(`${r.wallet} 钱`);
  if (r.exp > 0) parts.push(`${r.exp} 阅历`);
  if (r.item_grants && r.item_grants.length > 0) {
    parts.push(`${r.item_grants.join("、")}`);
  }
  if (r.unlock_zone && r.unlock_zone.trim()) parts.push(`解锁「${r.unlock_zone}」传送`);
  return parts.length > 0 ? parts.join(" · ") : "酬谢从优";
}

export function QuestPanel({ sessionId, unitId, zoneId, onClose, onChanged }: Props) {
  const [available, setAvailable] = useState<Quest[]>([]);
  const [active, setActive] = useState<Quest[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  // actError：接取/交付动作的错误（与 loadError 分开，不冲掉列表）。
  const [actError, setActError] = useState("");
  // busyID：正在接取/交付的任务 id（非空时该卡按钮禁点，防重复提交）。
  const [busyID, setBusyID] = useState("");

  // loadQuests 并发拉两侧列表（可接 + 进行中），区分「真没任务」与「后端出错/正忙」：
  //   - 用 allSettled 让一侧失败不连累另一侧（仍能展示另一侧拿到的列表）。
  //   - 任一侧 reject（409 执行互斥 / 400 归属·站位·终局 / fetch 断网，均为中文 APIError.message）→ 把真实错误透出 loadError，
  //     **不**再静默吞成 [] 伪装成空态。仅当两侧都成功且都为空时，才显示「此间暂无差遣」空态提示。
  // shouldSet 让 mount-effect 能取消过期请求的 setState（refresh 手动触发恒置 true）。
  const loadQuests = useCallback(
    async (shouldSet: () => boolean) => {
      setLoading(true);
      setLoadError("");
      const [availRes, actRes] = await Promise.allSettled([
        getAvailableQuests(sessionId, unitId, zoneId),
        getActiveQuests(sessionId, unitId),
      ]);
      if (!shouldSet()) return;

      const errMsg = (r: PromiseRejectedResult) =>
        r.reason instanceof Error ? r.reason.message : String(r.reason);

      const avail = availRes.status === "fulfilled" ? availRes.value : [];
      const act = actRes.status === "fulfilled" ? actRes.value : [];
      setAvailable(avail);
      setActive(act);

      if (availRes.status === "rejected" || actRes.status === "rejected") {
        // 透出第一条真实错误（两侧都错时优先报可接列表的错——那是面板主功能）。
        const reason =
          availRes.status === "rejected" ? errMsg(availRes) : errMsg(actRes as PromiseRejectedResult);
        setLoadError(reason || "差遣消息打听失败，请稍后再试。");
      } else if (avail.length === 0 && act.length === 0) {
        setLoadError("此间暂无差遣——往别处城镇问问，或许有人需要她。");
      }
      setLoading(false);
    },
    [sessionId, unitId, zoneId],
  );

  // refresh 手动重拉（接取/交付后调）——恒应用结果。
  const refresh = useCallback(() => loadQuests(() => true), [loadQuests]);

  useEffect(() => {
    let cancelled = false;
    void loadQuests(() => !cancelled);
    return () => {
      cancelled = true;
    };
  }, [loadQuests]);

  // Esc 关闭（busy 中不关，避免打断接取/交付反馈）。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !busyID) {
        onClose();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [busyID, onClose]);

  const handleAccept = useCallback(
    async (q: Quest) => {
      if (busyID) return;
      setActError("");
      setBusyID(q.id);
      try {
        await acceptQuest(sessionId, unitId, q.id);
        onChanged?.();
        await refresh();
      } catch (err) {
        setActError(err instanceof Error ? err.message : String(err));
      } finally {
        setBusyID("");
      }
    },
    [busyID, sessionId, unitId, onChanged, refresh],
  );

  const handleTurnIn = useCallback(
    async (q: Quest) => {
      if (busyID) return;
      setActError("");
      setBusyID(q.id);
      try {
        await turnInQuest(sessionId, unitId, q.id);
        onChanged?.();
        await refresh();
      } catch (err) {
        setActError(err instanceof Error ? err.message : String(err));
      } finally {
        setBusyID("");
      }
    },
    [busyID, sessionId, unitId, onChanged, refresh],
  );

  // 进行中任务里，objective 全满（completed 或本地判全满）的 → 可交付；其余 → 进行中。
  const { turnInable, inProgress } = useMemo(() => {
    const turnInableList: Quest[] = [];
    const inProgressList: Quest[] = [];
    for (const q of active) {
      if (q.state === "completed" || allObjectivesDone(q)) {
        turnInableList.push(q);
      } else {
        inProgressList.push(q);
      }
    }
    return { turnInable: turnInableList, inProgress: inProgressList };
  }, [active]);

  return (
    <div
      style={overlayStyle}
      role="dialog"
      aria-label="任务"
      aria-modal="true"
      onClick={(e) => {
        if (e.target === e.currentTarget && !busyID) {
          onClose();
        }
      }}
    >
      <div style={panelStyle}>
        <div style={headerStyle}>
          <div>
            <div style={titleStyle}>江湖 · 差遣</div>
            <div style={subtitleStyle}>替人了却心事，挣一份酬谢与名望。</div>
          </div>
          <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭任务面板">
            ×
          </button>
        </div>

        {actError ? <div style={errorBannerStyle}>{actError}</div> : null}
        {/* 部分失败横幅：一侧列表拉取出错但另一侧仍有内容时，非阻塞地透出真实中文错误（不冲掉已拿到的列表）。
            两侧都空的情形交给下方整屏 hint（区分「真没任务」空态与「出错/正忙」）。 */}
        {!loading && loadError && (available.length > 0 || active.length > 0) ? (
          <div style={errorBannerStyle}>{loadError}</div>
        ) : null}

        <div style={bodyStyle}>
          {loading ? (
            <div style={hintStyle}>正在打听差遣…</div>
          ) : loadError && available.length === 0 && active.length === 0 ? (
            <div style={hintStyle}>{loadError}</div>
          ) : (
            <>
              {/* 可交付（醒目居顶：目标已全达成，回城领赏）。 */}
              {turnInable.length > 0 ? (
                <div style={sectionStyle}>
                  <div style={sectionTitleStyle}>✓ 可交付（{turnInable.length}）</div>
                  {turnInable.map((q) => (
                    <QuestCard
                      key={q.id}
                      quest={q}
                      mode="turn-in"
                      busy={Boolean(busyID)}
                      busyHere={busyID === q.id}
                      onTurnIn={handleTurnIn}
                    />
                  ))}
                </div>
              ) : null}

              {/* 进行中（目标未全满，展示各目标进度）。 */}
              {inProgress.length > 0 ? (
                <div style={sectionStyle}>
                  <div style={sectionTitleStyle}>进行中（{inProgress.length}）</div>
                  {inProgress.map((q) => (
                    <QuestCard key={q.id} quest={q} mode="active" busy={Boolean(busyID)} busyHere={busyID === q.id} />
                  ))}
                </div>
              ) : null}

              {/* 可接任务（标题 + 叙事 + 目标 + 奖励 +「接取」）。 */}
              <div style={sectionStyle}>
                <div style={sectionTitleStyle}>可接 · 城镇张贴（{available.length}）</div>
                {available.length === 0 ? (
                  <div style={emptyHintStyle}>此地已无新差遣。去别处城镇看看。</div>
                ) : (
                  available.map((q) => (
                    <QuestCard
                      key={q.id}
                      quest={q}
                      mode="available"
                      busy={Boolean(busyID)}
                      busyHere={busyID === q.id}
                      onAccept={handleAccept}
                    />
                  ))
                )}
              </div>
            </>
          )}
        </div>
      </div>
    </div>
  );
}

// QuestCard 渲染单桩任务卡。mode 决定展示哪段 + 哪个动作按钮。
function QuestCard({
  quest,
  mode,
  busy,
  busyHere,
  onAccept,
  onTurnIn,
}: {
  quest: Quest;
  mode: "available" | "active" | "turn-in";
  busy: boolean;
  busyHere: boolean;
  onAccept?: (q: Quest) => void;
  onTurnIn?: (q: Quest) => void;
}) {
  const meta = questTypeMeta(quest.type);
  return (
    <div style={questCardStyle}>
      <div style={questCardTopStyle}>
        <span style={{ ...typeChipStyle, color: meta.color, borderColor: meta.color }}>{meta.label}</span>
        <span style={questTitleStyle}>{quest.title || "无名差遣"}</span>
      </div>

      {/* 叙事（按角色画像 LLM 动态生成）——仅可接卡展示完整切入叙事，进行中/可交付从简。 */}
      {mode === "available" && quest.narrative_zh ? (
        <div style={narrativeStyle}>{quest.narrative_zh}</div>
      ) : null}

      {/* 目标进度（每条一行 N/M，已达成打勾） */}
      {quest.objectives && quest.objectives.length > 0 ? (
        <ul style={objListStyle}>
          {quest.objectives.map((o, i) => {
            const ok = objectiveDone(o);
            return (
              <li key={`${o.kind}-${o.target}-${i}`} style={{ ...objItemStyle, color: ok ? "#3f7a4a" : "#6f5f48" }}>
                <span style={objMarkStyle}>{ok ? "✓" : "•"}</span>
                {objectiveText(o)}
              </li>
            );
          })}
        </ul>
      ) : null}

      {/* 奖励 + 动作按钮 */}
      <div style={questFooterStyle}>
        <span style={rewardStyle}>酬：{rewardText(quest.rewards)}</span>
        {mode === "available" ? (
          <button
            type="button"
            style={{ ...actionBtnStyle, borderColor: meta.color, color: meta.color }}
            disabled={busy}
            onClick={() => onAccept?.(quest)}
          >
            {busyHere ? "接取中…" : "接取"}
          </button>
        ) : mode === "turn-in" ? (
          <button
            type="button"
            style={{ ...actionBtnStyle, borderColor: "#3f7a4a", color: "#3f7a4a" }}
            disabled={busy}
            onClick={() => onTurnIn?.(quest)}
          >
            {busyHere ? "交付中…" : "交付领赏"}
          </button>
        ) : (
          <span style={pendingBadgeStyle}>进行中</span>
        )}
      </div>
    </div>
  );
}

// ── 内联样式（墨色宣纸调，与 WorldMap 同款，不引用任何外部 CSS） ──

const overlayStyle: React.CSSProperties = {
  position: "fixed",
  inset: 0,
  zIndex: zIndex.fullscreenModal,
  display: "flex",
  alignItems: "center",
  justifyContent: "center",
  background: "rgba(20, 14, 8, 0.55)",
  backdropFilter: "blur(2px)",
  padding: 24,
  boxSizing: "border-box",
};

const panelStyle: React.CSSProperties = {
  width: "min(720px, 96vw)",
  maxHeight: "90vh",
  display: "flex",
  flexDirection: "column",
  background: "rgba(245, 236, 220, 0.98)",
  border: "1px solid rgba(140, 100, 50, 0.45)",
  borderRadius: 14,
  boxShadow: "0 18px 48px rgba(40, 28, 14, 0.42)",
  color: "#3a2c1b",
  fontFamily: "'Noto Serif SC', 'Songti SC', serif",
  overflow: "hidden",
};

const headerStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "flex-start",
  justifyContent: "space-between",
  padding: "18px 22px 12px",
  borderBottom: "1px solid rgba(140, 100, 50, 0.24)",
};

const titleStyle: React.CSSProperties = {
  fontSize: 22,
  fontWeight: 700,
  letterSpacing: 2,
};

const subtitleStyle: React.CSSProperties = {
  marginTop: 4,
  fontSize: 13,
  color: "#7a6a52",
  letterSpacing: 1,
};

const closeBtnStyle: React.CSSProperties = {
  appearance: "none",
  border: "none",
  background: "transparent",
  fontSize: 26,
  lineHeight: 1,
  color: "#8a7458",
  cursor: "pointer",
  padding: "0 4px",
};

const errorBannerStyle: React.CSSProperties = {
  margin: "10px 22px 0",
  padding: "8px 12px",
  borderRadius: 8,
  background: "rgba(163, 67, 63, 0.12)",
  border: "1px solid rgba(163, 67, 63, 0.4)",
  color: "#8c3a36",
  fontSize: 13,
};

const bodyStyle: React.CSSProperties = {
  padding: "16px 22px 22px",
  overflowY: "auto",
};

const hintStyle: React.CSSProperties = {
  padding: "40px 0",
  textAlign: "center",
  color: "#7a6a52",
  fontSize: 14,
};

const emptyHintStyle: React.CSSProperties = {
  padding: "10px 2px",
  color: "#b3a791",
  fontSize: 13,
};

const sectionStyle: React.CSSProperties = {
  marginBottom: 18,
};

const sectionTitleStyle: React.CSSProperties = {
  fontSize: 13,
  fontWeight: 700,
  color: "#7a7268",
  letterSpacing: 1,
  marginBottom: 8,
  paddingBottom: 4,
  borderBottom: "1px dashed rgba(140, 100, 50, 0.3)",
};

const questCardStyle: React.CSSProperties = {
  display: "flex",
  flexDirection: "column",
  gap: 8,
  padding: "12px 14px",
  marginBottom: 10,
  borderRadius: 10,
  border: "1px solid rgba(120, 95, 60, 0.35)",
  background: "rgba(248, 241, 228, 0.92)",
  boxShadow: "0 4px 12px rgba(60, 44, 27, 0.1)",
  boxSizing: "border-box",
};

const questCardTopStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  gap: 8,
};

const typeChipStyle: React.CSSProperties = {
  fontSize: 11,
  fontWeight: 700,
  border: "1px solid",
  borderRadius: 999,
  padding: "1px 8px",
  letterSpacing: 1,
  flexShrink: 0,
};

const questTitleStyle: React.CSSProperties = {
  fontSize: 16,
  fontWeight: 700,
  letterSpacing: 1,
};

const narrativeStyle: React.CSSProperties = {
  fontSize: 13,
  lineHeight: 1.7,
  color: "#5a4a36",
};

const objListStyle: React.CSSProperties = {
  listStyle: "none",
  margin: 0,
  padding: 0,
  display: "flex",
  flexDirection: "column",
  gap: 3,
};

const objItemStyle: React.CSSProperties = {
  fontSize: 13,
  display: "flex",
  alignItems: "baseline",
  gap: 6,
};

const objMarkStyle: React.CSSProperties = {
  fontWeight: 700,
  width: 12,
  flexShrink: 0,
};

const questFooterStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  justifyContent: "space-between",
  gap: 10,
  marginTop: 2,
  flexWrap: "wrap",
};

const rewardStyle: React.CSSProperties = {
  fontSize: 12,
  color: "#8a6a3a",
};

const actionBtnStyle: React.CSSProperties = {
  appearance: "none",
  border: "1px solid",
  background: "transparent",
  borderRadius: 8,
  padding: "5px 14px",
  fontSize: 13,
  fontWeight: 700,
  cursor: "pointer",
  fontFamily: "inherit",
  flexShrink: 0,
};

const pendingBadgeStyle: React.CSSProperties = {
  fontSize: 12,
  color: "#9a8a72",
  fontWeight: 600,
};

export default QuestPanel;
