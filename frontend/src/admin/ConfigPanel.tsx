/* 文件说明：GM 后台「可运营配置」面板。
   拉 GET /api/admin/config（后端直接序列化 runtimeconfig.SnapshotEffective()）列所有可运营数值/枚举参数：
   名/命名空间/中文说明/类型/默认值（Default）/当前生效值（Effective）/是否设了运行时 override（OverrideSet）。
   按 Namespace 折叠分组渲染，每个参数按 Type 渲染控件：
   - bool：一个开关 toggle → POST /api/admin/config 设 "on"/"off"；
   - int/float：number input（min={Min} max={Max}），失焦/回车提交；
   - enum：select 含 Values；
   - string：input，失焦/回车提交；
   「回落默认」→ DELETE /api/admin/config 清 override，回落到注册默认值（spec.Default）。
   让运营能不重启副本就调平衡（战斗势头惩罚 / 命运预算 / 记忆衰减 / LLM 模型档 等）。

   后端 runtimeconfig 域层已就绪（SnapshotEffective/SetOverride/ClearOverride），仅 HTTP 路由
   /api/admin/config 待接线（crossFileNeeds）。未接线时（404/连接失败）面板提示需后端接线、控件禁用。 */

import { useCallback, useEffect, useMemo, useState } from "react";
import {
  clearAdminConfig,
  errText,
  flagTruthy,
  listAdminConfig,
  setAdminConfig,
  type AdminConfigItem,
} from "./adminApi";

// configIsBool / configIsEnum / configIsNumber 按 Type 判定控件类型（与后端 ParamType 档对齐）。
function configIsBool(item: AdminConfigItem): boolean {
  return item.Type === "bool";
}
function configIsEnum(item: AdminConfigItem): boolean {
  return item.Type === "enum";
}
function configIsNumber(item: AdminConfigItem): boolean {
  return item.Type === "int" || item.Type === "float";
}

export function ConfigPanel(): JSX.Element {
  const [items, setItems] = useState<AdminConfigItem[]>([]);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState("");
  // backendReady=false 表示后端 HTTP 路由 /api/admin/config 未接线（拉取失败）：控件禁用。
  const [backendReady, setBackendReady] = useState(true);
  // busy 记录正在提交中的参数 Name，避免重复点击。
  const [busy, setBusy] = useState<string>("");
  const [ok, setOk] = useState("");
  // 本地编辑缓冲：number/string 控件的待提交文本，键为参数 Name。失焦/回车才落库。
  const [drafts, setDrafts] = useState<Record<string, string>>({});

  const load = useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      const data = await listAdminConfig();
      setItems(data);
      setBackendReady(true);
    } catch (e) {
      // 后端 HTTP 路由未接线（404/连接失败）→ 禁用控件并提示。runtimeconfig 域层已就绪，仅差路由。
      setBackendReady(false);
      setErr(`运行时配置端点不可用：${errText(e)}（后端 runtimeconfig 域层已就绪，HTTP 路由 /api/admin/config 待接线）`);
      setItems([]);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // groups 把参数按 Namespace 折叠分组（保持后端注册顺序：首次出现的 namespace 排在前）。
  const groups = useMemo(() => {
    const order: string[] = [];
    const byNs: Record<string, AdminConfigItem[]> = {};
    for (const it of items) {
      const ns = it.Namespace || "(未分组)";
      if (!byNs[ns]) {
        byNs[ns] = [];
        order.push(ns);
      }
      byNs[ns].push(it);
    }
    return order.map((ns) => ({ ns, list: byNs[ns] }));
  }, [items]);

  // applyOverride 把某参数的运行时 override 设为给定原始字符串值。
  const applyOverride = useCallback(
    async (item: AdminConfigItem, value: string, humanLabel: string) => {
      if (!backendReady) {
        return;
      }
      setBusy(item.Name);
      setErr("");
      setOk("");
      try {
        const updated = await setAdminConfig(item.Name, value);
        if (updated) {
          setItems((prev) => prev.map((p) => (p.Name === item.Name ? updated : p)));
        } else {
          // 后端未回最新态：本地乐观更新（标 override、置生效值）。
          setItems((prev) =>
            prev.map((p) =>
              p.Name === item.Name ? { ...p, OverrideSet: true, OverrideValue: value, Effective: value } : p,
            ),
          );
        }
        setOk(`已运行时设 ${item.Name} = ${humanLabel}（override 生效，无需重启）。`);
      } catch (e) {
        setErr(`设置 ${item.Name} 失败：${errText(e)}`);
      } finally {
        setBusy("");
      }
    },
    [backendReady],
  );

  // resetToDefault 清除运行时 override，回落注册默认值（spec.Default）。
  const resetToDefault = useCallback(
    async (item: AdminConfigItem) => {
      if (!backendReady) {
        return;
      }
      setBusy(item.Name);
      setErr("");
      setOk("");
      try {
        const updated = await clearAdminConfig(item.Name);
        if (updated) {
          setItems((prev) => prev.map((p) => (p.Name === item.Name ? updated : p)));
        } else {
          setItems((prev) =>
            prev.map((p) =>
              p.Name === item.Name ? { ...p, OverrideSet: false, OverrideValue: "", Effective: p.Default } : p,
            ),
          );
        }
        // 回落后清掉该项的本地编辑缓冲，避免显示残留旧草稿。
        setDrafts((prev) => {
          const next = { ...prev };
          delete next[item.Name];
          return next;
        });
        setOk(`已清除 ${item.Name} 的运行时覆盖，回落到默认值（${item.Default}）。`);
      } catch (e) {
        setErr(`回落 ${item.Name} 失败：${errText(e)}`);
      } finally {
        setBusy("");
      }
    },
    [backendReady],
  );

  // draftValue 取某参数的当前显示文本：优先本地编辑缓冲，否则取生效值。
  const draftValue = useCallback(
    (item: AdminConfigItem): string => (item.Name in drafts ? drafts[item.Name] : item.Effective),
    [drafts],
  );

  // commitDraft 把 number/string 控件的本地草稿提交为 override（若与生效值一致则跳过）。
  const commitDraft = useCallback(
    (item: AdminConfigItem) => {
      const v = (item.Name in drafts ? drafts[item.Name] : item.Effective).trim();
      if (v === item.Effective) {
        return;
      }
      void applyOverride(item, v, v);
    },
    [applyOverride, drafts],
  );

  return (
    <div className="adm-card">
      <div className="adm-card-title">
        <span>可运营配置</span>
        <button type="button" className="adm-btn" onClick={() => void load()} disabled={loading}>
          {loading ? "加载中…" : "刷新"}
        </button>
      </div>
      <p className="adm-card-sub">
        不重启副本即可调游戏平衡参数（战斗势头惩罚 / 命运预算 / 记忆衰减 / LLM 模型档 等），按命名空间分组。
        数值/枚举/字符串就地编辑，开关型一键切换；「回落默认」清除运行时覆盖回到注册默认值。
      </p>

      {err ? <div className="adm-toast adm-toast-err">{err}</div> : null}
      {ok ? <div className="adm-toast adm-toast-ok">{ok}</div> : null}

      {items.length === 0 && !loading ? (
        <div className="adm-empty">暂无可运营配置参数。</div>
      ) : (
        <div style={{ marginTop: 8 }}>
          {groups.map(({ ns, list }) => (
            <div key={ns} style={{ marginBottom: 14 }}>
              <div className="adm-card-sub" style={{ fontWeight: 600, marginTop: 4, marginBottom: 4 }}>
                命名空间：{ns}
              </div>
              {/* llm 命名空间：补一行反 P2W 说明——LLM 模型/档位是全局热切，对所有玩家一视同仁，不存在付费分档。 */}
              {ns === "llm" ? (
                <p className="adm-card-sub" style={{ marginTop: 0, marginBottom: 6, opacity: 0.85 }}>
                  说明：LLM 模型/推理档为全局热切，对全体玩家一视同仁——非付费分档（反 P2W）。
                </p>
              ) : null}
              {list.map((item) => {
                const isBusy = busy === item.Name;
                return (
                  <div className="adm-flag-row" key={item.Name}>
                    <div className="adm-flag-main">
                      <div className="adm-flag-name">{item.Name}</div>
                      <div className="adm-flag-desc">{item.Description}</div>
                      <div className="adm-flag-desc" style={{ opacity: 0.7 }}>
                        类型 {item.Type} · 默认 {item.Default || "(空)"} · 生效 {item.Effective || "(空)"}
                        {item.HotReload ? "" : " · 需重启生效"}
                      </div>
                    </div>
                    <div className="adm-flag-state">
                      {configIsBool(item) ? (
                        <span
                          className={`adm-badge ${flagTruthy(item.Effective) ? "adm-badge-on" : "adm-badge-off"}`}
                        >
                          {flagTruthy(item.Effective) ? "ON" : "OFF"}
                        </span>
                      ) : (
                        <span className="adm-badge adm-badge-on" title="当前生效值">
                          {item.Effective || "(空)"}
                        </span>
                      )}
                      <span className={`adm-badge ${item.OverrideSet ? "adm-badge-override" : "adm-badge-env"}`}>
                        {item.OverrideSet ? "override" : "default"}
                      </span>

                      {configIsBool(item) ? (
                        <label
                          className="adm-toggle"
                          title={backendReady ? "切换运行时 override" : "HTTP 路由 /api/admin/config 待接线"}
                        >
                          <input
                            type="checkbox"
                            checked={flagTruthy(item.Effective)}
                            disabled={!backendReady || isBusy}
                            onChange={(e) =>
                              void applyOverride(
                                item,
                                e.target.checked ? "on" : "off",
                                e.target.checked ? "ON" : "OFF",
                              )
                            }
                          />
                          <span className="adm-toggle-track" />
                        </label>
                      ) : configIsEnum(item) ? (
                        <select
                          className="adm-select"
                          style={{ width: "auto", padding: "5px 8px", fontSize: 12 }}
                          value={item.Effective}
                          disabled={!backendReady || isBusy}
                          onChange={(e) => void applyOverride(item, e.target.value, e.target.value)}
                          aria-label={`${item.Name} 取值`}
                        >
                          {/* 当前生效值若不在 Values 内，补一个占位项避免受控告警。 */}
                          {item.Values && !item.Values.includes(item.Effective) ? (
                            <option value={item.Effective}>{item.Effective || "(空)"}</option>
                          ) : null}
                          {(item.Values ?? []).map((v) => (
                            <option key={v} value={v}>
                              {v}
                            </option>
                          ))}
                        </select>
                      ) : configIsNumber(item) ? (
                        <input
                          className="adm-input"
                          type="number"
                          style={{ width: 110, padding: "5px 8px", fontSize: 12 }}
                          value={draftValue(item)}
                          min={item.Min ?? undefined}
                          max={item.Max ?? undefined}
                          disabled={!backendReady || isBusy}
                          onChange={(e) =>
                            setDrafts((prev) => ({ ...prev, [item.Name]: e.target.value }))
                          }
                          onBlur={() => commitDraft(item)}
                          onKeyDown={(e) => {
                            if (e.key === "Enter") commitDraft(item);
                          }}
                          aria-label={`${item.Name} 数值`}
                        />
                      ) : (
                        <input
                          className="adm-input"
                          type="text"
                          style={{ width: 160, padding: "5px 8px", fontSize: 12 }}
                          value={draftValue(item)}
                          disabled={!backendReady || isBusy}
                          onChange={(e) =>
                            setDrafts((prev) => ({ ...prev, [item.Name]: e.target.value }))
                          }
                          onBlur={() => commitDraft(item)}
                          onKeyDown={(e) => {
                            if (e.key === "Enter") commitDraft(item);
                          }}
                          aria-label={`${item.Name} 取值`}
                        />
                      )}

                      <button
                        type="button"
                        className="adm-btn"
                        style={{ padding: "5px 9px", fontSize: 11 }}
                        disabled={!backendReady || isBusy || !item.OverrideSet}
                        onClick={() => void resetToDefault(item)}
                        title="清除运行时覆盖，回落注册默认值"
                      >
                        回落默认
                      </button>
                    </div>
                  </div>
                );
              })}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

export default ConfigPanel;
