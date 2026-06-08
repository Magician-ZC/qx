/* 文件说明：出海合规 UI——实名认证流（真名+身份证号）+ 生日登记（→未成年模式）+ 合规前置门裁决展示
   （allowed/minor_mode/reason），以及给 App 在建局/advance-phase 收到 403 时弹的拦截横幅
   ComplianceBlockedBanner（按 reason 渲染「需实名/宵禁时段/今日时长已达上限」并引导去实名）。
   合规端点强制已登录（Authorization Bearer），仅 QUNXIANG_COMPLIANCE_ENABLED 开时存在（关→404，
   据 APIError.status===404 渲染「合规未开启」）。自包含内联样式，参照 FatePanel.tsx / BillingPanel.tsx。

   导出三个件：
   - ComplianceGatePanel：完整合规面板（裁决 + 实名表单 + 生日登记），接进 App 浮层。
   - RealnameForm：可独立复用的实名/生日表单。
   - ComplianceBlockedBanner：建局/推进 403 拦截横幅（接收 reason，渲染文案 + 去实名按钮）。
   另导出 classifyComplianceReason 把后端 reason 文本归一为四类，供 App/横幅复用。*/

import { useCallback, useEffect, useState } from "react";
import {
  APIError,
  getAccountToken,
  getComplianceGate,
  verifyCompliance,
} from "../session/api";
import type { ComplianceGate } from "../session/types";

// ComplianceBlockKind 是合规拦截的归一类别（据后端 reason 文本启发式判定）。
export type ComplianceBlockKind = "realname" | "curfew" | "playtime" | "minor" | "unknown";

// classifyComplianceReason 把后端裁决 reason 文本归一为拦截类别（用于选文案/选引导动作）。
export function classifyComplianceReason(reason: string): ComplianceBlockKind {
  const r = (reason ?? "").toLowerCase();
  if (r.includes("realname") || r.includes("real_name") || r.includes("verify") || reason.includes("实名"))
    return "realname";
  if (r.includes("curfew") || r.includes("night") || reason.includes("宵禁") || reason.includes("夜间"))
    return "curfew";
  if (r.includes("playtime") || r.includes("duration") || r.includes("time_limit") || reason.includes("时长") || reason.includes("防沉迷"))
    return "playtime";
  if (r.includes("minor") || reason.includes("未成年")) return "minor";
  return "unknown";
}

// blockCopy 据拦截类别给出玩家可读文案（标题 + 说明）。
function blockCopy(kind: ComplianceBlockKind, rawReason: string): { title: string; detail: string } {
  switch (kind) {
    case "realname":
      return { title: "需完成实名认证", detail: "根据出海合规要求，进入游戏前请先完成实名认证。" };
    case "curfew":
      return {
        title: "当前为未成年宵禁时段",
        detail: "未成年用户在每日 22:00 至次日 8:00 之间无法游玩，请稍后再来。",
      };
    case "playtime":
      return { title: "今日游玩时长已达上限", detail: "未成年用户今日可游玩时长已用尽，请明日再来。" };
    case "minor":
      return { title: "未成年模式限制", detail: "当前账号处于未成年模式，部分功能受到限制。" };
    default:
      return { title: "合规校验未通过", detail: rawReason || "请稍后再试或联系客服。" };
  }
}

const panelStyle: React.CSSProperties = {
  position: "absolute",
  top: 64,
  right: 12,
  width: 340,
  maxHeight: "calc(100vh - 96px)",
  overflowY: "auto",
  zIndex: 42,
  background: "rgba(18, 20, 28, 0.95)",
  border: "1px solid rgba(217, 188, 115, 0.35)",
  borderRadius: 10,
  boxShadow: "0 8px 28px rgba(0,0,0,0.45)",
  color: "#e8e2d2",
  padding: 12,
  fontSize: 13,
};
const headerStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  justifyContent: "space-between",
  marginBottom: 8,
};
const brandStyle: React.CSSProperties = { color: "#f2d98f", fontWeight: 700, fontSize: 14 };
const subStyle: React.CSSProperties = { color: "#9aa0ad", fontSize: 11, marginTop: 2 };
const slotTitleStyle: React.CSSProperties = {
  color: "#cdb98a",
  fontSize: 11,
  letterSpacing: 0.5,
  margin: "12px 0 4px",
  textTransform: "uppercase",
};
const sectionCardStyle: React.CSSProperties = {
  background: "rgba(32, 36, 48, 0.7)",
  border: "1px solid rgba(255,255,255,0.06)",
  borderRadius: 8,
  padding: "8px 10px",
  margin: "4px 0",
};
const inputStyle: React.CSSProperties = {
  width: "100%",
  margin: "4px 0",
  background: "rgba(32, 36, 48, 0.9)",
  color: "#e8e2d2",
  border: "1px solid rgba(255,255,255,0.12)",
  borderRadius: 6,
  padding: "6px 8px",
  fontSize: 12,
  boxSizing: "border-box",
};
const labelStyle: React.CSSProperties = { color: "#9aa0ad", fontSize: 11, margin: "6px 0 0" };
const btnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "rgba(217, 188, 115, 0.14)",
  border: "1px solid rgba(217, 188, 115, 0.5)",
  color: "#f2d98f",
  borderRadius: 6,
  padding: "6px 10px",
  fontSize: 12,
  marginTop: 6,
};
const closeBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "transparent",
  border: "none",
  color: "#9aa0ad",
  fontSize: 18,
  lineHeight: 1,
};
const toastStyle: React.CSSProperties = {
  marginTop: 8,
  padding: "6px 8px",
  borderRadius: 6,
  background: "rgba(111, 141, 181, 0.18)",
  border: "1px solid rgba(111, 141, 181, 0.45)",
  color: "#cdd7e6",
  fontSize: 12,
};
const errToastStyle: React.CSSProperties = {
  ...toastStyle,
  background: "rgba(181, 91, 91, 0.18)",
  border: "1px solid rgba(181, 91, 91, 0.5)",
  color: "#e6c9c9",
};
const okToastStyle: React.CSSProperties = {
  ...toastStyle,
  background: "rgba(111, 181, 130, 0.18)",
  border: "1px solid rgba(111, 181, 130, 0.5)",
  color: "#c9e6cf",
};
const mutedStyle: React.CSSProperties = { color: "#9aa0ad" };

type RealnameFormProps = {
  // onVerified 实名/生日登记成功后的回调（参数为后端返回的 ok）。可选——通常用于回拉裁决。
  onVerified?: (ok: boolean) => void;
};

// RealnameForm 是可独立复用的实名认证 + 生日登记表单。强制已登录（Bearer），未登录提示先登录。
export function RealnameForm({ onVerified }: RealnameFormProps) {
  const loggedIn = getAccountToken().trim() !== "";
  const [name, setName] = useState("");
  const [idNumber, setIdNumber] = useState("");
  const [birthDate, setBirthDate] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [ok, setOk] = useState("");
  const [err, setErr] = useState("");

  const submit = useCallback(async () => {
    if (!loggedIn) {
      setErr("实名登记前需先登录账户。");
      return;
    }
    if (name.trim() === "" && idNumber.trim() === "" && birthDate.trim() === "") {
      setErr("请至少填写实名信息或出生日期之一。");
      return;
    }
    setSubmitting(true);
    setErr("");
    setOk("");
    try {
      const result = await verifyCompliance({
        name: name.trim() || undefined,
        idNumber: idNumber.trim() || undefined,
        birthDate: birthDate.trim() || undefined,
      });
      if (result) {
        setOk("已提交，合规信息核验通过。");
      } else {
        setErr("核验未通过，请检查姓名与身份证号是否一致。");
      }
      onVerified?.(result);
    } catch (e) {
      if (e instanceof APIError && e.status === 404) {
        setErr("合规模块未开启。");
      } else {
        setErr(`提交失败：${e instanceof Error ? e.message : String(e)}`);
      }
    } finally {
      setSubmitting(false);
    }
  }, [loggedIn, name, idNumber, birthDate, onVerified]);

  return (
    <div>
      {!loggedIn ? (
        <div style={{ ...sectionCardStyle, ...mutedStyle }}>请先登录账户后再进行实名认证。</div>
      ) : null}
      <div style={labelStyle}>真实姓名</div>
      <input
        style={inputStyle}
        value={name}
        onChange={(e) => setName(e.target.value)}
        placeholder="与证件一致的真名"
        autoComplete="off"
      />
      <div style={labelStyle}>身份证号</div>
      <input
        style={inputStyle}
        value={idNumber}
        onChange={(e) => setIdNumber(e.target.value)}
        placeholder="18 位身份证号（仅用于核验，不留存）"
        autoComplete="off"
        inputMode="text"
      />
      <div style={labelStyle}>出生日期（用于未成年模式判定）</div>
      <input
        style={inputStyle}
        value={birthDate}
        onChange={(e) => setBirthDate(e.target.value)}
        placeholder="YYYY-MM-DD"
        type="date"
      />
      <button
        type="button"
        style={{ ...btnStyle, opacity: submitting ? 0.6 : 1 }}
        disabled={submitting}
        onClick={() => void submit()}
      >
        {submitting ? "提交中…" : "提交实名认证"}
      </button>
      {ok ? <div style={okToastStyle}>{ok}</div> : null}
      {err ? <div style={errToastStyle}>{err}</div> : null}
    </div>
  );
}

type ComplianceGatePanelProps = {
  // accountId 仅占位（后端从 Bearer token 取账户）；通常传当前登录用户 id 或空串。
  accountId?: string;
  onClose: () => void;
  onRequireLogin?: () => void;
};

// gateBadge 据裁决渲染状态徽标。
function gateBadge(gate: ComplianceGate): { text: string; color: string } {
  if (gate.allowed && !gate.minor_mode) return { text: "可正常游玩", color: "#8fd9a0" };
  if (gate.allowed && gate.minor_mode) return { text: "未成年模式（受限可玩）", color: "#e6cf8f" };
  return { text: "当前受限", color: "#e6a9a9" };
}

// ComplianceGatePanel 是接进 App 的合规面板：展示当前裁决 + 实名/生日登记。
export function ComplianceGatePanel({ accountId = "", onClose, onRequireLogin }: ComplianceGatePanelProps) {
  const loggedIn = getAccountToken().trim() !== "";
  const [gate, setGate] = useState<ComplianceGate | null>(null);
  const [disabled, setDisabled] = useState(false);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState("");

  const refreshGate = useCallback(async () => {
    if (!loggedIn) {
      setGate(null);
      return;
    }
    setLoading(true);
    setErr("");
    try {
      const g = await getComplianceGate(accountId);
      setGate(g);
      setDisabled(false);
    } catch (e) {
      if (e instanceof APIError && e.status === 404) {
        setDisabled(true);
      } else {
        setErr(`读取合规裁决失败：${e instanceof Error ? e.message : String(e)}`);
      }
    } finally {
      setLoading(false);
    }
  }, [accountId, loggedIn]);

  useEffect(() => {
    void refreshGate();
  }, [refreshGate]);

  const badge = gate ? gateBadge(gate) : null;
  const blockKind = gate && !gate.allowed ? classifyComplianceReason(gate.reason) : null;
  const copy = blockKind ? blockCopy(blockKind, gate?.reason ?? "") : null;

  return (
    <aside style={panelStyle} role="dialog" aria-label="合规面板">
      <div style={headerStyle}>
        <div>
          <div style={brandStyle}>群像 · 合规</div>
          <div style={subStyle}>实名认证 · 未成年保护 · 防沉迷</div>
        </div>
        <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭合规面板">
          ×
        </button>
      </div>

      {disabled ? (
        <div style={{ ...sectionCardStyle, ...mutedStyle }}>合规模块暂未开启（后端未启用合规门）。</div>
      ) : !loggedIn ? (
        <div style={sectionCardStyle}>
          <span style={mutedStyle}>登录后可查看合规裁决并进行实名认证。</span>
          {onRequireLogin ? (
            <div>
              <button type="button" style={btnStyle} onClick={onRequireLogin}>
                先去登录
              </button>
            </div>
          ) : null}
        </div>
      ) : (
        <>
          {/* 当前裁决 */}
          <div style={slotTitleStyle}>当前裁决</div>
          {loading && !gate ? (
            <div style={{ ...sectionCardStyle, ...mutedStyle }}>正在读取…</div>
          ) : gate ? (
            <div style={sectionCardStyle}>
              <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
                <span style={mutedStyle}>状态</span>
                {badge ? <span style={{ color: badge.color, fontWeight: 600 }}>{badge.text}</span> : null}
              </div>
              {copy ? (
                <div style={{ marginTop: 6 }}>
                  <div style={{ color: "#e6c9a9", fontWeight: 600 }}>{copy.title}</div>
                  <div style={{ ...mutedStyle, fontSize: 11, marginTop: 2 }}>{copy.detail}</div>
                </div>
              ) : null}
              {gate.minor_mode ? (
                <div style={{ ...mutedStyle, fontSize: 11, marginTop: 6 }}>
                  未成年模式已开启：受宵禁与每日时长限制。
                </div>
              ) : null}
            </div>
          ) : (
            <div style={{ ...sectionCardStyle, ...mutedStyle }}>暂无裁决信息。</div>
          )}

          {/* 实名 / 生日登记 */}
          <div style={slotTitleStyle}>实名认证 / 生日登记</div>
          <div style={sectionCardStyle}>
            <RealnameForm onVerified={() => void refreshGate()} />
          </div>
        </>
      )}

      {err ? <div style={errToastStyle}>{err}</div> : null}
    </aside>
  );
}

type ComplianceBlockedBannerProps = {
  // reason 是后端建局/advance-phase 返回的 403 {reason}（透出自 APIError.reason）。
  reason: string;
  // onClose 关闭横幅。
  onClose: () => void;
  // onGoRealname 点击「去实名认证」——由 App 打开 ComplianceGatePanel / RealnameForm。可选。
  onGoRealname?: () => void;
};

const bannerOverlayStyle: React.CSSProperties = {
  position: "fixed",
  inset: 0,
  zIndex: 60,
  display: "flex",
  alignItems: "center",
  justifyContent: "center",
  background: "rgba(0,0,0,0.55)",
};
const bannerCardStyle: React.CSSProperties = {
  width: 360,
  maxWidth: "calc(100vw - 32px)",
  background: "rgba(22, 18, 18, 0.97)",
  border: "1px solid rgba(181, 91, 91, 0.55)",
  borderRadius: 12,
  boxShadow: "0 12px 40px rgba(0,0,0,0.6)",
  color: "#e8e2d2",
  padding: 18,
  fontSize: 13,
};

// ComplianceBlockedBanner 是 App 在建局/推进收到合规 403 时弹的模态拦截横幅。
// App 用法：catch(e){ if((e as APIError).status===403) setBlockReason((e as APIError).reason ?? "") }
// 然后 {blockReason && <ComplianceBlockedBanner reason={blockReason} onClose={...} onGoRealname={...} />}
export function ComplianceBlockedBanner({ reason, onClose, onGoRealname }: ComplianceBlockedBannerProps) {
  const kind = classifyComplianceReason(reason);
  const copy = blockCopy(kind, reason);
  // 仅「需实名」类提供去实名引导（宵禁/时长无法通过实名解除）。
  const showRealnameCTA = kind === "realname" && typeof onGoRealname === "function";

  return (
    <div style={bannerOverlayStyle} role="alertdialog" aria-label="合规拦截">
      <div style={bannerCardStyle}>
        <div style={{ color: "#f2c98f", fontWeight: 700, fontSize: 15, marginBottom: 8 }}>{copy.title}</div>
        <div style={{ ...mutedStyle, lineHeight: 1.5 }}>{copy.detail}</div>
        <div style={{ display: "flex", gap: 8, marginTop: 16, justifyContent: "flex-end" }}>
          {showRealnameCTA ? (
            <button
              type="button"
              style={btnStyle}
              onClick={() => {
                onGoRealname?.();
                onClose();
              }}
            >
              去实名认证
            </button>
          ) : null}
          <button
            type="button"
            style={{ ...btnStyle, background: "rgba(255,255,255,0.06)", borderColor: "rgba(255,255,255,0.18)", color: "#cbd1da" }}
            onClick={onClose}
          >
            知道了
          </button>
        </div>
      </div>
    </div>
  );
}

export default ComplianceGatePanel;
