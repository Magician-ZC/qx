/* 文件说明：GM 后台「操作者」面板（RBAC + 操作审计）。
   上半：列操作者（GET /api/admin/operators，name/role/created_at）+ 新增表单（name + role 下拉
   viewer/operator/admin + token 输入）+ 删除（DELETE /api/admin/operators?name=）。
   新增/更新走 POST /api/admin/operators {name,role,token}——token 仅此次提交、后端落库后不再回显，
   故提交成功后提示运营「该令牌仅此一次可见，请立即保存」。
   下半：列最近操作审计（GET /api/admin/audit?limit=，operator/role/action/target/created_at）。

   后端操作者表/审计域层待落地，HTTP 路由 /api/admin/operators · /audit 待接线（crossFileNeeds）。
   未接线时（404/连接失败）面板提示需后端接线、表单禁用。 */

import { useCallback, useEffect, useState } from "react";
import {
  deleteOperator,
  errText,
  listOperators,
  listOpsAudit,
  upsertOperator,
  type OpsAuditRow,
  type OpsOperator,
} from "./adminApi";

// OPERATOR_ROLES 是 RBAC 三档角色（与后端约定对齐）。
const OPERATOR_ROLES = ["viewer", "operator", "admin"] as const;

export function OperatorPanel(): JSX.Element {
  const [operators, setOperators] = useState<OpsOperator[]>([]);
  const [audit, setAudit] = useState<OpsAuditRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState("");
  const [ok, setOk] = useState("");
  // backendReady=false 表示后端 HTTP 路由未接线（拉取失败）：表单禁用。
  const [backendReady, setBackendReady] = useState(true);
  // busy 记录正在提交中的操作者 name，避免重复点击。
  const [busy, setBusy] = useState<string>("");

  // 新增表单字段。
  const [formName, setFormName] = useState("");
  const [formRole, setFormRole] = useState<string>("operator");
  const [formToken, setFormToken] = useState("");

  const load = useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      const [ops, log] = await Promise.all([listOperators(), listOpsAudit(50)]);
      setOperators(ops);
      setAudit(log);
      setBackendReady(true);
    } catch (e) {
      // 后端 HTTP 路由未接线（404/连接失败）→ 禁用表单并提示。
      setBackendReady(false);
      setErr(`操作者端点不可用：${errText(e)}（后端操作者/审计域层待落地，HTTP 路由 /api/admin/operators · /audit 待接线）`);
      setOperators([]);
      setAudit([]);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // submitOperator 新增/更新一名操作者；成功后提示 token 仅此一次可见，并刷新列表。
  const submitOperator = useCallback(async () => {
    if (!backendReady) {
      return;
    }
    const name = formName.trim();
    const token = formToken.trim();
    if (name === "" || token === "") {
      setErr("操作者名与令牌均不能为空。");
      return;
    }
    setBusy(name);
    setErr("");
    setOk("");
    try {
      await upsertOperator(name, formRole, token);
      setOk(`已保存操作者 ${name}（角色 ${formRole}）。令牌仅此一次可见，请立即妥善保存——后端落库后不再回显。`);
      setFormName("");
      setFormToken("");
      setFormRole("operator");
      await load();
    } catch (e) {
      setErr(`保存操作者 ${name} 失败：${errText(e)}`);
    } finally {
      setBusy("");
    }
  }, [backendReady, formName, formRole, formToken, load]);

  // removeOperator 删除一名操作者并刷新列表。
  const removeOperator = useCallback(
    async (name: string) => {
      if (!backendReady) {
        return;
      }
      setBusy(name);
      setErr("");
      setOk("");
      try {
        await deleteOperator(name);
        setOk(`已删除操作者 ${name}。`);
        await load();
      } catch (e) {
        setErr(`删除操作者 ${name} 失败：${errText(e)}`);
      } finally {
        setBusy("");
      }
    },
    [backendReady, load],
  );

  return (
    <div className="adm-card">
      <div className="adm-card-title">
        <span>操作者</span>
        <button type="button" className="adm-btn" onClick={() => void load()} disabled={loading}>
          {loading ? "加载中…" : "刷新"}
        </button>
      </div>
      <p className="adm-card-sub">
        管理 GM 后台操作者与角色（viewer 只读 / operator 可运营 / admin 全权），并查看最近操作审计。
        新增操作者时令牌仅此一次可见，后端落库（哈希）后不再回显。
      </p>

      {err ? <div className="adm-toast adm-toast-err">{err}</div> : null}
      {ok ? <div className="adm-toast adm-toast-ok">{ok}</div> : null}

      {/* 新增操作者表单 */}
      <div style={{ marginTop: 8, display: "flex", gap: 8, flexWrap: "wrap", alignItems: "flex-end" }}>
        <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
          <label className="adm-card-sub" htmlFor="op-name" style={{ margin: 0 }}>
            操作者名
          </label>
          <input
            id="op-name"
            className="adm-input"
            type="text"
            style={{ width: 160, padding: "5px 8px", fontSize: 12 }}
            value={formName}
            disabled={!backendReady}
            onChange={(e) => setFormName(e.target.value)}
            placeholder="如 alice"
          />
        </div>
        <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
          <label className="adm-card-sub" htmlFor="op-role" style={{ margin: 0 }}>
            角色
          </label>
          <select
            id="op-role"
            className="adm-select"
            style={{ width: "auto", padding: "5px 8px", fontSize: 12 }}
            value={formRole}
            disabled={!backendReady}
            onChange={(e) => setFormRole(e.target.value)}
          >
            {OPERATOR_ROLES.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </select>
        </div>
        <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
          <label className="adm-card-sub" htmlFor="op-token" style={{ margin: 0 }}>
            令牌（仅此一次可见）
          </label>
          <input
            id="op-token"
            className="adm-input"
            type="password"
            style={{ width: 200, padding: "5px 8px", fontSize: 12 }}
            value={formToken}
            disabled={!backendReady}
            onChange={(e) => setFormToken(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void submitOperator();
            }}
            placeholder="X-Ops-Token"
          />
        </div>
        <button
          type="button"
          className="adm-btn adm-btn-primary"
          style={{ padding: "6px 12px" }}
          disabled={!backendReady || busy !== "" || formName.trim() === "" || formToken.trim() === ""}
          onClick={() => void submitOperator()}
        >
          保存操作者
        </button>
      </div>

      {/* 操作者列表 */}
      {operators.length === 0 && !loading ? (
        <div className="adm-empty">暂无操作者。</div>
      ) : (
        <div style={{ marginTop: 12 }}>
          {operators.map((op) => {
            const isBusy = busy === op.name;
            return (
              <div className="adm-flag-row" key={op.name}>
                <div className="adm-flag-main">
                  <div className="adm-flag-name">{op.name}</div>
                  <div className="adm-flag-desc">
                    角色 {op.role} · 创建于 {op.created_at || "(未知)"}
                  </div>
                </div>
                <div className="adm-flag-state">
                  <span className="adm-badge adm-badge-on">{op.role}</span>
                  <button
                    type="button"
                    className="adm-btn"
                    style={{ padding: "5px 9px", fontSize: 11 }}
                    disabled={!backendReady || isBusy}
                    onClick={() => void removeOperator(op.name)}
                    title="删除该操作者"
                  >
                    删除
                  </button>
                </div>
              </div>
            );
          })}
        </div>
      )}

      {/* 操作审计 */}
      <div className="adm-card-sub" style={{ fontWeight: 600, marginTop: 18, marginBottom: 6 }}>
        最近操作审计
      </div>
      {audit.length === 0 ? (
        <div className="adm-empty">暂无审计记录。</div>
      ) : (
        <div>
          {audit.map((row, i) => (
            <div className="adm-flag-row" key={`${row.created_at}-${i}`}>
              <div className="adm-flag-main">
                <div className="adm-flag-name">
                  {row.action} → {row.target || "(无对象)"}
                </div>
                <div className="adm-flag-desc">
                  {row.operator}（{row.role}） · {row.created_at || "(未知时间)"}
                </div>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

export default OperatorPanel;
