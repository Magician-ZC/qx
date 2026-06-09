/* 文件说明：血仇网络可视化面板（审计跨玩家 C 可视化）。把 blood_feud 世仇传播做成「可感知」的浮层：
   对某个角色调 listBloodFeuds 拉取她当前怀有的强敌意关系（rivalry 达成仇阈的对外世仇），逐条渲染
   对象名 + 敌意强度（rivalry/fear 可视化为条与标签）。世仇可经多跳传播继承（在乎死者的人→在乎哀悼者
   的人，HopFidelity=0.6^hop），故若某条带二手来源（hop>0 / from_unit），标注「间接（听闻）」以区分
   亲历之仇与听闻之仇。纯读、无副作用。中文 UI，复用既有 components 浮层风格（深底金描边、内联样式）。*/

import { useCallback, useEffect, useState } from "react";
import { listBloodFeuds } from "../session/api";
import type { BloodFeudEntry } from "../session/types";
import { zIndex } from "../zindex-tokens";

type Props = {
  sessionID: string;
  unitID: string;
  // unitName 是发出血仇的主体角色名（标题用）；缺省则用「她」泛称。
  unitName?: string;
  onClose: () => void;
};

// FeudRow 是渲染期对 BloodFeudEntry 的宽松解读：后端契约只保证四轴 + 对象 id/name，
// 但世仇可经多跳继承，传播链可能透出 hop/来源。这里做防御式可选读取——后端补字段即自动生效，
// 未补则按「亲历之仇」渲染（不误标听闻）。
type FeudRow = BloodFeudEntry & {
  hop?: number;
  from_unit_id?: string;
  from_name?: string;
};

// 关系四轴后端 clamp 到 [-10,10]（见 CLAUDE.md / session.relation）。归一到 [0,1] 供条形渲染。
const AXIS_MAX = 10;
function axisPct(value: number): number {
  return Math.max(0, Math.min(100, (Math.max(0, value) / AXIS_MAX) * 100));
}

// feudKey 给每条世仇一个稳定 key（对象 id 唯一；缺失时退化到名 + 序号由调用方补）。
function feudKey(row: FeudRow, idx: number): string {
  return row.target_unit_id || `${row.target_name ?? "?"}#${idx}`;
}

// isSecondhand 判断这条世仇是否为二手听闻（hop>0 表示经传播继承，或显式带来源单位）。
function isSecondhand(row: FeudRow): boolean {
  if (typeof row.hop === "number" && row.hop > 0) return true;
  return Boolean(row.from_unit_id || row.from_name);
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
  border: "1px solid rgba(180, 84, 58, 0.4)",
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

const brandStyle: React.CSSProperties = { color: "#e58b73", fontWeight: 700, fontSize: 14 };
const subStyle: React.CSSProperties = { color: "#9aa0ad", fontSize: 11, marginTop: 2 };
const closeBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "transparent",
  border: "none",
  color: "#9aa0ad",
  fontSize: 18,
  lineHeight: 1,
};
const feudCardStyle: React.CSSProperties = {
  background: "rgba(32, 36, 48, 0.7)",
  border: "1px solid rgba(255,255,255,0.06)",
  borderLeft: "3px solid #b4543a",
  borderRadius: 8,
  padding: "8px 10px",
  margin: "6px 0",
};
const targetRowStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  justifyContent: "space-between",
  gap: 6,
};
const targetNameStyle: React.CSSProperties = { fontWeight: 600, color: "#f0ead8" };
const secondhandTagStyle: React.CSSProperties = {
  display: "inline-flex",
  alignItems: "center",
  gap: 3,
  padding: "1px 7px",
  borderRadius: 999,
  fontSize: 10,
  letterSpacing: 0.3,
  color: "#cdd7e6",
  background: "rgba(111, 141, 181, 0.18)",
  border: "1px solid rgba(111, 141, 181, 0.45)",
};
const emptyStyle: React.CSSProperties = {
  background: "rgba(32, 36, 48, 0.7)",
  border: "1px solid rgba(255,255,255,0.06)",
  borderRadius: 8,
  padding: "14px 10px",
  margin: "6px 0",
  color: "#9aa0ad",
  textAlign: "center",
};
const noticeStyle: React.CSSProperties = {
  marginTop: 8,
  padding: "6px 8px",
  borderRadius: 6,
  background: "rgba(180, 84, 58, 0.16)",
  border: "1px solid rgba(180, 84, 58, 0.45)",
  color: "#f0c4b6",
  fontSize: 12,
};

// HostilityBar 渲染一条敌意分量条（rivalry 红 / fear 紫）。value 取 [0,10]，归一到 [0,100]%。
function HostilityBar({ label, value, color }: { label: string; value: number; color: string }) {
  const pct = axisPct(value);
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 6, margin: "3px 0" }}>
      <span style={{ width: 32, color: "#9aa0ad", fontSize: 11 }}>{label}</span>
      <span style={{ flex: 1, height: 6, background: "rgba(255,255,255,0.08)", borderRadius: 3, overflow: "hidden" }}>
        <span style={{ display: "block", height: "100%", width: `${pct}%`, background: color }} />
      </span>
      <span style={{ width: 28, textAlign: "right", color: "#cbd1da", fontSize: 11 }}>
        {Math.round(Math.max(0, value) * 10) / 10}
      </span>
    </div>
  );
}

// BloodFeudPanel 是接进 App 的血仇网络浮层：让某角色当前怀有的强敌意（含多跳听闻之仇）可被一眼看见。
export function BloodFeudPanel({ sessionID, unitID, unitName, onClose }: Props) {
  const [feuds, setFeuds] = useState<FeudRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const refresh = useCallback(async () => {
    if (!sessionID || !unitID) {
      setFeuds([]);
      setLoading(false);
      return;
    }
    setLoading(true);
    setError("");
    try {
      const rows = await listBloodFeuds(sessionID, unitID);
      setFeuds((rows ?? []) as FeudRow[]);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setFeuds([]);
    } finally {
      setLoading(false);
    }
  }, [sessionID, unitID]);

  // 挂载 / unitID 变化时重新拉取该角色的世仇清单。
  useEffect(() => {
    void refresh();
  }, [refresh]);

  const subject = unitName?.trim() || "她";

  return (
    <aside style={panelStyle} role="dialog" aria-label="血仇网络面板">
      <div style={headerStyle}>
        <div>
          <div style={brandStyle}>群像 · 血仇</div>
          <div style={subStyle}>{subject}心里记着的恨。有些是亲历，有些是听闻而来。</div>
        </div>
        <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭血仇面板">
          ×
        </button>
      </div>

      {loading ? (
        <div style={emptyStyle}>正在追溯她结下的仇怨…</div>
      ) : error ? (
        <div style={noticeStyle}>读取血仇失败：{error}</div>
      ) : feuds.length === 0 ? (
        <div style={emptyStyle}>暂无血仇</div>
      ) : (
        feuds.map((row, idx) => {
          const secondhand = isSecondhand(row);
          const targetName = row.target_name?.trim() || "某个无名之人";
          return (
            <div key={feudKey(row, idx)} style={feudCardStyle}>
              <div style={targetRowStyle}>
                <span style={targetNameStyle}>{targetName}</span>
                {secondhand ? (
                  <span style={secondhandTagStyle} title="经传播继承的世仇，可信度按跳数衰减">
                    间接（听闻）
                  </span>
                ) : null}
              </div>
              {secondhand && row.from_name ? (
                <div style={{ color: "#9aa0ad", fontSize: 11, marginTop: 2 }}>因 {row.from_name} 而起的恨</div>
              ) : null}
              <div style={{ marginTop: 6 }}>
                <HostilityBar label="仇怨" value={row.rivalry} color="#b4543a" />
                <HostilityBar label="忌惮" value={row.fear} color="#8d6fb5" />
              </div>
            </div>
          );
        })
      )}
    </aside>
  );
}

export default BloodFeudPanel;
