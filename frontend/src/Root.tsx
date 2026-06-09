/* 文件说明：前端顶层路由。按 URL hash 在三处之间切换——
   默认（无 hash / #fate）→ 主世界「命运开盒」客户端（FateApp），由 AuthGate 包住，登录后才进；
   #battle/<sessionId> → 旧战棋指挥客户端（App）接管某一场关键战，把 sessionId 传给它，并给一个「返回命运」回调；
   #admin → GM 管理后台（W-C）：独立 AdminApp 根组件，自管 ops-token 登录与 5 面板。
   hash 变化实时切换。 */

import { useCallback, useEffect, useState } from "react";
import { App } from "./App";
import { FateApp } from "./fate/FateApp";
import { AuthGate } from "./components/AuthGate";
import AdminApp from "./admin/AdminApp";

// Route 是当前解析出的路由意图。
type Route =
  | { kind: "fate" } // 默认：主世界命运客户端（登录门后）
  | { kind: "battle"; sessionId: string } // #battle/<sessionId>：战棋接管视图
  | { kind: "admin" }; // #admin：GM 后台占位

// AppRouteProps 是 Root 对 App（B3 拥有）的 props 契约（crossFileNeeds）：
//   - sessionId：#battle/<id> 解析出的接管会话；App 据此直接进入该局战棋视图（无则走自身落地页）。
//   - onReturnToFate：玩家退出战棋、回到主世界命运客户端的回调（Root 把 hash 清回默认）。
// App 当前签名可能尚未声明这些 props（B3 渐进接入）；此处把 App 视为可选接收这些 props 的组件，
// 让 Root 独立编译通过——App 实装后按同名 props 取用即可，运行期无副作用。
export type AppRouteProps = {
  sessionId?: string;
  onReturnToFate?: () => void;
};

const RoutableApp = App as unknown as (props: AppRouteProps) => JSX.Element;

// parseRoute 从 location.hash 解析路由（去掉前导 # 与查询串后按段判定）。
function parseRoute(): Route {
  const raw = window.location.hash.replace(/^#/, "");
  const path = raw.split("?")[0];
  const segments = path.split("/").filter(Boolean);
  const head = segments[0] ?? "";

  if (head === "battle") {
    // #battle/<sessionId>：取第二段为会话 ID（缺失则退回命运客户端，避免空局白屏）。
    const sessionId = segments[1] ? decodeURIComponent(segments[1]) : "";
    if (sessionId) {
      return { kind: "battle", sessionId };
    }
    return { kind: "fate" };
  }
  if (head === "admin") {
    return { kind: "admin" };
  }
  // 默认（无 hash / #fate / 其它未知）→ 主世界命运客户端。
  return { kind: "fate" };
}

export function Root(): JSX.Element {
  const [route, setRoute] = useState<Route>(() => parseRoute());

  useEffect(() => {
    const onHash = () => setRoute(parseRoute());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  // returnToFate 把 hash 清回默认，触发 hashchange 回到命运客户端。
  const returnToFate = useCallback(() => {
    if (window.location.hash) {
      window.location.hash = "";
    } else {
      setRoute({ kind: "fate" });
    }
  }, []);

  if (route.kind === "battle") {
    // 战棋接管：不经 AuthGate（关键战入口由命运客户端内部带 sessionId 跳转，已是登录态）。
    return <RoutableApp sessionId={route.sessionId} onReturnToFate={returnToFate} />;
  }

  if (route.kind === "admin") {
    // GM 管理后台（W-C 接入）：独立 AdminApp 根组件，自管 ops-token 登录与 5 面板，无需 props。
    return <AdminApp />;
  }

  // 默认：主世界命运客户端，登录门包住——登录后才降生/续上她的人生。
  return (
    <AuthGate>
      <FateApp />
    </AuthGate>
  );
}
