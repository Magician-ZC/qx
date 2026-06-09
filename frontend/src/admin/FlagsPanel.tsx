/* 文件说明：GM 后台「运行时 flag 开关」面板（头牌功能）。
   拉 GET /api/admin/flags（后端直接序列化 featureflags.SnapshotEffective()）列所有可运营游戏 flag：
   名/中文说明/当前生效值（Effective）/是否设了运行时 override（OverrideSet）。每个 flag：
   - 布尔型（Values 空）：一个开关 toggle → POST /api/admin/flags 设 "on"/"off"（不重启即生效）；
   - 多档型（Values 非空，如 QUNXIANG_WORLD_BINDING）：一个 select 选档 → POST 设该档名；
   - 「回落 env」→ DELETE /api/admin/flags 清 override，回落到环境变量默认值。
   让运营能不重启副本就开关：自动 PvE / 破圈 / 世界 Boss 自刷 / 入向世界化 / 野心打分 / 血仇 / 零和 等。

   后端 featureflags 域层已就绪（SnapshotEffective/SetOverride/ClearOverride），仅 HTTP 路由
   /api/admin/flags 待接线（crossFileNeeds）。未接线时（404/连接失败）面板提示需后端接线、开关禁用。 */

import { useCallback, useEffect, useState } from "react";
import {
  clearAdminFlagOverride,
  errText,
  flagIsMultiValue,
  flagTruthy,
  listAdminFlags,
  setAdminFlagOverride,
  type AdminFlag,
} from "./adminApi";

export function FlagsPanel(): JSX.Element {
  const [flags, setFlags] = useState<AdminFlag[]>([]);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState("");
  // backendReady=false 表示后端 HTTP 路由 /api/admin/flags 未接线（拉取失败）：开关禁用。
  const [backendReady, setBackendReady] = useState(true);
  // busy 记录正在切换中的 flag name，避免重复点击。
  const [busy, setBusy] = useState<string>("");
  const [ok, setOk] = useState("");

  const load = useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      const data = await listAdminFlags();
      setFlags(data);
      setBackendReady(true);
    } catch (e) {
      // 后端 HTTP 路由未接线（404/连接失败）→ 禁用开关并提示。featureflags 域层已就绪，仅差路由。
      setBackendReady(false);
      setErr(`运行时 flag 端点不可用：${errText(e)}（后端 featureflags 域层已就绪，HTTP 路由 /api/admin/flags 待接线）`);
      setFlags([]);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // applyOverride 把某 flag 的运行时 override 设为给定原始字符串值（布尔传 "on"/"off"，多档传档名）。
  const applyOverride = useCallback(
    async (flag: AdminFlag, value: string, humanLabel: string) => {
      if (!backendReady) {
        return;
      }
      setBusy(flag.Name);
      setErr("");
      setOk("");
      try {
        const updated = await setAdminFlagOverride(flag.Name, value);
        if (updated) {
          setFlags((prev) => prev.map((f) => (f.Name === flag.Name ? updated : f)));
        } else {
          // 后端未回最新态：本地乐观更新（标 override、置生效值）。
          setFlags((prev) =>
            prev.map((f) =>
              f.Name === flag.Name ? { ...f, OverrideSet: true, OverrideValue: value, Effective: value } : f,
            ),
          );
        }
        setOk(`已运行时设 ${flag.Name} = ${humanLabel}（override 生效，无需重启）。`);
      } catch (e) {
        setErr(`切换 ${flag.Name} 失败：${errText(e)}`);
      } finally {
        setBusy("");
      }
    },
    [backendReady],
  );

  // resetToEnv 清除运行时 override，回落 env 默认值。
  const resetToEnv = useCallback(
    async (flag: AdminFlag) => {
      if (!backendReady) {
        return;
      }
      setBusy(flag.Name);
      setErr("");
      setOk("");
      try {
        const updated = await clearAdminFlagOverride(flag.Name);
        if (updated) {
          setFlags((prev) => prev.map((f) => (f.Name === flag.Name ? updated : f)));
        } else {
          setFlags((prev) =>
            prev.map((f) =>
              f.Name === flag.Name ? { ...f, OverrideSet: false, OverrideValue: "", Effective: f.EnvValue } : f,
            ),
          );
        }
        setOk(`已清除 ${flag.Name} 的运行时覆盖，回落到环境变量默认值。`);
      } catch (e) {
        setErr(`回落 ${flag.Name} 失败：${errText(e)}`);
      } finally {
        setBusy("");
      }
    },
    [backendReady],
  );

  return (
    <div className="adm-card">
      <div className="adm-card-title">
        <span>运行时 flag 开关</span>
        <button type="button" className="adm-btn" onClick={() => void load()} disabled={loading}>
          {loading ? "加载中…" : "刷新"}
        </button>
      </div>
      <p className="adm-card-sub">
        不重启副本即可开关游戏 flag（自动 PvE / 破圈 / 世界 Boss 自刷 / 入向世界化 / 野心打分 / 血仇 / 零和 等）。
        开关写运行时 override，「回落 env」清除覆盖回到环境变量默认值。多档型（如世界绑定策略）用下拉选档。
      </p>

      {err ? <div className="adm-toast adm-toast-err">{err}</div> : null}
      {ok ? <div className="adm-toast adm-toast-ok">{ok}</div> : null}

      {flags.length === 0 && !loading ? (
        <div className="adm-empty">暂无 flag 数据。</div>
      ) : (
        <div style={{ marginTop: 8 }}>
          {flags.map((flag) => {
            const isBusy = busy === flag.Name;
            const multi = flagIsMultiValue(flag);
            const on = flagTruthy(flag.Effective);
            return (
              <div className="adm-flag-row" key={flag.Name}>
                <div className="adm-flag-main">
                  <div className="adm-flag-name">{flag.Name}</div>
                  <div className="adm-flag-desc">{flag.Description}</div>
                </div>
                <div className="adm-flag-state">
                  {multi ? (
                    <span className="adm-badge adm-badge-on" title="当前生效值">
                      {flag.Effective || "(空)"}
                    </span>
                  ) : (
                    <span className={`adm-badge ${on ? "adm-badge-on" : "adm-badge-off"}`}>{on ? "ON" : "OFF"}</span>
                  )}
                  <span className={`adm-badge ${flag.OverrideSet ? "adm-badge-override" : "adm-badge-env"}`}>
                    {flag.OverrideSet ? "override" : "env"}
                  </span>
                  {multi ? (
                    <select
                      className="adm-select"
                      style={{ width: "auto", padding: "5px 8px", fontSize: 12 }}
                      value={flag.Effective}
                      disabled={!backendReady || isBusy}
                      onChange={(e) => void applyOverride(flag, e.target.value, e.target.value)}
                      aria-label={`${flag.Name} 取值`}
                    >
                      {/* 当前生效值若不在 Values 内（如未设环境变量），补一个占位项避免受控告警。 */}
                      {flag.Values && !flag.Values.includes(flag.Effective) ? (
                        <option value={flag.Effective}>{flag.Effective || "(空)"}</option>
                      ) : null}
                      {(flag.Values ?? []).map((v) => (
                        <option key={v} value={v}>
                          {v}
                        </option>
                      ))}
                    </select>
                  ) : (
                    <label className="adm-toggle" title={backendReady ? "切换运行时 override" : "HTTP 路由 /api/admin/flags 待接线"}>
                      <input
                        type="checkbox"
                        checked={on}
                        disabled={!backendReady || isBusy}
                        onChange={(e) => void applyOverride(flag, e.target.checked ? "on" : "off", e.target.checked ? "ON" : "OFF")}
                      />
                      <span className="adm-toggle-track" />
                    </label>
                  )}
                  <button
                    type="button"
                    className="adm-btn"
                    style={{ padding: "5px 9px", fontSize: 11 }}
                    disabled={!backendReady || isBusy || !flag.OverrideSet}
                    onClick={() => void resetToEnv(flag)}
                    title="清除运行时覆盖，回落环境变量默认值"
                  >
                    回落 env
                  </button>
                </div>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

export default FlagsPanel;
