/* 文件说明：副本（地下城）前端面板——消费 wave4 的 RunDungeon 端点（POST /api/sessions/:id/dungeon）。
   后端能力：组队下副本→逐层 PvE 威胁结算（普通层/boss 层）→通关 AllocateLoot 分赃 +
   各队员 InboxCards 祖魂语气卡；失败/逃跑则 PenaltyLayer 惩罚分级。
   QUNXIANG_DUNGEON 关时后端报错→本面板据 APIError 透出 status/reason，提示「副本未启用」。
   props 契约（供 App.tsx 挂载对齐）：
     - sessionID: string —— 当前会话 ID（端点路径用）。
     - partyCandidates: { id; name }[] —— 本局可组队单位（多选勾选）。
     - onClose: () => void —— 关闭面板。
   自包含内联样式，参照 WorldBossPanel.tsx / BillingPanel.tsx 右侧浮层范式，仅 import api.ts/types.ts。*/

import { useCallback, useMemo, useState } from "react";
import { APIError, runDungeon } from "../session/api";
import type { DungeonResult, EncounterAward } from "../session/types";

type Props = {
  // sessionID 当前会话 ID（端点路径用）。
  sessionID: string;
  // partyCandidates 本局可组队单位，由 App 传入用作多选勾选。
  partyCandidates: { id: string; name: string }[];
  // onClose 关闭面板。
  onClose: () => void;
};

// floors 取值范围（默认 3，2-6）。
const FLOOR_MIN = 2;
const FLOOR_MAX = 6;
const FLOOR_DEFAULT = 3;

// errText 把错误归一为可展示文案，合规/未启用类错误透出 status/reason（参照 WorldBossPanel）。
function errText(err: unknown): string {
  if (err instanceof APIError) {
    const parts = [err.message];
    if (typeof err.status === "number") parts.push(`(HTTP ${err.status})`);
    if (err.reason) parts.push(`原因：${err.reason}`);
    return parts.join(" ");
  }
  return err instanceof Error ? err.message : String(err);
}

// outcomeLabel 把后端 Outcome 枚举转中文。
function outcomeLabel(outcome: string): string {
  switch (outcome) {
    case "cleared":
      return "通关";
    case "fled":
      return "撤退";
    case "wiped":
      return "团灭";
    default:
      return outcome || "未知";
  }
}

// outcomePillStyle 按 Outcome 选徽标配色。
function outcomePillStyle(outcome: string): React.CSSProperties {
  if (outcome === "cleared") {
    return {
      ...pillBaseStyle,
      color: "#bfe6c8",
      background: "rgba(111, 181, 130, 0.16)",
      border: "1px solid rgba(111, 181, 130, 0.5)",
    };
  }
  if (outcome === "wiped") {
    return {
      ...pillBaseStyle,
      color: "#f0b0a6",
      background: "rgba(196, 84, 74, 0.16)",
      border: "1px solid rgba(196, 84, 74, 0.5)",
    };
  }
  // fled / 其它
  return {
    ...pillBaseStyle,
    color: "#e6d3a0",
    background: "rgba(217, 188, 115, 0.16)",
    border: "1px solid rgba(217, 188, 115, 0.4)",
  };
}

// candidateName 据队员 ID 反查名称（找不到则回退 ID）。
function candidateName(candidates: { id: string; name: string }[], id: string): string {
  return candidates.find((c) => c.id === id)?.name ?? id;
}

// ============ 内联样式（参照 WorldBossPanel.tsx 右侧浮层范式） ============

const panelStyle: React.CSSProperties = {
  position: "absolute",
  top: 64,
  right: 12,
  width: 380,
  maxHeight: "calc(100vh - 96px)",
  overflowY: "auto",
  zIndex: 41,
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
const closeBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "transparent",
  border: "none",
  color: "#9aa0ad",
  fontSize: 18,
  lineHeight: 1,
};
const pillBaseStyle: React.CSSProperties = {
  display: "inline-block",
  fontSize: 11,
  padding: "2px 10px",
  borderRadius: 999,
  fontWeight: 600,
};
const miniPillStyle: React.CSSProperties = {
  display: "inline-block",
  fontSize: 10,
  padding: "1px 7px",
  borderRadius: 999,
  background: "rgba(217, 188, 115, 0.16)",
  border: "1px solid rgba(217, 188, 115, 0.4)",
  color: "#e6d3a0",
  marginLeft: 6,
};
const bossPillStyle: React.CSSProperties = {
  ...miniPillStyle,
  color: "#f0b0a6",
  background: "rgba(196, 84, 74, 0.16)",
  border: "1px solid rgba(196, 84, 74, 0.5)",
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
const inboxCardStyle: React.CSSProperties = {
  ...sectionCardStyle,
  borderColor: "rgba(217, 188, 115, 0.45)",
  background: "rgba(217, 188, 115, 0.08)",
  color: "#f0ead8",
  fontStyle: "italic",
};
const mutedStyle: React.CSSProperties = { color: "#9aa0ad" };
const statRowStyle: React.CSSProperties = {
  display: "flex",
  justifyContent: "space-between",
  fontSize: 12,
  padding: "2px 0",
};

// DungeonPanel 是接进 App 的副本浮层面板（组队 + 选层 + 下副本 + 逐层/分赃/祖魂卡展示）。
export function DungeonPanel({ sessionID, partyCandidates, onClose }: Props) {
  // 已勾选的队员 ID 集合。
  const [selectedIDs, setSelectedIDs] = useState<string[]>([]);
  const [floors, setFloors] = useState<number>(FLOOR_DEFAULT);
  const [running, setRunning] = useState(false);
  const [result, setResult] = useState<DungeonResult | null>(null);
  const [err, setErr] = useState("");
  const [ok, setOk] = useState("");

  // toggleMember 勾选/取消一个队员。
  const toggleMember = useCallback((id: string) => {
    setSelectedIDs((prev) => (prev.includes(id) ? prev.filter((x) => x !== id) : [...prev, id]));
  }, []);

  // floorOptions 可选层数（FLOOR_MIN..FLOOR_MAX）。
  const floorOptions = useMemo(() => {
    const opts: number[] = [];
    for (let n = FLOOR_MIN; n <= FLOOR_MAX; n += 1) opts.push(n);
    return opts;
  }, []);

  // doRun 组队下副本，回填结果。
  const doRun = useCallback(async () => {
    if (selectedIDs.length === 0) {
      setErr("请至少勾选一名队员。");
      setOk("");
      return;
    }
    setRunning(true);
    setErr("");
    setOk("");
    try {
      const res = await runDungeon(sessionID, selectedIDs, floors);
      setResult(res);
      if (res.Outcome === "cleared") {
        setOk(`通关！攻克 ${res.FloorsClear}/${res.Floors} 层。`);
      } else if (res.Outcome === "wiped") {
        setOk(`团灭于第 ${res.FloorsClear + 1} 层（共攻克 ${res.FloorsClear} 层）。`);
      } else {
        setOk(`撤退（已攻克 ${res.FloorsClear}/${res.Floors} 层）。`);
      }
    } catch (e) {
      // QUNXIANG_DUNGEON 关时后端对 ErrDungeonDisabled 返回 400 且 message 含「未启用」——据此判未启用
      // （后端 dungeon 端点对所有错误统一 400，故按 message 判定比按 status 更准）。
      const disabled =
        e instanceof APIError &&
        (e.status === 404 || e.status === 403 || e.status === 501 || (e.message ?? "").includes("未启用"));
      if (disabled) {
        setErr(`副本未启用：${errText(e)}`);
      } else {
        setErr(`下副本失败：${errText(e)}`);
      }
    } finally {
      setRunning(false);
    }
  }, [floors, selectedIDs, sessionID]);

  const floorResults = result?.FloorResults ?? [];
  const awards: EncounterAward[] = result?.Awards ?? [];
  const contribution = result?.Contribution ?? null;
  const penaltyLayer = result?.PenaltyLayer ?? null;
  const inboxCards = result?.InboxCards ?? null;

  const contributionEntries = contribution ? Object.entries(contribution) : [];
  const penaltyEntries = penaltyLayer ? Object.entries(penaltyLayer) : [];
  const inboxEntries = inboxCards ? Object.entries(inboxCards) : [];

  return (
    <aside style={panelStyle} role="dialog" aria-label="副本面板">
      <div style={headerStyle}>
        <div>
          <div style={brandStyle}>副本 · 地下城</div>
          <div style={subStyle}>组队下副本 · 逐层 PvE 结算 · 通关分赃 · 祖魂语气卡</div>
        </div>
        <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭副本面板">
          ×
        </button>
      </div>

      {/* ---- 组队 ---- */}
      <div style={slotTitleStyle}>组队（已选 {selectedIDs.length}）</div>
      {partyCandidates.length === 0 ? (
        <div style={{ ...sectionCardStyle, ...mutedStyle }}>本局暂无可组队单位。</div>
      ) : (
        <div style={sectionCardStyle}>
          {partyCandidates.map((c) => {
            const checked = selectedIDs.includes(c.id);
            return (
              <div
                key={c.id}
                style={checked ? memberRowSelectedStyle : memberRowStyle}
                onClick={() => toggleMember(c.id)}
                role="checkbox"
                aria-checked={checked}
                tabIndex={0}
                onKeyDown={(e) => {
                  if (e.key === "Enter" || e.key === " ") {
                    e.preventDefault();
                    toggleMember(c.id);
                  }
                }}
              >
                <input
                  type="checkbox"
                  checked={checked}
                  onChange={() => toggleMember(c.id)}
                  onClick={(e) => e.stopPropagation()}
                />
                <span style={{ color: "#f0ead8" }}>{c.name}</span>
                <span style={{ ...mutedStyle, fontSize: 10, marginLeft: "auto" }}>{c.id}</span>
              </div>
            );
          })}
        </div>
      )}

      {/* ---- 选层 + 下副本 ---- */}
      <div style={sectionCardStyle}>
        <label style={labelStyle} htmlFor="dungeon-floors">
          层数
        </label>
        <select
          id="dungeon-floors"
          style={selectStyle}
          value={String(floors)}
          onChange={(e) => setFloors(Number.parseInt(e.target.value, 10))}
        >
          {floorOptions.map((n) => (
            <option key={n} value={n}>
              {n} 层{n === FLOOR_DEFAULT ? "（推荐）" : ""}
            </option>
          ))}
        </select>

        <div style={{ display: "flex", justifyContent: "flex-end", marginTop: 12 }}>
          <button
            type="button"
            style={{ ...primaryBtnStyle, opacity: running || selectedIDs.length === 0 ? 0.6 : 1 }}
            onClick={() => void doRun()}
            disabled={running || selectedIDs.length === 0}
          >
            {running ? "下副本中…" : "下副本"}
          </button>
        </div>
      </div>

      {/* ---- 结果 ---- */}
      {result ? (
        <>
          <div style={slotTitleStyle}>战报</div>
          <div style={sectionCardStyle}>
            <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between" }}>
              <span style={mutedStyle}>结果</span>
              <span style={outcomePillStyle(result.Outcome)}>{outcomeLabel(result.Outcome)}</span>
            </div>
            <div style={statRowStyle}>
              <span style={mutedStyle}>攻克进度</span>
              <span style={{ color: "#f2d98f", fontWeight: 600 }}>
                {result.FloorsClear} / {result.Floors} 层
              </span>
            </div>
            <div style={{ ...mutedStyle, fontSize: 10, marginTop: 4 }}>副本 ID {result.DungeonID}</div>
          </div>

          {/* 逐层结果 */}
          <div style={slotTitleStyle}>逐层结算</div>
          {floorResults.length === 0 ? (
            <div style={{ ...sectionCardStyle, ...mutedStyle }}>无逐层数据。</div>
          ) : (
            floorResults.map((fr, i) => (
              <div key={`${fr.Floor}-${i}`} style={sectionCardStyle}>
                <div style={{ display: "flex", justifyContent: "space-between", gap: 8 }}>
                  <span style={{ fontWeight: 600, color: "#f0ead8" }}>
                    第 {fr.Floor} 层
                    {fr.IsBoss ? <span style={bossPillStyle}>BOSS</span> : null}
                  </span>
                  <span style={outcomePillStyle(fr.Outcome)}>{outcomeLabel(fr.Outcome)}</span>
                </div>
                <div style={{ ...mutedStyle, fontSize: 11, marginTop: 4 }}>敌：{fr.ThreatName}</div>
                <div style={{ display: "flex", gap: 12, marginTop: 4, fontSize: 11 }}>
                  <span style={mutedStyle}>回合 {fr.Rounds}</span>
                  <span style={{ color: "#bfe6c8" }}>输出 {fr.DamageDealt}</span>
                  <span style={{ color: "#f0b0a6" }}>承伤 {fr.DamageTaken}</span>
                </div>
              </div>
            ))
          )}

          {/* 通关分赃 */}
          {result.Outcome === "cleared" ? (
            <>
              <div style={slotTitleStyle}>通关分赃</div>
              {awards.length === 0 ? (
                <div style={{ ...sectionCardStyle, ...mutedStyle }}>本次无可分战利品。</div>
              ) : (
                awards.map((a, i) => (
                  <div key={`${a.UnitID}-${a.ItemID}-${i}`} style={sectionCardStyle}>
                    <div style={{ display: "flex", justifyContent: "space-between", gap: 8 }}>
                      <span style={{ color: "#f0ead8" }}>
                        {a.ItemID}
                        <span style={miniPillStyle}>×{a.Quantity}</span>
                      </span>
                      <span style={{ ...mutedStyle, fontSize: 11, whiteSpace: "nowrap" }}>
                        归 {candidateName(partyCandidates, a.UnitID)}
                      </span>
                    </div>
                    {a.Reason ? (
                      <div style={{ ...mutedStyle, fontSize: 11, marginTop: 4 }}>{a.Reason}</div>
                    ) : null}
                  </div>
                ))
              )}
            </>
          ) : null}

          {/* 贡献评分 */}
          {contributionEntries.length > 0 ? (
            <>
              <div style={slotTitleStyle}>贡献评分</div>
              <div style={sectionCardStyle}>
                {contributionEntries.map(([uid, score]) => (
                  <div key={uid} style={statRowStyle}>
                    <span style={mutedStyle}>{candidateName(partyCandidates, uid)}</span>
                    <span style={{ color: "#f2d98f", fontWeight: 600 }}>{score}</span>
                  </div>
                ))}
              </div>
            </>
          ) : null}

          {/* 失败惩罚分级 */}
          {penaltyEntries.length > 0 ? (
            <>
              <div style={slotTitleStyle}>惩罚分级</div>
              <div style={sectionCardStyle}>
                {penaltyEntries.map(([uid, layer]) => (
                  <div key={uid} style={statRowStyle}>
                    <span style={mutedStyle}>{candidateName(partyCandidates, uid)}</span>
                    <span style={{ color: "#f0b0a6", fontWeight: 600 }}>D{layer}</span>
                  </div>
                ))}
              </div>
            </>
          ) : null}

          {/* 各队员祖魂卡 */}
          {inboxEntries.length > 0 ? (
            <>
              <div style={slotTitleStyle}>祖魂语气卡</div>
              {inboxEntries.map(([uid, card]) => (
                <div key={uid} style={inboxCardStyle}>
                  <div style={{ ...mutedStyle, fontSize: 11, fontStyle: "normal", marginBottom: 4 }}>
                    致 {candidateName(partyCandidates, uid)}
                  </div>
                  {card}
                </div>
              ))}
            </>
          ) : null}
        </>
      ) : null}

      {ok ? <div style={toastOkStyle}>{ok}</div> : null}
      {err ? <div style={toastErrStyle}>{err}</div> : null}
    </aside>
  );
}

export default DungeonPanel;
