/* 文件说明：独立 GM 管理后台应用（挂 #admin 路由，由 Root.tsx 接入——crossFileNeeds）。
   与游戏客户端完全分离的运营界面：ops-token 登录门 → 左导航在六块面板间切换。

   六块面板（任务 ①–⑥）：
   ① 运行时 flag 开关（头牌）：FlagsPanel —— GET/POST/DELETE /api/admin/flags（后端待接线）。
   ② 世界配置：WorldConfigPanel —— 世界/region/人口列表 + 设威胁度 + 村庄播种（部分后端待接线）。
   ③ GM 事件注入：GmEventPanel —— POST /api/ops/worlds/:worldId/events（已落地）。
   ④ 赛季：SeasonPanel —— POST /api/ops/seasons / :id/finalize（已落地），列季（待接线）。
   ⑤ 监控：MonitoringPanel —— 北极星/产品漏斗/成本/零和审计（ops 端点，已落地）。

   鉴权：所有 GM 后台端点都套后端 opsTokenGuard（QUNXIANG_OPS_TOKEN）。本应用用 ops-token 输入作独立登录：
   存 localStorage（adminApi 自持），后续请求经 adminApi 自动带 X-Ops-Token 头。未配 token 时后端原型放行，
   但本前端仍要求先填（或显式「跳过」）——保证「以运营身份进后台」这层心智明确。

   crossFileNeeds（主控集成，本 agent 不编辑）：
   - frontend/src/Root.tsx：#admin 路由 `return <AdminApp />;`（替换现有占位卡）。需 `import { AdminApp } from "./admin/AdminApp";`。
   - 后端（router.go，他人）：落地以下 HTTP 路由，均套 opsTokenGuard：
     · GET /api/admin/flags：直接序列化 featureflags.SnapshotEffective()（域层已就绪：SnapshotEffective/
       SetOverride/ClearOverride/IsKnownGameplayFlag），返回 {flags: EffectiveFlag[]}。
     · POST /api/admin/flags {name,value}：校验 IsKnownGameplayFlag → featureflags.SetOverride，返回 {flag}。
     · DELETE /api/admin/flags?name=：featureflags.ClearOverride，返回 {flag}（回落后的最新态）。
     · GET /api/admin/worlds-detail（世界+region+人口）、POST /api/admin/worlds/:worldId/regions/:regionId/threat、
       POST /api/admin/worlds/:worldId/seed-village、GET /api/ops/seasons（列季）。
     未落地时本应用已优雅降级（flag 端点禁用开关并提示、世界退基本 /api/worlds 列表、赛季退本会话新建）。 */

import { useCallback, useState } from "react";
import "./admin.css";
import { getAdminOpsToken, hasAdminOpsToken, setAdminOpsToken } from "./adminApi";
import { FlagsPanel } from "./FlagsPanel";
import { ConfigPanel } from "./ConfigPanel";
import { OperatorPanel } from "./OperatorPanel";
import { WorldConfigPanel } from "./WorldConfigPanel";
import { FactionPanel } from "./FactionPanel";
import { GmEventPanel } from "./GmEventPanel";
import { SeasonPanel } from "./SeasonPanel";
import { ContentPanel } from "./ContentPanel";
import { MonitoringPanel } from "./MonitoringPanel";
import { ClientPanel } from "./ClientPanel";

// AdminTab 是左导航的页签标识。
type AdminTab =
  | "flags"
  | "config"
  | "worlds"
  | "factions"
  | "gm-events"
  | "seasons"
  | "content"
  | "monitoring"
  | "operators"
  | "clients";

const TABS: { id: AdminTab; label: string }[] = [
  { id: "flags", label: "运行时开关" },
  { id: "config", label: "可运营配置" },
  { id: "worlds", label: "世界配置" },
  { id: "factions", label: "阵营配置" },
  { id: "gm-events", label: "事件注入" },
  { id: "seasons", label: "赛季" },
  { id: "content", label: "内容运营" },
  { id: "monitoring", label: "监控" },
  { id: "operators", label: "操作者" },
  { id: "clients", label: "客户管理" },
];

// AdminLogin 是 ops-token 登录门：填 token → 存 localStorage（adminApi 持有）→ 进后台。
// 后端原型未配 QUNXIANG_OPS_TOKEN 时放行，故提供「跳过（原型放行）」入口——但仍走一次显式确认，
// 让「以运营身份进后台」的心智明确，避免误把游戏客户端当后台。
function AdminLogin({ onEnter }: { onEnter: () => void }): JSX.Element {
  const [token, setToken] = useState(getAdminOpsToken());

  const submit = useCallback(() => {
    setAdminOpsToken(token.trim());
    onEnter();
  }, [onEnter, token]);

  return (
    <div className="adm-shell">
      <div className="adm-login">
        <div className="adm-login-card">
          <h1>一念 · 司命台</h1>
          <p>
            GM 管理后台。请输入运营令牌（X-Ops-Token）登录。
            <br />
            令牌经后端 QUNXIANG_OPS_TOKEN 校验；原型未配则可留空跳过（仍以运营身份进入）。
          </p>
          <input
            className="adm-input"
            type="password"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") submit();
            }}
            placeholder="X-Ops-Token（运营令牌）"
            aria-label="运营令牌"
          />
          <div style={{ marginTop: 14, display: "flex", gap: 8, justifyContent: "center" }}>
            <button type="button" className="adm-btn adm-btn-primary" onClick={submit} disabled={token.trim() === ""}>
              登录
            </button>
            <button
              type="button"
              className="adm-btn"
              onClick={() => {
                setAdminOpsToken("");
                onEnter();
              }}
              title="后端未配 QUNXIANG_OPS_TOKEN 时可不带令牌进入"
            >
              跳过（原型放行）
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

// AdminApp 是 GM 后台根组件。Root.tsx 在 #admin 路由直接挂载它（无 props）。
export function AdminApp(): JSX.Element {
  // entered 表示已过登录门（填了 token 或显式跳过）。初值看是否已有持久化 token。
  const [entered, setEntered] = useState(hasAdminOpsToken());
  const [tab, setTab] = useState<AdminTab>("flags");

  const logout = useCallback(() => {
    setAdminOpsToken("");
    setEntered(false);
  }, []);

  if (!entered) {
    return <AdminLogin onEnter={() => setEntered(true)} />;
  }

  return (
    <div className="adm-shell">
      <div className="adm-topbar">
        <div className="adm-brand">
          <span className="adm-brand-title">一念 · 司命台</span>
          <span className="adm-brand-sub">GM 管理后台 · 与游戏客户端分离的运营界面</span>
        </div>
        <div className="adm-topbar-right">
          <span className="adm-token-hint">
            {hasAdminOpsToken() ? "已带运营令牌（X-Ops-Token）" : "未带令牌（依赖后端原型放行）"}
          </span>
          <button type="button" className="adm-btn" onClick={logout}>
            退出登录
          </button>
        </div>
      </div>

      <div className="adm-body">
        <nav className="adm-nav" aria-label="GM 后台导航">
          {TABS.map((t) => (
            <button
              key={t.id}
              type="button"
              className={`adm-nav-item ${tab === t.id ? "adm-nav-active" : ""}`}
              onClick={() => setTab(t.id)}
              aria-current={tab === t.id ? "page" : undefined}
            >
              {t.label}
            </button>
          ))}
        </nav>

        <main className="adm-content">
          {tab === "flags" ? <FlagsPanel /> : null}
          {tab === "config" ? <ConfigPanel /> : null}
          {tab === "worlds" ? <WorldConfigPanel /> : null}
          {tab === "factions" ? <FactionPanel /> : null}
          {tab === "gm-events" ? <GmEventPanel /> : null}
          {tab === "seasons" ? <SeasonPanel /> : null}
          {tab === "content" ? <ContentPanel /> : null}
          {tab === "monitoring" ? <MonitoringPanel /> : null}
          {tab === "operators" ? <OperatorPanel /> : null}
          {tab === "clients" ? <ClientPanel /> : null}
        </main>
      </div>
    </div>
  );
}

export default AdminApp;
