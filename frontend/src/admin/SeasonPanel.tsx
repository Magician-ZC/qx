/* 文件说明：GM 后台「赛季」面板。
   - 建季：POST /api/ops/seasons（已落地，建世界 + 落 seasons）。
   - 收尾：POST /api/ops/seasons/:id/finalize（已落地，存活角色回流名人堂 + 世界封存）。
   - 列季：GET /api/ops/seasons（待后端落地；未落地时退回展示本会话内刚创建的赛季）。
   建季后自动把新赛季 id 填入收尾框、并并入列表，便于一站式运营。 */

import { useCallback, useEffect, useState } from "react";
import {
  createSeason,
  errText,
  finalizeSeason,
  listSeasons,
  type FinalizeResult,
  type Season,
} from "./adminApi";

export function SeasonPanel(): JSX.Element {
  // 列季（后端列表端点 + 本会话内新建合并去重）。
  const [seasons, setSeasons] = useState<Season[]>([]);
  const [listErr, setListErr] = useState("");
  const [listLoading, setListLoading] = useState(false);

  // 建季表单。
  const [name, setName] = useState("");
  const [themeId, setThemeId] = useState("");
  const [maxPop, setMaxPop] = useState<number | "">("");
  const [createBusy, setCreateBusy] = useState(false);
  const [createErr, setCreateErr] = useState("");
  const [createOk, setCreateOk] = useState("");

  // 收尾表单。
  const [finalizeId, setFinalizeId] = useState("");
  const [finalizeBusy, setFinalizeBusy] = useState(false);
  const [finalizeErr, setFinalizeErr] = useState("");
  const [finalizeRes, setFinalizeRes] = useState<FinalizeResult | null>(null);

  // mergeSeason 把一条赛季并入列表（按 id 去重、新者置顶）。
  const mergeSeason = useCallback((s: Season) => {
    setSeasons((prev) => {
      const rest = prev.filter((p) => p.id !== s.id);
      return [s, ...rest];
    });
  }, []);

  const loadList = useCallback(async () => {
    setListLoading(true);
    setListErr("");
    try {
      const data = await listSeasons();
      if (data.length > 0) {
        setSeasons(data);
      }
    } catch (e) {
      // 列表端点未落地（404）不致命：保留本会话已建赛季，仅提示。
      setListErr(`赛季列表端点不可用：${errText(e)}（后端 GET /api/ops/seasons 待接线，仅展示本次新建）`);
    } finally {
      setListLoading(false);
    }
  }, []);

  useEffect(() => {
    void loadList();
  }, [loadList]);

  const submitCreate = useCallback(async () => {
    if (!name.trim()) {
      setCreateErr("赛季名必填。");
      setCreateOk("");
      return;
    }
    setCreateBusy(true);
    setCreateErr("");
    setCreateOk("");
    try {
      const s = await createSeason({
        name: name.trim(),
        content_theme_id: themeId.trim() || undefined,
        max_population: typeof maxPop === "number" ? maxPop : undefined,
      });
      mergeSeason(s);
      setFinalizeId(s.id);
      setCreateOk(`已创建赛季「${s.name}」：id=${s.id.slice(0, 8)}…，世界=${s.world_id.slice(0, 8)}…`);
    } catch (e) {
      setCreateErr(`创建赛季失败：${errText(e)}`);
    } finally {
      setCreateBusy(false);
    }
  }, [maxPop, mergeSeason, name, themeId]);

  const submitFinalize = useCallback(async () => {
    if (!finalizeId.trim()) {
      setFinalizeErr("收尾需赛季 ID。");
      return;
    }
    setFinalizeBusy(true);
    setFinalizeErr("");
    try {
      const r = await finalizeSeason(finalizeId.trim());
      setFinalizeRes(r);
      // 收尾后把该赛季在列表标记为已封存（若在列表中）。
      setSeasons((prev) =>
        prev.map((p) => (p.id === finalizeId.trim() ? { ...p, status: r.sealed ? "sealed" : p.status } : p)),
      );
    } catch (e) {
      setFinalizeErr(`收尾赛季失败：${errText(e)}`);
    } finally {
      setFinalizeBusy(false);
    }
  }, [finalizeId]);

  return (
    <>
      {/* 建季 */}
      <div className="adm-card">
        <div className="adm-card-title">创建赛季</div>
        <p className="adm-card-sub">建世界 + 落 seasons。创建后自动把赛季 ID 填入下方「收尾」框。</p>
        <label className="adm-label">赛季名</label>
        <input className="adm-input" value={name} onChange={(e) => setName(e.target.value)} placeholder="如 开元一季" />
        <div className="adm-row">
          <div style={{ flex: "1 1 200px" }}>
            <label className="adm-label">内容母题 ID（可选）</label>
            <input
              className="adm-input"
              value={themeId}
              onChange={(e) => setThemeId(e.target.value)}
              placeholder="content_theme_id"
            />
          </div>
          <div style={{ flex: "0 0 160px" }}>
            <label className="adm-label">最大人口（可选）</label>
            <input
              className="adm-input"
              type="number"
              min={0}
              value={maxPop}
              onChange={(e) => setMaxPop(e.target.value === "" ? "" : Number(e.target.value))}
              placeholder="不限"
            />
          </div>
        </div>
        <div style={{ marginTop: 12 }}>
          <button type="button" className="adm-btn adm-btn-primary" onClick={() => void submitCreate()} disabled={createBusy}>
            {createBusy ? "处理中…" : "创建赛季"}
          </button>
        </div>
        {createErr ? <div className="adm-toast adm-toast-err">{createErr}</div> : null}
        {createOk ? <div className="adm-toast adm-toast-ok">{createOk}</div> : null}
      </div>

      {/* 收尾 */}
      <div className="adm-card">
        <div className="adm-card-title">收尾赛季</div>
        <p className="adm-card-sub">存活角色回流名人堂 + 世界封存。不可逆。</p>
        <label className="adm-label">赛季 ID</label>
        <input
          className="adm-input"
          value={finalizeId}
          onChange={(e) => setFinalizeId(e.target.value)}
          placeholder="season_id"
        />
        <div style={{ marginTop: 12 }}>
          <button type="button" className="adm-btn adm-btn-danger" onClick={() => void submitFinalize()} disabled={finalizeBusy}>
            {finalizeBusy ? "收尾中…" : "收尾赛季（封存 + 回流名人堂）"}
          </button>
        </div>
        {finalizeErr ? <div className="adm-toast adm-toast-err">{finalizeErr}</div> : null}
        {finalizeRes ? (
          <div className="adm-toast adm-toast-ok">
            赛季已收尾：成员 {finalizeRes.members_total} 人，回流 {finalizeRes.archived} 人
            {finalizeRes.archive_errors.length ? `（${finalizeRes.archive_errors.length} 人回流失败）` : ""}，世界
            {finalizeRes.sealed ? "已封存" : "未封存"}。
          </div>
        ) : null}
      </div>

      {/* 列季 */}
      <div className="adm-card">
        <div className="adm-card-title">
          <span>赛季列表</span>
          <button type="button" className="adm-btn" onClick={() => void loadList()} disabled={listLoading}>
            {listLoading ? "加载中…" : "刷新"}
          </button>
        </div>
        {listErr ? <div className="adm-toast adm-toast-err">{listErr}</div> : null}
        {seasons.length === 0 ? (
          <div className="adm-empty">暂无赛季（创建后会出现在此）。</div>
        ) : (
          <table className="adm-table" style={{ marginTop: 8 }}>
            <thead>
              <tr>
                <th>赛季</th>
                <th>状态</th>
                <th>世界</th>
                <th>母题</th>
                <th>起止</th>
              </tr>
            </thead>
            <tbody>
              {seasons.map((s) => (
                <tr key={s.id}>
                  <td>
                    <div style={{ color: "#f0ead8" }}>{s.name || "(未命名)"}</div>
                    <div style={{ color: "#9aa0ad", fontSize: 11, fontFamily: "ui-monospace, monospace" }}>{s.id}</div>
                  </td>
                  <td>{s.status}</td>
                  <td style={{ fontFamily: "ui-monospace, monospace", fontSize: 11 }}>{s.world_id.slice(0, 8)}…</td>
                  <td>{s.content_theme_id || "—"}</td>
                  <td style={{ fontSize: 11 }}>
                    {s.started_at || "—"}
                    {s.ends_at ? ` → ${s.ends_at}` : ""}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </>
  );
}

export default SeasonPanel;
