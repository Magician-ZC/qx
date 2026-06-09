/* 文件说明：登录/注册门——主世界命运客户端的鉴权前置。未登录显示登录/注册表单（用户名+密码），
   登录成功把 Bearer 令牌存入 localStorage（经 api.ts 的 setAccountToken，request() 据此自动带 Authorization）；
   已登录则透传 children。祖魂语气文案，复用 fate.css 的墨色宣纸风（.fate-shell / .fate-create）。*/

import { useCallback, useEffect, useState } from "react";
import {
  APIError,
  getMe,
  loginAccount,
  logoutAccount,
  registerAccount,
  getAccountToken,
} from "../session/api";
import type { AccountUser } from "../session/types";
import "../fate/fate.css";

// 登录态判定的三态：检查中 / 未登录 / 已登录。检查中先拦一帧 loading，避免闪过登录表单。
type AuthPhase = "checking" | "anon" | "authed";
type FormMode = "login" | "register";

// AuthGate 用 props 暴露已登录账号给子树（命运客户端可据 account 显示「她的主人」等）。
// children 既可是 ReactNode，也可是「(account) => ReactNode」渲染函数（拿到已登录账号注入下游）。
type AuthGateProps = {
  children: React.ReactNode | ((account: AccountUser) => React.ReactNode);
};

export function AuthGate({ children }: AuthGateProps): JSX.Element {
  const [phase, setPhase] = useState<AuthPhase>(() =>
    // 有本地令牌才进 checking（去后端核验）；无令牌直接 anon，省一次必失败的请求。
    getAccountToken().trim() === "" ? "anon" : "checking",
  );
  const [account, setAccount] = useState<AccountUser | null>(null);
  const [mode, setMode] = useState<FormMode>("login");
  const [username, setUsername] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  // 首屏（或令牌变化）核验登录态：getMe 把未登录/失效收敛为 null，不抛。
  useEffect(() => {
    let alive = true;
    if (getAccountToken().trim() === "") {
      setPhase("anon");
      return;
    }
    void getMe().then((me) => {
      if (!alive) return;
      if (me) {
        setAccount(me);
        setPhase("authed");
      } else {
        setAccount(null);
        setPhase("anon");
      }
    });
    return () => {
      alive = false;
    };
  }, []);

  const submit = useCallback(async () => {
    const name = username.trim();
    if (name === "" || password === "") {
      setError("请填写用户名与密码。");
      return;
    }
    setBusy(true);
    setError("");
    try {
      // register/login 内部已 setAccountToken（写 localStorage + 模块级，供 request() 带 Bearer）。
      const result =
        mode === "register"
          ? await registerAccount({ username: name, display_name: displayName.trim() || undefined, password })
          : await loginAccount({ username: name, password });
      setAccount(result.user);
      setPhase("authed");
      setPassword("");
    } catch (err) {
      const reason =
        err instanceof APIError
          ? err.message
          : err instanceof Error
            ? err.message
            : String(err);
      setError(mode === "register" ? `降生受阻：${reason}` : `没能认出你：${reason}`);
    } finally {
      setBusy(false);
    }
  }, [mode, username, displayName, password]);

  const signOut = useCallback(async () => {
    const token = getAccountToken();
    setBusy(true);
    try {
      if (token.trim() !== "") {
        await logoutAccount(token); // 内部 finally 必清本地令牌
      }
    } finally {
      setAccount(null);
      setPhase("anon");
      setMode("login");
      setUsername("");
      setPassword("");
      setError("");
      setBusy(false);
    }
  }, []);

  if (phase === "checking") {
    return (
      <div className="fate-shell fate-onboarding">
        <div className="fate-create" style={{ textAlign: "center" }}>
          <p className="fate-create-lead" style={{ margin: 0 }}>
            正在确认是不是你回来了…
          </p>
        </div>
      </div>
    );
  }

  if (phase === "authed" && account) {
    // 已登录：透传 children（渲染函数则注入已登录账号），并提供右上角登出。
    return (
      <>
        {typeof children === "function" ? (children as (a: AccountUser) => React.ReactNode)(account) : children}
        <button
          className="fate-restart"
          style={{ left: 16, right: "auto" }}
          disabled={busy}
          onClick={() => void signOut()}
        >
          {account.display_name || account.username} · 离世（登出）
        </button>
      </>
    );
  }

  // 未登录：登录 / 注册表单。
  const isRegister = mode === "register";
  return (
    <div className="fate-shell fate-onboarding">
      <div className="fate-create">
        <h1>群像 · 命运开盒</h1>
        <p className="fate-create-lead">
          {isRegister
            ? "立一个名号，让她的命运从此有人牵挂。这名号会在你每次归来时认出你。"
            : "报上名号，祖魂便会把你记挂的那个人，从世间唤回到你眼前。"}
        </p>

        <label>
          名号（用户名）
          <input
            value={username}
            autoComplete="username"
            placeholder="你在此世的称呼"
            onChange={(e) => setUsername(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void submit();
            }}
          />
        </label>

        {isRegister && (
          <label>
            别号（可不填）
            <input
              value={displayName}
              placeholder="旁人如何称呼你"
              onChange={(e) => setDisplayName(e.target.value)}
            />
          </label>
        )}

        <label>
          凭信（密码）
          <input
            type="password"
            value={password}
            autoComplete={isRegister ? "new-password" : "current-password"}
            placeholder="只有你知道的暗记"
            onChange={(e) => setPassword(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void submit();
            }}
          />
        </label>

        {error && <div className="fate-error">{error}</div>}

        <button className="fate-create-btn" disabled={busy} onClick={() => void submit()}>
          {busy ? (isRegister ? "正在为你立名…" : "正在认你…") : isRegister ? "立名 · 归来此世" : "报名 · 续上前缘"}
        </button>

        <button
          type="button"
          className="fate-auth-switch"
          onClick={() => {
            setMode(isRegister ? "login" : "register");
            setError("");
          }}
          style={{
            marginTop: 14,
            width: "100%",
            background: "none",
            border: "none",
            color: "#8a6a3c",
            fontSize: 13,
            cursor: "pointer",
            fontFamily: "inherit",
          }}
        >
          {isRegister ? "已有名号？回来报名 →" : "还没有名号？立一个 →"}
        </button>
      </div>
    </div>
  );
}

export default AuthGate;
