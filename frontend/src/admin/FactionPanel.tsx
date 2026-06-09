/* 文件说明：GM 后台「阵营配置」面板（三阵营开放世界 F3，只读概览）。
   GET /api/admin/factions（后端 session.ListFactionsDetail，opsTokenGuard）：列三阵营的
   标识 / 中文名 / 道德信条 / 道德基准（freedom/order/chaos 三维）/ 出生据点 / 当前人口。
   供 GM 一眼看清世界的阵营分布。后端未接线（404/空）时友好降级为提示，不报红。

   配套运行时开关在「运行时开关」页（FlagsPanel）：QUNXIANG_FACTION_SWITCH（阵营切换）、
   QUNXIANG_FACTION_PVE（阵营冲突遭遇）已进可运营白名单，GM 可在那里运行时开关。 */

import type { CSSProperties } from "react";
import { useCallback, useEffect, useState } from "react";
import { errText, listFactionsDetail, type AdminFactionDetail } from "./adminApi";

// monoStyle 是等宽字体的内联样式（admin.css 无专用类，故就地声明，便于阵营 ID / 基准数值对齐读数）。
const monoStyle: CSSProperties = { fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace" };

// factionAccent 给三阵营各一个主题色（仅做表头小色点，纯视觉、不承载语义）。
function factionAccent(id: string): string {
  switch (id) {
    case "freedom":
      return "#4aa3ff";
    case "order":
      return "#f0c24a";
    case "chaos":
      return "#e0556b";
    default:
      return "#8a8f99";
  }
}

export function FactionPanel(): JSX.Element {
  const [factions, setFactions] = useState<AdminFactionDetail[]>([]);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState("");
  // available=false 表示后端 /api/admin/factions 未接线（已友好降级）。
  const [available, setAvailable] = useState(true);

  const load = useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      const list = await listFactionsDetail();
      setFactions(list);
      setAvailable(list.length > 0);
    } catch (e) {
      // 端点未落地（404）或鉴权失败 → 友好降级（不报红）。
      setFactions([]);
      setAvailable(false);
      setErr(`读取阵营概览失败：${errText(e)}`);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  return (
    <div className="adm-card">
      <div className="adm-card-title">
        三阵营概览（只读）
        <button type="button" className="adm-btn" onClick={() => void load()} disabled={loading}>
          {loading ? "刷新中…" : "刷新"}
        </button>
      </div>
      <p className="adm-card-sub">
        三阵营开放世界的阵营分布：道德信条 / 道德基准（自由·秩序·混乱三维）/ 出生据点 / 当前人口（按该阵营 units 计数）。
        阵营切换、阵营冲突遭遇的运行时开关在「运行时开关」页（QUNXIANG_FACTION_SWITCH / QUNXIANG_FACTION_PVE，默认关）。
      </p>

      {err ? <div className="adm-toast adm-toast-err">{err}</div> : null}
      {!available && !loading ? (
        <div className="adm-empty">
          暂无阵营概览数据（后端 /api/admin/factions 未接线或暂无单位）。
        </div>
      ) : null}

      {factions.length > 0 ? (
        <table className="adm-table" style={{ marginTop: 8 }}>
          <thead>
            <tr>
              <th>阵营</th>
              <th>道德信条</th>
              <th>道德基准（自由/秩序/混乱）</th>
              <th>出生据点</th>
              <th className="adm-num">人口</th>
            </tr>
          </thead>
          <tbody>
            {factions.map((f) => (
              <tr key={f.id}>
                <td>
                  <span
                    style={{
                      display: "inline-block",
                      width: 8,
                      height: 8,
                      borderRadius: "50%",
                      background: factionAccent(f.id),
                      marginRight: 6,
                    }}
                  />
                  {f.name_zh}
                  <span style={{ ...monoStyle, marginLeft: 6, opacity: 0.6 }}>{f.id}</span>
                </td>
                <td>{f.moral_creed}</td>
                <td style={monoStyle}>
                  {f.baseline.freedom} / {f.baseline.order} / {f.baseline.chaos}
                </td>
                <td style={monoStyle}>{(f.spawn_points ?? []).join("、") || "—"}</td>
                <td className="adm-num">{f.population}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
    </div>
  );
}

export default FactionPanel;
