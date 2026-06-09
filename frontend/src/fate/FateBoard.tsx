/* 文件说明：命运地图舞台（观战模式）——把战棋的 PixiBoard 搬进命运客户端作她日常生活的主舞台。
   她随世界推进在六边形格子上移动；玩家**只是垂看的先祖**，不下令、不操控，故这里是纯观战：
   - commanderFactionID 固定 "player"（按她的阵营着色），fogPerspectiveUnitID 留空（命运主世界雾已关、全可见），
   - onTileClick 不再下令，只弹一张只读「这是谁」浮卡（lineage/faction），selectedTileCoord 用于浮卡定位/高亮。
   数据来自 GET /api/sessions/:id 的整块快照（getSession），与 FateView 文字命运卡同源同一会话。
   刷新节奏：自身挂载即拉一次 + 每隔若干秒轻量轮询；另接受 refreshSignal——父层在「世界往前走一拍」执行完后
   bump 该值，FateBoard 即重拉快照，board 随她移动重渲（PixiBoard 数据变化自动重渲）。
   祖魂语气/宣纸墨色：本文件不可改 fate.css/styles.css，故浮卡等用内联样式贴合 .fate-* 墨色调；绝不出现指挥/下令 UI。*/

import { Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { getMapPOIs, getSession, moveUnit } from "../session/api";
import type { MapPOI } from "../session/api";
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
  // onGuidanceSuggested：点地图格子/人时，把一句祖魂语气的「指向型指引草稿」上抛父层（父层预填进 FateView 指引框）。
  onGuidanceSuggested?: (text: string) => void;
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

// TERRAIN_NAME_ZH 把地形代码译成中文（点格子时展示「这是什么地方」）。未知回落原码。
const TERRAIN_NAME_ZH: Record<string, string> = {
  plains: "平原",
  forest: "森林",
  mountain: "山地",
  river: "河流",
  river_valley: "河谷",
  grassland: "草原",
  desert: "荒漠",
  swamp: "沼泽",
  ruins: "废墟",
  village: "村庄",
  city: "城市",
  snowfield: "雪原",
  road: "道路",
};

function terrainNameZH(code: string): string {
  return TERRAIN_NAME_ZH[(code ?? "").trim().toLowerCase()] ?? (code ?? "").trim() ?? "未知之地";
}

// 城镇类地形（点击展示「这里住着谁」名单）。
const TOWN_TERRAINS = new Set(["city", "village"]);

// BOARD_POLL_MS：观战自身轮询间隔。她在执行阶段被唤醒移动后，board 至多滞后这一拍即追平。
// 取较慢节奏（避免高频拉整快照增成本）；父层 refreshSignal 才是「这拍刚跑完，立刻看她在哪」的精确刷新。
const BOARD_POLL_MS = 8000;

// 「这是谁」只读浮卡的内联样式（墨色宣纸调，叠在 PixiBoard 之上）。本文件不可改 css，故内联。
const whoCardStyle: React.CSSProperties = {
  position: "absolute",
  bottom: 16,
  left: 16,
  zIndex: 8,
  maxWidth: 280,
  maxHeight: "44vh",
  overflowY: "auto",
  padding: "12px 16px",
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

// 舞台容器内联样式：相对定位以承载浮卡。**确定高度由 .fate-board-stage（fate.css）提供**——不能用 auto 高度
// （minHeight），否则与 PixiBoard 的 resizeTo:container 形成「容器高=canvas 高」反馈循环致地图无限拉长。
const boardWrapStyle: React.CSSProperties = {
  position: "relative",
  width: "100%",
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

// TileOccupant 是格子上一个角色的简介（名字 + 称谓 + 阵营 + 是否就是她）。
type TileOccupant = {
  name: string;
  lineage: string;
  faction: string;
  isHer: boolean;
};

// TilePanel 是点格子弹出的「这是什么地方 + 这里有谁 + 有什么」只读面板内容。
type TilePanel = {
  q: number;
  r: number;
  terrain: string; // 中文地形名
  landmark: string; // 地标（如有）
  isTown: boolean; // 城镇（城市/村庄）——展示「镇上的人」名单
  occupants: TileOccupant[];
  pois: { kind: string; label: string }[]; // 这格的兴趣点（资源/事件）
};

export function FateBoard({ sessionId, unitId, refreshSignal, onGuidanceSuggested }: Props) {
  const [snap, setSnap] = useState<SessionSnapshot | null>(null);
  // pois：地图兴趣点（地块资源 / 野外 NPC 事件），画在格子上的徽标 + 点击查看。
  const [pois, setPois] = useState<MapPOI[]>([]);
  // selected：当前点选的格子（用于 PixiBoard 高亮 + 浮卡定位锚点）。
  const [selected, setSelected] = useState<{ q: number; r: number } | null>(null);
  // tile：点格子弹出的「这是什么地方 + 这里有谁」面板内容；null=不展示。
  const [tile, setTile] = useState<TilePanel | null>(null);
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
    try {
      const ps = await getMapPOIs(sessionId);
      if (mountedRef.current) setPois(ps);
    } catch {
      // POI 拉取失败：保持上一帧 POI，不影响地图主体。
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

  // 点格子（观战不下令）：弹一张只读面板，展示「这是什么地方」（地形 + 地标）+「这里有谁」（占据者名单）。
  // 城镇（城市/村庄）会把住户当名单列出；空地也展示地形信息（不再是点了没反应）。
  const onTileClick = useCallback(
    (q: number, r: number) => {
      const mapTile = snap?.map?.tiles?.find((t) => t.coord.q === q && t.coord.r === r);
      const terrainCode = (mapTile?.terrain ?? "").toLowerCase();
      const occupants: TileOccupant[] = allUnitsOf(snap)
        .filter((u) => u.status.position_q === q && u.status.position_r === r)
        .map((u) => ({
          name: u.identity?.name || u.identity?.nickname || "无名之人",
          lineage: u.identity?.lineage ?? "",
          faction: u.faction_id ?? "",
          isHer: u.id === unitId,
        }));
      const tilePois = pois.filter((p) => p.q === q && p.r === r).map((p) => ({ kind: p.kind, label: p.label_zh }));
      const landmark = mapTile?.landmark ?? "";
      setSelected({ q, r });
      setTile({
        q,
        r,
        terrain: terrainNameZH(terrainCode),
        landmark,
        isTown: TOWN_TERRAINS.has(terrainCode),
        occupants,
        pois: tilePois,
      });
      // 指向型指引草稿：点中具名的人→「留意」她；点中地标/POI/地形→「去那看看」。上抛父层预填进指引框。
      if (onGuidanceSuggested) {
        const namedOther = occupants.find((o) => !o.isHer);
        let draft = "";
        if (namedOther) {
          draft = `留意「${namedOther.name}」`;
        } else if (tilePois.length > 0) {
          draft = `去探一探那处${tilePois[0].label}`;
        } else if (landmark) {
          draft = `去${landmark}那边看看`;
        } else {
          draft = `往${terrainNameZH(terrainCode)}那边走走`;
        }
        onGuidanceSuggested(draft);
      }
    },
    [snap, unitId, pois, onGuidanceSuggested],
  );

  // moveBusy：玩家「让她去这里」直接移动进行中（防重复点）。
  const [moveBusy, setMoveBusy] = useState(false);
  // onMoveHere：玩家在线直接把她移到点选的格子（混合模型：上线可操作）。成功后重拉快照让 board 追平。
  const onMoveHere = useCallback(
    async (q: number, r: number) => {
      setMoveBusy(true);
      try {
        await moveUnit(sessionId, unitId, q, r);
        await refresh();
        setTile(null);
      } catch (e) {
        // 移动失败（越界/水山阻挡）：保留面板，提示走 alert 兜底（命运墨色调下不便弹 toast，从简）。
        window.alert(e instanceof Error ? e.message : "她去不了那里");
      } finally {
        setMoveBusy(false);
      }
    },
    [sessionId, unitId, refresh],
  );

  // commanderFactionID：按她所属阵营给玩家暖色（snap 里有 player_faction_id 即用，缺则回落 "player"）。
  const commanderFactionID = useMemo(() => snap?.player_faction_id || "player", [snap?.player_faction_id]);

  return (
    <div className="fate-board-stage" style={boardWrapStyle} aria-label="她的命运地图">
      <Suspense fallback={<div style={boardLoadingStyle}>正在铺开她脚下的天地…</div>}>
        <LazyPixiBoard
          session={snap}
          commanderFactionID={commanderFactionID}
          fogPerspectiveUnitID=""
          selectedTileCoord={selected}
          onTileClick={onTileClick}
          spectator
          zoom={1.3}
          pois={pois.map((p) => ({ q: p.q, r: p.r, kind: p.kind, label: p.label_zh }))}
        />
      </Suspense>
      {tile && (
        <div style={whoCardStyle} role="dialog" aria-label="这是什么地方">
          <button style={whoCardCloseStyle} aria-label="收起" onClick={() => setTile(null)}>
            ×
          </button>
          <div style={whoCardNameStyle}>
            {tile.terrain}
            {tile.landmark && (
              <span style={{ fontSize: 12, color: "#8a7556", marginLeft: 6 }}>· {tile.landmark}</span>
            )}
          </div>
          <div style={whoCardLineStyle}>
            坐标（{tile.q}, {tile.r}）
          </div>
          {/* 玩家在线操作：让她直接走到这里（混合模型——你可指挥她，世界也自治推进）。 */}
          <button
            type="button"
            disabled={moveBusy}
            onClick={() => void onMoveHere(tile.q, tile.r)}
            style={{
              marginTop: 8,
              padding: "6px 12px",
              borderRadius: 8,
              border: "1px solid rgba(140, 100, 50, 0.5)",
              background: moveBusy ? "rgba(220,210,195,0.7)" : "rgba(196, 132, 58, 0.16)",
              color: "#7a5226",
              fontFamily: "inherit",
              fontSize: 13,
              cursor: moveBusy ? "default" : "pointer",
            }}
          >
            {moveBusy ? "她正动身…" : "🚶 让她去这里"}
          </button>
          {tile.pois.length > 0 && (
            <div style={{ marginTop: 8, display: "flex", flexWrap: "wrap", gap: 6 }}>
              {tile.pois.map((p, i) => (
                <span
                  key={i}
                  style={{
                    fontSize: 12,
                    padding: "2px 8px",
                    borderRadius: 999,
                    background: p.kind === "resource" ? "rgba(217, 188, 115, 0.3)" : "rgba(198, 109, 72, 0.22)",
                    border: "1px solid rgba(120, 90, 50, 0.3)",
                    color: "#6b4a22",
                  }}
                >
                  {p.kind === "resource" ? "💎 " : "❗ "}
                  {p.label}
                </span>
              ))}
            </div>
          )}
          {tile.occupants.length === 0 ? (
            <div style={{ ...whoCardLineStyle, marginTop: 8 }}>
              {tile.isTown ? "镇上此刻无人露面。" : "此地此刻空无一人。"}
            </div>
          ) : (
            <div style={{ marginTop: 10, display: "flex", flexDirection: "column", gap: 6 }}>
              <div style={{ fontSize: 12, color: "#6b4a22" }}>
                {tile.isTown ? `镇上的人（${tile.occupants.length}）` : `这里的人（${tile.occupants.length}）`}
              </div>
              {tile.occupants.map((o, i) => (
                <div key={i} style={{ borderTop: "1px solid rgba(120, 90, 50, 0.16)", paddingTop: 5 }}>
                  <div style={{ fontSize: 14, color: "#4a3417" }}>
                    {o.name}
                    {o.isHer && <span style={{ fontSize: 11, color: "#a83a28", marginLeft: 6 }}>· 就是她</span>}
                  </div>
                  {(o.lineage || o.faction) && (
                    <div style={whoCardLineStyle}>
                      {o.lineage}
                      {o.lineage && o.faction ? " · " : ""}
                      {o.faction ? `心向${factionNameZH(o.faction)}` : ""}
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
