/* 文件说明：跨玩家同意收件箱浮层（C-CONSENT）。
   一个角色（target unit）会收到来自别的玩家世界的「跨玩家社交意图」请求——结识/结盟/交易/
   联姻/反目/复仇/开战。这些后果层互动需要被请求方先点头才生效（异步同意），故由其玩家
   在此逐条「接受/拒绝」（resolveConsent）。另有「世界事件主动同步」按钮：把世界总线上与该
   角色有关的跨玩家事件主动拉进她的命运收件箱（surfaceCrossEvents），告知「N 件与你相关的
   世界之事已惊动」。鉴权：consent 走玩家档路由 /api/fate/consent/*（强制 Bearer，后端按账号→角色归属校验，
   只能列/处理 target 属本账号角色的 pending），不再复用 ops/GM 的 X-Ops-Token 路由——生产配 OPS_TOKEN 后玩家仍可用。
   祖魂语气：玩家是替后人拿主意的先祖，措辞不出现「命令/操控」。自包含内联样式。*/

import { useCallback, useEffect, useMemo, useState } from "react";
import {
  listPendingConsents,
  resolveConsent,
  surfaceCrossEvents,
} from "../session/api";
import type { ConsentRequest } from "../session/types";
import { zIndex } from "../zindex-tokens";

type Props = {
  // unitId 是收件人（被请求方）角色 ID；同意请求按 target_unit_id 归集到她名下。
  unitId: string;
  // worldId 用于「世界事件主动同步」；缺省则隐藏该按钮（跨玩家事件需世界归属才可拉取）。
  worldId?: string;
  // unitName 供标题展示（可选，缺省用「她」）。
  unitName?: string;
  onClose: () => void;
};

// interaction 类型中文映射（与后端 interaction 枚举对齐）。
const INTERACTION_LABELS: Record<string, string> = {
  acquaint: "结识",
  alliance: "结盟",
  trade: "交易",
  marriage: "联姻",
  fallout: "反目",
  vengeance: "复仇",
  war: "开战",
};

function interactionLabel(interaction: string): string {
  return INTERACTION_LABELS[interaction] ?? interaction;
}

// 后果层分档（tier）中文提示，越高越重。
function tierLabel(tier: string): string {
  const t = tier.toLowerCase();
  if (t.includes("high") || t === "3") return "重大后果";
  if (t.includes("mid") || t.includes("medium") || t === "2") return "牵动局面";
  if (t.includes("low") || t === "1") return "无伤大雅";
  return tier;
}

const panelStyle: React.CSSProperties = {
  position: "absolute",
  top: 64,
  right: 12,
  width: 340,
  maxHeight: "calc(100vh - 96px)",
  overflowY: "auto",
  zIndex: zIndex.rightPanel,
  background: "rgba(18, 20, 28, 0.94)",
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
const subStyle: React.CSSProperties = { color: "#9aa0ad", fontSize: 11, marginTop: 2, lineHeight: 1.4 };
const slotTitleStyle: React.CSSProperties = {
  color: "#cdb98a",
  fontSize: 11,
  letterSpacing: 0.5,
  margin: "10px 0 4px",
  textTransform: "uppercase",
};
const reqCardStyle: React.CSSProperties = {
  background: "rgba(40, 34, 18, 0.7)",
  border: "1px solid rgba(217, 188, 115, 0.5)",
  borderRadius: 8,
  padding: "8px 10px",
  margin: "6px 0",
};
const emptyCardStyle: React.CSSProperties = {
  background: "rgba(32, 36, 48, 0.7)",
  border: "1px solid rgba(255,255,255,0.06)",
  borderRadius: 8,
  padding: "10px 12px",
  margin: "4px 0",
  color: "#9aa0ad",
};
const actionRowStyle: React.CSSProperties = { display: "flex", gap: 6, marginTop: 8 };
const acceptBtnStyle: React.CSSProperties = {
  flex: "1 1 auto",
  cursor: "pointer",
  background: "rgba(120, 180, 130, 0.16)",
  border: "1px solid rgba(120, 180, 130, 0.5)",
  color: "#bfe6c6",
  borderRadius: 6,
  padding: "5px 6px",
  fontSize: 12,
};
const rejectBtnStyle: React.CSSProperties = {
  flex: "1 1 auto",
  cursor: "pointer",
  background: "rgba(200, 110, 110, 0.14)",
  border: "1px solid rgba(200, 110, 110, 0.5)",
  color: "#e6bcbc",
  borderRadius: 6,
  padding: "5px 6px",
  fontSize: 12,
};
const syncBtnStyle: React.CSSProperties = {
  width: "100%",
  cursor: "pointer",
  background: "rgba(111, 141, 181, 0.16)",
  border: "1px solid rgba(111, 141, 181, 0.5)",
  color: "#cdd7e6",
  borderRadius: 6,
  padding: "6px 8px",
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
const pillStyle: React.CSSProperties = {
  display: "inline-block",
  fontSize: 10,
  letterSpacing: 0.4,
  padding: "1px 6px",
  borderRadius: 999,
  background: "rgba(217, 188, 115, 0.16)",
  border: "1px solid rgba(217, 188, 115, 0.4)",
  color: "#f2d98f",
  marginLeft: 6,
};

// ConsentInbox 是接进 App 的跨玩家同意收件箱浮层（scoped 到一个角色）。
export function ConsentInbox({ unitId, worldId, unitName, onClose }: Props) {
  const [requests, setRequests] = useState<ConsentRequest[]>([]);
  const [loading, setLoading] = useState(false);
  const [resolving, setResolving] = useState<string>("");
  const [syncing, setSyncing] = useState(false);
  const [toast, setToast] = useState("");

  const who = unitName?.trim() || "她";

  const refresh = useCallback(async () => {
    if (!unitId) {
      setRequests([]);
      return;
    }
    setLoading(true);
    try {
      const pending = await listPendingConsents(unitId);
      // 只留真正待处理的（防御性：后端可能混入已处理）。
      setRequests(pending.filter((r) => (r.status ?? "pending") === "pending"));
    } catch (err) {
      setToast(`读取请求失败：${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setLoading(false);
    }
  }, [unitId]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const onResolve = useCallback(
    async (reqID: string, accept: boolean, label: string) => {
      if (!reqID) return;
      setResolving(reqID);
      try {
        await resolveConsent(reqID, accept);
        setRequests((prev) => prev.filter((r) => r.id !== reqID));
        setToast(accept ? `你替${who}应下了这桩${label}。` : `你替${who}回绝了这桩${label}。`);
      } catch (err) {
        setToast(`没能传达：${err instanceof Error ? err.message : String(err)}`);
      } finally {
        setResolving("");
      }
    },
    [who],
  );

  const onSyncWorld = useCallback(async () => {
    if (!worldId || !unitId) return;
    setSyncing(true);
    try {
      const surfaced = await surfaceCrossEvents(worldId, unitId, 8);
      if (surfaced > 0) {
        setToast(`${surfaced} 件与你相关的世界之事已惊动。去命运收件箱看看。`);
      } else {
        setToast("世界暂无与你相关的新动静。");
      }
      // 同步后顺带刷新同意列表（世界事件可能伴生新的同意请求）。
      await refresh();
    } catch (err) {
      setToast(`惊动世界失败：${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setSyncing(false);
    }
  }, [worldId, unitId, refresh]);

  const headerSub = useMemo(() => {
    if (loading) return "正在收拢各方来意…";
    if (requests.length === 0) return "眼下没有谁来求你点头。";
    return `有 ${requests.length} 桩牵涉${who}的事，等你拿主意。`;
  }, [loading, requests.length, who]);

  return (
    <aside style={panelStyle} role="dialog" aria-label="跨玩家同意收件箱">
      <div style={headerStyle}>
        <div>
          <div style={brandStyle}>一念 · 来意</div>
          <div style={subStyle}>{headerSub}</div>
        </div>
        <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭来意收件箱">
          ×
        </button>
      </div>

      {/* 世界事件主动同步 */}
      {worldId ? (
        <button
          type="button"
          style={{ ...syncBtnStyle, opacity: syncing ? 0.6 : 1, cursor: syncing ? "default" : "pointer" }}
          disabled={syncing}
          onClick={() => void onSyncWorld()}
        >
          {syncing ? "正在惊动世界…" : "惊动世界 · 把与她相关的事拉进来"}
        </button>
      ) : null}

      {/* 待处理同意请求 */}
      <div style={slotTitleStyle}>有人想与你的人来往</div>
      {requests.length === 0 ? (
        <div style={emptyCardStyle}>
          {loading ? "正在收拢各方来意…" : "暂无人来求你点头。世界往前走走，自会有人寻来。"}
        </div>
      ) : (
        requests.map((req) => {
          const label = interactionLabel(req.interaction);
          const busy = resolving === req.id;
          return (
            <div key={req.id} style={reqCardStyle}>
              <div style={{ fontWeight: 600, color: "#f0ead8" }}>
                有人欲与{who}「{label}」
                <span style={pillStyle}>{tierLabel(req.tier)}</span>
              </div>
              <div style={{ color: "#9aa0ad", fontSize: 11, marginTop: 4 }}>
                来自一位远方的人（{req.actor_unit_id}）
              </div>
              <div style={actionRowStyle}>
                <button
                  type="button"
                  style={{ ...acceptBtnStyle, opacity: busy ? 0.6 : 1 }}
                  disabled={busy}
                  onClick={() => void onResolve(req.id, true, label)}
                >
                  应下
                </button>
                <button
                  type="button"
                  style={{ ...rejectBtnStyle, opacity: busy ? 0.6 : 1 }}
                  disabled={busy}
                  onClick={() => void onResolve(req.id, false, label)}
                >
                  回绝
                </button>
              </div>
            </div>
          );
        })
      )}

      {toast ? <div style={toastStyle}>{toast}</div> : null}
    </aside>
  );
}

export default ConsentInbox;
