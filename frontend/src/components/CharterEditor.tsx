/* 文件说明：离线宪章前端编辑面板（设计宪法 §4 离线宪章 + 盘点 M1「无前端编辑 UI，玩家只能靠外部 REST 立约」）。
   为当前指挥阵营的某个单位「立约/改约/撤约」：读现有宪章（fetchCharter）展示三组——
   Redlines 红线（她绝不会做的事）/ LongTermGoals 长期目标（她的人生北极星）/ SocialMandates 社交授权
   （你不在时她能自主到什么程度），可逐条增删改，保存（saveCharter PUT）或整份撤销（deleteCharter DELETE）。
   依赖注入：所有 charter REST 经 props 传入（fetch/save/delete），本组件不直接 import api.ts，
   避免与并发改 api.ts 冲突；需 api.ts 新增 getCharter/putCharter/deleteCharter 与 App.tsx 挂载入口（主控集成）。
   祖魂语气：玩家是垂看后人、替她立约的先祖，措辞不出现「命令/操控」。自包含内联样式，不依赖 fate.css。*/

import { useCallback, useEffect, useMemo, useState } from "react";
import { zIndex } from "../zindex-tokens";

// CharterRedline 对齐后端 session.CharterRedline（id/text/severity）。
// id 由后端 NormalizeCharter 派生/补齐；前端新建条目可留空，保存后读回即带稳定 id。
export type CharterRedline = {
  id?: string;
  text: string;
  severity?: string;
};

// OfflineCharter 对齐后端 session.OfflineCharter 三段长效授权（与 PUT body / GET charter 同构）。
export type OfflineCharter = {
  long_term_goals?: string[];
  redlines?: CharterRedline[];
  social_mandates?: string[];
};

type CharterUnitOption = {
  id: string;
  name: string;
};

type Props = {
  sessionId: string;
  units: CharterUnitOption[];
  // initialUnitID 是面板首选聚焦的单位（通常为 App 当前选中的单位）。
  initialUnitID?: string | null;
  // fetchCharter 读某单位现有宪章；exists=false 表示从未立约（区分「显式空宪章」与「未设置」）。
  fetchCharter: (
    sessionID: string,
    unitID: string,
  ) => Promise<{ charter: OfflineCharter; exists: boolean }>;
  // saveCharter 设立/覆盖某单位宪章（PUT），返回后端规范化后的宪章（带补齐的红线 id）。
  saveCharter: (sessionID: string, unitID: string, charter: OfflineCharter) => Promise<OfflineCharter>;
  // deleteCharter 撤销某单位整份宪章（DELETE）。
  deleteCharter: (sessionID: string, unitID: string) => Promise<void>;
  onClose: () => void;
};

// 红线严重度档位（对齐后端 severity：soft/hard，空=普通）。
const SEVERITY_OPTIONS: { value: string; label: string }[] = [
  { value: "", label: "普通底线" },
  { value: "soft", label: "尽量守住" },
  { value: "hard", label: "绝对禁区" },
];

function severityLabel(severity?: string): string {
  const s = (severity ?? "").toLowerCase();
  if (s === "hard") return "绝对禁区";
  if (s === "soft") return "尽量守住";
  return "普通底线";
}

const panelStyle: React.CSSProperties = {
  position: "absolute",
  top: 64,
  right: 12,
  width: 360,
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
const subStyle: React.CSSProperties = { color: "#9aa0ad", fontSize: 11, marginTop: 2, lineHeight: 1.4 };
const slotTitleStyle: React.CSSProperties = {
  color: "#cdb98a",
  fontSize: 11,
  letterSpacing: 0.5,
  margin: "12px 0 2px",
  textTransform: "uppercase",
};
const slotHintStyle: React.CSSProperties = {
  color: "#7e8493",
  fontSize: 11,
  lineHeight: 1.5,
  margin: "0 0 6px",
};
const sectionCardStyle: React.CSSProperties = {
  background: "rgba(32, 36, 48, 0.7)",
  border: "1px solid rgba(255,255,255,0.06)",
  borderRadius: 8,
  padding: "8px 10px",
  margin: "4px 0",
};
const redlineCardStyle: React.CSSProperties = {
  ...sectionCardStyle,
  borderLeft: "3px solid #c87a7a",
};
const rowStyle: React.CSSProperties = { display: "flex", gap: 6, alignItems: "center", margin: "4px 0" };
const inputStyle: React.CSSProperties = {
  flex: "1 1 auto",
  minWidth: 0,
  background: "rgba(12, 14, 20, 0.9)",
  color: "#e8e2d2",
  border: "1px solid rgba(255,255,255,0.12)",
  borderRadius: 6,
  padding: "5px 7px",
  fontSize: 12,
};
const selectStyle: React.CSSProperties = {
  background: "rgba(32, 36, 48, 0.9)",
  color: "#e8e2d2",
  border: "1px solid rgba(255,255,255,0.12)",
  borderRadius: 6,
  padding: "5px 6px",
  fontSize: 12,
};
const unitSelectStyle: React.CSSProperties = {
  ...selectStyle,
  width: "100%",
  margin: "6px 0",
};
const removeBtnStyle: React.CSSProperties = {
  flex: "0 0 auto",
  cursor: "pointer",
  background: "transparent",
  border: "1px solid rgba(200, 110, 110, 0.45)",
  color: "#e6bcbc",
  borderRadius: 6,
  padding: "4px 8px",
  fontSize: 12,
  lineHeight: 1,
};
const addBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "rgba(217, 188, 115, 0.12)",
  border: "1px dashed rgba(217, 188, 115, 0.45)",
  color: "#cdb98a",
  borderRadius: 6,
  padding: "5px 8px",
  fontSize: 12,
  marginTop: 4,
};
const footerRowStyle: React.CSSProperties = {
  display: "flex",
  gap: 8,
  marginTop: 14,
  paddingTop: 10,
  borderTop: "1px solid rgba(255,255,255,0.08)",
};
const saveBtnStyle: React.CSSProperties = {
  flex: "1 1 auto",
  cursor: "pointer",
  background: "rgba(120, 180, 130, 0.18)",
  border: "1px solid rgba(120, 180, 130, 0.55)",
  color: "#bfe6c6",
  borderRadius: 6,
  padding: "7px 8px",
  fontSize: 13,
  fontWeight: 600,
};
const deleteBtnStyle: React.CSSProperties = {
  flex: "0 0 auto",
  cursor: "pointer",
  background: "rgba(200, 110, 110, 0.12)",
  border: "1px solid rgba(200, 110, 110, 0.5)",
  color: "#e6bcbc",
  borderRadius: 6,
  padding: "7px 10px",
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
const toastStyle: React.CSSProperties = {
  marginTop: 10,
  padding: "6px 8px",
  borderRadius: 6,
  background: "rgba(111, 141, 181, 0.18)",
  border: "1px solid rgba(111, 141, 181, 0.45)",
  color: "#cdd7e6",
  fontSize: 12,
};

// 把后端读回的宪章正规化成「可编辑草稿」：保证三段恒为数组（避免 omitempty 省略时的 undefined 解引用）。
function toDraft(charter: OfflineCharter): {
  goals: string[];
  redlines: CharterRedline[];
  mandates: string[];
} {
  return {
    goals: [...(charter.long_term_goals ?? [])],
    redlines: (charter.redlines ?? []).map((r) => ({ id: r.id, text: r.text, severity: r.severity })),
    mandates: [...(charter.social_mandates ?? [])],
  };
}

// CharterEditor 是接进 App 的离线宪章编辑浮层（scoped 到一个角色）。
export function CharterEditor({
  sessionId,
  units,
  initialUnitID,
  fetchCharter,
  saveCharter,
  deleteCharter,
  onClose,
}: Props) {
  const firstUnitID = useMemo(() => {
    if (initialUnitID && units.some((u) => u.id === initialUnitID)) {
      return initialUnitID;
    }
    return units[0]?.id ?? "";
  }, [initialUnitID, units]);

  const [unitID, setUnitID] = useState<string>(firstUnitID);
  const [goals, setGoals] = useState<string[]>([]);
  const [redlines, setRedlines] = useState<CharterRedline[]>([]);
  const [mandates, setMandates] = useState<string[]>([]);
  const [exists, setExists] = useState(false);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [toast, setToast] = useState("");

  const who = useMemo(() => units.find((u) => u.id === unitID)?.name?.trim() || "她", [units, unitID]);

  // 外部选中单位变化、或当前单位已不在列表里时，跟随首选聚焦。
  useEffect(() => {
    if (firstUnitID && !units.some((u) => u.id === unitID)) {
      setUnitID(firstUnitID);
    }
  }, [firstUnitID, unitID, units]);

  const refresh = useCallback(async () => {
    if (!unitID) {
      setGoals([]);
      setRedlines([]);
      setMandates([]);
      setExists(false);
      return;
    }
    setLoading(true);
    try {
      const { charter, exists: e } = await fetchCharter(sessionId, unitID);
      const draft = toDraft(charter);
      setGoals(draft.goals);
      setRedlines(draft.redlines);
      setMandates(draft.mandates);
      setExists(e);
    } catch (err) {
      setToast(`读取宪章失败：${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setLoading(false);
    }
  }, [sessionId, unitID, fetchCharter]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  // ---- 三组条目的增删改（纯本地草稿，保存时才落库）----
  const updateGoal = useCallback((i: number, value: string) => {
    setGoals((prev) => prev.map((g, idx) => (idx === i ? value : g)));
  }, []);
  const removeGoal = useCallback((i: number) => {
    setGoals((prev) => prev.filter((_, idx) => idx !== i));
  }, []);
  const addGoal = useCallback(() => setGoals((prev) => [...prev, ""]), []);

  const updateMandate = useCallback((i: number, value: string) => {
    setMandates((prev) => prev.map((m, idx) => (idx === i ? value : m)));
  }, []);
  const removeMandate = useCallback((i: number) => {
    setMandates((prev) => prev.filter((_, idx) => idx !== i));
  }, []);
  const addMandate = useCallback(() => setMandates((prev) => [...prev, ""]), []);

  const updateRedlineText = useCallback((i: number, value: string) => {
    setRedlines((prev) => prev.map((r, idx) => (idx === i ? { ...r, text: value } : r)));
  }, []);
  const updateRedlineSeverity = useCallback((i: number, value: string) => {
    setRedlines((prev) => prev.map((r, idx) => (idx === i ? { ...r, severity: value } : r)));
  }, []);
  const removeRedline = useCallback((i: number) => {
    setRedlines((prev) => prev.filter((_, idx) => idx !== i));
  }, []);
  const addRedline = useCallback(() => setRedlines((prev) => [...prev, { text: "", severity: "" }]), []);

  // buildCharter 把草稿收成 PUT body：裁空白条目（与后端 NormalizeCharter 同向，省一次无效往返）。
  const buildCharter = useCallback((): OfflineCharter => {
    return {
      long_term_goals: goals.map((g) => g.trim()).filter((g) => g.length > 0),
      redlines: redlines
        .map((r) => ({ id: r.id, text: r.text.trim(), severity: r.severity }))
        .filter((r) => r.text.length > 0),
      social_mandates: mandates.map((m) => m.trim()).filter((m) => m.length > 0),
    };
  }, [goals, redlines, mandates]);

  const onSave = useCallback(async () => {
    if (!unitID) return;
    setSaving(true);
    try {
      const stored = await saveCharter(sessionId, unitID, buildCharter());
      // 用后端规范化结果回填草稿（拿到补齐的红线 id，并清掉被裁的空白条目）。
      const draft = toDraft(stored);
      setGoals(draft.goals);
      setRedlines(draft.redlines);
      setMandates(draft.mandates);
      setExists(true);
      setToast(`你为${who}立下的约，她记住了。`);
    } catch (err) {
      setToast(`没能立约：${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setSaving(false);
    }
  }, [unitID, sessionId, saveCharter, buildCharter, who]);

  const onDelete = useCallback(async () => {
    if (!unitID) return;
    setSaving(true);
    try {
      await deleteCharter(sessionId, unitID);
      setGoals([]);
      setRedlines([]);
      setMandates([]);
      setExists(false);
      setToast(`你与${who}的约，已收回。往后她全凭本心。`);
    } catch (err) {
      setToast(`没能撤约：${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setSaving(false);
    }
  }, [unitID, sessionId, deleteCharter, who]);

  return (
    <aside style={panelStyle} role="dialog" aria-label="离线宪章编辑">
      <div style={headerStyle}>
        <div>
          <div style={brandStyle}>群像 · 立约</div>
          <div style={subStyle}>
            你不在时，{who}依这份约自处。立下她绝不越的底线、她一生奔赴的方向、你许她自主的边界。
          </div>
        </div>
        <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭立约面板">
          ×
        </button>
      </div>

      {units.length > 1 ? (
        <select
          style={unitSelectStyle}
          value={unitID}
          onChange={(e) => setUnitID(e.target.value)}
          aria-label="选择要立约的角色"
        >
          {units.map((u) => (
            <option key={u.id} value={u.id}>
              {u.name}
            </option>
          ))}
        </select>
      ) : null}

      {loading ? <div style={{ ...sectionCardStyle, color: "#9aa0ad" }}>正在翻阅她现下的约…</div> : null}

      {/* 一组：红线（她绝不会做的事） */}
      <div style={slotTitleStyle}>红线 · 她绝不会做的事</div>
      <p style={slotHintStyle}>
        哪怕你不在、哪怕走投无路，这些事她也绝不碰——如「永不卖传家宝」「永不叛变」。这是她为人的底线。
      </p>
      {redlines.length === 0 ? (
        <div style={{ ...redlineCardStyle, color: "#9aa0ad" }}>还没为她划下任何底线。</div>
      ) : (
        redlines.map((r, i) => (
          <div key={r.id ?? `rl-${i}`} style={redlineCardStyle}>
            <div style={rowStyle}>
              <input
                style={inputStyle}
                value={r.text}
                placeholder="她绝不会做的一件事"
                onChange={(e) => updateRedlineText(i, e.target.value)}
                aria-label={`红线第 ${i + 1} 条`}
              />
              <button
                type="button"
                style={removeBtnStyle}
                onClick={() => removeRedline(i)}
                aria-label={`删除红线第 ${i + 1} 条`}
              >
                删去
              </button>
            </div>
            <div style={rowStyle}>
              <span style={{ color: "#7e8493", fontSize: 11 }}>守约之重：</span>
              <select
                style={selectStyle}
                value={r.severity ?? ""}
                onChange={(e) => updateRedlineSeverity(i, e.target.value)}
                aria-label={`红线第 ${i + 1} 条的严重度`}
              >
                {SEVERITY_OPTIONS.map((opt) => (
                  <option key={opt.value} value={opt.value}>
                    {opt.label}
                  </option>
                ))}
              </select>
              <span style={{ color: "#9aa0ad", fontSize: 11 }}>{severityLabel(r.severity)}</span>
            </div>
          </div>
        ))
      )}
      <button type="button" style={addBtnStyle} onClick={addRedline}>
        + 再划一条底线
      </button>

      {/* 二组：长期目标（她的人生北极星） */}
      <div style={slotTitleStyle}>长期目标 · 她的人生北极星</div>
      <p style={slotHintStyle}>
        她一生奔赴的方向——如「替父报仇」「重振家声」。你不在时，她日常自处也朝它努力。
      </p>
      {goals.length === 0 ? (
        <div style={{ ...sectionCardStyle, color: "#9aa0ad" }}>还没为她指下一生的方向。</div>
      ) : (
        goals.map((g, i) => (
          <div key={`goal-${i}`} style={rowStyle}>
            <input
              style={inputStyle}
              value={g}
              placeholder="她一生奔赴的方向"
              onChange={(e) => updateGoal(i, e.target.value)}
              aria-label={`长期目标第 ${i + 1} 条`}
            />
            <button
              type="button"
              style={removeBtnStyle}
              onClick={() => removeGoal(i)}
              aria-label={`删除长期目标第 ${i + 1} 条`}
            >
              删去
            </button>
          </div>
        ))
      )}
      <button type="button" style={addBtnStyle} onClick={addGoal}>
        + 再立一个方向
      </button>

      {/* 三组：社交授权（你不在时她能自主到什么程度） */}
      <div style={slotTitleStyle}>社交授权 · 你不在时她能自主到什么程度</div>
      <p style={slotHintStyle}>
        你许她自行处理的人际事，无需事事等你点头——如「可自主结盟」「可代我议和」「勿与某派结仇」。
      </p>
      {mandates.length === 0 ? (
        <div style={{ ...sectionCardStyle, color: "#9aa0ad" }}>还没许她任何自主之权。她会事事等你拿主意。</div>
      ) : (
        mandates.map((m, i) => (
          <div key={`mandate-${i}`} style={rowStyle}>
            <input
              style={inputStyle}
              value={m}
              placeholder="你许她自主的一桩人际事"
              onChange={(e) => updateMandate(i, e.target.value)}
              aria-label={`社交授权第 ${i + 1} 条`}
            />
            <button
              type="button"
              style={removeBtnStyle}
              onClick={() => removeMandate(i)}
              aria-label={`删除社交授权第 ${i + 1} 条`}
            >
              删去
            </button>
          </div>
        ))
      )}
      <button type="button" style={addBtnStyle} onClick={addMandate}>
        + 再许一分自主
      </button>

      <div style={footerRowStyle}>
        <button
          type="button"
          style={{ ...saveBtnStyle, opacity: saving || !unitID ? 0.6 : 1 }}
          disabled={saving || !unitID}
          onClick={() => void onSave()}
        >
          {saving ? "正在立约…" : exists ? "改约 · 重立此约" : "立约 · 替她定下"}
        </button>
        {exists ? (
          <button
            type="button"
            style={{ ...deleteBtnStyle, opacity: saving ? 0.6 : 1 }}
            disabled={saving}
            onClick={() => void onDelete()}
          >
            撤约
          </button>
        ) : null}
      </div>

      {toast ? <div style={toastStyle}>{toast}</div> : null}
    </aside>
  );
}

export default CharterEditor;
