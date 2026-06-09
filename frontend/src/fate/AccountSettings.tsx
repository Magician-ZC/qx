/* 文件说明：账号设置浮层（命运客户端）。这版只做「改密码」——旧密码 / 新密码 / 确认新密码，
   前端校验两次一致 + 新密码 ≥6 位（与后端 <6 返错对齐），调 changePassword（Bearer 登录态）。
   改密成功后后端吊销全部会话令牌，故成功后提示「改密成功，请重新登录」并触发登出（复用 FateApp 传入的
   onSignOut 回调——清本地 Bearer + 整页 reload 交还 AuthGate 重核验）。
   另预留「绑定飞书（即将上线）」灰置入口（占位，无功能）。仿 CharterEditor 的浮层定位/遮罩/关闭，墨色宣纸调。*/

import { useCallback, useState } from "react";
import { zIndex } from "../zindex-tokens";
import { changePassword } from "../session/api";

type Props = {
  // onSignOut 复用 FateApp 的登出（清本地 Bearer + reload 交还 AuthGate）。改密成功后调用。
  onSignOut: () => void;
  onClose: () => void;
};

const MIN_PASSWORD_LEN = 6;

export function AccountSettings({ onSignOut, onClose }: Props) {
  const [oldPw, setOldPw] = useState("");
  const [newPw, setNewPw] = useState("");
  const [confirmPw, setConfirmPw] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // done：改密成功后切到「请重新登录」收尾态（按钮触发 onSignOut）。
  const [done, setDone] = useState(false);

  const submit = useCallback(async () => {
    setError("");
    if (!oldPw) {
      setError("请输入当前密码。");
      return;
    }
    if (newPw.length < MIN_PASSWORD_LEN) {
      setError(`新密码至少 ${MIN_PASSWORD_LEN} 位。`);
      return;
    }
    if (newPw !== confirmPw) {
      setError("两次输入的新密码不一致。");
      return;
    }
    if (newPw === oldPw) {
      setError("新密码不能与当前密码相同。");
      return;
    }
    setBusy(true);
    try {
      await changePassword(oldPw, newPw);
      setDone(true);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }, [oldPw, newPw, confirmPw]);

  return (
    <aside style={panelStyle} role="dialog" aria-label="账号设置">
      <div style={headerStyle}>
        <div>
          <div style={brandStyle}>账号设置</div>
          <div style={subStyle}>看顾好你登入此间的凭信。</div>
        </div>
        <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭账号设置">
          ×
        </button>
      </div>

      {done ? (
        <div className="as-done">
          <div className="as-done-title">改密成功</div>
          <p className="as-done-text">为护你周全，旧凭信已尽数作废。请用新密码重新登入。</p>
          <button type="button" className="as-primary" onClick={onSignOut}>
            重新登入 →
          </button>
        </div>
      ) : (
        <>
          <div className="as-slot-title">修改密码</div>
          <label className="as-field">
            当前密码
            <input
              type="password"
              autoComplete="current-password"
              value={oldPw}
              onChange={(e) => setOldPw(e.target.value)}
              placeholder="输入当前登入密码"
            />
          </label>
          <label className="as-field">
            新密码
            <input
              type="password"
              autoComplete="new-password"
              value={newPw}
              onChange={(e) => setNewPw(e.target.value)}
              placeholder={`至少 ${MIN_PASSWORD_LEN} 位`}
            />
          </label>
          <label className="as-field">
            确认新密码
            <input
              type="password"
              autoComplete="new-password"
              value={confirmPw}
              onChange={(e) => setConfirmPw(e.target.value)}
              placeholder="再输一遍新密码"
            />
          </label>

          {error ? <div className="as-error">{error}</div> : null}

          <button
            type="button"
            className="as-primary"
            disabled={busy}
            onClick={() => void submit()}
          >
            {busy ? "正在改密…" : "确认改密"}
          </button>

          {/* 预留：绑定飞书（即将上线），灰置占位无功能。 */}
          <div className="as-slot-title">第三方绑定</div>
          <button type="button" className="as-disabled" disabled aria-disabled>
            绑定飞书 · 即将上线
          </button>
        </>
      )}
    </aside>
  );
}

// ── 浮层定位（仿 CharterEditor：右侧浮层，墨色宣纸调；内容样式走 fate.css 的 .as-* 类）──
const panelStyle: React.CSSProperties = {
  position: "absolute",
  top: 64,
  right: 12,
  width: 360,
  maxWidth: "94vw",
  maxHeight: "calc(100vh - 96px)",
  overflowY: "auto",
  zIndex: zIndex.rightPanel,
  background: "rgba(245, 236, 220, 0.98)",
  border: "1px solid rgba(140, 100, 50, 0.4)",
  borderRadius: 12,
  boxShadow: "0 10px 30px rgba(60, 44, 27, 0.3)",
  color: "#4a3417",
  padding: 14,
  fontFamily: '"Noto Serif SC", "Songti SC", serif',
  fontSize: 13,
};
const headerStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "flex-start",
  justifyContent: "space-between",
  marginBottom: 8,
};
const brandStyle: React.CSSProperties = { color: "#6b4a22", fontWeight: 700, fontSize: 18 };
const subStyle: React.CSSProperties = { color: "#97825f", fontSize: 11, marginTop: 3 };
const closeBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "transparent",
  border: "none",
  color: "#97825f",
  fontSize: 20,
  lineHeight: 1,
};

export default AccountSettings;
