/* 文件说明：小地图（分区大世界三层导航 §8.1 的「小地图」层），左上角小卡 ~160×120px。
   把当前区地图（snap.map.tiles）缩略成小格点阵：按 terrain 简色铺底，城镇(city/village)画小标记，
   主角（按 unitId 命中，缺省 player_units[0]）位置画红点。用 SVG 渲染（轻量，不引 Pixi）。
   全内联样式，不碰 fate.css/styles.css。阶段1纯展示导航参考，不接点击移动。 */

import { useMemo } from "react";
import type { SessionSnapshot } from "../session/types";

type Props = {
  // snap 是会话快照（含 map.tiles + player_units）；null/缺图时不渲染。
  snap: SessionSnapshot | null;
  // unitId 主角单位 id（命中 player_units 取其坐标；命不中退回 player_units[0]）。
  unitId: string;
  onClose?: () => void;
};

// terrain → 小地图简色（与渲染器 terrain id 对齐：grassland/forest/swamp/mountain/river/...）。
function terrainColor(terrain: string): string {
  switch (terrain) {
    case "plains":
      return "#8fb05f"; // 平原草黄绿（主区地形，避免落默认色）
    case "grassland":
      return "#7ba05b"; // 草地绿
    case "forest":
      return "#3f6b3a"; // 森林深绿
    case "swamp":
      return "#5a6b3a"; // 沼泽黄绿
    case "mountain":
      return "#8a8278"; // 山灰
    case "river":
    case "river_valley":
      return "#4f86b8"; // 河蓝
    case "desert":
      return "#cdb377"; // 沙黄
    case "snowfield":
      return "#dfe6ea"; // 雪白
    case "road":
      return "#b09a72"; // 路土
    case "city":
      return "#d4af37"; // 城金
    case "village":
      return "#a9764a"; // 村棕
    case "ruins":
      return "#9a8d76"; // 废墟灰褐
    default:
      return "#9bb07c"; // 默认浅绿
  }
}

// 是城镇（城/村）→ 画小标记。
function isTown(terrain: string): boolean {
  return terrain === "city" || terrain === "village";
}

const VIEW_W = 160;
const VIEW_H = 120;

export function Minimap({ snap, unitId, onClose }: Props) {
  const data = useMemo(() => {
    const map = snap?.map;
    const tiles = map?.tiles ?? [];
    if (!map || tiles.length === 0) {
      return null;
    }
    const width = map.width || 1;
    const height = map.height || 1;
    // 在画布内留 6px 边距，等比缩放格点。
    const pad = 6;
    const cellW = (VIEW_W - pad * 2) / width;
    const cellH = (VIEW_H - pad * 2) / height;
    const cell = Math.max(1.2, Math.min(cellW, cellH));
    // 整体居中偏移。
    const offsetX = pad + (VIEW_W - pad * 2 - cell * width) / 2;
    const offsetY = pad + (VIEW_H - pad * 2 - cell * height) / 2;

    // 主角坐标：优先按 unitId 命中 player_units，否则取 player_units[0]。
    const units = snap?.player_units ?? [];
    const hero = (unitId && units.find((u) => u.id === unitId)) || units[0] || null;
    const heroQ = hero?.status?.position_q;
    const heroR = hero?.status?.position_r;

    return { map, tiles, cell, offsetX, offsetY, heroQ, heroR };
  }, [snap, unitId]);

  if (!data) {
    return null;
  }

  const { map, tiles, cell, offsetX, offsetY, heroQ, heroR } = data;

  return (
    <div style={cardStyle} aria-label="小地图">
      <div style={headerStyle}>
        <span style={titleStyle}>小地图</span>
        {onClose ? (
          <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭小地图">
            ×
          </button>
        ) : null}
      </div>
      <svg
        width={VIEW_W}
        height={VIEW_H}
        viewBox={`0 0 ${VIEW_W} ${VIEW_H}`}
        style={svgStyle}
        role="img"
        aria-label="当前区域缩略图"
      >
        <rect x={0} y={0} width={VIEW_W} height={VIEW_H} fill="#1c150c" rx={6} />
        {tiles.map((t, i) => {
          const x = offsetX + t.coord.q * cell;
          const y = offsetY + t.coord.r * cell;
          return (
            <rect
              key={`${t.coord.q},${t.coord.r},${i}`}
              x={x}
              y={y}
              width={Math.max(1, cell - 0.4)}
              height={Math.max(1, cell - 0.4)}
              fill={terrainColor(t.terrain)}
            />
          );
        })}
        {/* 城镇标记（在格点中心画小黄/棕方块加描边） */}
        {tiles
          .filter((t) => isTown(t.terrain))
          .map((t, i) => {
            const cx = offsetX + t.coord.q * cell + cell / 2;
            const cy = offsetY + t.coord.r * cell + cell / 2;
            const s = Math.max(2.2, cell * 0.7);
            return (
              <rect
                key={`town-${t.coord.q},${t.coord.r},${i}`}
                x={cx - s / 2}
                y={cy - s / 2}
                width={s}
                height={s}
                fill={t.terrain === "city" ? "#ffd95a" : "#d79a5e"}
                stroke="#3a2c1b"
                strokeWidth={0.5}
              />
            );
          })}
        {/* 主角红点 */}
        {typeof heroQ === "number" && typeof heroR === "number" ? (
          <circle
            cx={offsetX + heroQ * cell + cell / 2}
            cy={offsetY + heroR * cell + cell / 2}
            r={Math.max(2.4, cell * 0.85)}
            fill="#e23b2e"
            stroke="#fff3e0"
            strokeWidth={0.8}
          />
        ) : null}
      </svg>
      {map.id ? <div style={footerStyle}>{`${map.width}×${map.height}`}</div> : null}
    </div>
  );
}

// ── 内联样式（墨色宣纸调，不引用任何外部 CSS） ──

const cardStyle: React.CSSProperties = {
  position: "absolute",
  top: 12,
  left: 12,
  width: VIEW_W + 12,
  background: "rgba(28, 21, 12, 0.86)",
  border: "1px solid rgba(180, 150, 100, 0.45)",
  borderRadius: 8,
  padding: 6,
  boxShadow: "0 6px 16px rgba(0, 0, 0, 0.4)",
  zIndex: 14,
  boxSizing: "border-box",
  fontFamily: "'Noto Serif SC', 'Songti SC', serif",
};

const headerStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  justifyContent: "space-between",
  padding: "0 2px 4px",
};

const titleStyle: React.CSSProperties = {
  fontSize: 12,
  fontWeight: 700,
  color: "#e8d8b8",
  letterSpacing: 1,
};

const closeBtnStyle: React.CSSProperties = {
  appearance: "none",
  border: "none",
  background: "transparent",
  color: "#c9b48c",
  fontSize: 16,
  lineHeight: 1,
  cursor: "pointer",
  padding: "0 2px",
};

const svgStyle: React.CSSProperties = {
  display: "block",
  borderRadius: 6,
};

const footerStyle: React.CSSProperties = {
  marginTop: 3,
  textAlign: "right",
  fontSize: 10,
  color: "#b3a37e",
  fontFamily: "ui-monospace, SFMono-Regular, monospace",
};

export default Minimap;
