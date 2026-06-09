/* 文件说明：GM 后台「世界配置」面板。
   - ListWorldsDetail：展示活跃世界 + region + 人口（后端 worlds-detail 待落地时回退到 /api/worlds 基本列表）。
   - 设 region 威胁度：POST /api/admin/worlds/:worldId/regions/:regionId/threat（待后端落地）。
   - 触发村庄播种：POST /api/admin/worlds/:worldId/seed-village（待后端落地）。
   每个动作各自 try/catch、独立 toast，互不阻断。 */

import { useCallback, useEffect, useState } from "react";
import {
  errText,
  listWorlds,
  listWorldsDetail,
  seedVillage,
  setRegionThreat,
  type AdminWorld,
  type AdminWorldDetail,
} from "./adminApi";

// worldsToDetail 把基本世界列表（GO 大写键名）升格为 detail 形（无 region、人口未知），
// 供 worlds-detail 未接线时统一渲染。population 用 -1 表示未知（detail 端点才有真值）。
function worldsToDetail(worlds: AdminWorld[]): AdminWorldDetail[] {
  return worlds.map((w) => ({
    id: w.ID,
    name: w.Name,
    status: w.Status,
    tick: w.Tick,
    max_population: w.MaxPopulation,
    population: -1,
    regions: null,
  }));
}

export function WorldConfigPanel(): JSX.Element {
  const [worlds, setWorlds] = useState<AdminWorldDetail[]>([]);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState("");
  const [ok, setOk] = useState("");
  // detailAvailable=false 表示后端 worlds-detail 未落地（已回退基本列表）。
  const [detailAvailable, setDetailAvailable] = useState(true);

  // 设威胁度表单（按世界/region 选择）。
  const [threatWorldId, setThreatWorldId] = useState("");
  const [threatRegionId, setThreatRegionId] = useState("");
  const [threatLevel, setThreatLevel] = useState(50);
  const [threatBusy, setThreatBusy] = useState(false);

  // 村庄播种表单（SeedWorldVillage 需非空 sessionID；factionID 缺省取 sessionID）。
  const [seedWorldId, setSeedWorldId] = useState("");
  const [seedSessionId, setSeedSessionId] = useState("");
  const [seedFactionId, setSeedFactionId] = useState("");
  const [seedBusy, setSeedBusy] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      const detail = await listWorldsDetail();
      if (detail.length > 0) {
        setWorlds(detail);
        setDetailAvailable(true);
        return;
      }
      // detail 端点返回空（可能未落地或暂无世界）→ 退回基本列表。
      const basic = await listWorlds();
      setWorlds(worldsToDetail(basic));
      setDetailAvailable(false);
    } catch {
      // worlds-detail 未落地（404）→ 回退基本世界列表（已落地的 /api/worlds）。
      try {
        const basic = await listWorlds();
        setWorlds(worldsToDetail(basic));
        setDetailAvailable(false);
      } catch (e2) {
        setErr(`读取世界列表失败：${errText(e2)}`);
      }
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const submitThreat = useCallback(async () => {
    if (!threatWorldId.trim() || !threatRegionId.trim()) {
      setErr("设威胁度需世界 ID 与 region ID。");
      setOk("");
      return;
    }
    setThreatBusy(true);
    setErr("");
    setOk("");
    try {
      const newLevel = await setRegionThreat(threatWorldId.trim(), threatRegionId.trim(), threatLevel);
      setOk(`已置位 ${threatWorldId.trim()}/${threatRegionId.trim()} 威胁等级=${newLevel}。`);
    } catch (e) {
      setErr(`设威胁度失败：${errText(e)}`);
    } finally {
      setThreatBusy(false);
    }
  }, [threatLevel, threatRegionId, threatWorldId]);

  const submitSeed = useCallback(async () => {
    if (!seedWorldId.trim() || !seedSessionId.trim()) {
      setErr("村庄播种需世界 ID 与会话 ID（session_id）。");
      setOk("");
      return;
    }
    setSeedBusy(true);
    setErr("");
    setOk("");
    try {
      const res = await seedVillage(seedWorldId.trim(), seedSessionId.trim(), seedFactionId.trim() || undefined);
      setOk(`已为世界 ${seedWorldId.trim()} 播种村庄：新增 ${res.seeded} 名村民。`);
    } catch (e) {
      setErr(`村庄播种失败：${errText(e)}`);
    } finally {
      setSeedBusy(false);
    }
  }, [seedFactionId, seedSessionId, seedWorldId]);

  return (
    <>
      {/* 世界 + region + 人口列表 */}
      <div className="adm-card">
        <div className="adm-card-title">
          <span>世界 / Region / 人口</span>
          <button type="button" className="adm-btn" onClick={() => void load()} disabled={loading}>
            {loading ? "加载中…" : "刷新"}
          </button>
        </div>
        <p className="adm-card-sub">
          活跃世界的 region 与人口运营视图。
          {detailAvailable ? "" : "（worlds-detail 端点待后端接线，当前仅展示基本世界列表，无 region/人口）"}
        </p>
        {err ? <div className="adm-toast adm-toast-err">{err}</div> : null}
        {ok ? <div className="adm-toast adm-toast-ok">{ok}</div> : null}

        {worlds.length === 0 && !loading ? (
          <div className="adm-empty">暂无活跃世界。</div>
        ) : (
          <table className="adm-table" style={{ marginTop: 8 }}>
            <thead>
              <tr>
                <th>世界</th>
                <th>状态</th>
                <th className="adm-num">Tick</th>
                <th className="adm-num">人口</th>
                <th>Regions（人口 · 威胁度）</th>
              </tr>
            </thead>
            <tbody>
              {worlds.map((d) => (
                <tr key={d.id || d.name}>
                  <td>
                    <div style={{ color: "#f0ead8" }}>{d.name || "(未命名)"}</div>
                    <div style={{ color: "#9aa0ad", fontSize: 11, fontFamily: "ui-monospace, monospace" }}>{d.id}</div>
                  </td>
                  <td>{d.status}</td>
                  <td className="adm-num">{d.tick}</td>
                  <td className="adm-num">
                    {d.population >= 0 ? d.population : d.max_population ? `≤${d.max_population}` : "—"}
                  </td>
                  <td>
                    {d.regions && d.regions.length > 0 ? (
                      <div style={{ display: "flex", flexDirection: "column", gap: 2 }}>
                        {d.regions.map((r) => (
                          <span key={r.id} style={{ fontSize: 11 }}>
                            {r.id}
                            {r.activity_tier ? `（${r.activity_tier}）` : ""} · 威胁 {r.threat_level}
                          </span>
                        ))}
                      </div>
                    ) : (
                      <span style={{ color: "#9aa0ad", fontSize: 11 }}>—</span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {/* 设 region 威胁度 */}
      <div className="adm-card">
        <div className="adm-card-title">设 Region 威胁度</div>
        <p className="adm-card-sub">
          把某世界某 region 的威胁等级绝对置位（越高越易撞 PvE 威胁，供人工拉高某地威胁度做活动/演练）。
          后端 session.SetRegionThreatLevel 域层已就绪，HTTP 路由待接线。
        </p>
        <div className="adm-row">
          <div style={{ flex: "1 1 200px" }}>
            <label className="adm-label">世界 ID</label>
            <input
              className="adm-input"
              value={threatWorldId}
              onChange={(e) => setThreatWorldId(e.target.value)}
              placeholder="world_id"
            />
          </div>
          <div style={{ flex: "1 1 200px" }}>
            <label className="adm-label">Region ID</label>
            <input
              className="adm-input"
              value={threatRegionId}
              onChange={(e) => setThreatRegionId(e.target.value)}
              placeholder="region_id"
            />
          </div>
          <div style={{ flex: "0 0 140px" }}>
            <label className="adm-label">威胁等级（绝对值）</label>
            <input
              className="adm-input"
              type="number"
              min={0}
              value={threatLevel}
              onChange={(e) => setThreatLevel(Number(e.target.value))}
            />
          </div>
        </div>
        <div style={{ marginTop: 12 }}>
          <button type="button" className="adm-btn adm-btn-primary" onClick={() => void submitThreat()} disabled={threatBusy}>
            {threatBusy ? "设置中…" : "设威胁度"}
          </button>
        </div>
      </div>

      {/* 触发村庄播种 */}
      <div className="adm-card">
        <div className="adm-card-title">触发村庄播种</div>
        <p className="adm-card-sub">
          确定性地为某世界/会话织一张 20 人出生关系网（建人/落库/入世界/织关系/写记忆全套）。
          注意：同一局应只播种一次（会新建行，不幂等去重）。后端 session.SeedWorldVillage 域层已就绪，HTTP 路由待接线。
        </p>
        <div className="adm-row">
          <div style={{ flex: "1 1 160px" }}>
            <label className="adm-label">世界 ID</label>
            <input
              className="adm-input"
              value={seedWorldId}
              onChange={(e) => setSeedWorldId(e.target.value)}
              placeholder="world_id"
            />
          </div>
          <div style={{ flex: "1 1 160px" }}>
            <label className="adm-label">会话 ID（session_id）</label>
            <input
              className="adm-input"
              value={seedSessionId}
              onChange={(e) => setSeedSessionId(e.target.value)}
              placeholder="session_id"
            />
          </div>
          <div style={{ flex: "0 0 150px" }}>
            <label className="adm-label">阵营 ID（可选）</label>
            <input
              className="adm-input"
              value={seedFactionId}
              onChange={(e) => setSeedFactionId(e.target.value)}
              placeholder="缺省取 session_id"
            />
          </div>
        </div>
        <div style={{ marginTop: 12 }}>
          <button type="button" className="adm-btn adm-btn-primary" onClick={() => void submitSeed()} disabled={seedBusy}>
            {seedBusy ? "播种中…" : "播种村庄"}
          </button>
        </div>
      </div>
    </>
  );
}

export default WorldConfigPanel;
