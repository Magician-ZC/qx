/* 文件说明：商业化面板（充值/SKU 目录 + 已购权益/会员到期 + 账户 LLM 配额仪表）。
   接进主指挥客户端 App.tsx 的浮层面板。数据来自后端 billing 端点（仅 QUNXIANG_BILLING_ENABLED 开时存在，
   关闭则相应端点返回 404，本面板据 APIError.status===404 渲染「商城未开启」而非报错）。
   购买强制已登录（Authorization Bearer），未登录时引导先去登录；platform 取 apple/google，
   receipt 原型传占位 JSON（真机由原生 IAP 层提供收据，此处仅打通链路）。自包含内联样式，参照 FatePanel.tsx。*/

import { useCallback, useEffect, useMemo, useState } from "react";
import {
  APIError,
  getAccountToken,
  getBillingQuota,
  listBillingSKUs,
  listEntitlements,
  purchaseSKU,
} from "../session/api";
import type { BillingSKU, Entitlement } from "../session/types";
import { zIndex } from "../zindex-tokens";

type Props = {
  // accountId 仅用于占位调用（后端实际从 Bearer token 取账户）；通常传当前登录用户 id 或空串。
  accountId?: string;
  // onClose 关闭面板。
  onClose: () => void;
  // onRequireLogin 未登录时点击「先去登录」的回调（由 App 打开登录/账户面板）。可选。
  onRequireLogin?: () => void;
};

// formatYuan 把后端最小货币单位（分）格式化为人民币元字符串。
function formatYuan(cents: number): string {
  return `¥${(cents / 100).toFixed(2)}`;
}

// platformLabel 平台键名转中文。
function platformLabel(p: string): string {
  if (p === "apple") return "App Store";
  if (p === "google") return "Google Play";
  return p;
}

// detectPlatform 简易平台探测：iOS→apple，其余→google（原型默认，真机由原生层下发）。
function detectPlatform(): "apple" | "google" {
  try {
    return /iphone|ipad|ipod|mac os/i.test(navigator.userAgent) ? "apple" : "google";
  } catch {
    return "google";
  }
}

const panelStyle: React.CSSProperties = {
  position: "absolute",
  top: 64,
  right: 12,
  width: 360,
  maxHeight: "calc(100vh - 96px)",
  overflowY: "auto",
  zIndex: zIndex.rightPanel,
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
const skuCardStyle: React.CSSProperties = {
  ...sectionCardStyle,
  display: "flex",
  alignItems: "center",
  justifyContent: "space-between",
  gap: 8,
};
const btnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "rgba(217, 188, 115, 0.14)",
  border: "1px solid rgba(217, 188, 115, 0.5)",
  color: "#f2d98f",
  borderRadius: 6,
  padding: "5px 10px",
  fontSize: 12,
  whiteSpace: "nowrap",
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
const mutedStyle: React.CSSProperties = { color: "#9aa0ad" };

// kindLabel SKU 种类转中文。
function kindLabel(kind: string): string {
  if (kind === "subscription") return "会员订阅";
  if (kind === "one_time") return "永久解锁";
  if (kind === "consumable") return "消耗道具";
  return kind;
}

// BillingPanel 是接进 App 的商业化浮层面板。
export function BillingPanel({ accountId = "", onClose, onRequireLogin }: Props) {
  const loggedIn = getAccountToken().trim() !== "";
  const [skus, setSkus] = useState<BillingSKU[]>([]);
  const [entitlements, setEntitlements] = useState<Entitlement[]>([]);
  const [quotaAllowed, setQuotaAllowed] = useState<boolean | null>(null);
  // disabled=true 表示后端 BILLING_ENABLED 关闭（端点 404），渲染「商城未开启」。
  const [disabled, setDisabled] = useState(false);
  const [loading, setLoading] = useState(false);
  const [buying, setBuying] = useState<string>("");
  const [toast, setToast] = useState("");
  const [err, setErr] = useState("");

  const platform = useMemo(() => detectPlatform(), []);

  const refresh = useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      // SKU 目录无鉴权，可先列；BILLING_ENABLED 关→404。
      const list = await listBillingSKUs();
      setSkus(list);
      setDisabled(false);
    } catch (e) {
      if (e instanceof APIError && e.status === 404) {
        setDisabled(true);
        setLoading(false);
        return;
      }
      setErr(`读取商城失败：${e instanceof Error ? e.message : String(e)}`);
      setLoading(false);
      return;
    }
    // 已登录才拉权益/配额（强制 Bearer）。
    if (loggedIn) {
      try {
        const [ents, quota] = await Promise.all([
          listEntitlements(accountId),
          getBillingQuota(accountId),
        ]);
        setEntitlements(ents);
        setQuotaAllowed(quota.allowed);
      } catch (e) {
        // 权益/配额读取失败不阻断目录展示，仅提示。
        if (!(e instanceof APIError && e.status === 404)) {
          setErr(`读取权益/配额失败：${e instanceof Error ? e.message : String(e)}`);
        }
      }
    }
    setLoading(false);
  }, [accountId, loggedIn]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const onBuy = useCallback(
    async (sku: BillingSKU) => {
      if (!loggedIn) {
        setErr("购买前需先登录账户。");
        return;
      }
      setBuying(sku.id);
      setErr("");
      setToast("");
      try {
        // receipt 原型传占位 JSON——真机由原生 IAP 层（StoreKit / Play Billing）提供真实收据再回传。
        const receipt = JSON.stringify({ prototype: true, sku_id: sku.id, ts: Date.now() });
        const charge = await purchaseSKU(sku.id, platform, receipt);
        setToast(`已下单「${sku.name}」（${formatYuan(charge.amount_cents)}，状态：${charge.status}）。`);
        // 购买后回拉权益/配额。
        await refresh();
      } catch (e) {
        setErr(`购买失败：${e instanceof Error ? e.message : String(e)}`);
      } finally {
        setBuying("");
      }
    },
    [loggedIn, platform, refresh],
  );

  return (
    <aside style={panelStyle} role="dialog" aria-label="商城面板">
      <div style={headerStyle}>
        <div>
          <div style={brandStyle}>一念 · 商城</div>
          <div style={subStyle}>会员订阅 / 永久解锁 / 配额仪表 · 支付走 {platformLabel(platform)}</div>
        </div>
        <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭商城面板">
          ×
        </button>
      </div>

      {disabled ? (
        <div style={{ ...sectionCardStyle, ...mutedStyle }}>商城暂未开启（后端未启用计费）。</div>
      ) : (
        <>
          {/* 配额仪表 */}
          {loggedIn ? (
            <div style={sectionCardStyle}>
              <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between" }}>
                <span style={mutedStyle}>本账号 LLM 配额</span>
                {quotaAllowed === null ? (
                  <span style={mutedStyle}>—</span>
                ) : quotaAllowed ? (
                  <span style={{ color: "#8fd9a0", fontWeight: 600 }}>充足</span>
                ) : (
                  <span style={{ color: "#e6a9a9", fontWeight: 600 }}>已达上限</span>
                )}
              </div>
              {quotaAllowed === false ? (
                <div style={{ ...mutedStyle, fontSize: 11, marginTop: 4 }}>
                  当日 LLM 成本配额已用尽，后续决策将降级为规则推演。订阅会员可提升配额。
                </div>
              ) : null}
            </div>
          ) : (
            <div style={sectionCardStyle}>
              <span style={mutedStyle}>登录后可购买、查看权益与配额。</span>
              {onRequireLogin ? (
                <div style={{ marginTop: 6 }}>
                  <button type="button" style={btnStyle} onClick={onRequireLogin}>
                    先去登录
                  </button>
                </div>
              ) : null}
            </div>
          )}

          {/* SKU 目录 */}
          <div style={slotTitleStyle}>在售目录</div>
          {loading && skus.length === 0 ? (
            <div style={{ ...sectionCardStyle, ...mutedStyle }}>正在加载商品…</div>
          ) : skus.length === 0 ? (
            <div style={{ ...sectionCardStyle, ...mutedStyle }}>暂无在售商品。</div>
          ) : (
            skus.map((sku) => (
              <div key={sku.id} style={skuCardStyle}>
                <div style={{ minWidth: 0 }}>
                  <div style={{ fontWeight: 600, color: "#f0ead8" }}>{sku.name}</div>
                  <div style={{ ...mutedStyle, fontSize: 11 }}>
                    {kindLabel(sku.kind)}
                    {sku.period ? ` · ${sku.period}` : ""} · {formatYuan(sku.price_cents)}
                  </div>
                </div>
                <button
                  type="button"
                  style={{ ...btnStyle, opacity: !sku.active || buying === sku.id ? 0.6 : 1 }}
                  disabled={!sku.active || buying === sku.id}
                  onClick={() => void onBuy(sku)}
                >
                  {buying === sku.id ? "处理中…" : sku.active ? "购买" : "已下架"}
                </button>
              </div>
            ))
          )}

          {/* 已购权益 / 会员到期 */}
          {loggedIn ? (
            <>
              <div style={slotTitleStyle}>我的权益</div>
              {entitlements.length === 0 ? (
                <div style={{ ...sectionCardStyle, ...mutedStyle }}>尚无已购权益。</div>
              ) : (
                entitlements.map((ent, i) => (
                  <div key={`${ent.sku_id}-${i}`} style={sectionCardStyle}>
                    <div style={{ display: "flex", justifyContent: "space-between", gap: 8 }}>
                      <span style={{ color: "#f0ead8" }}>{ent.sku_id}</span>
                      <span style={{ color: ent.status === "active" ? "#8fd9a0" : "#9aa0ad" }}>
                        {ent.status === "active" ? "生效中" : ent.status}
                      </span>
                    </div>
                    {ent.expires_at ? (
                      <div style={{ ...mutedStyle, fontSize: 11, marginTop: 2 }}>到期：{ent.expires_at}</div>
                    ) : null}
                  </div>
                ))
              )}
            </>
          ) : null}
        </>
      )}

      {toast ? <div style={toastStyle}>{toast}</div> : null}
      {err ? <div style={errToastStyle}>{err}</div> : null}
    </aside>
  );
}

export default BillingPanel;
