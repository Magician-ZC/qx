/* 文件说明：编年史时间线面板（编年史「读侧」可视化，对应后端 session.ChronicleFeed / ChronicleMomentByID）。
   把散落的世界事件物化成的编年史条目（击杀 / 死亡 / 继承 / 升级 / 血仇等）渲染成一条倒序时间线——
   每条带回合、类型徽标、叙述文案，并提供「回到那一刻」：点击后展开该条目定位回的 turn + 同主角事件 id 列表
   （onAnchor 把 turn/event 交给主控做战报复盘 / 高亮跳转）。支持「加载更多」无限滚动（ChronicleFeed 的
   has_more/next_offset 游标）。

   依赖注入：本面板不直接 import session/api.ts（避免改 api.ts），而是把读侧调用作为 props 注入——
   fetchChronicle(分页拉取) 必传、resolveMoment(可选，单条「回到那一刻」精确反查) 可选。主控接线时把
   GET /api/sessions/:id/units/:unitId/chronicle 与 …/chronicle/:chronicleId/moment 的 client 适配进来即可。
   纯读、无副作用。中文 UI，复用既有 components 浮层风格（深底金描边、内联样式，仿 BloodFeudPanel）。*/

import { useCallback, useEffect, useState } from "react";
import { ShareCardButton } from "./ShareCardButton";
import { deathCard, biographyCard, type ShareCardOptions } from "../fate/shareCard";
import { zIndex } from "../zindex-tokens";

// ── 后端契约对齐（与 session.ChronicleEntry / MomentAnchor / ChronicleView / ChronicleFeed 的 json tag 一一对应）──
// 这里就地声明以免改 session/types.ts（本面板只编辑自身文件）；字段口径以后端 json tag 为准。

// ChronicleEntry 一条物化后的编年史条目。
export type ChronicleEntry = {
  id: string;
  session_id: string;
  unit_id?: string;
  turn: number;
  kind: string;
  text: string;
  created_at?: string;
};

// MomentAnchor 「回到那一刻」定位结果：条目 → 它发生的 turn + 同 turn/同主角相关事件 id 列表。
export type MomentAnchor = {
  chronicle_id: string;
  unit_id?: string;
  turn: number;
  event_ids?: string[];
};

// ChronicleView 一条编年史条目的完整可渲染视图：条目本身 + 它的「回到那一刻」锚点。
export type ChronicleView = {
  entry: ChronicleEntry;
  anchor: MomentAnchor;
};

// ChronicleFeed 装配好的一页编年史（倒序）+ 分页游标。
export type ChronicleFeed = {
  session_id: string;
  unit_id?: string;
  views: ChronicleView[];
  limit: number;
  offset: number;
  has_more: boolean;
  next_offset?: number;
};

type Props = {
  sessionID: string;
  // unitID 为空 → 读整局编年史总览；非空 → 只读该单位的传记。
  unitID?: string;
  // 标题用的主角名（传记场景）；缺省按整局编年史标题。
  unitName?: string;
  // 每页条数（默认 30）。
  pageSize?: number;
  // fetchChronicle 注入的分页读侧调用：对应 GET …/chronicle?limit=&offset=（返回 ChronicleFeed）。
  fetchChronicle: (params: { sessionID: string; unitID?: string; limit: number; offset: number }) => Promise<ChronicleFeed>;
  // resolveMoment 可选注入：单条「回到那一刻」精确反查（GET …/chronicle/:chronicleId/moment）。
  // 不注入时，「回到那一刻」直接用 Feed 内嵌的 anchor（已够用）；注入则点击时拉最新锚点（事件可能后补）。
  resolveMoment?: (params: { sessionID: string; chronicleID: string }) => Promise<ChronicleView | null>;
  // onAnchor 主控回调：玩家点「回到那一刻」时把定位结果交出去（做战报复盘 / 回合高亮）。不传则仅本地展开。
  onAnchor?: (anchor: MomentAnchor) => void;
  onClose: () => void;
};

// kindMeta 把编年史类型映射为中文标签 + 强调色（与写侧 kind 字面量对齐：kill/death/legacy_inherit/legacy_upgrade…）。
// 未登记的 kind 回退为通用「记事」灰，保证后端新增 kind 也不至于渲染崩。
const kindMeta: Record<string, { label: string; color: string }> = {
  kill: { label: "斩杀", color: "#b4543a" },
  death: { label: "陨落", color: "#8d6fb5" },
  birth: { label: "降生", color: "#5aa06f" },
  battle: { label: "鏖战", color: "#c08a3e" },
  vengeance: { label: "复仇", color: "#b4543a" },
  legacy_inherit: { label: "承继", color: "#c9a227" },
  legacy_upgrade: { label: "传家", color: "#c9a227" },
};
function metaForKind(kind: string): { label: string; color: string } {
  return kindMeta[kind] ?? { label: "记事", color: "#7f8896" };
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
  border: "1px solid rgba(201, 162, 39, 0.4)",
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
const brandStyle: React.CSSProperties = { color: "#d8b84a", fontWeight: 700, fontSize: 14 };
const subStyle: React.CSSProperties = { color: "#9aa0ad", fontSize: 11, marginTop: 2 };
const closeBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "transparent",
  border: "none",
  color: "#9aa0ad",
  fontSize: 18,
  lineHeight: 1,
};
const cardStyle: React.CSSProperties = {
  background: "rgba(32, 36, 48, 0.7)",
  border: "1px solid rgba(255,255,255,0.06)",
  borderLeft: "3px solid #c9a227",
  borderRadius: 8,
  padding: "8px 10px",
  margin: "6px 0",
};
const topRowStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  justifyContent: "space-between",
  gap: 6,
  marginBottom: 4,
};
const kindTagStyle = (color: string): React.CSSProperties => ({
  display: "inline-flex",
  alignItems: "center",
  padding: "1px 8px",
  borderRadius: 999,
  fontSize: 10,
  letterSpacing: 0.4,
  color: "#11131a",
  background: color,
  fontWeight: 700,
});
const turnTagStyle: React.CSSProperties = { color: "#9aa0ad", fontSize: 11 };
const textStyle: React.CSSProperties = { color: "#f0ead8", lineHeight: 1.5, margin: "2px 0 4px" };
const anchorBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "rgba(201, 162, 39, 0.14)",
  border: "1px solid rgba(201, 162, 39, 0.45)",
  color: "#e6cd72",
  borderRadius: 6,
  padding: "3px 9px",
  fontSize: 11,
};
const anchorDetailStyle: React.CSSProperties = {
  marginTop: 6,
  padding: "6px 8px",
  borderRadius: 6,
  background: "rgba(201, 162, 39, 0.08)",
  border: "1px solid rgba(201, 162, 39, 0.25)",
  color: "#cbd1da",
  fontSize: 11,
};
const emptyStyle: React.CSSProperties = {
  background: "rgba(32, 36, 48, 0.7)",
  border: "1px solid rgba(255,255,255,0.06)",
  borderRadius: 8,
  padding: "14px 10px",
  margin: "6px 0",
  color: "#9aa0ad",
  textAlign: "center",
};
const noticeStyle: React.CSSProperties = {
  marginTop: 8,
  padding: "6px 8px",
  borderRadius: 6,
  background: "rgba(180, 84, 58, 0.16)",
  border: "1px solid rgba(180, 84, 58, 0.45)",
  color: "#f0c4b6",
  fontSize: 12,
};
const moreBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  width: "100%",
  marginTop: 6,
  background: "rgba(255,255,255,0.05)",
  border: "1px solid rgba(255,255,255,0.12)",
  color: "#cbd1da",
  borderRadius: 8,
  padding: "7px 0",
  fontSize: 12,
};

// viewKey 给每条视图一个稳定 key（条目 id 唯一）。
function viewKey(v: ChronicleView): string {
  return v.entry.id || `${v.entry.turn}-${v.entry.kind}`;
}

// buildEntryShareCard 把一条编年史条目化成可分享的图片卡：死亡条目用悼卡（death），其余用传记卡（biography）。
// name 取传入的主角名（整局总览无主角名时用「群像」兜底）；副标带类型徽标 + 回合，正文用条目文案。
function buildEntryShareCard(entry: ChronicleEntry, unitName: string | undefined): ShareCardOptions {
  const who = unitName?.trim() || "群像";
  const meta = metaForKind(entry.kind);
  const subtitle = `${meta.label} · 第 ${entry.turn} 回合`;
  if (entry.kind === "death") {
    return deathCard({ name: who, epitaph: entry.text, lineage: subtitle });
  }
  return biographyCard({ name: who, biography: entry.text, subtitle });
}

// ChroniclePanel 编年史时间线浮层：倒序展示某局 / 某角色的编年史条目，逐条可「回到那一刻」。
export function ChroniclePanel({
  sessionID,
  unitID,
  unitName,
  pageSize = 30,
  fetchChronicle,
  resolveMoment,
  onAnchor,
  onClose,
}: Props) {
  const [views, setViews] = useState<ChronicleView[]>([]);
  const [nextOffset, setNextOffset] = useState(0);
  const [hasMore, setHasMore] = useState(false);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState("");
  // expanded 记录哪些条目展开了「回到那一刻」详情；anchorOverride 缓存 resolveMoment 拉到的最新锚点。
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const [anchorOverride, setAnchorOverride] = useState<Record<string, MomentAnchor>>({});

  // load 拉一页：offset=0 为首屏（重置列表），否则追加。best-effort 失败只置 error，不抛。
  const load = useCallback(
    async (offset: number) => {
      if (!sessionID) {
        setViews([]);
        setHasMore(false);
        setLoading(false);
        return;
      }
      if (offset === 0) {
        setLoading(true);
      } else {
        setLoadingMore(true);
      }
      setError("");
      try {
        const feed = await fetchChronicle({ sessionID, unitID, limit: pageSize, offset });
        const incoming = feed?.views ?? [];
        setViews((prev) => (offset === 0 ? incoming : [...prev, ...incoming]));
        setHasMore(Boolean(feed?.has_more));
        setNextOffset(feed?.next_offset ?? offset + incoming.length);
      } catch (err) {
        setError(err instanceof Error ? err.message : String(err));
        if (offset === 0) {
          setViews([]);
          setHasMore(false);
        }
      } finally {
        setLoading(false);
        setLoadingMore(false);
      }
    },
    [sessionID, unitID, pageSize, fetchChronicle],
  );

  // 挂载 / sessionID / unitID 变化时重拉首屏，并清空展开态。
  useEffect(() => {
    setExpanded({});
    setAnchorOverride({});
    void load(0);
  }, [load]);

  // toggleMoment 点「回到那一刻」：展开本地锚点详情；若注入了 resolveMoment 则顺带拉最新锚点（事件可能后补）。
  // 始终把锚点交给主控 onAnchor（做战报复盘 / 回合高亮）。
  const toggleMoment = useCallback(
    async (view: ChronicleView) => {
      const id = view.entry.id;
      const willExpand = !expanded[id];
      setExpanded((prev) => ({ ...prev, [id]: willExpand }));
      if (!willExpand) {
        return;
      }
      let anchor: MomentAnchor = anchorOverride[id] ?? view.anchor;
      if (resolveMoment && !anchorOverride[id]) {
        try {
          const fresh = await resolveMoment({ sessionID, chronicleID: id });
          if (fresh?.anchor) {
            anchor = fresh.anchor;
            setAnchorOverride((prev) => ({ ...prev, [id]: fresh.anchor }));
          }
        } catch {
          // best-effort：精确反查失败就退用 Feed 内嵌锚点，不打断。
        }
      }
      onAnchor?.(anchor);
    },
    [expanded, anchorOverride, resolveMoment, sessionID, onAnchor],
  );

  const title = unitName?.trim() ? `${unitName.trim()} 的传记` : "群像 · 编年史";
  const subtitle = unitName?.trim()
    ? "她走过的路，一笔一笔都记着。点一条可回到那一刻。"
    : "这片土地上发生过的事，按时间倒着翻。点一条可回到那一刻。";

  return (
    <aside style={panelStyle} role="dialog" aria-label="编年史面板">
      <div style={headerStyle}>
        <div>
          <div style={brandStyle}>{title}</div>
          <div style={subStyle}>{subtitle}</div>
        </div>
        <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭编年史面板">
          ×
        </button>
      </div>

      {loading ? (
        <div style={emptyStyle}>正在翻阅那些过往…</div>
      ) : error ? (
        <div style={noticeStyle}>读取编年史失败：{error}</div>
      ) : views.length === 0 ? (
        <div style={emptyStyle}>尚无记述</div>
      ) : (
        <>
          {views.map((view) => {
            const meta = metaForKind(view.entry.kind);
            const id = view.entry.id;
            const anchor = anchorOverride[id] ?? view.anchor;
            const open = Boolean(expanded[id]);
            return (
              <div key={viewKey(view)} style={cardStyle}>
                <div style={topRowStyle}>
                  <span style={kindTagStyle(meta.color)}>{meta.label}</span>
                  <span style={turnTagStyle}>第 {view.entry.turn} 回合</span>
                </div>
                <div style={textStyle}>{view.entry.text}</div>
                <button type="button" style={anchorBtnStyle} onClick={() => void toggleMoment(view)}>
                  {open ? "收起那一刻" : "回到那一刻"}
                </button>
                {open ? (
                  <div style={anchorDetailStyle}>
                    定位到第 {anchor.turn} 回合
                    {anchor.event_ids && anchor.event_ids.length > 0
                      ? `，那一刻有 ${anchor.event_ids.length} 桩相关事件`
                      : "（那一刻的事件已无从追溯，仍可跳到那一回合）"}
                    。
                  </div>
                ) : null}
                {/* 图片卡分享：陨落条目制悼卡（death），其余制传记卡（biography），供截图传播。*/}
                <ShareCardButton compact card={buildEntryShareCard(view.entry, unitName)} />
              </div>
            );
          })}
          {hasMore ? (
            <button
              type="button"
              style={moreBtnStyle}
              disabled={loadingMore}
              onClick={() => void load(nextOffset)}
            >
              {loadingMore ? "加载中…" : "再往前翻"}
            </button>
          ) : null}
        </>
      )}
    </aside>
  );
}

export default ChroniclePanel;
