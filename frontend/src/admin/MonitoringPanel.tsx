/* 文件说明：GM 后台「监控」面板。复用既有 ops 只读端点：
   - 北极星（GET /api/ops/north-star）：惊喜命中率/OOC 率（核心乐趣）+ 收件箱处理率/分享/付费/回访。
   - 产品漏斗（GET /api/ops/product-funnel）：AARRR 各阶段 + 事件名计数 + 唯一会话。
   - 成本（GET /api/ops/cost-dashboard）：LLM 总成本/每会话成本/fallback 率/按 provider 拆分。
   - 零和审计（GET /api/ops/worlds/:worldId/arbitration-audit）：付费/非付费组胜率，判 P2W 红线。
   days 选择（7/30/0=全量）+ 刷新；各块独立 try/catch、互不阻断。 */

import { useCallback, useEffect, useMemo, useState } from "react";
import {
  errText,
  fetchArbitrationAudit,
  fetchCostDashboard,
  fetchNorthStar,
  fetchProductFunnel,
  type ArbitrationAuditReport,
  type CostDashboardData,
  type NorthStarReport,
  type ProductFunnelReport,
  type ProviderCost,
} from "./adminApi";

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

export function MonitoringPanel(): JSX.Element {
  const [days, setDays] = useState(30);

  const [northStar, setNorthStar] = useState<NorthStarReport | null>(null);
  const [northStarErr, setNorthStarErr] = useState("");

  const [productFunnel, setProductFunnel] = useState<ProductFunnelReport | null>(null);
  const [productFunnelErr, setProductFunnelErr] = useState("");

  const [cost, setCost] = useState<CostDashboardData | null>(null);
  const [costErr, setCostErr] = useState("");

  const [loading, setLoading] = useState(false);

  // 零和审计（独立输入，不随 days 自动刷新）。
  const [auditWorldId, setAuditWorldId] = useState("");
  const [turnStart, setTurnStart] = useState(0);
  const [turnEnd, setTurnEnd] = useState(100);
  const [audit, setAudit] = useState<ArbitrationAuditReport | null>(null);
  const [auditBusy, setAuditBusy] = useState(false);
  const [auditErr, setAuditErr] = useState("");

  const loadAll = useCallback(async () => {
    setLoading(true);
    setNorthStarErr("");
    setProductFunnelErr("");
    setCostErr("");
    await Promise.all([
      fetchNorthStar(days)
        .then(setNorthStar)
        .catch((e) => setNorthStarErr(`读取北极星失败：${errText(e)}`)),
      fetchProductFunnel(days)
        .then(setProductFunnel)
        .catch((e) => setProductFunnelErr(`读取产品漏斗失败：${errText(e)}`)),
      fetchCostDashboard(days)
        .then(setCost)
        .catch((e) => setCostErr(`读取成本看板失败：${errText(e)}`)),
    ]);
    setLoading(false);
  }, [days]);

  useEffect(() => {
    void loadAll();
  }, [loadAll]);

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
  }, [auditWorldId, turnEnd, turnStart]);

  const providerRows: ProviderCost[] = useMemo(() => {
    if (!cost?.by_provider) return [];
    return Object.values(cost.by_provider).sort((a, b) => b.cost_usd - a.cost_usd);
  }, [cost]);

  const productStageRows: [string, number][] = useMemo(() => {
    if (!productFunnel?.by_stage) return [];
    return Object.entries(productFunnel.by_stage).sort((a, b) => b[1] - a[1]);
  }, [productFunnel]);

  return (
    <>
      {/* 控制条 */}
      <div className="adm-card">
        <div className="adm-card-title">监控控制</div>
        <div className="adm-row">
          <select className="adm-select" style={{ width: "auto" }} value={days} onChange={(e) => setDays(Number(e.target.value))} aria-label="时间窗口">
            {DAYS_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
          <button type="button" className="adm-btn" onClick={() => void loadAll()} disabled={loading}>
            {loading ? "加载中…" : "刷新全部"}
          </button>
        </div>
      </div>

      {/* 北极星 */}
      <div className="adm-card">
        <div className="adm-card-title">
          <span>北极星指标</span>
          <span className="adm-card-sub" style={{ margin: 0 }}>
            {northStar?.generated_at ? `生成于 ${northStar.generated_at}` : ""}
          </span>
        </div>
        {northStarErr ? <div className="adm-toast adm-toast-err">{northStarErr}</div> : null}
        {northStar ? (
          <div className="adm-metric-grid">
            <div className="adm-metric-box adm-metric-hot">
              <div className="adm-metric-label">惊喜命中率（核心乐趣）</div>
              <div className="adm-metric-value">{fmtPct(northStar.surprise_hit_rate)}</div>
            </div>
            <div className="adm-metric-box adm-metric-hot">
              <div className="adm-metric-label">OOC 率（失格）</div>
              <div className="adm-metric-value adm-metric-warn">{fmtPct(northStar.ooc_rate)}</div>
            </div>
            <div className="adm-metric-box">
              <div className="adm-metric-label">收件箱处理率</div>
              <div className="adm-metric-value">{fmtPct(northStar.inbox_process_rate)}</div>
            </div>
            <div className="adm-metric-box">
              <div className="adm-metric-label">分享数</div>
              <div className="adm-metric-value">{fmtInt(northStar.share_initiated)}</div>
            </div>
            <div className="adm-metric-box">
              <div className="adm-metric-label">付费数</div>
              <div className="adm-metric-value">{fmtInt(northStar.purchases)}</div>
            </div>
            <div className="adm-metric-box">
              <div className="adm-metric-label">回访数</div>
              <div className="adm-metric-value">{fmtInt(northStar.return_visits)}</div>
            </div>
            <div className="adm-metric-box">
              <div className="adm-metric-label">命运待决 / 已处理</div>
              <div className="adm-metric-value">
                {fmtInt(northStar.decision_pending)} / {fmtInt(northStar.decision_resolved)}
              </div>
            </div>
          </div>
        ) : northStarErr ? null : (
          <div className="adm-empty">暂无数据。</div>
        )}
      </div>

      {/* 成本 */}
      <div className="adm-card">
        <div className="adm-card-title">
          <span>成本与单位经济</span>
          <span className="adm-card-sub" style={{ margin: 0 }}>
            {cost?.generated_at ? `生成于 ${cost.generated_at}` : ""}
          </span>
        </div>
        {costErr ? <div className="adm-toast adm-toast-err">{costErr}</div> : null}
        {cost ? (
          <>
            <div className="adm-metric-grid">
              <div className="adm-metric-box">
                <div className="adm-metric-label">总成本（USD）</div>
                <div className="adm-metric-value">{fmtUSD(cost.total_cost_usd)}</div>
              </div>
              <div className="adm-metric-box">
                <div className="adm-metric-label">每会话成本</div>
                <div className="adm-metric-value">{fmtUSD(cost.cost_per_session_usd)}</div>
              </div>
              <div className="adm-metric-box">
                <div className="adm-metric-label">Fallback 率</div>
                <div className="adm-metric-value">{fmtPct(cost.fallback_rate)}</div>
              </div>
              <div className="adm-metric-box">
                <div className="adm-metric-label">活跃会话（MAU 代理）</div>
                <div className="adm-metric-value">{fmtInt(cost.distinct_sessions)}</div>
              </div>
            </div>
            {providerRows.length > 0 ? (
              <table className="adm-table">
                <thead>
                  <tr>
                    <th>Provider</th>
                    <th className="adm-num">调用</th>
                    <th className="adm-num">成本</th>
                    <th className="adm-num">Token</th>
                    <th className="adm-num">Fallback</th>
                  </tr>
                </thead>
                <tbody>
                  {providerRows.map((p) => (
                    <tr key={p.provider || "(unknown)"}>
                      <td>{p.provider || "(未知)"}</td>
                      <td className="adm-num">{fmtInt(p.calls)}</td>
                      <td className="adm-num">{fmtUSD(p.cost_usd)}</td>
                      <td className="adm-num">{fmtInt(p.total_tokens)}</td>
                      <td className="adm-num">{fmtInt(p.fallback_hits)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ) : null}
          </>
        ) : costErr ? null : (
          <div className="adm-empty">暂无数据。</div>
        )}
      </div>

      {/* 产品漏斗 */}
      <div className="adm-card">
        <div className="adm-card-title">产品漏斗（AARRR）</div>
        {productFunnelErr ? <div className="adm-toast adm-toast-err">{productFunnelErr}</div> : null}
        {productFunnel ? (
          <>
            <div className="adm-metric-grid">
              <div className="adm-metric-box">
                <div className="adm-metric-label">唯一会话</div>
                <div className="adm-metric-value">{fmtInt(productFunnel.distinct_sessions)}</div>
              </div>
            </div>
            {productStageRows.length > 0 ? (
              <table className="adm-table">
                <thead>
                  <tr>
                    <th>阶段</th>
                    <th className="adm-num">计数</th>
                  </tr>
                </thead>
                <tbody>
                  {productStageRows.map(([stage, count]) => (
                    <tr key={stage || "(unknown)"}>
                      <td>{stage || "(未知)"}</td>
                      <td className="adm-num">{fmtInt(count)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ) : (
              <div className="adm-empty">窗口内无阶段事件。</div>
            )}
          </>
        ) : productFunnelErr ? null : (
          <div className="adm-empty">暂无数据。</div>
        )}
      </div>

      {/* 零和审计 */}
      <div className="adm-card">
        <div className="adm-card-title">零和监控审计（反 P2W）</div>
        <p className="adm-card-sub">
          扫某世界某回合区间的仲裁结局，按付费态分组算胜率。付费组胜率 &gt; {fmtPct(audit?.redline_rate ?? 0.6)} 判红线——
          观测付费有没有不公平地赢（付费态绝不进 Score）。
        </p>
        <label className="adm-label">世界 ID</label>
        <input className="adm-input" value={auditWorldId} onChange={(e) => setAuditWorldId(e.target.value)} placeholder="world_id" />
        <div className="adm-row">
          <div style={{ flex: "1 1 120px" }}>
            <label className="adm-label">起始回合</label>
            <input className="adm-input" type="number" value={turnStart} onChange={(e) => setTurnStart(Number(e.target.value))} />
          </div>
          <div style={{ flex: "1 1 120px" }}>
            <label className="adm-label">结束回合</label>
            <input className="adm-input" type="number" value={turnEnd} onChange={(e) => setTurnEnd(Number(e.target.value))} />
          </div>
        </div>
        <div style={{ marginTop: 12 }}>
          <button type="button" className="adm-btn" onClick={() => void runAudit()} disabled={auditBusy}>
            {auditBusy ? "审计中…" : "运行审计"}
          </button>
        </div>
        {auditErr ? <div className="adm-toast adm-toast-err">{auditErr}</div> : null}
        {audit ? (
          <>
            <div className="adm-metric-grid" style={{ marginTop: 10 }}>
              <div className="adm-metric-box">
                <div className="adm-metric-label">付费组胜率</div>
                <div className={`adm-metric-value ${audit.issue_detected ? "adm-metric-warn" : ""}`}>
                  {fmtPct(audit.paid.win_rate)}
                </div>
              </div>
              <div className="adm-metric-box">
                <div className="adm-metric-label">非付费组胜率</div>
                <div className="adm-metric-value">{fmtPct(audit.non_paid.win_rate)}</div>
              </div>
              <div className="adm-metric-box">
                <div className="adm-metric-label">付费组样本</div>
                <div className="adm-metric-value">{fmtInt(audit.paid.total)}</div>
              </div>
            </div>
            <div className={`adm-toast ${audit.issue_detected ? "adm-toast-err" : "adm-toast-ok"}`}>
              {audit.issue_detected ? "⚠ 红线触发：" : "✓ "}
              {audit.note}
            </div>
          </>
        ) : null}
      </div>
    </>
  );
}

export default MonitoringPanel;
