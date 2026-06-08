/* 文件说明：治理 UI 三件套（举报弹窗 / 审计·举报管理台 / 隐私擦除），接进 App.tsx。
   - ReportDialog：面向玩家的举报弹窗（选分类+填详情+可选目标单位），调 submitModerationReport，原型放行无需 token。
   - GovernancePanel：面向运营/dev 的审计·举报管理台，调 getAuditBundle 分区折叠展示
     （未解决举报/对话/LLM 交互/原始事件），每条未决举报可标记 已解决/警告/封禁（resolveModerationReport）。
   - PrivacyEraseDialog：面向运营的不可逆数据擦除，5 类开关 + 二次确认，调 eraseSessionPrivateData 后展示各计数。
   ops 端点需 X-Ops-Token：管理台与擦除弹窗留一个 ops token 输入框调 setOpsToken（原型缺头放行）。
   自包含内联样式（参照 FatePanel.tsx 的 panelStyle 模式），仅 import api.ts/types.ts，不依赖其它并行组件。*/

import { useCallback, useEffect, useMemo, useState } from "react";
import {
  APIError,
  getAuditBundle,
  eraseSessionPrivateData,
  getOpsToken,
  resolveModerationReport,
  setOpsToken,
  submitModerationReport,
} from "../session/api";
import type {
  AuditBundle,
  DialogueMessage,
  LLMInteraction,
  ModerationReport,
  PrivacyEraseOptions,
  PrivacyEraseResult,
  RawEventEntry,
} from "../session/types";

// ============ 共享样式基元（自包含，参照 FatePanel.tsx） ============

const overlayStyle: React.CSSProperties = {
  position: "fixed",
  inset: 0,
  zIndex: 60,
  background: "rgba(8, 9, 14, 0.62)",
  display: "flex",
  alignItems: "flex-start",
  justifyContent: "center",
  padding: "48px 16px",
  overflowY: "auto",
};

const dialogStyle: React.CSSProperties = {
  width: "min(560px, 100%)",
  background: "rgba(18, 20, 28, 0.97)",
  border: "1px solid rgba(217, 188, 115, 0.35)",
  borderRadius: 12,
  boxShadow: "0 16px 48px rgba(0,0,0,0.55)",
  color: "#e8e2d2",
  padding: 18,
  fontSize: 13,
};

const wideDialogStyle: React.CSSProperties = {
  ...dialogStyle,
  width: "min(820px, 100%)",
  maxHeight: "calc(100vh - 96px)",
  overflowY: "auto",
};

const headerStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  justifyContent: "space-between",
  marginBottom: 12,
};

const brandStyle: React.CSSProperties = { color: "#f2d98f", fontWeight: 700, fontSize: 16 };
const subStyle: React.CSSProperties = { color: "#9aa0ad", fontSize: 11, marginTop: 2 };
const labelStyle: React.CSSProperties = {
  display: "block",
  color: "#cdb98a",
  fontSize: 11,
  letterSpacing: 0.4,
  textTransform: "uppercase",
  margin: "12px 0 4px",
};
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
const selectStyle: React.CSSProperties = { ...inputStyle };
const textareaStyle: React.CSSProperties = { ...inputStyle, minHeight: 80, resize: "vertical", fontFamily: "inherit" };

const closeBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "transparent",
  border: "none",
  color: "#9aa0ad",
  fontSize: 20,
  lineHeight: 1,
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
const ghostBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "transparent",
  border: "1px solid rgba(255,255,255,0.18)",
  color: "#cbd1da",
  borderRadius: 6,
  padding: "8px 14px",
  fontSize: 13,
};
const dangerBtnStyle: React.CSSProperties = {
  ...primaryBtnStyle,
  background: "rgba(196, 84, 74, 0.2)",
  border: "1px solid rgba(196, 84, 74, 0.7)",
  color: "#f0b0a6",
};
const footerRowStyle: React.CSSProperties = { display: "flex", gap: 8, justifyContent: "flex-end", marginTop: 16 };

const toastOkStyle: React.CSSProperties = {
  marginTop: 12,
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

const sectionCardStyle: React.CSSProperties = {
  background: "rgba(32, 36, 48, 0.7)",
  border: "1px solid rgba(255,255,255,0.06)",
  borderRadius: 8,
  padding: "10px 12px",
  margin: "8px 0",
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

// errText 把错误归一为可展示文案，合规/鉴权类错误透出 status/reason。
function errText(err: unknown): string {
  if (err instanceof APIError) {
    const parts = [err.message];
    if (typeof err.status === "number") parts.push(`(HTTP ${err.status})`);
    if (err.reason) parts.push(`原因：${err.reason}`);
    return parts.join(" ");
  }
  return err instanceof Error ? err.message : String(err);
}

// ============ 1) 举报弹窗（面向玩家） ============

const REPORT_CATEGORIES: { value: string; label: string }[] = [
  { value: "harassment", label: "骚扰/辱骂" },
  { value: "cheating", label: "作弊/外挂" },
  { value: "inappropriate", label: "不当内容" },
  { value: "other", label: "其他" },
];

export type ReportDialogProps = {
  sessionId: string;
  // 预填的被举报单位（可选，玩家从某角色卡发起时带上）。
  targetUnitId?: string;
  // 举报人标识（可选，登录态可传账号名；默认匿名）。
  reporter?: string;
  onClose: () => void;
};

// ReportDialog 是面向玩家的举报弹窗。提交成功/失败均给提示，成功后稍等自动关闭。
export function ReportDialog({ sessionId, targetUnitId, reporter, onClose }: ReportDialogProps) {
  const [category, setCategory] = useState<string>(REPORT_CATEGORIES[0].value);
  const [detail, setDetail] = useState("");
  const [unitId, setUnitId] = useState<string>(targetUnitId ?? "");
  const [busy, setBusy] = useState(false);
  const [ok, setOk] = useState("");
  const [err, setErr] = useState("");

  const submit = useCallback(async () => {
    if (detail.trim() === "") {
      setErr("请填写举报详情。");
      setOk("");
      return;
    }
    setBusy(true);
    setErr("");
    setOk("");
    try {
      await submitModerationReport(sessionId, {
        reporter: reporter?.trim() || undefined,
        unit_id: unitId.trim() || undefined,
        category,
        detail: detail.trim(),
      });
      setOk("已收到你的举报。我们会尽快处理。");
      setDetail("");
      window.setTimeout(onClose, 1400);
    } catch (e) {
      setErr(`提交失败：${errText(e)}`);
    } finally {
      setBusy(false);
    }
  }, [category, detail, onClose, reporter, sessionId, unitId]);

  return (
    <div style={overlayStyle} role="dialog" aria-label="举报" aria-modal>
      <div style={dialogStyle}>
        <div style={headerStyle}>
          <div>
            <div style={brandStyle}>举报</div>
            <div style={subStyle}>看到不对劲的事？告诉我们。提交将记入本局治理记录。</div>
          </div>
          <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭举报弹窗">
            ×
          </button>
        </div>

        <label style={labelStyle} htmlFor="report-category">
          举报类型
        </label>
        <select
          id="report-category"
          style={selectStyle}
          value={category}
          onChange={(e) => setCategory(e.target.value)}
        >
          {REPORT_CATEGORIES.map((c) => (
            <option key={c.value} value={c.value}>
              {c.label}
            </option>
          ))}
        </select>

        <label style={labelStyle} htmlFor="report-unit">
          被举报角色 ID（可选）
        </label>
        <input
          id="report-unit"
          style={inputStyle}
          value={unitId}
          onChange={(e) => setUnitId(e.target.value)}
          placeholder="留空表示举报整局/无关具体角色"
        />

        <label style={labelStyle} htmlFor="report-detail">
          详情
        </label>
        <textarea
          id="report-detail"
          style={textareaStyle}
          value={detail}
          onChange={(e) => setDetail(e.target.value)}
          placeholder="请描述发生了什么、何时、涉及谁。"
        />

        {ok ? <div style={toastOkStyle}>{ok}</div> : null}
        {err ? <div style={toastErrStyle}>{err}</div> : null}

        <div style={footerRowStyle}>
          <button type="button" style={ghostBtnStyle} onClick={onClose} disabled={busy}>
            取消
          </button>
          <button type="button" style={primaryBtnStyle} onClick={() => void submit()} disabled={busy}>
            {busy ? "提交中…" : "提交举报"}
          </button>
        </div>
      </div>
    </div>
  );
}

// ============ 2) 审计·举报管理台（面向运营/dev） ============

// CollapsibleSection 是可折叠分区（审计数据量大，默认折叠非关键区）。
function CollapsibleSection({
  title,
  count,
  defaultOpen,
  children,
}: {
  title: string;
  count: number;
  defaultOpen?: boolean;
  children: React.ReactNode;
}) {
  const [open, setOpen] = useState<boolean>(defaultOpen ?? false);
  return (
    <div style={{ margin: "10px 0" }}>
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        style={{
          width: "100%",
          display: "flex",
          alignItems: "center",
          justifyContent: "space-between",
          cursor: "pointer",
          background: "rgba(32, 36, 48, 0.85)",
          border: "1px solid rgba(255,255,255,0.1)",
          borderRadius: 8,
          padding: "8px 12px",
          color: "#e8e2d2",
          fontSize: 13,
        }}
        aria-expanded={open}
      >
        <span>
          {open ? "▾" : "▸"} {title}
          <span style={miniPillStyle}>{count}</span>
        </span>
      </button>
      {open ? <div style={{ marginTop: 6 }}>{children}</div> : null}
    </div>
  );
}

function ReportRow({
  report,
  busy,
  onResolve,
}: {
  report: ModerationReport;
  busy: boolean;
  onResolve: (action: "resolve" | "warn" | "ban") => void;
}) {
  return (
    <div style={sectionCardStyle}>
      <div style={{ display: "flex", justifyContent: "space-between", gap: 8 }}>
        <div style={{ fontWeight: 600, color: "#f0ead8" }}>
          {report.category}
          {report.unit_id ? <span style={miniPillStyle}>角色 {report.unit_id}</span> : null}
          {report.resolved ? <span style={{ ...miniPillStyle, color: "#bfe6c8" }}>已处理</span> : null}
        </div>
        <div style={{ color: "#7e8493", fontSize: 11, whiteSpace: "nowrap" }}>
          回合 {report.turn} · {report.reporter || "匿名"}
        </div>
      </div>
      <p style={{ margin: "6px 0 0", color: "#c2c7d0", fontSize: 12 }}>{report.detail}</p>
      {!report.resolved ? (
        <div style={{ display: "flex", gap: 6, marginTop: 8, flexWrap: "wrap" }}>
          <button type="button" style={ghostBtnStyle} disabled={busy} onClick={() => onResolve("resolve")}>
            标记已解决
          </button>
          <button type="button" style={ghostBtnStyle} disabled={busy} onClick={() => onResolve("warn")}>
            警告
          </button>
          <button type="button" style={dangerBtnStyle} disabled={busy} onClick={() => onResolve("ban")}>
            封禁
          </button>
        </div>
      ) : (
        <div style={{ color: "#7e8493", fontSize: 11, marginTop: 6 }}>
          已于 {report.resolved_at ?? "—"} 处理
        </div>
      )}
    </div>
  );
}

export type GovernancePanelProps = {
  sessionId: string;
  // 审计数据量大，默认拉 80 条。
  limit?: number;
  onClose: () => void;
  // 打开隐私擦除弹窗（由 App 编排两个弹窗的挂载；可选）。
  onOpenPrivacyErase?: () => void;
};

// GovernancePanel 是面向运营/dev 的审计·举报管理台。
export function GovernancePanel({ sessionId, limit = 80, onClose, onOpenPrivacyErase }: GovernancePanelProps) {
  const [bundle, setBundle] = useState<AuditBundle | null>(null);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState("");
  const [resolvingID, setResolvingID] = useState("");
  const [opsTokenInput, setOpsTokenInput] = useState<string>(getOpsToken());

  const load = useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      const data = await getAuditBundle(sessionId, limit);
      setBundle(data);
    } catch (e) {
      setErr(`读取审计包失败：${errText(e)}`);
    } finally {
      setLoading(false);
    }
  }, [limit, sessionId]);

  useEffect(() => {
    void load();
  }, [load]);

  const applyOpsToken = useCallback(() => {
    setOpsToken(opsTokenInput.trim());
    // 重新拉取（带上新令牌）。
    void load();
  }, [load, opsTokenInput]);

  const onResolveReport = useCallback(
    async (reportID: string, action: "resolve" | "warn" | "ban") => {
      setResolvingID(reportID);
      setErr("");
      try {
        await resolveModerationReport(sessionId, reportID, action);
        await load();
      } catch (e) {
        setErr(`处理举报失败：${errText(e)}`);
      } finally {
        setResolvingID("");
      }
    },
    [load, sessionId],
  );

  const reports = useMemo(() => bundle?.reports ?? [], [bundle]);
  const openReports = useMemo(() => reports.filter((r) => !r.resolved), [reports]);
  const closedReports = useMemo(() => reports.filter((r) => r.resolved), [reports]);
  const dialogue: DialogueMessage[] = bundle?.dialogue_history ?? [];
  const llm: LLMInteraction[] = bundle?.llm_interactions ?? [];
  const rawEvents: RawEventEntry[] = bundle?.raw_event_log ?? [];

  return (
    <div style={overlayStyle} role="dialog" aria-label="治理管理台" aria-modal>
      <div style={wideDialogStyle}>
        <div style={headerStyle}>
          <div>
            <div style={brandStyle}>治理 · 审计管理台</div>
            <div style={subStyle}>运营/开发可见。举报处理与审计链只读浏览（含完整 LLM prompt，请谨慎）。</div>
          </div>
          <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭管理台">
            ×
          </button>
        </div>

        {/* ops token 注入：原型缺头放行，正式运营态填入令牌后才带 X-Ops-Token */}
        <div style={{ display: "flex", gap: 6, alignItems: "center", marginBottom: 8, flexWrap: "wrap" }}>
          <input
            style={{ ...inputStyle, flex: "1 1 200px", width: "auto" }}
            value={opsTokenInput}
            onChange={(e) => setOpsTokenInput(e.target.value)}
            placeholder="X-Ops-Token（运营令牌，原型可留空）"
            aria-label="运营令牌"
          />
          <button type="button" style={ghostBtnStyle} onClick={applyOpsToken}>
            应用令牌并刷新
          </button>
          <button type="button" style={ghostBtnStyle} onClick={() => void load()} disabled={loading}>
            {loading ? "加载中…" : "刷新"}
          </button>
          {onOpenPrivacyErase ? (
            <button type="button" style={dangerBtnStyle} onClick={onOpenPrivacyErase}>
              隐私擦除…
            </button>
          ) : null}
        </div>

        {err ? <div style={toastErrStyle}>{err}</div> : null}

        {/* 未解决举报：默认展开（最优先） */}
        <CollapsibleSection title="未解决举报" count={openReports.length} defaultOpen>
          {openReports.length === 0 ? (
            <div style={{ ...sectionCardStyle, color: "#9aa0ad" }}>暂无待处理举报。</div>
          ) : (
            openReports.map((r) => (
              <ReportRow
                key={r.id}
                report={r}
                busy={resolvingID === r.id}
                onResolve={(action) => void onResolveReport(r.id, action)}
              />
            ))
          )}
        </CollapsibleSection>

        {/* 已处理举报 */}
        <CollapsibleSection title="已处理举报" count={closedReports.length}>
          {closedReports.length === 0 ? (
            <div style={{ ...sectionCardStyle, color: "#9aa0ad" }}>无。</div>
          ) : (
            closedReports.map((r) => (
              <ReportRow key={r.id} report={r} busy={false} onResolve={() => undefined} />
            ))
          )}
        </CollapsibleSection>

        {/* 最近对话 */}
        <CollapsibleSection title="最近对话" count={dialogue.length}>
          {dialogue.length === 0 ? (
            <div style={{ ...sectionCardStyle, color: "#9aa0ad" }}>无对话记录。</div>
          ) : (
            dialogue.slice(-40).reverse().map((d) => (
              <div key={d.id} style={sectionCardStyle}>
                <div style={{ fontSize: 11, color: "#7e8493" }}>
                  回合 {d.turn} · {d.speaker}
                  {d.used_fallback ? <span style={miniPillStyle}>fallback</span> : null}
                </div>
                <p style={{ margin: "4px 0 0", color: "#c2c7d0", fontSize: 12 }}>{d.message}</p>
              </div>
            ))
          )}
        </CollapsibleSection>

        {/* LLM 交互（含 prompt，审计高危） */}
        <CollapsibleSection title="LLM 交互（含完整 prompt）" count={llm.length}>
          {llm.length === 0 ? (
            <div style={{ ...sectionCardStyle, color: "#9aa0ad" }}>无 LLM 交互记录。</div>
          ) : (
            llm.slice(-30).reverse().map((it) => (
              <details key={it.id} style={sectionCardStyle}>
                <summary style={{ cursor: "pointer", fontSize: 12, color: "#e0d8c4" }}>
                  {it.kind} · 回合 {it.turn}
                  {it.used_fallback ? <span style={miniPillStyle}>fallback</span> : null}
                  {typeof it.estimated_cost_usd === "number" ? (
                    <span style={{ ...miniPillStyle, color: "#cbd1da" }}>
                      ${it.estimated_cost_usd.toFixed(4)}
                    </span>
                  ) : null}
                </summary>
                {it.summary ? (
                  <p style={{ margin: "6px 0", color: "#c2c7d0", fontSize: 12 }}>{it.summary}</p>
                ) : null}
                <pre
                  style={{
                    whiteSpace: "pre-wrap",
                    wordBreak: "break-word",
                    fontSize: 11,
                    color: "#9aa0ad",
                    background: "rgba(0,0,0,0.25)",
                    borderRadius: 6,
                    padding: 8,
                    margin: "6px 0 0",
                    maxHeight: 200,
                    overflowY: "auto",
                  }}
                >
                  {it.user_prompt || it.system_prompt || it.parsed_output || it.error_message || "(空)"}
                </pre>
              </details>
            ))
          )}
        </CollapsibleSection>

        {/* 原始事件 */}
        <CollapsibleSection title="原始事件流" count={rawEvents.length}>
          {rawEvents.length === 0 ? (
            <div style={{ ...sectionCardStyle, color: "#9aa0ad" }}>无原始事件。</div>
          ) : (
            rawEvents.slice(-50).reverse().map((ev) => (
              <div key={ev.id} style={sectionCardStyle}>
                <div style={{ fontSize: 11, color: "#7e8493" }}>
                  回合 {ev.turn} · {ev.source}/{ev.kind}
                </div>
                <p style={{ margin: "4px 0 0", color: "#c2c7d0", fontSize: 12 }}>{ev.summary}</p>
              </div>
            ))
          )}
        </CollapsibleSection>
      </div>
    </div>
  );
}

// ============ 3) 隐私擦除弹窗（面向运营，不可逆，二次确认） ============

const ERASE_TOGGLES: { key: keyof PrivacyEraseOptions; label: string; hint: string }[] = [
  { key: "erase_dialogue", label: "对话记录", hint: "玩家与角色的全部对话" },
  { key: "erase_llm_details", label: "LLM 细节", hint: "提示词/原始输出（红字段，保留 token 统计）" },
  { key: "erase_audit_trail", label: "审计链", hint: "操作日志与原始事件流" },
  { key: "erase_memories", label: "角色记忆", hint: "单位记忆行与高光" },
  { key: "erase_reports", label: "举报记录", hint: "本局全部举报" },
];

export type PrivacyEraseDialogProps = {
  sessionId: string;
  onClose: () => void;
};

// PrivacyEraseDialog 是面向运营的不可逆数据擦除弹窗（二次确认）。
export function PrivacyEraseDialog({ sessionId, onClose }: PrivacyEraseDialogProps) {
  const [options, setOptions] = useState<PrivacyEraseOptions>({});
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [result, setResult] = useState<PrivacyEraseResult | null>(null);
  const [opsTokenInput, setOpsTokenInput] = useState<string>(getOpsToken());

  const anySelected = useMemo(() => ERASE_TOGGLES.some((t) => options[t.key]), [options]);

  const toggle = useCallback((key: keyof PrivacyEraseOptions) => {
    setOptions((prev) => ({ ...prev, [key]: !prev[key] }));
  }, []);

  const doErase = useCallback(async () => {
    setBusy(true);
    setErr("");
    try {
      // 应用 ops 令牌（若填了），擦除是 ops 端点。
      setOpsToken(opsTokenInput.trim());
      const { result: res } = await eraseSessionPrivateData(sessionId, options);
      setResult(res);
      setConfirming(false);
    } catch (e) {
      setErr(`擦除失败：${errText(e)}`);
      setConfirming(false);
    } finally {
      setBusy(false);
    }
  }, [options, opsTokenInput, sessionId]);

  return (
    <div style={overlayStyle} role="dialog" aria-label="隐私擦除" aria-modal>
      <div style={dialogStyle}>
        <div style={headerStyle}>
          <div>
            <div style={brandStyle}>隐私擦除</div>
            <div style={subStyle}>不可逆操作。擦除后无法恢复，请确认范围。</div>
          </div>
          <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭擦除弹窗">
            ×
          </button>
        </div>

        {result ? (
          // 擦除完成：展示各计数。
          <>
            <div style={toastOkStyle}>擦除完成。以下为各项删除计数：</div>
            <div style={sectionCardStyle}>
              <CountRow label="对话条目" value={result.dialogue_entries_erased} />
              <CountRow label="LLM 交互（脱敏）" value={result.llm_interactions_redacted} />
              <CountRow label="审计日志" value={result.audit_logs_erased} />
              <CountRow label="原始事件" value={result.raw_events_erased} />
              <CountRow label="举报记录" value={result.reports_erased} />
              <CountRow label="角色高光" value={result.unit_highlights_erased} />
              <CountRow label="记忆行" value={result.memory_rows_erased} />
              <CountRow label="记忆全文索引行" value={result.memory_fts_rows_erased} />
              <div style={{ color: "#7e8493", fontSize: 11, marginTop: 6 }}>
                阶段快照{result.phase_snapshots_regenerated ? "已重建" : "未变更"}。
              </div>
            </div>
            <div style={footerRowStyle}>
              <button type="button" style={primaryBtnStyle} onClick={onClose}>
                完成
              </button>
            </div>
          </>
        ) : (
          <>
            <div style={{ ...toastErrStyle, marginTop: 0 }}>
              ⚠ 不可逆：被勾选的数据将永久删除，无法找回。
            </div>

            {ERASE_TOGGLES.map((t) => (
              <label
                key={t.key}
                style={{
                  display: "flex",
                  alignItems: "flex-start",
                  gap: 8,
                  margin: "8px 0",
                  cursor: "pointer",
                }}
              >
                <input
                  type="checkbox"
                  checked={Boolean(options[t.key])}
                  onChange={() => toggle(t.key)}
                  style={{ marginTop: 2 }}
                />
                <span>
                  <span style={{ color: "#f0ead8" }}>{t.label}</span>
                  <span style={{ display: "block", color: "#9aa0ad", fontSize: 11 }}>{t.hint}</span>
                </span>
              </label>
            ))}

            <label style={labelStyle} htmlFor="erase-ops-token">
              运营令牌（X-Ops-Token，原型可留空）
            </label>
            <input
              id="erase-ops-token"
              style={inputStyle}
              value={opsTokenInput}
              onChange={(e) => setOpsTokenInput(e.target.value)}
              placeholder="X-Ops-Token"
            />

            {err ? <div style={toastErrStyle}>{err}</div> : null}

            {confirming ? (
              <div style={{ ...toastErrStyle }}>
                确认要永久擦除所选数据吗？此操作不可撤销。
                <div style={footerRowStyle}>
                  <button type="button" style={ghostBtnStyle} onClick={() => setConfirming(false)} disabled={busy}>
                    再想想
                  </button>
                  <button type="button" style={dangerBtnStyle} onClick={() => void doErase()} disabled={busy}>
                    {busy ? "擦除中…" : "确认永久擦除"}
                  </button>
                </div>
              </div>
            ) : (
              <div style={footerRowStyle}>
                <button type="button" style={ghostBtnStyle} onClick={onClose} disabled={busy}>
                  取消
                </button>
                <button
                  type="button"
                  style={dangerBtnStyle}
                  onClick={() => setConfirming(true)}
                  disabled={busy || !anySelected}
                >
                  擦除所选…
                </button>
              </div>
            )}
          </>
        )}
      </div>
    </div>
  );
}

function CountRow({ label, value }: { label: string; value: number }) {
  return (
    <div style={{ display: "flex", justifyContent: "space-between", fontSize: 12, padding: "2px 0" }}>
      <span style={{ color: "#9aa0ad" }}>{label}</span>
      <span style={{ color: value > 0 ? "#f2d98f" : "#7e8493", fontWeight: value > 0 ? 600 : 400 }}>{value}</span>
    </div>
  );
}

export default GovernancePanel;
