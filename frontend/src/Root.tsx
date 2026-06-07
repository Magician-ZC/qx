/* 文件说明：按 URL hash 在「角色命运开盒 UI」与「旧战棋指挥客户端」之间路由。 */

import { useEffect, useState } from "react";
import { App } from "./App";
import { FateApp } from "./fate/FateApp";

function isFateRoute(): boolean {
  return window.location.hash.replace(/^#/, "").split("?")[0] === "fate";
}

export function Root() {
  const [fate, setFate] = useState<boolean>(isFateRoute());
  useEffect(() => {
    const onHash = () => setFate(isFateRoute());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);
  return fate ? <FateApp /> : <App />;
}
