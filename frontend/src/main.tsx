/* 文件说明：前端应用入口。挂载 Root（按 URL hash 在命运开盒 UI 与旧战棋客户端间路由）。 */

import React from "react";
import ReactDOM from "react-dom/client";
import { Root } from "./Root";
import "./styles.css";

ReactDOM.createRoot(document.getElementById("root") as HTMLElement).render(
  <React.StrictMode>
    <Root />
  </React.StrictMode>,
);
