/* 文件说明：命运地图舞台（观战模式）——把战棋的 PixiBoard 搬进命运客户端作她日常生活的主舞台。
   她随世界推进在六边形格子上移动；玩家**只是垂看的先祖**，不下令、不操控，故这里是纯观战：
   - commanderFactionID 固定 "player"（按她的阵营着色），fogPerspectiveUnitID 留空（命运主世界雾已关、全可见），
   - onTileClick 不再下令，只弹一张只读「这是谁」浮卡（lineage/faction），selectedTileCoord 用于浮卡定位/高亮。
   数据来自 GET /api/sessions/:id 的整块快照（getSession），与 FateView 文字命运卡同源同一会话。
   刷新节奏：自身挂载即拉一次 + 每隔若干秒轻量轮询；另接受 refreshSignal——父层在「世界往前走一拍」执行完后
   bump 该值，FateBoard 即重拉快照，board 随她移动重渲（PixiBoard 数据变化自动重渲）。
   祖魂语气/宣纸墨色：本文件不可改 fate.css/styles.css，故浮卡等用内联样式贴合 .fate-* 墨色调；绝不出现指挥/下令 UI。*/

import { Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { getSession } from "../session/api";
import type { BattleUnit, SessionSnapshot } from "../session/types";

// LazyPixiBoard 与 App.tsx 同款懒加载 PixiBoard：让 Pixi 战场代码留在独立 chunk（不并进命运首屏主包），
// 既复用 App 的代码分割收益，也消除「同一模块被静态+动态双导入」的打包合并告警。
const LazyPixiBoard = lazy(() => import("../game/PixiBoard").then((m) => ({ default: m.PixiBoard })));

type Props = {
  sessionId: string;
  // unitId：主角的单位 ID，仅用于在浮卡里标注「这就是她」。观战不依赖它做视野（雾已关）。
  unitId: string;
  // refreshSignal：父层每推世界往前一拍并执行完后 bump 此值，FateBoard 据此重拉快照让 board 随她移动重渲。
  // 不传则只靠自身轮询刷新。
  refreshSignal?: number;
};

// FACTION_NAME_ZH 把阵营 id 译成中文名（与 FateView 同口径，未知 id 回落原串）。
const FACTION_NAME_ZH: Record<string, string> = {
  freedom: "自由",
  order: "秩序",
  chaos: "混乱",
};

function factionNameZH(id: string): string {
  const key = (id ?? "").trim().toLowerCase();
  return FACTION_NAME_ZH[key] ?? (id ?? "").trim();
}

// BOARD_POLL_MS：观战自身轮询间隔。她在执行阶段被唤醒移动后，board 至多滞后这一拍即追平。
// 取较慢节奏（避免高频拉整快照增成本）；父层 refreshSignal 才是「这拍刚跑完，立刻看她在哪」的精确刷新。
const BOARD_POLL_MS = 8000;

// 「这是谁」只读浮卡的内联样式（墨色宣纸调，叠在 PixiBoard 之上）。本文件不可改 css，故内联。
const whoCardStyle: React.CSSProperties = {
  position: "absolute",
  top: 12,
  left: 12,
  zIndex: 5,
  maxWidth: 240,
  padding: "10px 14px",
  borderRadius: 10,
  background: "rgba(250, 244, 232, 0.96)",
  border: "1px solid rgba(120, 90, 50, 0.4)",
  boxShadow: "0 6px 20px rgba(60, 44, 27, 0.22)",
  color: "#4a3417",
  fontFamily: '"Noto Serif SC", "Songti SC", serif',
  fontSize: 13,
  lineHeight: 1.7,
};
const whoCardNameStyle: React.CSSProperties = {
  fontSize: 16,
  color: "#6b4a22",
  marginBottom: 2,
};
const whoCardLineStyle: React.CSSProperties = {
  fontSize: 12,
  color: "#8a7556",
};
const whoCardCloseStyle: React.CSSProperties = {
  position: "absolute",
  top: 6,
  right: 8,
  border: "none",
  background: "transparent",
  color: "#a08a60",
  fontSize: 16,
  cursor: "pointer",
  lineHeight: 1,
  padding: 2,
};

// 舞台容器内联样式：相对定位以承载浮卡；高度撑满让 PixiBoard 的 resizeTo 拿到可视面积。
const boardWrapStyle: React.CSSProperties = {
  position: "relative",
  width: "100%",
  minHeight: 360,
  borderRadius: 12,
  overflow: "hidden",
  border: "1px solid rgba(120, 90, 50, 0.28)",
  boxShadow: "0 4px 18px rgba(120, 90, 50, 0.1)",
};

// 懒加载 PixiBoard 期间的占位（墨色宣纸调）。
const boardLoadingStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  justifyContent: "center",
  width: "100%",
  minHeight: 360,
  color: "#8a7556",
  fontFamily: '"Noto Serif SC", "Songti SC", serif',
  fontSize: 14,
  letterSpacing: "0.08em",
};

// allUnitsOf 把一份快照里四类单位（玩家/敌方/据点 NPC/野外散人）汇成一张「坐标 → 单位」可查表，
// 供点格子时定位「这格上站的是谁」。同格多单位取第一个（观战只读，足够回答「这是谁」）。
function allUnitsOf(snap: SessionSnapshot | null): BattleUnit[] {
  if (!snap) return [];
  return [
    ...(snap.player_units ?? []),
    ...(snap.enemy_units ?? []),
    ...(snap.ambient_units ?? []),
    ...(snap.wild_units ?? []),
  ];
}

// WhoInfo 是只读浮卡要展示的「这是谁」——名字 + 称谓（lineage）+ 阵营 + 是否就是她。
type WhoInfo = {
  name: string;
  lineage: string;
  faction: string;
  isHer: boolean;
};

export function FateBoard({ sessionId, unitId, refreshSignal }: Props) {
  const [snap, setSnap] = useState<SessionSnapshot | null>(null);
  // selected：当前点选的格子（用于 PixiBoard 高亮 + 浮卡定位锚点）。
  const [selected, setSelected] = useState<{ q: number; r: number } | null>(null);
  // who：只读「这是谁」浮卡内容；null=不展示。
  const [who, setWho] = useState<WhoInfo | null>(null);
  // mountedRef 守卫异步拉取返回时组件已卸载就不再 setState。
  const mountedRef = useRef(true);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  const refresh = useCallback(async () => {
    try {
      const res = await getSession(sessionId);
      if (mountedRef.current) setSnap(res.session);
    } catch {
      // 观战快照拉取失败不致命（轮询/下次 refreshSignal 会再试），保持上一帧、不打断她的舞台。
    }
  }, [sessionId]);

  // 挂载即拉一次；父层 refreshSignal 变化（世界往前走一拍执行完）时重拉，让 board 随她移动追平。
  useEffect(() => {
    void refresh();
  }, [refresh, refreshSignal]);

  // 自身轻量轮询：即便玩家不指引、父层不 bump，她若在执行阶段自己移动了，board 也会至多滞后一拍追平。
  useEffect(() => {
    const timer = window.setInterval(() => void refresh(), BOARD_POLL_MS);
    return () => window.clearInterval(timer);
  }, [refresh]);

  // 点格子（观战不下令）：查这格上站的是谁，弹只读浮卡；空格则收起浮卡、清选中。
  const onTileClick = useCallback(
    (q: number, r: number) => {
      const units = allUnitsOf(snap);
      const hit = units.find((u) => u.status.position_q === q && u.status.position_r === r);
      if (!hit) {
        setSelected(null);
        setWho(null);
        return;
      }
      setSelected({ q, r });
      setWho({
        name: hit.identity?.name || hit.identity?.nickname || "无名之人",
        lineage: hit.identity?.lineage ?? "",
        faction: hit.faction_id ?? "",
        isHer: hit.id === unitId,
      });
    },
    [snap, unitId],
  );

  // commanderFactionID：按她所属阵营给玩家暖色（snap 里有 player_faction_id 即用，缺则回落 "player"）。
  const commanderFactionID = useMemo(() => snap?.player_faction_id || "player", [snap?.player_faction_id]);

  return (
    <div style={boardWrapStyle} aria-label="她的命运地图">
      <Suspense fallback={<div style={boardLoadingStyle}>正在铺开她脚下的天地…</div>}>
        <LazyPixiBoard
          session={snap}
          commanderFactionID={commanderFactionID}
          fogPerspectiveUnitID=""
          selectedTileCoord={selected}
          onTileClick={onTileClick}
        />
      </Suspense>
      {who && (
        <div style={whoCardStyle} role="dialog" aria-label="这是谁">
          <button style={whoCardCloseStyle} aria-label="收起" onClick={() => setWho(null)}>
            ×
          </button>
          <div style={whoCardNameStyle}>
            {who.name}
            {who.isHer && <span style={{ fontSize: 12, color: "#a83a28", marginLeft: 6 }}>· 就是她</span>}
          </div>
          {who.lineage && <div style={whoCardLineStyle}>{who.lineage}</div>}
          {who.faction && <div style={whoCardLineStyle}>心向{factionNameZH(who.faction)}</div>}
        </div>
      )}
    </div>
  );
}
