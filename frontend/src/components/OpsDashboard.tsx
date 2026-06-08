/* 文件说明：运营看板（成本/单位经济 + 假门转化漏斗），面向运营/dev，接进 App.tsx。
   - 消费后端 GET /api/ops/cost-dashboard（成本/MAU 代理/单位生命态）与 GET /api/ops/leads-funnel（按 kind 计数 + 唯一访客）。
   - 两端点均套 opsTokenGuard：未配 token 放行，配了需 X-Ops-Token；本组件留一个 ops token 输入框调 setOpsToken（参照 GovernancePanel）。
   - days 选择（7/30/0=全量）+ 刷新按钮；两块各自 try/catch + loading/error，错误态提示可能需填 X-Ops-Token。
   自包含内联样式（与 GovernancePanel.tsx 同一基元集），仅 import api.ts/types.ts，不依赖其它并行组件。*/

import { useCallback, useEffect, useMemo, useState } from "react";
import { APIError, fetchCostDashboard, fetchLeadsFunnel, getOpsToken, setOpsToken } from "../session/api";
import type { CostDashboardData, LeadsFunnelData, ProviderCost } from "../session/types";

// ============ 共享样式基元（自包含，与 GovernancePanel.tsx 一致） ============

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

const wideDialogStyle: React.CSSProperties = {
  width: "min(820px, 100%)",
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
const selectStyle: React.CSSProperties = { ...inputStyle };

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

const toastErrStyle: React.CSSProperties = {
  marginTop: 12,
  padding: "8px 10px",
  borderRadius: 6,
  background: "rgba(196, 84, 74, 0.16)",
  border: "1px solid rgba(196, 84, 74, 0.5)",
  color: "#f0b0a6",
  fontSize: 12,
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
  display: "flex",
  alignItems: "center",
  justifyContent: "space-between",
};

const metricGridStyle: React.CSSProperties = {
  display: "grid",
  gridTemplateColumns: "repeat(auto-fit, minmax(150px, 1fr))",
  gap: 8,
  margin: "6px 0 12px",
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
  letterSpacing: 0.3,
  textTransform: "uppercase",
};
const metricValueStyle: React.CSSProperties = { color: "#f2d98f", fontSize: 20, fontWeight: 700, marginTop: 4 };

const tableStyle: React.CSSProperties = {
  width: "100%",
  borderCollapse: "collapse",
  fontSize: 12,
};
const thStyle: React.CSSProperties = {
  textAlign: "left",
  color: "#cdb98a",
  fontSize: 11,
  letterSpacing: 0.4,
  textTransform: "uppercase",
  padding: "6px 8px",
  borderBottom: "1px solid rgba(255,255,255,0.1)",
};
const tdStyle: React.CSSProperties = {
  padding: "6px 8px",
  color: "#c2c7d0",
  borderBottom: "1px solid rgba(255,255,255,0.05)",
};
const tdNumStyle: React.CSSProperties = { ...tdStyle, textAlign: "right", fontVariantNumeric: "tabular-nums" };

const emptyStyle: React.CSSProperties = { color: "#9aa0ad", fontSize: 12, padding: "6px 2px" };

// errText 把错误归一为可展示文案，鉴权类错误（403/401）额外提示填 X-Ops-Token。
function errText(err: unknown): string {
  if (err instanceof APIError) {
    const parts = [err.message];
    if (typeof err.status === "number") parts.push(`(HTTP ${err.status})`);
    if (err.reason) parts.push(`原因：${err.reason}`);
    if (err.status === 401 || err.status === 403) parts.push("— 可能需填 X-Ops-Token");
    return parts.join(" ");
  }
  return err instanceof Error ? err.message : String(err);
}

// fmtUSD/fmtPct/fmtInt 是展示格式化。成本到 4 位小数，率到 1 位百分比，整数加千分位。
function fmtUSD(n: number): string {
  return `$${(Number.isFinite(n) ? n : 0).toFixed(4)}`;
}
function fmtPct(n: number): string {
  return `${((Number.isFinite(n) ? n : 0) * 100).toFixed(1)}%`;
}
function fmtInt(n: number): string {
  return (Number.isFinite(n) ? n : 0).toLocaleString("en-US");
}

const DAYS_OPTIONS: { value: number; label: string }[] = [
  { value: 7, label: "最近 7 天" },
  { value: 30, label: "最近 30 天" },
  { value: 0, label: "全量" },
];

export type OpsDashboardProps = {
  onClose: () => void;
};

// OpsDashboard 是面向运营/dev 的成本与转化看板。两块数据独立加载，互不阻断。
export function OpsDashboard({ onClose }: OpsDashboardProps) {
  const [opsTokenInput, setOpsTokenInput] = useState<string>(getOpsToken());
  const [days, setDays] = useState<number>(30);

  const [cost, setCost] = useState<CostDashboardData | null>(null);
  const [costLoading, setCostLoading] = useState(false);
  const [costErr, setCostErr] = useState("");

  const [funnel, setFunnel] = useState<LeadsFunnelData | null>(null);
  const [funnelLoading, setFunnelLoading] = useState(false);
  const [funnelErr, setFunnelErr] = useState("");

  const loadCost = useCallback(async () => {
    setCostLoading(true);
    setCostErr("");
    try {
      const data = await fetchCostDashboard(days);
      setCost(data);
    } catch (e) {
      setCostErr(`读取成本看板失败：${errText(e)}`);
    } finally {
      setCostLoading(false);
    }
  }, [days]);

  const loadFunnel = useCallback(async () => {
    setFunnelLoading(true);
    setFunnelErr("");
    try {
      const data = await fetchLeadsFunnel();
      setFunnel(data);
    } catch (e) {
      setFunnelErr(`读取转化漏斗失败：${errText(e)}`);
    } finally {
      setFunnelLoading(false);
    }
  }, []);

  const loadAll = useCallback(() => {
    void loadCost();
    void loadFunnel();
  }, [loadCost, loadFunnel]);

  // 首次挂载 + days 变化时自动刷新（loadAll 依赖 loadCost，loadCost 依赖 days）。
  useEffect(() => {
    loadAll();
  }, [loadAll]);

  const applyOpsToken = useCallback(() => {
    setOpsToken(opsTokenInput.trim());
    // 带上新令牌重拉两块。
    loadAll();
  }, [loadAll, opsTokenInput]);

  // 按 provider 成本降序排列，便于一眼看大头。
  const providerRows: ProviderCost[] = useMemo(() => {
    if (!cost?.by_provider) return [];
    return Object.values(cost.by_provider).sort((a, b) => b.cost_usd - a.cost_usd);
  }, [cost]);

  // 单位生命态按数量降序。
  const lifeStateRows: [string, number][] = useMemo(() => {
    if (!cost?.units_by_life_state) return [];
    return Object.entries(cost.units_by_life_state).sort((a, b) => b[1] - a[1]);
  }, [cost]);

  // 漏斗 kind 按计数降序。
  const funnelRows: [string, number][] = useMemo(() => {
    if (!funnel?.by_kind) return [];
    return Object.entries(funnel.by_kind).sort((a, b) => b[1] - a[1]);
  }, [funnel]);

  return (
    <div style={overlayStyle} role="dialog" aria-label="运营看板" aria-modal>
      <div style={wideDialogStyle}>
        <div style={headerStyle}>
          <div>
            <div style={brandStyle}>运营看板</div>
            <div style={subStyle}>
              运营/开发可见。LLM 成本与单位经济 + 假门转化漏斗只读聚合（ops 端点，缺头放行，正式态需 X-Ops-Token）。
            </div>
          </div>
          <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭运营看板">
            ×
          </button>
        </div>

        {/* 控制条：ops token + days 选择 + 刷新 */}
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
          <select
            style={{ ...selectStyle, flex: "0 0 auto", width: "auto" }}
            value={days}
            onChange={(e) => setDays(Number(e.target.value))}
            aria-label="时间窗口"
          >
            {DAYS_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
          <button
            type="button"
            style={ghostBtnStyle}
            onClick={loadAll}
            disabled={costLoading || funnelLoading}
          >
            {costLoading || funnelLoading ? "加载中…" : "刷新"}
          </button>
        </div>

        {/* ============ 成本卡片 ============ */}
        <div style={sectionCardStyle}>
          <div style={cardTitleStyle}>
            <span>成本与单位经济</span>
            <span style={subStyle}>
              {cost?.generated_at ? `生成于 ${cost.generated_at}` : costLoading ? "加载中…" : ""}
            </span>
          </div>

          {costErr ? <div style={toastErrStyle}>{costErr}</div> : null}

          {cost ? (
            <>
              <div style={metricGridStyle}>
                <div style={metricBoxStyle}>
                  <div style={metricLabelStyle}>总成本（USD）</div>
                  <div style={metricValueStyle}>{fmtUSD(cost.total_cost_usd)}</div>
                </div>
                <div style={metricBoxStyle}>
                  <div style={metricLabelStyle}>总交互数</div>
                  <div style={metricValueStyle}>{fmtInt(cost.total_interactions)}</div>
                </div>
                <div style={metricBoxStyle}>
                  <div style={metricLabelStyle}>Fallback 率</div>
                  <div style={metricValueStyle}>{fmtPct(cost.fallback_rate)}</div>
                </div>
                <div style={metricBoxStyle}>
                  <div style={metricLabelStyle}>每会话成本（USD）</div>
                  <div style={metricValueStyle}>{fmtUSD(cost.cost_per_session_usd)}</div>
                </div>
                <div style={metricBoxStyle}>
                  <div style={metricLabelStyle}>活跃会话（MAU 代理）</div>
                  <div style={metricValueStyle}>{fmtInt(cost.distinct_sessions)}</div>
                </div>
                <div style={metricBoxStyle}>
                  <div style={metricLabelStyle}>总 Token</div>
                  <div style={metricValueStyle}>{fmtInt(cost.total_tokens)}</div>
                </div>
                <div style={metricBoxStyle}>
                  <div style={metricLabelStyle}>Fallback 次数</div>
                  <div style={metricValueStyle}>{fmtInt(cost.fallback_count)}</div>
                </div>
                <div style={metricBoxStyle}>
                  <div style={metricLabelStyle}>单位总数</div>
                  <div style={metricValueStyle}>{fmtInt(cost.units_total)}</div>
                </div>
              </div>

              {/* by_provider 表 */}
              <div style={{ ...metricLabelStyle, marginBottom: 4 }}>按 Provider 拆分</div>
              {providerRows.length === 0 ? (
                <div style={emptyStyle}>窗口内无 LLM 交互。</div>
              ) : (
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={thStyle}>Provider</th>
                      <th style={{ ...thStyle, textAlign: "right" }}>调用</th>
                      <th style={{ ...thStyle, textAlign: "right" }}>成本</th>
                      <th style={{ ...thStyle, textAlign: "right" }}>Token</th>
                      <th style={{ ...thStyle, textAlign: "right" }}>Fallback</th>
                    </tr>
                  </thead>
                  <tbody>
                    {providerRows.map((p) => (
                      <tr key={p.provider || "(unknown)"}>
                        <td style={tdStyle}>{p.provider || "(未知)"}</td>
                        <td style={tdNumStyle}>{fmtInt(p.calls)}</td>
                        <td style={tdNumStyle}>{fmtUSD(p.cost_usd)}</td>
                        <td style={tdNumStyle}>{fmtInt(p.total_tokens)}</td>
                        <td style={tdNumStyle}>{fmtInt(p.fallback_hits)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}

              {/* units_by_life_state 表 */}
              <div style={{ ...metricLabelStyle, margin: "14px 0 4px" }}>按生命态拆分（单位经济）</div>
              {lifeStateRows.length === 0 ? (
                <div style={emptyStyle}>无单位数据。</div>
              ) : (
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={thStyle}>生命态</th>
                      <th style={{ ...thStyle, textAlign: "right" }}>数量</th>
                    </tr>
                  </thead>
                  <tbody>
                    {lifeStateRows.map(([state, count]) => (
                      <tr key={state || "(unknown)"}>
                        <td style={tdStyle}>{state || "(未知)"}</td>
                        <td style={tdNumStyle}>{fmtInt(count)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </>
          ) : costErr ? null : (
            <div style={emptyStyle}>暂无数据。</div>
          )}
        </div>

        {/* ============ 漏斗卡片 ============ */}
        <div style={sectionCardStyle}>
          <div style={cardTitleStyle}>
            <span>假门转化漏斗</span>
            <span style={subStyle}>{funnelLoading ? "加载中…" : ""}</span>
          </div>

          {funnelErr ? <div style={toastErrStyle}>{funnelErr}</div> : null}

          {funnel ? (
            <>
              <div style={metricGridStyle}>
                <div style={metricBoxStyle}>
                  <div style={metricLabelStyle}>总事件数</div>
                  <div style={metricValueStyle}>{fmtInt(funnel.total)}</div>
                </div>
                <div style={metricBoxStyle}>
                  <div style={metricLabelStyle}>唯一访客</div>
                  <div style={metricValueStyle}>{fmtInt(funnel.unique_visitors)}</div>
                </div>
              </div>

              <div style={{ ...metricLabelStyle, marginBottom: 4 }}>按 Kind 拆分</div>
              {funnelRows.length === 0 ? (
                <div style={emptyStyle}>暂无留资事件。</div>
              ) : (
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={thStyle}>Kind</th>
                      <th style={{ ...thStyle, textAlign: "right" }}>计数</th>
                    </tr>
                  </thead>
                  <tbody>
                    {funnelRows.map(([kind, count]) => (
                      <tr key={kind || "(unknown)"}>
                        <td style={tdStyle}>{kind || "(未知)"}</td>
                        <td style={tdNumStyle}>{fmtInt(count)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </>
          ) : funnelErr ? null : (
            <div style={emptyStyle}>暂无数据。</div>
          )}
        </div>
      </div>
    </div>
  );
}

export default OpsDashboard;
