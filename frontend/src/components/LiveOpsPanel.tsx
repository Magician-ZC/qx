/* 文件说明：Live-Ops 运营专属看板组件（GM 世界事件注入表单 + 赛季卡 + 零和审计卡），面向运营/dev。
   设计 docs/产品方案PRD.md §8（live-ops 手柄）+ docs/验证实验设计.md。

   这三块都走 ops_token 鉴权的敏感运营端点。为与并行开发解耦、不强依赖 api.ts 的尚未落地的导出，
   本组件用**依赖注入**：所有 API 调用经 props 传入（由 App.tsx/主控用 api.ts 的新函数接线）。
   这样组件可独立编译/测试，主控只需把 api.ts 的 5 个新函数透传进来即可（详见 crossFileNeeds.frontendWiring）。

   自包含内联样式（与 OpsDashboard.tsx 同一基元集），不 import api.ts/types.ts，零并发冲突。 */

import { useCallback, useState } from "react";
import { zIndex } from "../zindex-tokens";

// ============ 注入的 API 契约（主控用 api.ts 新函数实现并透传） ============

// GM 世界事件注入入参（对应后端 liveops.GMEvent）。
export type GmWorldEventInput = {
  kind: string;
  importance: number;
  actorId?: string;
  targetId?: string;
  regionId?: string;
  payload?: Record<string, unknown>;
};

// GM 注入回执（对应后端 liveops.GMEventResult）。
export type GmWorldEventResult = {
  cross_event_id: string;
  audit_id: string;
  world_tick: number;
};

// 赛季创建入参 / 记录（对应后端 liveops.CreateSeasonInput / Season）。
export type CreateSeasonInput = {
  name: string;
  world_name?: string;
  content_theme_id?: string;
  max_population?: number;
  region_seed?: string;
};
export type Season = {
  id: string;
  world_id: string;
  name: string;
  status: string;
  started_at: string;
  ends_at: string;
  content_theme_id: string;
  created_at: string;
};
export type FinalizeResult = {
  season_id: string;
  world_id: string;
  members_total: number;
  archived: number;
  archive_errors: string[];
  sealed: boolean;
};

// 零和审计报告（对应后端 liveops.ArbitrationAuditReport）。
export type GroupStat = { wins: number; losses: number; total: number; win_rate: number };
export type ArbitrationAuditReport = {
  world_id: string;
  turn_start: number;
  turn_end: number;
  paid: GroupStat;
  non_paid: GroupStat;
  issue_detected: boolean;
  redline_rate: number;
  sample_sufficient: boolean;
  note: string;
};

// LiveOpsPanelProps 是注入的 API 句柄集合（均经 X-Ops-Token，由 api.ts 的新函数提供）。
export type LiveOpsPanelProps = {
  onClose: () => void;
  // POST /api/ops/worlds/:worldId/events
  injectWorldEvent: (worldId: string, input: GmWorldEventInput) => Promise<GmWorldEventResult>;
  // POST /api/ops/seasons
  createSeason: (input: CreateSeasonInput) => Promise<Season>;
  // POST /api/ops/seasons/:id/finalize
  finalizeSeason: (seasonId: string) => Promise<FinalizeResult>;
  // GET /api/ops/worlds/:worldId/arbitration-audit?turn_start&turn_end
  fetchArbitrationAudit: (
    worldId: string,
    turnStart: number,
    turnEnd: number,
  ) => Promise<ArbitrationAuditReport>;
};

// ============ 样式基元（自包含，与 OpsDashboard.tsx 一致） ============

const overlayStyle: React.CSSProperties = {
  position: "fixed",
  inset: 0,
  zIndex: zIndex.fullscreenModal,
  background: "rgba(8, 9, 14, 0.62)",
  display: "flex",
  alignItems: "flex-start",
  justifyContent: "center",
  padding: "48px 16px",
  overflowY: "auto",
};
const wideDialogStyle: React.CSSProperties = {
  width: "min(720px, 100%)",
  maxHeight: "calc(100vh - 96px)",
  overflowY: "auto",
  background: "rgba(18, 20, 28, 0.97)",
  border: "1px solid rgba(217, 188, 115, 0.35)",
  borderRadius: 12,
  boxShadow: "0 16px 48px rgba(0,0,0,0.55)",
  color: "#e8e2d2",
  padding: 18,
  fontSize: 13,
};
const headerStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  justifyContent: "space-between",
  marginBottom: 12,
};
const brandStyle: React.CSSProperties = { color: "#f2d98f", fontWeight: 700, fontSize: 16 };
const subStyle: React.CSSProperties = { color: "#9aa0ad", fontSize: 11, marginTop: 2 };
const inputStyle: React.CSSProperties = {
  width: "100%",
  boxSizing: "border-box",
  background: "rgba(32, 36, 48, 0.9)",
  color: "#e8e2d2",
  border: "1px solid rgba(255,255,255,0.12)",
  borderRadius: 6,
  padding: "7px 8px",
  fontSize: 13,
};
const closeBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "transparent",
  border: "none",
  color: "#9aa0ad",
  fontSize: 20,
  lineHeight: 1,
};
const ghostBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "transparent",
  border: "1px solid rgba(255,255,255,0.18)",
  color: "#cbd1da",
  borderRadius: 6,
  padding: "8px 14px",
  fontSize: 13,
};
const primaryBtnStyle: React.CSSProperties = {
  ...ghostBtnStyle,
  border: "1px solid rgba(217, 188, 115, 0.6)",
  color: "#ffe7a0",
};
const toastErrStyle: React.CSSProperties = {
  marginTop: 10,
  padding: "8px 10px",
  borderRadius: 6,
  background: "rgba(196, 84, 74, 0.16)",
  border: "1px solid rgba(196, 84, 74, 0.5)",
  color: "#f0b0a6",
  fontSize: 12,
};
const toastOkStyle: React.CSSProperties = {
  ...toastErrStyle,
  background: "rgba(96, 170, 110, 0.16)",
  border: "1px solid rgba(96, 170, 110, 0.5)",
  color: "#a9e0b4",
};
const sectionCardStyle: React.CSSProperties = {
  background: "rgba(32, 36, 48, 0.7)",
  border: "1px solid rgba(255,255,255,0.06)",
  borderRadius: 8,
  padding: "12px 14px",
  margin: "10px 0",
};
const cardTitleStyle: React.CSSProperties = {
  color: "#f0ead8",
  fontWeight: 700,
  fontSize: 14,
  marginBottom: 8,
};
const fieldLabelStyle: React.CSSProperties = {
  color: "#9aa0ad",
  fontSize: 11,
  margin: "8px 0 3px",
  display: "block",
};
const metricGridStyle: React.CSSProperties = {
  display: "grid",
  gridTemplateColumns: "repeat(auto-fit, minmax(120px, 1fr))",
  gap: 8,
  margin: "6px 0",
};
const metricBoxStyle: React.CSSProperties = {
  background: "rgba(0,0,0,0.25)",
  border: "1px solid rgba(255,255,255,0.06)",
  borderRadius: 8,
  padding: "10px 12px",
};
const metricLabelStyle: React.CSSProperties = {
  color: "#9aa0ad",
  fontSize: 11,
  textTransform: "uppercase",
};
const metricValueStyle: React.CSSProperties = {
  color: "#f2d98f",
  fontSize: 20,
  fontWeight: 700,
  marginTop: 4,
};
const metricValueWarnStyle: React.CSSProperties = { ...metricValueStyle, color: "#f0b0a6" };

function errText(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
function fmtPct(n: number): string {
  return `${((Number.isFinite(n) ? n : 0) * 100).toFixed(1)}%`;
}

// LiveOpsPanel 是 GM 注入 + 赛季 + 零和审计的运营专属看板（三块独立，互不阻断）。
export function LiveOpsPanel(props: LiveOpsPanelProps) {
  const { onClose, injectWorldEvent, createSeason, finalizeSeason, fetchArbitrationAudit } = props;

  // ---- GM 世界事件注入 ----
  const [gmWorldId, setGmWorldId] = useState("");
  const [gmKind, setGmKind] = useState("");
  const [gmImportance, setGmImportance] = useState(5);
  const [gmNarrative, setGmNarrative] = useState("");
  const [gmBusy, setGmBusy] = useState(false);
  const [gmErr, setGmErr] = useState("");
  const [gmOk, setGmOk] = useState("");

  const submitGm = useCallback(async () => {
    if (!gmWorldId.trim() || !gmKind.trim()) {
      setGmErr("世界 ID 与事件类型均必填。");
      setGmOk("");
      return;
    }
    setGmBusy(true);
    setGmErr("");
    setGmOk("");
    try {
      const res = await injectWorldEvent(gmWorldId.trim(), {
        kind: gmKind.trim(),
        importance: gmImportance,
        payload: gmNarrative.trim() ? { narrative: gmNarrative.trim() } : undefined,
      });
      setGmOk(`已注入：tick=${res.world_tick}，cross_event=${res.cross_event_id.slice(0, 8)}…，审计=${res.audit_id.slice(0, 8)}…`);
    } catch (e) {
      setGmErr(`注入失败：${errText(e)}`);
    } finally {
      setGmBusy(false);
    }
  }, [gmImportance, gmKind, gmNarrative, gmWorldId, injectWorldEvent]);

  // ---- 赛季 ----
  const [seasonName, setSeasonName] = useState("");
  const [seasonThemeId, setSeasonThemeId] = useState("");
  const [createdSeason, setCreatedSeason] = useState<Season | null>(null);
  const [seasonBusy, setSeasonBusy] = useState(false);
  const [seasonErr, setSeasonErr] = useState("");
  const [finalizeId, setFinalizeId] = useState("");
  const [finalizeRes, setFinalizeRes] = useState<FinalizeResult | null>(null);

  const submitCreateSeason = useCallback(async () => {
    if (!seasonName.trim()) {
      setSeasonErr("赛季名必填。");
      return;
    }
    setSeasonBusy(true);
    setSeasonErr("");
    try {
      const s = await createSeason({
        name: seasonName.trim(),
        content_theme_id: seasonThemeId.trim() || undefined,
      });
      setCreatedSeason(s);
      setFinalizeId(s.id);
    } catch (e) {
      setSeasonErr(`创建赛季失败：${errText(e)}`);
    } finally {
      setSeasonBusy(false);
    }
  }, [createSeason, seasonName, seasonThemeId]);

  const submitFinalize = useCallback(async () => {
    if (!finalizeId.trim()) {
      setSeasonErr("收尾需赛季 ID。");
      return;
    }
    setSeasonBusy(true);
    setSeasonErr("");
    try {
      const r = await finalizeSeason(finalizeId.trim());
      setFinalizeRes(r);
    } catch (e) {
      setSeasonErr(`收尾赛季失败：${errText(e)}`);
    } finally {
      setSeasonBusy(false);
    }
  }, [finalizeId, finalizeSeason]);

  // ---- 零和审计 ----
  const [auditWorldId, setAuditWorldId] = useState("");
  const [turnStart, setTurnStart] = useState(0);
  const [turnEnd, setTurnEnd] = useState(100);
  const [audit, setAudit] = useState<ArbitrationAuditReport | null>(null);
  const [auditBusy, setAuditBusy] = useState(false);
  const [auditErr, setAuditErr] = useState("");

  const runAudit = useCallback(async () => {
    if (!auditWorldId.trim()) {
      setAuditErr("审计需世界 ID。");
      return;
    }
    setAuditBusy(true);
    setAuditErr("");
    try {
      const r = await fetchArbitrationAudit(auditWorldId.trim(), turnStart, turnEnd);
      setAudit(r);
    } catch (e) {
      setAuditErr(`审计失败：${errText(e)}`);
    } finally {
      setAuditBusy(false);
    }
  }, [auditWorldId, fetchArbitrationAudit, turnEnd, turnStart]);

  return (
    <div style={overlayStyle} role="dialog" aria-label="Live-Ops 运营看板" aria-modal>
      <div style={wideDialogStyle}>
        <div style={headerStyle}>
          <div>
            <div style={brandStyle}>Live-Ops 运营台</div>
            <div style={subStyle}>
              GM 世界事件注入 + 赛季骨架 + 零和审计。均为敏感运营端点（X-Ops-Token），写入 append-only、可仲裁。
            </div>
          </div>
          <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭">
            ×
          </button>
        </div>

        {/* ============ GM 世界事件注入 ============ */}
        <div style={sectionCardStyle}>
          <div style={cardTitleStyle}>GM 世界事件注入</div>
          <div style={subStyle}>
            往某活世界投一条权威跨事件（天灾/外敌/丰年…），与玩家事件同总线、同时钟，全量留审计。
          </div>
          <label style={fieldLabelStyle}>世界 ID</label>
          <input
            style={inputStyle}
            value={gmWorldId}
            onChange={(e) => setGmWorldId(e.target.value)}
            placeholder="world_id"
          />
          <label style={fieldLabelStyle}>事件类型（kind）</label>
          <input
            style={inputStyle}
            value={gmKind}
            onChange={(e) => setGmKind(e.target.value)}
            placeholder="如 天灾 / 外敌压境 / 集市丰年"
          />
          <label style={fieldLabelStyle}>重要度（0–10）</label>
          <input
            style={inputStyle}
            type="number"
            min={0}
            max={10}
            value={gmImportance}
            onChange={(e) => setGmImportance(Number(e.target.value))}
          />
          <label style={fieldLabelStyle}>叙事文案（可选，进 payload）</label>
          <input
            style={inputStyle}
            value={gmNarrative}
            onChange={(e) => setGmNarrative(e.target.value)}
            placeholder="如 山洪暴发，沿河村落告急"
          />
          <div style={{ marginTop: 10 }}>
            <button type="button" style={primaryBtnStyle} onClick={() => void submitGm()} disabled={gmBusy}>
              {gmBusy ? "注入中…" : "注入世界事件"}
            </button>
          </div>
          {gmErr ? <div style={toastErrStyle}>{gmErr}</div> : null}
          {gmOk ? <div style={toastOkStyle}>{gmOk}</div> : null}
        </div>

        {/* ============ 赛季 ============ */}
        <div style={sectionCardStyle}>
          <div style={cardTitleStyle}>赛季</div>
          <div style={subStyle}>创建赛季（建世界 + 落 seasons）；收尾把存活角色回流名人堂、世界封存。</div>
          <label style={fieldLabelStyle}>赛季名</label>
          <input
            style={inputStyle}
            value={seasonName}
            onChange={(e) => setSeasonName(e.target.value)}
            placeholder="如 开元一季"
          />
          <label style={fieldLabelStyle}>内容母题 ID（可选）</label>
          <input
            style={inputStyle}
            value={seasonThemeId}
            onChange={(e) => setSeasonThemeId(e.target.value)}
            placeholder="content_theme_id"
          />
          <div style={{ marginTop: 10, display: "flex", gap: 6, flexWrap: "wrap" }}>
            <button type="button" style={primaryBtnStyle} onClick={() => void submitCreateSeason()} disabled={seasonBusy}>
              {seasonBusy ? "处理中…" : "创建赛季"}
            </button>
          </div>
          {createdSeason ? (
            <div style={toastOkStyle}>
              已创建赛季「{createdSeason.name}」：id={createdSeason.id.slice(0, 8)}…，世界=
              {createdSeason.world_id.slice(0, 8)}…
            </div>
          ) : null}

          <label style={fieldLabelStyle}>收尾赛季 ID</label>
          <input
            style={inputStyle}
            value={finalizeId}
            onChange={(e) => setFinalizeId(e.target.value)}
            placeholder="season_id"
          />
          <div style={{ marginTop: 10 }}>
            <button type="button" style={ghostBtnStyle} onClick={() => void submitFinalize()} disabled={seasonBusy}>
              收尾赛季（封存 + 回流名人堂）
            </button>
          </div>
          {finalizeRes ? (
            <div style={toastOkStyle}>
              赛季已收尾：成员 {finalizeRes.members_total} 人，回流 {finalizeRes.archived} 人
              {finalizeRes.archive_errors.length ? `（${finalizeRes.archive_errors.length} 人回流失败）` : ""}，
              世界{finalizeRes.sealed ? "已封存" : "未封存"}。
            </div>
          ) : null}
          {seasonErr ? <div style={toastErrStyle}>{seasonErr}</div> : null}
        </div>

        {/* ============ 零和审计 ============ */}
        <div style={sectionCardStyle}>
          <div style={cardTitleStyle}>零和监控审计（反 P2W）</div>
          <div style={subStyle}>
            扫某世界某回合区间的仲裁结局，按付费态分组算胜率。付费组胜率 &gt; {fmtPct(audit?.redline_rate ?? 0.6)}{" "}
            判红线——观测付费有没有不公平地赢（付费态绝不进 Score）。
          </div>
          <label style={fieldLabelStyle}>世界 ID</label>
          <input
            style={inputStyle}
            value={auditWorldId}
            onChange={(e) => setAuditWorldId(e.target.value)}
            placeholder="world_id"
          />
          <div style={{ display: "flex", gap: 6 }}>
            <div style={{ flex: 1 }}>
              <label style={fieldLabelStyle}>起始回合</label>
              <input
                style={inputStyle}
                type="number"
                value={turnStart}
                onChange={(e) => setTurnStart(Number(e.target.value))}
              />
            </div>
            <div style={{ flex: 1 }}>
              <label style={fieldLabelStyle}>结束回合</label>
              <input
                style={inputStyle}
                type="number"
                value={turnEnd}
                onChange={(e) => setTurnEnd(Number(e.target.value))}
              />
            </div>
          </div>
          <div style={{ marginTop: 10 }}>
            <button type="button" style={ghostBtnStyle} onClick={() => void runAudit()} disabled={auditBusy}>
              {auditBusy ? "审计中…" : "运行审计"}
            </button>
          </div>
          {auditErr ? <div style={toastErrStyle}>{auditErr}</div> : null}
          {audit ? (
            <>
              <div style={metricGridStyle}>
                <div style={metricBoxStyle}>
                  <div style={metricLabelStyle}>付费组胜率</div>
                  <div style={audit.issue_detected ? metricValueWarnStyle : metricValueStyle}>
                    {fmtPct(audit.paid.win_rate)}
                  </div>
                </div>
                <div style={metricBoxStyle}>
                  <div style={metricLabelStyle}>非付费组胜率</div>
                  <div style={metricValueStyle}>{fmtPct(audit.non_paid.win_rate)}</div>
                </div>
                <div style={metricBoxStyle}>
                  <div style={metricLabelStyle}>付费组样本</div>
                  <div style={metricValueStyle}>{audit.paid.total}</div>
                </div>
              </div>
              <div style={audit.issue_detected ? toastErrStyle : toastOkStyle}>
                {audit.issue_detected ? "⚠ 红线触发：" : "✓ "}
                {audit.note}
              </div>
            </>
          ) : null}
        </div>
      </div>
    </div>
  );
}

export default LiveOpsPanel;
