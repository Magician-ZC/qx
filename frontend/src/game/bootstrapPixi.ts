/* 文件说明：Pixi 场景渲染实现，负责地形贴图、单位层、HUD 以及执行阶段标记可视化。 */

import { Application, Assets, Circle, Container, Graphics, Polygon, Rectangle, Sprite, Text, TextStyle, Texture } from "pixi.js";
import { avatarFiles } from "./avatarList";
import type { DecisionTrace, RawEventEntry, SessionLog, SessionSnapshot } from "../session/types";

const palette = {
  void: 0x050a0e,
  sea: 0x0b1b2a,
  panel: 0x122333,
  panelLight: 0xe9f0f4,
  panelLine: 0x9f8751,
  ink: 0xf2e7cb,
  inkDark: 0x10202b,
  muted: 0xb9b194,
  brass: 0xd9bc73,
  ember: 0xc66d48,
  success: 0x8cb572,
  player: 0xd66a45,
  enemy: 0x4d86a3,
  // ambient：阵营据点公共 NPC / 野外散人（命运主世界静态可见的「世间众生」）的 token 色。
  // 刻意取一抹素淡的墨褐——比 player 暖、比 enemy 冷都更低调，提示「这是路人，不是敌我交战单位」。
  ambient: 0x9a8f6e,
  selected: 0xf2d98f,
  plains: 0x92a66e,
  forest: 0x5f7f4f,
  mountain: 0x5f6976,
  road: 0x9a8159,
  river: 0x4a7895,
  valley: 0x7d9f67,
  grassland: 0x91ab5f,
  desert: 0xb99d63,
  swamp: 0x5c6f56,
  ruin: 0x747067,
  village: 0x8b7b58,
  city: 0x7f7f8e,
  snowfield: 0x9ca7b0,
  line: 0x2b3d4d,
};

const uncivWorldBackground = "/unciv/ui/world_screen.png";
const MAX_UNIT_BUBBLE_LINES = 3;

const boardSafeInset = {
  top: 64,
  right: 112,
  bottom: 118,
  left: 24,
};

const terrainOrder = [
  "plains",
  "forest",
  "mountain",
  "river",
  "river_valley",
  "grassland",
  "desert",
  "swamp",
  "ruins",
  "village",
  "city",
  "snowfield",
  "road",
] as const;

type TerrainVisual = {
  label: string;
  color: number;
  icon: string;
  alpha?: number;
};

const terrainVisuals: Record<string, TerrainVisual> = {
  plains: {
    label: "平原",
    color: palette.plains,
    icon: "/unciv/terrain/plains.png",
  },
  forest: {
    label: "森林",
    color: palette.forest,
    icon: "/unciv/terrain/forest.png",
  },
  mountain: {
    label: "山地",
    color: palette.mountain,
    icon: "/unciv/terrain/mountain.png",
  },
  river: {
    label: "河流",
    color: palette.river,
    icon: "/unciv/terrain/river.png",
    alpha: 0.9,
  },
  river_valley: {
    label: "河谷",
    color: palette.valley,
    icon: "/unciv/terrain/river_valley.png",
  },
  grassland: {
    label: "草原",
    color: palette.grassland,
    icon: "/unciv/terrain/grassland.png",
  },
  desert: {
    label: "沙漠",
    color: palette.desert,
    icon: "/unciv/terrain/desert.png",
  },
  swamp: {
    label: "沼泽",
    color: palette.swamp,
    icon: "/unciv/terrain/swamp.png",
  },
  ruins: {
    label: "废墟",
    color: palette.ruin,
    icon: "/unciv/terrain/ruins.png",
  },
  village: {
    label: "村庄",
    color: palette.village,
    icon: "/unciv/terrain/village.png",
  },
  city: {
    label: "城市",
    color: palette.city,
    icon: "/unciv/terrain/city.png",
  },
  snowfield: {
    label: "雪原",
    color: palette.snowfield,
    icon: "/unciv/terrain/snowfield.png",
  },
  road: {
    label: "道路",
    color: palette.road,
    icon: "/unciv/terrain/road.png",
    alpha: 0.8,
  },
};

type SceneModel = {
  session: SessionSnapshot | null;
  commanderFactionID?: string;
  fogPerspectiveUnitID?: string;
  selectedTileCoord: { q: number; r: number } | null;
  onTileClick: (q: number, r: number) => void;
  onOpenDialogues?: () => void;
  onOpenUnitChat?: (unitID: string) => void;
  nowMs?: number;
  zoom?: number;
  // spectator=true（命运主世界全屏观战）：跳过战棋专属 HUD（回合卡/地形图例），地图更纯净。
  spectator?: boolean;
  // focusUnitID：命运观战模式下「她」的单位 ID。首次拿到含该单位的快照时把相机居中到她的格子（仅一次，之后玩家拖动不被打断）。
  focusUnitID?: string;
  // pois：地图兴趣点（地块特殊资源 / 野外 NPC 身上的事件），在对应格子画小徽标。consumed=true 表示已采完/探完，徽标变淡。
  pois?: Array<{ q: number; r: number; kind: string; label: string; consumed?: boolean }>;
  executionMarkers?: Array<{
    unitID: string;
    status: "started" | "completed";
    turn: number;
    startedUnits?: number;
    completedUnits?: number;
    totalUnits?: number;
  }>;
};

type MountedScene = {
  destroy: () => void;
  render: (model: SceneModel) => void;
};

type BoardPlacement = {
  radius: number;
  horizontalStep: number;
  verticalStep: number;
  originX: number;
  originY: number;
};

type UnitScreenCenter = {
  x: number;
  y: number;
};

type UnitDialoguePair = {
  leftID: string;
  rightID: string;
  summary: string;
};

type InjuryMarker = {
  unitID: string;
  title: string;
  detail: string;
  occurredAtMs: number;
  severity: "low" | "medium" | "high";
};

type StatusEventPayload = {
  unit_id?: string;
  field?: string;
  delta?: number;
  before?: number;
  after?: number;
  reason_code?: string;
  reason_text?: string;
  actors?: string[];
};

const terrainTextures = new Map<string, Texture>();
let worldBackdropTexture: Texture | null = null;
let assetsReadyPromise: Promise<void> | null = null;
const INJURY_MARKER_LIFETIME_MS = 4500;
const TRADE_BUBBLE_LIFETIME_MS = 4000;

// mountPixiBoard 负责初始化 Pixi 舞台，并暴露“增量 render + destroy”接口给 React 层。
export async function mountPixiBoard(container: HTMLDivElement): Promise<MountedScene> {
  const app = new Application({
    antialias: true,
    autoDensity: true,
    autoStart: false,
    backgroundAlpha: 0,
    resizeTo: container,
    resolution: Math.min(window.devicePixelRatio || 1, 2),
  });
  // 战场是事件驱动的静态棋盘，不需要 Pixi 默认 60fps ticker 常驻。
  // 开发时长时间开着页面会让 GPU 持续高负载，部分显卡/驱动会表现为 macOS 重启或 Windows 蓝屏。
  app.stop();
  container.replaceChildren(app.view as HTMLCanvasElement);

  const boardLayer = new Container();
  const worldLayer = new Container();
  const unitLayer = new Container();
  const hudLayer = new Container();
  boardLayer.addChild(worldLayer, unitLayer);
  app.stage.addChild(boardLayer, hudLayer);
  let worldRenderKey = "";
  let cachedPlacement: BoardPlacement | null = null;
  let boardOffset = { x: 0, y: 0 };
  let viewScale = 1; // 相机缩放（滚轮调，作用于 boardLayer.scale；与 boardOffset 的 position 组合成可拖可缩的相机）。
  let dragging = false;
  let didDrag = false;
  let dragStart = { x: 0, y: 0 };
  let dragOrigin = { x: 0, y: 0 };
  // didInitialFocus：首屏「以她为中心」只执行一次的标志——之后快照刷新/重绘绝不再重置相机，玩家拖动不被打断。
  let didInitialFocus = false;
  let destroyed = false;
  let pendingRenderFrame = 0;
  let latestAssetVersion = 0;

  const renderFrame = () => {
    if (destroyed) {
      return;
    }
    app.renderer.render(app.stage);
  };

  const syncRendererSize = () => {
    const bounds = container.getBoundingClientRect();
    const width = Math.max(1, Math.round(bounds.width));
    const height = Math.max(1, Math.round(bounds.height));
    if (app.renderer.width !== width || app.renderer.height !== height) {
      app.renderer.resize(width, height);
    }
  };

  const scheduleRenderScene = () => {
    if (destroyed || pendingRenderFrame !== 0) {
      return;
    }
    pendingRenderFrame = window.requestAnimationFrame(() => {
      pendingRenderFrame = 0;
      renderScene();
    });
  };

  let latest: SceneModel = {
    session: null,
    commanderFactionID: "player",
    fogPerspectiveUnitID: "",
    selectedTileCoord: null,
    onTileClick: () => undefined,
    onOpenDialogues: () => undefined,
    onOpenUnitChat: () => undefined,
    nowMs: Date.now(),
    zoom: 1,
    executionMarkers: [],
  };

  const handleTileClick = (q: number, r: number) => {
    if (didDrag) return;
    latest.onTileClick(q, r);
  };

  const renderScene = () => {
    if (destroyed) {
      return;
    }
    syncRendererSize();
    destroyChildren(unitLayer);
    destroyChildren(hudLayer);

    const width = app.screen.width;
    const height = app.screen.height;
    app.stage.hitArea = app.screen;
    boardLayer.position.set(boardOffset.x, boardOffset.y);
    const zoom = normalizeBoardZoom(latest.zoom);
    const nextWorldKey = buildWorldRenderKey(latest.session, width, height, zoom, latest.commanderFactionID, latest.fogPerspectiveUnitID, latestAssetVersion);
    if (nextWorldKey !== worldRenderKey) {
      worldRenderKey = nextWorldKey;
      destroyChildren(worldLayer);
      worldLayer.addChild(drawBackdrop(width, height));
      cachedPlacement = null;
      if (latest.session) {
        cachedPlacement = drawTerrain(worldLayer, { ...latest, onTileClick: handleTileClick }, width, height);
      }
    }

    if (!latest.session) {
      hudLayer.addChild(drawPlaceholder(width, height));
      renderFrame();
      return;
    }

    const placement = cachedPlacement ?? computeBoardPlacement(latest.session, width, height, zoom);
    // 首屏相机以她为中心（仅 spectator 观战模式、仅首次拿到含该单位的快照时执行一次）：
    // 主世界 24×16 图远大于视口，她默认出生在屏外左上角聚落，不居中就逼玩家「看人必先拖图」。
    // 坐标系推导：boardLayer.position=boardOffset、boardLayer.scale=viewScale，
    // 世界点 w 的屏幕坐标 = w×viewScale + boardOffset；要让她的 tile 中心落在画布中心，
    // 即 screenCenter = herTileCenter×viewScale + boardOffset → boardOffset = screenCenter − herTileCenter×viewScale。
    // 找不到该单位时静默跳过（不置标志，等后续含她的快照再试）。
    if (latest.spectator && latest.focusUnitID && !didInitialFocus) {
      const focusID = latest.focusUnitID;
      const focusUnit = [
        ...latest.session.player_units,
        ...latest.session.enemy_units,
        ...(latest.session.ambient_units ?? []),
        ...(latest.session.wild_units ?? []),
      ].find((unit) => unit.id === focusID);
      if (focusUnit) {
        const herCenter = tileCenter(focusUnit.status.position_q, focusUnit.status.position_r, placement);
        boardOffset = {
          x: width / 2 - herCenter.x * viewScale,
          y: height / 2 - herCenter.y * viewScale,
        };
        boardLayer.position.set(boardOffset.x, boardOffset.y);
        didInitialFocus = true;
      }
    }
    drawStructures(unitLayer, latest.session, placement, latest.commanderFactionID, latest.fogPerspectiveUnitID);
    drawBattlefieldRemnants(unitLayer, latest.session, placement, latest.commanderFactionID, latest.fogPerspectiveUnitID);
    // POI 徽标层：放在 drawUnits 之前，让站到 POI 上的单位 token 盖在徽标之上（人在前）。unitLayer 每帧重画，POI 即时刷新。
    drawPOIs(unitLayer, latest.pois ?? [], placement);
    drawUnits(unitLayer, { ...latest, onTileClick: handleTileClick }, placement, width, height);
    // 观战模式（命运主世界全屏地图）：不画战棋专属的「回合卡 / 地形图例」HUD——那是部署/执行战棋概念，
    // 命运观战里只会变成压在地图上的无关浮窗。spectator=false（战棋视图）时照旧显示。
    if (!latest.spectator) {
      hudLayer.addChild(drawTurnCard(width, height, latest.session));
      hudLayer.addChild(drawTerrainLegend(width, latest.session));
    }
    renderFrame();
  };

  const resizeObserver = new ResizeObserver(() => scheduleRenderScene());
  resizeObserver.observe(container);
  app.stage.eventMode = "static";
  app.stage.hitArea = app.screen;
  app.stage.cursor = "grab";
  app.stage.on("pointerdown", (event) => {
    dragging = true;
    didDrag = false;
    dragStart = { x: event.global.x, y: event.global.y };
    dragOrigin = { ...boardOffset };
  });
  app.stage.on("pointermove", (event) => {
    if (!dragging) return;
    const deltaX = event.global.x - dragStart.x;
    const deltaY = event.global.y - dragStart.y;
    // 拖拽判定用欧氏距离 >9px（原 |dx|+|dy|>5 曼哈顿阈值过敏，触控板普通点击的微抖会被概率性误判成拖拽而吞掉点击）。
    if (Math.hypot(deltaX, deltaY) > 9) {
      didDrag = true;
    }
    boardOffset = {
      x: dragOrigin.x + deltaX,
      y: dragOrigin.y + deltaY,
    };
    boardLayer.position.set(boardOffset.x, boardOffset.y);
    renderFrame();
  });
  const endDrag = () => {
    dragging = false;
  };
  app.stage.on("pointerup", endDrag);
  app.stage.on("pointerupoutside", endDrag);

  // 滚轮缩放（朝光标缩放）：缩放 boardLayer.scale 作相机变焦，与拖拽 pan 的 position 组合。clamp [0.4, 3]。
  // 用 DOM canvas 的 wheel（passive:false 以 preventDefault 阻止页面滚动），缩放时保持光标下的世界点不动。
  const canvasEl = app.view as HTMLCanvasElement;
  const onWheel = (event: WheelEvent) => {
    event.preventDefault();
    const rect = canvasEl.getBoundingClientRect();
    const cx = event.clientX - rect.left;
    const cy = event.clientY - rect.top;
    const factor = event.deltaY < 0 ? 1.12 : 1 / 1.12;
    const next = clamp(viewScale * factor, 0.4, 3);
    if (next === viewScale) {
      return;
    }
    const worldX = (cx - boardOffset.x) / viewScale;
    const worldY = (cy - boardOffset.y) / viewScale;
    viewScale = next;
    boardOffset = { x: cx - worldX * viewScale, y: cy - worldY * viewScale };
    boardLayer.scale.set(viewScale);
    boardLayer.position.set(boardOffset.x, boardOffset.y);
    renderFrame();
  };
  canvasEl.addEventListener("wheel", onWheel, { passive: false });

  renderScene();
  scheduleRenderScene();
  void ensureVisualAssets().then((assetVersion) => {
    if (destroyed || assetVersion === latestAssetVersion) {
      return;
    }
    latestAssetVersion = assetVersion;
    scheduleRenderScene();
  });

  return {
    render(model) {
      latest = model;
      renderScene();
    },
    destroy() {
      destroyed = true;
      if (pendingRenderFrame !== 0) {
        window.cancelAnimationFrame(pendingRenderFrame);
        pendingRenderFrame = 0;
      }
      resizeObserver.disconnect();
      canvasEl.removeEventListener("wheel", onWheel);
      app.stop();
      // terrainTextures / unitAvatarTextures 是全局资源缓存，不能在组件卸载时销毁底层纹理。
      // React StrictMode 在开发环境会挂载-卸载-再挂载一次；销毁共享纹理会导致后续 Pixi 场景复用已释放资源。
      app.destroy(true, { children: true, texture: false, baseTexture: false });
    },
  };
}

// 资源只加载一次：失败时自动降级到纯色地形渲染，不阻断游戏主流程。
let assetsVersion = 0;

async function ensureVisualAssets(): Promise<number> {
  if (!assetsReadyPromise) {
    const icons = terrainOrder.map((terrain) => terrainVisuals[terrain].icon);
    assetsReadyPromise = Assets.load([uncivWorldBackground, ...icons])
      .then(() => {
        worldBackdropTexture = Texture.from(uncivWorldBackground);
        for (const terrain of terrainOrder) {
          terrainTextures.set(terrain, Texture.from(terrainVisuals[terrain].icon));
        }
        assetsVersion += 1;
      })
      .catch((e) => {
        console.error("Failed to load visual assets", e);
        worldBackdropTexture = null;
        terrainTextures.clear();
      });
  }
  await assetsReadyPromise;
  return assetsVersion;
}

// drawBackdrop 绘制战场背景层（底图、遮罩与网格纹理）。
function drawBackdrop(width: number, height: number): Container {
  const layer = new Container();

  if (worldBackdropTexture && worldBackdropTexture.valid) {
    const bg = new Sprite(worldBackdropTexture);
    bg.width = width;
    bg.height = height;
    bg.alpha = 0.24;
    layer.addChild(bg);
  }

  const shell = new Graphics();
  shell.beginFill(palette.void, 0.92);
  shell.drawRoundedRect(0, 0, width, height, 22);
  shell.endFill();

  shell.beginFill(palette.sea, 0.54);
  shell.drawRoundedRect(0, 0, width, height, 22);
  shell.endFill();

  // （原先这里画一个暖色装饰椭圆光晕；在全屏世界地图上会变成一坨突兀的怪斑，已移除。）

  shell.lineStyle({
    color: palette.panelLine,
    alpha: 0.16,
    width: 1,
  });
  const gridStep = 28;
  for (let x = 0; x <= width; x += gridStep) {
    shell.moveTo(x, 0);
    shell.lineTo(x, height);
  }
  for (let y = 0; y <= height; y += gridStep) {
    shell.moveTo(0, y);
    shell.lineTo(width, y);
  }
  layer.addChild(shell);
  return layer;
}

// drawPlaceholder 在无会话数据时绘制占位提示。
function drawPlaceholder(width: number, height: number): Container {
  const layer = new Container();

  const title = new Text(
    "等待战场同步",
    new TextStyle({
      fill: palette.ink,
      fontFamily: "Iowan Old Style, Palatino Linotype, serif",
      fontSize: 30,
      fontWeight: "700",
    }),
  );
  title.anchor.set(0.5);
  title.position.set(width / 2, height / 2 - 16);

  const note = new Text(
    "所有动作和文本都由 AI 单位生成",
    new TextStyle({
      fill: palette.muted,
      fontFamily: "Avenir Next, Helvetica Neue, sans-serif",
      fontSize: 14,
      letterSpacing: 0.4,
    }),
  );
  note.anchor.set(0.5);
  note.position.set(width / 2, height / 2 + 18);

  layer.addChild(title, note);
  return layer;
}

// drawTerrain 按服务端地块快照绘制地形六边形、贴图与关键地标标签。
function drawTerrain(
  layer: Container,
  model: SceneModel,
  width: number,
  height: number,
): BoardPlacement {
  if (!model.session) {
    return { radius: 16, horizontalStep: 0, verticalStep: 0, originX: 0, originY: 0 }; // Fallback, won't be used
  }
  const session = model.session;
  const placement = computeBoardPlacement(session, width, height, model.zoom);
  const visibleCoords = fogVisibleCoordSet(session, model.commanderFactionID ?? session.player_faction_id, model.fogPerspectiveUnitID);

  for (const tile of session.map.tiles) {
    const center = tileCenter(tile.coord.q, tile.coord.r, placement);
    const visual = terrainVisualFor(tile.terrain);
    const tileVisible = !session.fog_of_war_enabled || visibleCoords.has(coordKey(tile.coord.q, tile.coord.r));

    const hexPoints = createHexPoints(center.x, center.y, placement.radius);
    const base = new Graphics();
    base.beginFill(tileVisible ? visual.color : 0x071019, tileVisible ? 0.9 : 0.96);
    
    // 如果当前地块被选中，高亮边框
    const isSelected = model.selectedTileCoord?.q === tile.coord.q && model.selectedTileCoord?.r === tile.coord.r;
    
    base.lineStyle({
      color: isSelected ? palette.selected : palette.line,
      alpha: isSelected ? 1 : 0.68,
      width: isSelected ? 4 : 2,
    });
    base.drawPolygon(hexPoints);
    base.endFill();
    
    // 给地块加上交互事件
    base.eventMode = "static";
    base.cursor = "pointer";
    base.hitArea = new Polygon(hexPoints);
    base.on("pointertap", () => model.onTileClick(tile.coord.q, tile.coord.r));

    const highlight = new Graphics();
    highlight.lineStyle({
      color: 0xffffff,
      alpha: 0.08,
      width: 1,
    });
    highlight.moveTo(center.x - placement.radius * 0.34, center.y - placement.radius * 0.44);
    highlight.lineTo(center.x + placement.radius * 0.2, center.y - placement.radius * 0.58);

    layer.addChild(base);

    const iconTexture = terrainTextures.get(tile.terrain);
    if (tileVisible && iconTexture && iconTexture.valid) {
      const icon = new Sprite(iconTexture);
      icon.anchor.set(0.5);
      const targetWidth = tile.terrain === "road" ? placement.radius * 1.1 : placement.radius * 0.96;
      const iconScale = targetWidth / Math.max(1, iconTexture.width);
      icon.scale.set(iconScale);
      icon.alpha = visual.alpha ?? 0.86;
      icon.position.set(center.x, center.y - placement.radius * 0.08);
      icon.eventMode = "none";
      layer.addChild(icon);
    }

    if (tileVisible && shouldShowTerrainTag(tile.terrain, placement.radius)) {
      layer.addChild(drawTerrainTag(center.x, center.y + placement.radius * 0.56, visual.label, placement.radius));
    }

    if (!tileVisible && placement.radius >= 24) {
      layer.addChild(drawFogTag(center.x, center.y, placement.radius));
    }

    highlight.eventMode = "none";
    layer.addChild(highlight);

    // 放在地形装饰最上层的透明命中区，避免贴图、文字标签或半透明高亮挡住空地块点击。
    const hit = new Graphics();
    hit.beginFill(0xffffff, 0.001);
    hit.drawPolygon(hexPoints);
    hit.endFill();
    hit.eventMode = "static";
    hit.cursor = "pointer";
    hit.hitArea = new Polygon(hexPoints);
    hit.on("pointertap", () => model.onTileClick(tile.coord.q, tile.coord.r));
    layer.addChild(hit);
  }

  return placement;
}

// shouldShowTerrainTag 只给关键地标显示文字，避免每格地形名铺满战场。
function shouldShowTerrainTag(terrain: string, radius: number): boolean {
  if (radius < 26) {
    return false;
  }
  return terrain === "city" || terrain === "village" || terrain === "ruins";
}

// poiColor / poiEmoji：按 POI 类别给徽标配色与图标（资源=金、事件=暖红）。
function poiColor(kind: string): number {
  return kind === "resource" ? palette.brass : palette.ember;
}
function poiEmoji(kind: string): string {
  return kind === "resource" ? "💎" : "❗";
}

// drawPOIs 在对应格子画 POI 小徽标（左下角色点 + emoji 图标），让出右下给 structure 徽章。纯展示、不拦截点击。
function drawPOIs(
  layer: Container,
  pois: Array<{ q: number; r: number; kind: string; label: string; consumed?: boolean }>,
  placement: BoardPlacement,
): void {
  for (const poi of pois) {
    const center = tileCenter(poi.q, poi.r, placement);
    const badgeR = Math.max(6, placement.radius * 0.26);
    const cx = center.x - placement.radius * 0.5;
    const cy = center.y + placement.radius * 0.55;
    // consumed=true（已采完/探完）：圆点与 emoji 整体变淡到 0.35——留痕可见但提示「这里已被掏过、不可重复触发」。
    const badgeAlpha = poi.consumed ? 0.35 : 1;
    const dot = new Graphics();
    dot.lineStyle({ color: palette.panelLine, alpha: 0.9, width: 1.2 });
    dot.beginFill(poiColor(poi.kind), 0.92);
    dot.drawCircle(cx, cy, badgeR);
    dot.endFill();
    dot.alpha = badgeAlpha;
    dot.eventMode = "none";
    layer.addChild(dot);
    const icon = new Text(
      poiEmoji(poi.kind),
      new TextStyle({
        fontFamily: '"Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", sans-serif',
        fontSize: badgeR * 1.3,
      }),
    );
    icon.anchor.set(0.5);
    icon.position.set(cx, cy);
    icon.alpha = badgeAlpha;
    icon.eventMode = "none";
    layer.addChild(icon);
  }
}

// computeBoardPlacement 根据地图尺寸与视口计算棋盘布局参数。
function computeBoardPlacement(session: SessionSnapshot, width: number, height: number, zoom = 1): BoardPlacement {
  const safeTop = height < 680 ? 56 : boardSafeInset.top;
  const safeRight = width < 820 ? 24 : boardSafeInset.right;
  const safeBottom = height < 680 ? 82 : boardSafeInset.bottom;
  const safeLeft = width < 820 ? 14 : boardSafeInset.left;
  const usableWidth = Math.max(width - safeLeft - safeRight, width * 0.58);
  const usableHeight = Math.max(height - safeTop - safeBottom, height * 0.52);
  const fitRadius = Math.min(usableWidth / (session.map.width * 2.05), usableHeight / (session.map.height * 1.58));
  const radius = clamp(fitRadius * 1.28 * normalizeBoardZoom(zoom), 16, 96);
  const horizontalStep = radius * 1.86;
  const verticalStep = Math.sqrt(3) * radius * 0.98;
  const boardWidth = (session.map.width - 1) * horizontalStep + radius * 2;
  const boardHeight = (session.map.height - 1) * verticalStep + radius * 2;
  return {
    radius,
    horizontalStep,
    verticalStep,
    originX: safeLeft + (usableWidth - boardWidth) / 2 + radius,
    originY: safeTop + (usableHeight - boardHeight) / 2 + radius,
  };
}

// buildWorldRenderKey 构建地形层缓存 key，减少不必要重绘。
function buildWorldRenderKey(session: SessionSnapshot | null, width: number, height: number, zoom = 1, commanderFactionID = "", fogPerspectiveUnitID = "", assetVersion = 0): string {
  const w = Math.round(width);
  const h = Math.round(height);
  const z = normalizeBoardZoom(zoom).toFixed(2);
  if (!session) {
    return `empty:${w}x${h}:${z}:assets:${assetVersion}`;
  }
  return [
    session.id,
    session.map.id,
    session.map.width,
    session.map.height,
    session.map.tiles.length,
    session.fog_of_war_enabled ? "fog" : "open",
    commanderFactionID,
    fogPerspectiveUnitID,
    visibilityRenderKey(session),
    w,
    h,
    z,
    assetVersion,
  ].join(":");
}

// normalizeBoardZoom 约束前端传入的地图缩放倍率，避免异常值导致棋盘不可见。
function normalizeBoardZoom(zoom: number | undefined): number {
  if (!Number.isFinite(zoom)) {
    return 1;
  }
  return clamp(zoom ?? 1, 0.55, 1.8);
}

function coordKey(q: number, r: number): string {
  return `${q},${r}`;
}

function visibilityRenderKey(session: SessionSnapshot): string {
  if (!session.fog_of_war_enabled) {
    return "all";
  }
  return [...session.player_units, ...session.enemy_units]
    .map((unit) => `${unit.id}:${unit.faction_id}:${unit.status.position_q},${unit.status.position_r}:${unit.status.life_state}:${unit.stats?.derived?.vision ?? 5}`)
    .join("|");
}

function fogVisibleCoordSet(session: SessionSnapshot, commanderFactionID: string, fogPerspectiveUnitID = ""): Set<string> {
  if (!session.fog_of_war_enabled) {
    return new Set(session.map.tiles.map((tile) => coordKey(tile.coord.q, tile.coord.r)));
  }
  const visible = new Set<string>();
  const units = [...session.player_units, ...session.enemy_units];
  const activeFriendlyUnits = units.filter((unit) => unit.faction_id === commanderFactionID && unit.status.life_state === "active");
  const viewers = fogPerspectiveUnitID
    ? activeFriendlyUnits.filter((unit) => unit.id === fogPerspectiveUnitID)
    : activeFriendlyUnits;
  for (const viewer of viewers.length > 0 ? viewers : activeFriendlyUnits) {
    for (const key of visibleCoordsFromUnit(session, viewer)) {
      visible.add(key);
    }
  }
  return visible;
}

function visibleCoordsFromUnit(session: SessionSnapshot, unit: SessionSnapshot["player_units"][number]): Set<string> {
  const origin = { q: unit.status.position_q, r: unit.status.position_r };
  const originTile = session.map.tiles.find((tile) => tile.coord.q === origin.q && tile.coord.r === origin.r);
  const baseRange = Math.max(1, unit.stats?.derived?.vision ?? 5);
  const effectiveRange = Math.max(1, baseRange + (terrainVisionRange(originTile?.terrain ?? "plains") - 5));
  const tileByKey = new Map(session.map.tiles.map((tile) => [coordKey(tile.coord.q, tile.coord.r), tile]));
  const bestCost = new Map<string, number>([[coordKey(origin.q, origin.r), 0]]);
  const queue: Array<{ q: number; r: number; cost: number }> = [{ ...origin, cost: 0 }];
  while (queue.length > 0) {
    queue.sort((left, right) => left.cost - right.cost);
    const current = queue.shift();
    if (!current || current.cost > effectiveRange) {
      continue;
    }
    for (const neighbor of axialNeighbors(current.q, current.r)) {
      const tile = tileByKey.get(coordKey(neighbor.q, neighbor.r));
      if (!tile) {
        continue;
      }
      const nextCost = current.cost + 1 + terrainVisionPenalty(tile.terrain);
      if (nextCost > effectiveRange) {
        continue;
      }
      const key = coordKey(neighbor.q, neighbor.r);
      const previous = bestCost.get(key);
      if (previous !== undefined && nextCost >= previous) {
        continue;
      }
      bestCost.set(key, nextCost);
      queue.push({ ...neighbor, cost: nextCost });
    }
  }
  return new Set(bestCost.keys());
}

function axialNeighbors(q: number, r: number): Array<{ q: number; r: number }> {
  return [
    { q: q + 1, r },
    { q: q - 1, r },
    { q, r: r + 1 },
    { q, r: r - 1 },
    { q: q + 1, r: r - 1 },
    { q: q - 1, r: r + 1 },
  ];
}

function terrainVisionRange(terrain: string): number {
  switch (terrain) {
    case "forest":
    case "swamp":
      return 2;
    case "mountain":
      return 8;
    case "grassland":
      return 6;
    case "river":
    case "ruins":
    case "village":
    case "city":
      return 3;
    case "river_valley":
    case "desert":
    case "road":
      return 4;
    default:
      return 5;
  }
}

function terrainVisionPenalty(terrain: string): number {
  return ["forest", "swamp", "river", "ruins", "village", "city", "snowfield"].includes(terrain) ? 1 : 0;
}

// drawTerrainTag 绘制单格地形名称标签。
function drawTerrainTag(x: number, y: number, label: string, radius: number): Container {
  const layer = new Container();
  const text = new Text(
    label,
    new TextStyle({
      fill: palette.ink,
      fontFamily: "Avenir Next, Helvetica Neue, sans-serif",
      fontSize: Math.max(8, radius * 0.21),
      fontWeight: "700",
      letterSpacing: 0.3,
    }),
  );
  text.anchor.set(0.5, 0.5);
  text.position.set(x, y);

  const bg = new Graphics();
  bg.beginFill(0x0a161f, 0.76);
  bg.lineStyle({
    color: palette.panelLine,
    alpha: 0.22,
    width: 1,
  });
  bg.drawRoundedRect(
    x - text.width / 2 - 4,
    y - text.height / 2 - 2,
    text.width + 8,
    text.height + 4,
    6,
  );
  bg.endFill();

  layer.addChild(bg, text);
  layer.eventMode = "none";
  bg.eventMode = "none";
  text.eventMode = "none";
  return layer;
}

function drawFogTag(x: number, y: number, radius: number): Container {
  const layer = new Container();
  const mist = new Graphics();
  mist.beginFill(0x000000, 0.16);
  mist.drawCircle(x, y, radius * 0.52);
  mist.endFill();
  const text = new Text(
    "?",
    new TextStyle({
      fill: 0x5f7280,
      fontFamily: "Avenir Next, Helvetica Neue, sans-serif",
      fontSize: Math.max(11, radius * 0.28),
      fontWeight: "800",
    }),
  );
  text.anchor.set(0.5, 0.5);
  text.position.set(x, y - radius * 0.04);
  layer.addChild(mist, text);
  layer.eventMode = "none";
  mist.eventMode = "none";
  text.eventMode = "none";
  return layer;
}

// drawStructures 绘制建筑标记、施工进度与完工名称。
function drawStructures(layer: Container, session: SessionSnapshot, placement: BoardPlacement, commanderFactionID = session.player_faction_id, fogPerspectiveUnitID = ""): void {
  // 建筑：羊皮纸圆形徽章 + 系统 emoji 图标，emoji 自带形状辨识度，与 HUD 羊皮纸调性融合。
  const visibleCoords = fogVisibleCoordSet(session, commanderFactionID, fogPerspectiveUnitID);
  for (const structure of session.structures) {
    if (session.fog_of_war_enabled && !visibleCoords.has(coordKey(structure.q, structure.r))) {
      continue;
    }
    const center = tileCenter(structure.q, structure.r, placement);
    // 徽章放到格子右下角，让出中心给单位 token。
    const badgeRadius = placement.radius * 0.32;
    const offset = placement.radius * 0.5;
    const anchorX = center.x + offset * 0.55;
    const anchorY = center.y + offset * 0.7;
    const completed = structure.completed;

    const badge = new Graphics();
    drawStructureBadgePlate(badge, badgeRadius, completed);
    badge.position.set(anchorX, anchorY);
    badge.eventMode = "none";
    layer.addChild(badge);

    const icon = new Text(
      structureEmoji(structure.type),
      new TextStyle({
        fontFamily:
          "\"Apple Color Emoji\", \"Segoe UI Emoji\", \"Noto Color Emoji\", \"Twemoji Mozilla\", sans-serif",
        fontSize: badgeRadius * 1.55,
      }),
    );
    icon.anchor.set(0.5, 0.5);
    icon.position.set(anchorX, anchorY);
    icon.alpha = completed ? 1 : 0.6;
    icon.eventMode = "none";
    layer.addChild(icon);

    if (!completed) {
      // 施工中显示进度数字，置于徽章下方。
      const progress = new Text(
        `${Math.max(0, structure.build_progress)}/${Math.max(1, structure.build_required)}`,
        new TextStyle({
          fill: palette.ink,
          stroke: 0x10202b,
          strokeThickness: 2,
          fontFamily: "Avenir Next, Helvetica Neue, sans-serif",
          fontSize: Math.max(8, placement.radius * 0.18),
          fontWeight: "700",
        }),
      );
      progress.anchor.set(0.5, 0);
      progress.position.set(anchorX, anchorY + badgeRadius + 1);
      progress.eventMode = "none";
      layer.addChild(progress);
    }
  }
}

function drawBattlefieldRemnants(layer: Container, session: SessionSnapshot, placement: BoardPlacement, commanderFactionID = session.player_faction_id, fogPerspectiveUnitID = ""): void {
  const visibleCoords = fogVisibleCoordSet(session, commanderFactionID, fogPerspectiveUnitID);
  const canSee = (q: number, r: number) => !session.fog_of_war_enabled || visibleCoords.has(coordKey(q, r));
  for (const marker of session.grave_markers ?? []) {
    if (!canSee(marker.q, marker.r)) continue;
    const center = tileCenter(marker.q, marker.r, placement);
    const text = new Text("🪦", new TextStyle({
      fontFamily: "\"Apple Color Emoji\", \"Segoe UI Emoji\", \"Noto Color Emoji\", sans-serif",
      fontSize: Math.max(13, placement.radius * 0.38),
    }));
    text.anchor.set(0.5);
    text.position.set(center.x - placement.radius * 0.46, center.y + placement.radius * 0.55);
    text.alpha = 0.88;
    text.eventMode = "none";
    layer.addChild(text);
  }
  for (const drop of session.ground_loot_drops ?? []) {
    if (!canSee(drop.q, drop.r)) continue;
    const center = tileCenter(drop.q, drop.r, placement);
    const text = new Text("🎒", new TextStyle({
      fontFamily: "\"Apple Color Emoji\", \"Segoe UI Emoji\", \"Noto Color Emoji\", sans-serif",
      fontSize: Math.max(13, placement.radius * 0.38),
    }));
    text.anchor.set(0.5);
    text.position.set(center.x - placement.radius * 0.18, center.y + placement.radius * 0.58);
    text.alpha = 0.92;
    text.eventMode = "none";
    layer.addChild(text);
  }
}

// drawStructureBadgePlate 画羊皮纸圆形徽章底盘。
function drawStructureBadgePlate(g: Graphics, r: number, completed: boolean): void {
  const fill = completed ? 0xe8d6a5 : 0xc7b287;
  const fillAlpha = completed ? 0.95 : 0.75;
  const ring = palette.panelLine; // 0x9f8751 古铜
  const ink = 0x2b2218;

  g.lineStyle({ color: ring, alpha: 0.95, width: 1.4 });
  g.beginFill(fill, fillAlpha);
  g.drawCircle(0, 0, r);
  g.endFill();
  g.lineStyle({ color: ink, alpha: 0.3, width: 0.7 });
  g.drawCircle(0, 0, r * 0.82);
  g.lineStyle({ color: 0, alpha: 0, width: 0 });
}

// structureEmoji 返回各建筑对应的 emoji。
function structureEmoji(type: string): string {
  switch (type) {
    case "farmland":
      return "🌾";
    case "forge":
      return "⚒️";
    case "watchtower":
      return "🗼";
    case "turret":
      return "🏰";
    case "trap":
      return "⚠️";
    default:
      return "🏗️";
  }
}


const unitAvatarTextures = new Map<string, Texture>();

function getUnitAvatarUrl(unit: SessionSnapshot["player_units"][number]): string {
  let hash = 0;
  for (let i = 0; i < unit.id.length; i++) {
    hash = (hash << 5) - hash + unit.id.charCodeAt(i);
    hash |= 0;
  }
  hash = Math.abs(hash);
  const isFemale = unit.identity?.gender === "female";
  
  // avatarFiles are sorted: 0-49 male, 50-99 female
  const pool = isFemale ? avatarFiles.slice(50) : avatarFiles.slice(0, 50);
  const filename = pool[hash % pool.length];
  return `/characters/${filename}`;
}

// drawUnits 绘制单位 token、血条、气泡与执行进度徽章。
function drawUnits(
  layer: Container,
  model: SceneModel,
  placement: BoardPlacement,
  viewportWidth: number,
  viewportHeight: number,
): void {
  if (!model.session) {
    return;
  }
  const session = model.session;
  const nowMs = model.nowMs ?? Date.now();
  const traces = latestDecisionByUnit(session.decision_traces);
  const bubbleLines = unitBubbleLinesByUnit(session, nowMs);
  const aiTurnLines = latestAITurnLineByUnit(session.logs, session.turn_state.turn);
  const dialogueThreadCount = unitDialogueThreadCountByUnit(session.logs);
  const executionMap = latestExecutionMarkerByUnit(model.executionMarkers ?? [], session.turn_state.turn);
  // ambient/wild：命运主世界里「世间众生」——据点公共 NPC（ambient_units，faction_spawn 播种、静态可见）
  // 与野外散人（wild_units，此前 drawUnits 也漏画了）。它们随 player/enemy 一起上图，但用区别色 + 小一号 token，
  // 让她在地图上看得见身边有名有姓的人，又不与敌我交战单位混淆。ambientIDs 供下方 token 取色/取半径时区分。
  // 防御性过滤已死的背景 NPC（life_state!=="active"）：mortal NPC 衰老老死/战斗被杀后，后端死亡链会把其 id 从
  // ambient/wild 名册摘除（数据正源），这里再滤一道兜底——避免后端残留把死者当「世间众生」画成永久幽灵 token。
  const ambientAndWild = [...(session.ambient_units ?? []), ...(session.wild_units ?? [])].filter(
    (unit) => unit.status.life_state === "active",
  );
  const ambientIDs = new Set(ambientAndWild.map((unit) => unit.id));
  const units = [...session.player_units, ...session.enemy_units, ...ambientAndWild];
  const unitNames = new Map(units.map((unit) => [unit.id, unit.identity.name]));
  const injuryMap = latestInjuryMarkerByUnit(session.logs, session.raw_event_log ?? [], unitNames, nowMs);
  const selectedCoord = model.selectedTileCoord;
  const orderedUnits = selectedCoord
    ? [
        ...units.filter((unit) => unit.status.position_q !== selectedCoord.q || unit.status.position_r !== selectedCoord.r),
        ...units.filter((unit) => unit.status.position_q === selectedCoord.q && unit.status.position_r === selectedCoord.r),
      ]
    : units;

  const unitCenters = new Map<string, UnitScreenCenter>();
  for (const unit of units) {
    let center = tileCenter(unit.status.position_q, unit.status.position_r, placement);
    center = { x: center.x - placement.radius * 0.25, y: center.y - placement.radius * 0.25 };
    unitCenters.set(unit.id, center);
  }

  const dialoguePairs = latestUnitDialoguePairs(session.logs, session.turn_state.turn);
  for (const pair of dialoguePairs) {
    const leftCenter = unitCenters.get(pair.leftID);
    const rightCenter = unitCenters.get(pair.rightID);
    if (!leftCenter || !rightCenter) {
      continue;
    }
    layer.addChild(drawUnitDialogueThread(leftCenter, rightCenter, placement.radius, pair.summary, viewportWidth, viewportHeight));
  }

  // 单位头顶文案不再只取最新一条：同一回合内一个单位可能经历多次 LLM 交互，全部合并成多行气泡展示。
  for (const unit of orderedUnits) {
    const center = unitCenters.get(unit.id) ?? tileCenter(unit.status.position_q, unit.status.position_r, placement);
    const alive = unit.status.life_state === "active";
    const selected = selectedCoord?.q === unit.status.position_q && selectedCoord?.r === unit.status.position_r;
    // ambient/wild NPC 画小一号（0.28× 对 player/enemy 的 0.36×），视觉上「退到背景里」当路人。
    const isAmbient = ambientIDs.has(unit.id);
    const tokenRadius = placement.radius * (isAmbient ? 0.28 : 0.36);

    // 不再给选中单位画圆环，因为地块已经有高亮边框了
    // 只有在没有被选中的情况下，或者保留作为补充提示
    
    const tokenContainer = new Container();
    tokenContainer.eventMode = "static";
    tokenContainer.cursor = "pointer";
    tokenContainer.hitArea = new Circle(center.x, center.y, tokenRadius + Math.max(8, placement.radius * 0.18));
    // 为什么是 pointertap 而非 pointerdown（开发计划 2026-06-10 §1，吸收态根因）：
    // didDrag 全文件唯一复位点在 stage 的 pointerdown；token 若绑 pointerdown 且 stopPropagation，
    // 会短路这次复位——拖一次图（didDrag=true）后点任何「人」永远没反应，且连续点人无法自愈。
    // 改为与地形格一致的 pointertap 且不拦截冒泡：按下时 stage 正常复位 didDrag、还可从 token 起手拖图；
    // 抬起构成 tap 才触发选中，真拖拽则被 handleTileClick 的 didDrag 防护吞掉。
    // 注意：不得改成在 pointerup/endDrag 里清 didDrag——pointertap 在 pointerup 之后派发，先清会让防护彻底失效。
    tokenContainer.on("pointertap", () => {
      model.onTileClick(unit.status.position_q, unit.status.position_r);
    });

    const token = new Graphics();
    // 取色：阵亡=废墟灰；ambient/wild=素淡墨褐（路人，不分敌我）；其余按指挥阵营分玩家暖/敌方冷。
    const tokenFill = !alive
      ? palette.ruin
      : isAmbient
        ? palette.ambient
        : unit.faction_id === session.player_faction_id
          ? palette.player
          : palette.enemy;
    token.beginFill(tokenFill, alive ? 0.98 : 0.5);
    token.lineStyle({
      color: selected ? palette.selected : palette.ink,
      alpha: 0.85,
      width: selected ? 3 : 2,
    });
    token.drawCircle(center.x, center.y, tokenRadius);
    token.endFill();
    const tokenShade = new Graphics();
    tokenShade.beginFill(0x000000, 0.18);
    tokenShade.drawCircle(center.x, center.y + tokenRadius * 0.08, tokenRadius * 0.84);
    tokenShade.endFill();

    const avatarUrl = unit.identity.portrait_url || getUnitAvatarUrl(unit);
    if (!unitAvatarTextures.has(avatarUrl)) {
      unitAvatarTextures.set(avatarUrl, Texture.from(avatarUrl));
    }
    const cachedAvatarTexture = unitAvatarTextures.get(avatarUrl);
    const avatarSprite = new Sprite(cachedAvatarTexture && cachedAvatarTexture.baseTexture ? cachedAvatarTexture : Texture.WHITE);
    avatarSprite.anchor.set(0.5);
    avatarSprite.position.set(center.x, center.y);
    avatarSprite.width = tokenRadius * 1.5;
    avatarSprite.height = tokenRadius * 1.5;
    
    if (!alive) {
      avatarSprite.tint = 0x555555;
      avatarSprite.alpha = 0.6;
    }

    const hpTrack = new Graphics();
    const hpStart = -Math.PI * 0.15;
    const hpSweep = Math.PI * 1.3;
    const hpRatio = clamp(Math.max(0, unit.status.hp) / 100, 0, 1);
    hpTrack.lineStyle({
      color: 0x000000,
      alpha: 0.38,
      width: Math.max(2, placement.radius * 0.08),
    });
    hpTrack.arc(center.x, center.y, tokenRadius + 3, hpStart, hpStart + hpSweep);
    hpTrack.lineStyle({
      color: hpRingColor(unit.status.hp, alive),
      alpha: alive ? 0.96 : 0.62,
      width: Math.max(2, placement.radius * 0.08),
    });
    hpTrack.arc(center.x, center.y, tokenRadius + 3, hpStart, hpStart + hpSweep * hpRatio);

    tokenContainer.addChild(token, tokenShade, avatarSprite, hpTrack);
    layer.addChild(tokenContainer);
    const liveExecution = executionMap.get(unit.id);
    if (liveExecution) {
      layer.addChild(drawExecutionBadge(center.x, center.y + placement.radius * 0.88, placement.radius, liveExecution));
    }
    const liveInjury = injuryMap.get(unit.id);
    if (liveInjury) {
      const injuryY = center.y + placement.radius * (liveExecution ? 1.48 : 1.14);
      layer.addChild(drawInjuryBadge(center.x, injuryY, placement.radius, liveInjury, nowMs));
    }

    const trace = traces.get(unit.id);
    const tagText = selected ? unitTagText(session, trace, aiTurnLines.get(unit.id)) : "";
    if (tagText) {
      const tag = new Text(
        tagText,
        new TextStyle({
          fill: palette.brass,
          fontFamily: "Avenir Next, Helvetica Neue, sans-serif",
          fontSize: Math.max(9, placement.radius * 0.22),
          fontWeight: "700",
          letterSpacing: 0.5,
        }),
      );
      tag.anchor.set(0.5);
      tag.position.set(center.x, center.y - placement.radius * 0.74);
      layer.addChild(tag);
    }

    const bubbleText = bubbleLines.get(unit.id)?.join("\n") ?? "";
    if (bubbleText) {
      const hasDialogueThreads = (dialogueThreadCount.get(unit.id) ?? 0) > 0;
      const canOpenChat = selected && hasDialogueThreads;
      layer.addChild(
        drawBubble(
          center.x,
          center.y,
          placement.radius,
          bubbleText,
          viewportWidth,
          viewportHeight,
          canOpenChat ? "点此查看对话" : "",
          canOpenChat ? () => model.onOpenUnitChat?.(unit.id) : undefined,
        ),
      );
    }
  }
}

// drawTurnCard 绘制右上角回合与阶段信息卡。
function drawTurnCard(width: number, height: number, session: SessionSnapshot): Container {
  const layer = new Container();
  const cardW = Math.min(430, width * 0.58);
  const cardH = 94;
  const x = 16;
  const y = height - cardH - 14;

  const box = new Graphics();
  box.beginFill(palette.panel, 0.95);
  box.lineStyle({
    color: palette.panelLine,
    alpha: 0.35,
    width: 2,
  });
  box.drawRoundedRect(x, y, cardW, cardH, 14);
  box.endFill();

  const title = new Text(
    `T${session.turn_state.turn} · ${phaseLabel(session.turn_state.phase)}`,
    new TextStyle({
      fill: palette.ink,
      fontFamily: "Iowan Old Style, Palatino Linotype, serif",
      fontSize: 21,
      fontWeight: "700",
    }),
  );
  title.position.set(x + 14, y + 12);

  const directive = truncateDirective(
    session.outcome === "ongoing" ? session.global_directive.text : outcomeLabel(session.outcome),
    72,
  );
  const subtitle = new Text(
    directive,
    new TextStyle({
      fill: palette.muted,
      fontFamily: "Avenir Next, Helvetica Neue, sans-serif",
      fontSize: 12,
      lineHeight: 16,
    }),
  );
  subtitle.position.set(x + 14, y + 46);

  layer.addChild(box, title, subtitle);
  return layer;
}

// drawTerrainLegend 绘制地形图例与当前规则摘要。
function drawTerrainLegend(width: number, session: SessionSnapshot): Container {
  const layer = new Container();
  const entries = terrainOrder
    .map((terrain) => ({
      terrain,
      count: session.map.counts[terrain] ?? 0,
      visual: terrainVisualFor(terrain),
    }))
    .filter((entry) => entry.count > 0);

  if (entries.length === 0) {
    return layer;
  }

  const rowHeight = 20;
  const cardWidth = 188;
  const cardHeight = 36 + entries.length * rowHeight;
  const x = width - cardWidth - 14;
  const y = 14;

  const box = new Graphics();
  box.beginFill(palette.panel, 0.92);
  box.lineStyle({
    color: palette.panelLine,
    alpha: 0.35,
    width: 2,
  });
  box.drawRoundedRect(x, y, cardWidth, cardHeight, 12);
  box.endFill();
  layer.addChild(box);

  const title = new Text(
    "地形图例",
    new TextStyle({
      fill: palette.ink,
      fontFamily: "Iowan Old Style, Palatino Linotype, serif",
      fontSize: 14,
      fontWeight: "700",
    }),
  );
  title.position.set(x + 12, y + 9);
  layer.addChild(title);

  for (let i = 0; i < entries.length; i += 1) {
    const entry = entries[i];
    const rowY = y + 30 + i * rowHeight;

    const swatch = new Graphics();
    swatch.beginFill(entry.visual.color, 0.92);
    swatch.drawRoundedRect(x + 12, rowY + 1, 13, 13, 4);
    swatch.endFill();
    layer.addChild(swatch);

    const texture = terrainTextures.get(entry.terrain);
    if (texture && texture.valid) {
      const icon = new Sprite(texture);
      icon.anchor.set(0.5);
      icon.position.set(x + 19, rowY + 8);
      icon.scale.set(0.3);
      icon.alpha = 0.9;
      layer.addChild(icon);
    }

    const line = new Text(
      `${entry.visual.label} × ${entry.count}`,
      new TextStyle({
        fill: palette.ink,
        fontFamily: "Avenir Next, Helvetica Neue, sans-serif",
        fontSize: 11,
      }),
    );
    line.position.set(x + 32, rowY);
    layer.addChild(line);
  }

  return layer;
}

// latestDecisionByUnit 按单位提取最新决策轨迹。
function latestDecisionByUnit(traces: DecisionTrace[]): Map<string, DecisionTrace> {
  const result = new Map<string, DecisionTrace>();
  for (const trace of traces) {
    result.set(trace.unit_id, trace);
  }
  return result;
}

// latestAITurnLineByUnit 按单位提取当前回合最新日志行。
function latestAITurnLineByUnit(logs: SessionLog[], turn: number): Map<string, string> {
  const ranked = new Map<string, { text: string; priority: number }>();
  for (const entry of logs) {
    if (entry.turn !== turn) {
      continue;
    }
    const targets = lineTargetUnitIDs(entry);
    if (targets.length === 0) {
      continue;
    }
    const candidate = aiTurnLineFromLog(entry);
    if (!candidate) {
      continue;
    }
    for (const unitID of targets) {
      const previous = ranked.get(unitID);
      if (!previous || candidate.priority >= previous.priority) {
        ranked.set(unitID, candidate);
      }
    }
  }
  const result = new Map<string, string>();
  for (const [unitID, value] of ranked) {
    result.set(unitID, value.text);
  }
  return result;
}

// unitBubbleLinesByUnit 汇总当前回合单位所有可展示头顶气泡文本。
function unitBubbleLinesByUnit(session: SessionSnapshot, nowMs: number): Map<string, string[]> {
  const result = new Map<string, string[]>();
  const seen = new Map<string, Set<string>>();
  const addLine = (unitID: string | undefined, text: string | undefined, maxRunes = 18) => {
    const id = unitID?.trim();
    const value = truncateRunes(trimSpeakerPrefix(text ?? ""), maxRunes);
    if (!id || !value) {
      return;
    }
    const unitSeen = seen.get(id) ?? new Set<string>();
    const dedupeKey = normalizeBubbleDedupeKey(value);
    for (const existing of unitSeen) {
      if (isSameBubbleText(existing, dedupeKey)) {
        return;
      }
    }
    if (unitSeen.has(dedupeKey)) {
      return;
    }
    unitSeen.add(dedupeKey);
    seen.set(id, unitSeen);
    const lines = result.get(id) ?? [];
    lines.push(value);
    result.set(id, lines);
  };

  for (const trace of session.decision_traces) {
    if (trace.turn !== session.turn_state.turn) {
      continue;
    }
    addLine(trace.unit_id, trace.speak, 18);
  }

  for (const entry of session.dialogue_history ?? []) {
    if (entry.turn !== session.turn_state.turn || entry.speaker === "player") {
      continue;
    }
    addLine(entry.unit_id, entry.message, 22);
  }

  for (const entry of session.logs) {
    if (entry.turn !== session.turn_state.turn || !isTwoPartyBubbleLog(entry.kind)) {
      continue;
    }
    const occurredAtMs = Date.parse(entry.occurred_at);
    const ageMs = nowMs - occurredAtMs;
    if (!Number.isFinite(ageMs) || ageMs < 0 || ageMs > TRADE_BUBBLE_LIFETIME_MS) {
      continue;
    }
    addLine(entry.actor_unit_id, twoPartyBubbleText(entry), 22);
    addLine(entry.target_unit_id, twoPartyBubbleText(entry), 22);
  }

  for (const [unitID, lines] of result.entries()) {
    result.set(unitID, lines.slice(-MAX_UNIT_BUBBLE_LINES));
  }

  return result;
}

function normalizeBubbleDedupeKey(value: string): string {
  return value.replace(/…$/u, "").replace(/\s+/g, " ").trim();
}

function isSameBubbleText(left: string, right: string): boolean {
  if (!left || !right) {
    return false;
  }
  if (left === right) {
    return true;
  }
  const minLength = Math.min(Array.from(left).length, Array.from(right).length);
  return minLength >= 8 && (left.startsWith(right) || right.startsWith(left));
}

function isTwoPartyBubbleLog(kind: string): boolean {
  return [
    "trade_offer",
    "trade_accept",
    "trade_rejected",
    "trade",
    "romance",
    "romance_hold",
    "romance_proposal",
    "family",
    "family_hold",
    "pregnancy",
  ].includes(kind);
}

function twoPartyBubbleText(entry: SessionLog): string {
  const text = trimSpeakerPrefix(entry.message);
  const colonIndex = Math.max(text.lastIndexOf("："), text.lastIndexOf(":"));
  if (colonIndex >= 0 && colonIndex + 1 < text.length) {
    return text.slice(colonIndex + 1).trim();
  }
  if (entry.kind === "trade_rejected") {
    return text.replace(/^.*?拒绝交易[：:]/, "拒绝：").trim();
  }
  if (entry.kind === "trade_accept") {
    return text.replace(/^.*?接受交易[：:]/, "接受：").trim();
  }
  return text;
}

// latestUnitDialoguePairs 提取当前回合最近的单位-单位对话关系，用于在地图上画连接线。
function latestUnitDialoguePairs(logs: SessionLog[], turn: number): UnitDialoguePair[] {
  const pairs: UnitDialoguePair[] = [];
  const seen = new Set<string>();
  for (let index = logs.length - 1; index >= 0 && pairs.length < 4; index--) {
    const entry = logs[index];
    if (entry.turn !== turn || !isUnitDialogueLog(entry)) {
      continue;
    }
    const leftID = entry.actor_unit_id?.trim() ?? "";
    const rightID = entry.target_unit_id?.trim() ?? "";
    if (!leftID || !rightID || leftID === rightID) {
      continue;
    }
    const key = [leftID, rightID].sort().join(":");
    if (seen.has(key)) {
      continue;
    }
    seen.add(key);
    pairs.push({
      leftID,
      rightID,
      summary: unitDialogueTextFromLog(entry),
    });
  }
  return pairs.reverse();
}

// unitDialogueThreadCountByUnit 统计每个单位参与过多少条 unit_dialogue 线程。
function unitDialogueThreadCountByUnit(logs: SessionLog[]): Map<string, number> {
  const result = new Map<string, number>();
  for (const entry of logs) {
    if (!isUnitDialogueLog(entry)) {
      continue;
    }
    for (const unitID of lineTargetUnitIDs(entry)) {
      result.set(unitID, (result.get(unitID) ?? 0) + 1);
    }
  }
  return result;
}

function isUnitDialogueLog(entry: SessionLog): boolean {
  return isTwoPartyDialogueLog(entry.kind);
}

function isTwoPartyDialogueLog(kind: string): boolean {
  return [
    "unit_dialogue",
    "romance_proposal",
    "romance",
    "romance_hold",
    "family",
    "family_hold",
    "pregnancy",
    "trade_offer",
    "trade_accept",
    "trade_rejected",
    "trade",
  ].includes(kind);
}

function unitDialogueTextFromLog(entry: SessionLog): string {
  return entry.message.trim();
}

// lineTargetUnitIDs 解析日志中的目标单位 ID。
function lineTargetUnitIDs(entry: SessionLog): string[] {
  const ids: string[] = [];
  const actorUnitID = entry.actor_unit_id?.trim();
  if (actorUnitID) {
    ids.push(actorUnitID);
  }
  const targetUnitID = entry.target_unit_id?.trim();
  if (targetUnitID && !ids.includes(targetUnitID)) {
    ids.push(targetUnitID);
  }
  return ids;
}

// aiTurnLineFromLog 从日志文本提取 AI 可读行动句。
function aiTurnLineFromLog(entry: SessionLog): { text: string; priority: number } | null {
  switch (entry.kind) {
    case "action_narration":
      return { text: trimSpeakerPrefix(entry.message), priority: 6 };
    case "reaction_queue":
      return { text: trimSpeakerPrefix(entry.message), priority: 6 };
    case "unit_dialogue":
      return { text: trimSpeakerPrefix(entry.message), priority: 5 };
    case "shake":
    case "emotional_override":
      return { text: trimSpeakerPrefix(entry.message), priority: 5 };
    case "trade":
    case "trade_hold":
    case "trade_blocked":
      return { text: trimSpeakerPrefix(entry.message), priority: 4 };
    case "knowledge":
      return { text: trimSpeakerPrefix(entry.message), priority: 4 };
    case "eat":
    case "pigeon_send":
    case "pigeon_deliver":
    case "pigeon_intercept":
    case "pigeon_blocked":
    case "pigeon_lost":
    case "pigeon_attachment":
    case "pigeon_attachment_lost":
    case "random_event":
      return { text: trimSpeakerPrefix(entry.message), priority: 3 };
    case "speech":
      return { text: trimSpeakerPrefix(entry.message), priority: 3 };
    case "attack":
    case "attack_miss":
    case "move":
    case "move_blocked":
    case "advance":
    case "defend":
    case "observe":
    case "assist":
    case "hold":
    case "skill":
    case "build":
    case "gather":
      return { text: trimSpeakerPrefix(entry.message), priority: 2 };
    default:
      return null;
  }
}

// trimSpeakerPrefix 去除“角色名：”前缀，保留有效内容。
function trimSpeakerPrefix(message: string): string {
  const value = message.trim();
  if (value === "") {
    return "";
  }
  const parts = value.split("：");
  if (parts.length < 2) {
    return value;
  }
  return parts.slice(1).join("：").trim();
}

// unitTagText 生成单位头顶短标签文本。
function unitTagText(session: SessionSnapshot, trace: DecisionTrace | undefined, aiTurnLine: string | undefined): string {
  if (aiTurnLine) {
    return truncateTag(aiTurnLine);
  }
  if (!trace || trace.turn !== session.turn_state.turn) {
    return "";
  }
  if (trace.next_action) {
    return truncateTag(trace.next_action);
  }
  if (trace.speak) {
    return truncateTag(trace.speak);
  }
  if (trace.memory) {
    return truncateTag(trace.memory);
  }
  if (trace.reasoning) {
    return truncateTag(trace.reasoning);
  }
  return "";
}

// truncateTag 裁剪短标签长度。
function truncateTag(text: string): string {
  return truncateRunes(text, 8);
}

// truncateRunes 按 rune 数裁剪字符串并追加省略号。
function truncateRunes(text: string, max: number): string {
  const value = text.trim();
  if (!value) {
    return "";
  }
  const runes = Array.from(value);
  if (runes.length <= max) {
    return value;
  }
  return `${runes.slice(0, max).join("")}…`;
}

// truncateDirective 裁剪方针文本长度。
function truncateDirective(text: string, max: number): string {
  const value = text.trim();
  if (value.length <= max) {
    return value;
  }
  return `${value.slice(0, max)}…`;
}

// drawBubble 绘制固定在单位头顶的文本气泡，减少跨格遮挡。
function drawBubble(
  unitX: number,
  unitY: number,
  radius: number,
  text: string,
  boardWidth: number,
  boardHeight: number,
  ctaText = "",
  onOpenDialogues?: () => void,
): Container {
  const layer = new Container();
  layer.eventMode = "passive";
  const padX = 9;
  const padY = 6;
  const maxBubbleWidth = Math.max(82, Math.min(178, radius * 4.4));

  const label = new Text(
    text,
    new TextStyle({
      fill: palette.inkDark,
      fontFamily: "Avenir Next, Helvetica Neue, sans-serif",
      fontSize: Math.max(9, radius * 0.19),
      fontWeight: "700",
      lineHeight: Math.max(12, radius * 0.25),
      wordWrap: true,
      wordWrapWidth: maxBubbleWidth - padX * 2,
      breakWords: true,
    }),
  );
  label.anchor.set(0.5, 0);
  label.eventMode = "none";

  const normalizedCTA = ctaText.trim();
  const hasCTA = normalizedCTA !== "" && typeof onOpenDialogues === "function";
  const ctaLabel = hasCTA
    ? new Text(
        normalizedCTA,
        new TextStyle({
          fill: palette.enemy,
          fontFamily: "Avenir Next, Helvetica Neue, sans-serif",
          fontSize: Math.max(8, radius * 0.16),
          fontWeight: "700",
          letterSpacing: 0.2,
        }),
      )
    : null;
  if (ctaLabel) {
    ctaLabel.anchor.set(0.5, 0);
    ctaLabel.eventMode = "none";
  }

  const ctaGap = ctaLabel ? 5 : 0;
  const ctaHeight = ctaLabel ? ctaLabel.height + 8 : 0;
  const bubbleWidth = Math.max(54, label.width + padX * 2, (ctaLabel?.width ?? 0) + padX * 2);
  const bubbleHeight = Math.max(18, label.height + padY * 2 + ctaGap + ctaHeight);
  const topSafePadding = boardSafeInset.top + 8;
  const rightSafePadding = boardWidth < 820 ? 12 : boardSafeInset.right + 8;
  const bottomSafePadding = boardHeight < 680 ? 74 : boardSafeInset.bottom;
  const preferredTop = unitY - radius * 1.18 - bubbleHeight;
  const bubbleTop = preferredTop < topSafePadding
    ? clamp(unitY + radius * 0.62, topSafePadding, Math.max(topSafePadding, boardHeight - bottomSafePadding - bubbleHeight))
    : clamp(preferredTop, topSafePadding, Math.max(topSafePadding, boardHeight - bottomSafePadding - bubbleHeight));
  const bubbleLeft = clamp(unitX - bubbleWidth / 2, 8, Math.max(8, boardWidth - rightSafePadding - bubbleWidth));
  const bubbleCenterX = bubbleLeft + bubbleWidth / 2;
  label.position.set(bubbleCenterX, bubbleTop + padY);

  const tailCenterX = unitX;
  const bubbleBelowUnit = bubbleTop > unitY;
  const tailTipY = bubbleBelowUnit ? unitY + radius * 0.36 : unitY - radius * 0.36;
  const tailBaseY = bubbleTop + bubbleHeight - 1;

  const bubble = new Graphics();
  bubble.eventMode = "none";
  bubble.beginFill(palette.panelLight, 0.78);
  bubble.lineStyle({
    color: palette.enemy,
    alpha: 0.22,
    width: 1.1,
  });
  bubble.drawRoundedRect(
    bubbleLeft,
    bubbleTop,
    bubbleWidth,
    bubbleHeight,
    8,
  );
  bubble.endFill();

  bubble.beginFill(palette.panelLight, 0.78);
  if (bubbleBelowUnit) {
    bubble.drawPolygon([tailCenterX - 4, bubbleTop + 1, tailCenterX + 4, bubbleTop + 1, unitX, tailTipY]);
  } else {
    bubble.drawPolygon([tailCenterX - 4, tailBaseY, tailCenterX + 4, tailBaseY, unitX, tailTipY]);
  }
  bubble.endFill();

  layer.addChild(bubble, label);

  if (ctaLabel) {
    const ctaTop = bubbleTop + bubbleHeight - ctaHeight - 4;
    const ctaBox = new Graphics();
    ctaBox.eventMode = "none";
    ctaBox.beginFill(0xd8e6ee, 0.88);
    ctaBox.lineStyle({
      color: palette.enemy,
      alpha: 0.28,
      width: 1,
    });
    ctaBox.drawRoundedRect(
      bubbleLeft + 6,
      ctaTop,
      bubbleWidth - 12,
      ctaHeight,
      6,
    );
    ctaBox.endFill();
    ctaLabel.position.set(bubbleCenterX, ctaTop + 3);

    const hitbox = new Graphics();
    hitbox.beginFill(0xffffff, 0.001);
    hitbox.drawRect(bubbleLeft + 6, ctaTop, bubbleWidth - 12, ctaHeight);
    hitbox.endFill();
    hitbox.eventMode = "static";
    hitbox.cursor = "pointer";
    hitbox.hitArea = new Rectangle(bubbleLeft + 6, ctaTop, bubbleWidth - 12, ctaHeight);
    // pointerdown 不再 stopPropagation（与单位 token 同理，开发计划 2026-06-10 §1）：
    // 让 stage 的 pointerdown——didDrag 的唯一复位点——正常收到冒泡，避免「拖图后点击失效」吸收态，且可从 CTA 起手拖图。
    // pointertap 保留 stopPropagation：tap 语义上 CTA 独占这次点击，防止冒泡穿透触发底下 tile/token 的选中。
    hitbox.on("pointertap", (event) => {
      event.stopPropagation();
      onOpenDialogues?.();
    });

    layer.addChild(ctaBox, ctaLabel, hitbox);
  }

  return layer;
}

// drawUnitDialogueThread 在两个正在交流的单位之间绘制柔和连线和“交谈”摘要标签。
function drawUnitDialogueThread(
  left: UnitScreenCenter,
  right: UnitScreenCenter,
  radius: number,
  summary: string,
  boardWidth: number,
  boardHeight: number,
): Container {
  const layer = new Container();
  const dx = right.x - left.x;
  const dy = right.y - left.y;
  const distance = Math.max(1, Math.hypot(dx, dy));
  const nx = dx / distance;
  const ny = dy / distance;
  const startX = left.x + nx * radius * 0.5;
  const startY = left.y + ny * radius * 0.5;
  const endX = right.x - nx * radius * 0.5;
  const endY = right.y - ny * radius * 0.5;

  const line = new Graphics();
  line.lineStyle({
    color: palette.brass,
    alpha: 0.58,
    width: Math.max(1.4, radius * 0.045),
  });
  drawDashedLine(line, startX, startY, endX, endY, Math.max(6, radius * 0.18), Math.max(4, radius * 0.12));

  line.beginFill(palette.brass, 0.22);
  line.drawCircle(left.x, left.y, Math.max(5, radius * 0.16));
  line.drawCircle(right.x, right.y, Math.max(5, radius * 0.16));
  line.endFill();
  line.lineStyle({ color: palette.brass, alpha: 0.62, width: 1 });
  line.drawCircle(left.x, left.y, Math.max(7, radius * 0.2));
  line.drawCircle(right.x, right.y, Math.max(7, radius * 0.2));
  layer.addChild(line);

  const labelText = summary ? `交谈：${truncateRunes(summary, 12)}` : "交谈中";
  const label = new Text(
    labelText,
    new TextStyle({
      fill: palette.ink,
      fontFamily: "Avenir Next, Helvetica Neue, sans-serif",
      fontSize: Math.max(9, radius * 0.18),
      fontWeight: "800",
      letterSpacing: 0.3,
    }),
  );
  const padX = 7;
  const padY = 4;
  const labelWidth = label.width + padX * 2;
  const labelHeight = label.height + padY * 2;
  const rightSafePadding = boardWidth < 820 ? 12 : boardSafeInset.right + 8;
  const bottomSafePadding = boardHeight < 680 ? 74 : boardSafeInset.bottom;
  const midX = clamp((left.x + right.x) / 2, 8 + labelWidth / 2, Math.max(8 + labelWidth / 2, boardWidth - rightSafePadding - labelWidth / 2));
  const midY = clamp((left.y + right.y) / 2 - radius * 0.38, boardSafeInset.top, Math.max(boardSafeInset.top, boardHeight - bottomSafePadding - labelHeight));
  label.anchor.set(0.5, 0.5);
  label.position.set(midX, midY);

  const labelBg = new Graphics();
  labelBg.beginFill(palette.panel, 0.84);
  labelBg.lineStyle({ color: palette.brass, alpha: 0.38, width: 1 });
  labelBg.drawRoundedRect(midX - labelWidth / 2, midY - labelHeight / 2, labelWidth, labelHeight, 999);
  labelBg.endFill();
  layer.addChild(labelBg, label);
  return layer;
}

// drawDashedLine 用短线段模拟虚线，避免依赖 Pixi 额外插件。
function drawDashedLine(graphics: Graphics, startX: number, startY: number, endX: number, endY: number, dash: number, gap: number): void {
  const dx = endX - startX;
  const dy = endY - startY;
  const distance = Math.hypot(dx, dy);
  if (distance <= 0) {
    return;
  }
  const steps = Math.ceil(distance / (dash + gap));
  for (let index = 0; index < steps; index++) {
    const segmentStart = Math.min(distance, index * (dash + gap));
    const segmentEnd = Math.min(distance, segmentStart + dash);
    if (segmentStart >= segmentEnd) {
      continue;
    }
    const startRatio = segmentStart / distance;
    const endRatio = segmentEnd / distance;
    graphics.moveTo(startX + dx * startRatio, startY + dy * startRatio);
    graphics.lineTo(startX + dx * endRatio, startY + dy * endRatio);
  }
}

// latestExecutionMarkerByUnit 按单位提取本回合最近执行状态。
function latestExecutionMarkerByUnit(
  markers: Array<{
    unitID: string;
    status: "started" | "completed";
    turn: number;
    startedUnits?: number;
    completedUnits?: number;
    totalUnits?: number;
  }>,
  currentTurn: number,
): Map<
  string,
  {
    status: "started" | "completed";
    startedUnits?: number;
    completedUnits?: number;
    totalUnits?: number;
  }
> {
  const result = new Map<
    string,
    {
      status: "started" | "completed";
      startedUnits?: number;
      completedUnits?: number;
      totalUnits?: number;
    }
  >();
  for (let index = 0; index < markers.length; index += 1) {
    const marker = markers[index];
    if (!marker || marker.turn !== currentTurn || !marker.unitID) {
      continue;
    }
    if (result.has(marker.unitID)) {
      continue;
    }
    result.set(marker.unitID, {
      status: marker.status,
      startedUnits: marker.startedUnits,
      completedUnits: marker.completedUnits,
      totalUnits: marker.totalUnits,
    });
  }
  return result;
}

// drawExecutionBadge 绘制单位“思考中/已完成 + 进度”徽章。
function drawExecutionBadge(
  x: number,
  y: number,
  radius: number,
  marker: {
    status: "started" | "completed";
    startedUnits?: number;
    completedUnits?: number;
    totalUnits?: number;
  },
): Container {
  // 每个单位独立展示“思考中/已完成 + 进度”，对应服务端逐单位事件流。
  const layer = new Container();
  const isCompleted = marker.status === "completed";
  const bgColor = isCompleted ? palette.success : palette.enemy;
  const labelText = isCompleted ? "已完成" : "思考中";

  const label = new Text(
    labelText,
    new TextStyle({
      fill: palette.ink,
      fontFamily: "Avenir Next, Helvetica Neue, sans-serif",
      fontSize: Math.max(8, radius * 0.2),
      fontWeight: "700",
      letterSpacing: 0.3,
    }),
  );
  label.anchor.set(0.5, 0.5);
  label.position.set(x, y);

  const progress = new Text(
    formatExecutionMarkerProgress(marker),
    new TextStyle({
      fill: palette.muted,
      fontFamily: "Avenir Next, Helvetica Neue, sans-serif",
      fontSize: Math.max(7, radius * 0.16),
      fontWeight: "600",
    }),
  );
  progress.anchor.set(0.5, 0.5);
  progress.position.set(x, y + Math.max(10, radius * 0.34));

  const w = Math.max(label.width, progress.width) + 12;
  const h = Math.max(14, radius * 0.38) + Math.max(10, radius * 0.28) + 6;
  const top = y - h / 2;

  const bg = new Graphics();
  bg.beginFill(0x07121a, 0.92);
  bg.lineStyle({
    color: bgColor,
    alpha: 0.6,
    width: 1.3,
  });
  bg.drawRoundedRect(x - w / 2, top, w, h, 7);
  bg.endFill();

  layer.addChild(bg, label, progress);
  return layer;
}

// formatExecutionMarkerProgress 格式化执行进度文本（x/total）。
function formatExecutionMarkerProgress(marker: {
  status: "started" | "completed";
  startedUnits?: number;
  completedUnits?: number;
  totalUnits?: number;
}): string {
  const total = marker.totalUnits ?? 0;
  if (!total || total <= 0) {
    return "";
  }
  if (marker.status === "started" && marker.startedUnits && marker.startedUnits > 0) {
    return `${marker.startedUnits}/${total}`;
  }
  if (marker.status === "completed" && marker.completedUnits && marker.completedUnits > 0) {
    return `${marker.completedUnits}/${total}`;
  }
  return "";
}

// latestInjuryMarkerByUnit 提取短时受伤提示，只保留每个单位最近一条。
function latestInjuryMarkerByUnit(
  logs: SessionLog[],
  rawEvents: RawEventEntry[],
  unitNames: Map<string, string>,
  nowMs: number,
): Map<string, InjuryMarker> {
  const result = new Map<string, InjuryMarker>();

  for (let index = rawEvents.length - 1; index >= 0; index -= 1) {
    const marker = injuryMarkerFromRawEvent(rawEvents[index], unitNames);
    if (!marker || result.has(marker.unitID)) {
      continue;
    }
    const ageMs = nowMs - marker.occurredAtMs;
    if (!Number.isFinite(ageMs) || ageMs < 0 || ageMs > INJURY_MARKER_LIFETIME_MS) {
      continue;
    }
    result.set(marker.unitID, marker);
  }

  for (let index = logs.length - 1; index >= 0; index -= 1) {
    const marker = injuryMarkerFromLog(logs[index], unitNames);
    if (!marker || result.has(marker.unitID)) {
      continue;
    }
    const ageMs = nowMs - marker.occurredAtMs;
    if (!Number.isFinite(ageMs) || ageMs < 0 || ageMs > INJURY_MARKER_LIFETIME_MS) {
      continue;
    }
    result.set(marker.unitID, marker);
  }
  return result;
}

// injuryMarkerFromRawEvent 优先读取结构化状态事件，避免 AI 文案覆盖战斗日志后无法解析“造成 X 伤害”。
function injuryMarkerFromRawEvent(entry: RawEventEntry, unitNames: Map<string, string>): InjuryMarker | null {
  const occurredAtMs = Date.parse(entry.occurred_at);
  if (!Number.isFinite(occurredAtMs)) {
    return null;
  }
  const payload = parseStatusEventPayload(entry.payload_json);
  const field = (payload?.field ?? entry.kind).trim().toLowerCase();
  const delta = typeof payload?.delta === "number" ? payload.delta : 0;
  const unitID = (payload?.unit_id ?? entry.target_unit_id ?? "").trim();
  if (field !== "hp" || delta >= 0 || !unitID) {
    return null;
  }

  const amount = Math.abs(Math.round(delta));
  const actorID = payload?.actors?.find((id) => id && id !== unitID)?.trim() || entry.actor_unit_id?.trim() || "";
  const reasonCode = payload?.reason_code ?? "";
  const reasonText = payload?.reason_text || entry.summary;
  const actorName = unitNames.get(actorID) ?? "未知来源";
  let detail = actorID && actorID !== unitID ? `来自 ${actorName}` : "来自 负面状态";
  if (reasonCode.includes("combat") || /命中|攻击|重击|冲锋/.test(reasonText)) {
    detail = actorID && actorID !== unitID ? `来自 ${actorName}的攻击` : "来自 战斗伤害";
  } else if (/陷阱/.test(reasonText)) {
    detail = "来自 敌方陷阱";
  } else if (/野兽|打猎|擦伤/.test(reasonText)) {
    detail = "来自 野兽擦伤";
  } else if (/饥饿|断粮/.test(reasonText)) {
    detail = "来自 饥饿";
  }

  return {
    unitID,
    title: `受伤 -${amount}`,
    detail: truncateRunes(detail, 12),
    occurredAtMs,
    severity: injurySeverity(amount),
  };
}

function parseStatusEventPayload(payloadJSON?: string): StatusEventPayload | null {
  if (!payloadJSON) {
    return null;
  }
  try {
    const parsed = JSON.parse(payloadJSON) as StatusEventPayload;
    return parsed && typeof parsed === "object" ? parsed : null;
  } catch {
    return null;
  }
}

// injuryMarkerFromLog 从伤害类日志推导“受伤提示”内容。
function injuryMarkerFromLog(entry: SessionLog, unitNames: Map<string, string>): InjuryMarker | null {
  const occurredAtMs = Date.parse(entry.occurred_at);
  if (!Number.isFinite(occurredAtMs)) {
    return null;
  }

  const attackDamage = parseDamageAmount(entry.message);
  const hpLoss = parseLostHP(entry.message);

  if ((entry.kind === "attack" || entry.kind === "skill") && attackDamage > 0) {
    const unitID = entry.target_unit_id?.trim() ?? "";
    if (!unitID) {
      return null;
    }
    return {
      unitID,
      title: `受伤 -${attackDamage}`,
      detail: truncateRunes(formatInjurySourceFromCombat(entry, unitNames), 12),
      occurredAtMs,
      severity: injurySeverity(attackDamage),
    };
  }

  if (entry.kind === "trap" && hpLoss > 0) {
    const unitID = entry.actor_unit_id?.trim() ?? "";
    if (!unitID) {
      return null;
    }
    return {
      unitID,
      title: `受伤 -${hpLoss}`,
      detail: "来自 敌方陷阱",
      occurredAtMs,
      severity: injurySeverity(hpLoss),
    };
  }

  if (entry.kind === "gather_risk" && hpLoss > 0) {
    const unitID = entry.actor_unit_id?.trim() ?? "";
    if (!unitID) {
      return null;
    }
    return {
      unitID,
      title: `受伤 -${hpLoss}`,
      detail: "来自 野兽擦伤",
      occurredAtMs,
      severity: injurySeverity(hpLoss),
    };
  }

  if (entry.kind === "stat_change") {
    const directMatch = entry.message.match(/数值变动\s+([A-Za-z_]+)\s+(-?\d+(?:\.\d+)?)/);
    const triggeredMatch = entry.message.match(/的\s+([A-Za-z_]+)\s+(-?\d+(?:\.\d+)?)/);
    const statMatch = directMatch ?? triggeredMatch;
    const field = statMatch?.[1]?.trim().toLowerCase();
    const delta = statMatch?.[2] ? Number(statMatch[2]) : 0;
    const unitID = entry.target_unit_id?.trim() || entry.actor_unit_id?.trim() || "";
    if (field === "hp" && delta < 0 && unitID) {
      const actorID = entry.actor_unit_id?.trim() ?? "";
      const actorName = unitNames.get(actorID) ?? "未知来源";
      const detail = actorID && actorID !== unitID ? `来自 ${actorName}` : "来自 负面状态";
      return {
        unitID,
        title: `受伤 -${Math.abs(Math.round(delta))}`,
        detail,
        occurredAtMs,
        severity: injurySeverity(Math.abs(delta)),
      };
    }
  }

  return null;
}

// formatInjurySourceFromCombat 拼出战斗受伤来源文案。
function formatInjurySourceFromCombat(entry: SessionLog, unitNames: Map<string, string>): string {
  const actorName = unitNames.get(entry.actor_unit_id?.trim() ?? "") ?? "未知来源";
  if (entry.kind === "skill") {
    const bracketSkill = entry.message.match(/技能【([^】]+)】/);
    const plainSkill = entry.message.match(/技能\[([^\]]+)\]/);
    const skillName = bracketSkill?.[1]?.trim() || plainSkill?.[1]?.trim() || "技能";
    return `来自 ${actorName}的${skillName}`;
  }
  const styleMatch = entry.message.match(/发起(.+?)造成/);
  const styleName = styleMatch?.[1]?.trim() || "攻击";
  return `来自 ${actorName}的${styleName}`;
}

// parseDamageAmount 解析“造成 X 伤害”中的数值。
function parseDamageAmount(message: string): number {
  const matched = message.match(/造成\s*(\d+)\s*伤害/);
  return matched ? Number(matched[1]) : 0;
}

// parseLostHP 解析“失去 X HP”中的数值。
function parseLostHP(message: string): number {
  const matched = message.match(/失去\s*(\d+)\s*HP/i);
  return matched ? Number(matched[1]) : 0;
}

// injurySeverity 按伤害数值给受伤提示分级，决定颜色。
function injurySeverity(amount: number): "low" | "medium" | "high" {
  if (amount >= 20) {
    return "high";
  }
  if (amount >= 8) {
    return "medium";
  }
  return "low";
}

// drawInjuryBadge 在单位附近绘制短暂存在的受伤提示。
function drawInjuryBadge(
  x: number,
  y: number,
  radius: number,
  marker: InjuryMarker,
  nowMs: number,
): Container {
  const layer = new Container();
  layer.eventMode = "none";

  const ageMs = Math.max(0, nowMs - marker.occurredAtMs);
  const progress = clamp(ageMs / INJURY_MARKER_LIFETIME_MS, 0, 1);
  const alpha = 1 - progress;
  layer.alpha = alpha;
  layer.position.y -= progress * Math.max(8, radius * 0.36);

  const accent = marker.severity === "high"
    ? 0xd66a45
    : marker.severity === "medium"
      ? palette.brass
      : 0xc59b86;

  const title = new Text(
    marker.title,
    new TextStyle({
      fill: 0xffebe3,
      fontFamily: "Avenir Next, Helvetica Neue, sans-serif",
      fontSize: Math.max(8, radius * 0.17),
      fontWeight: "800",
    }),
  );
  title.anchor.set(0.5, 0.5);
  title.position.set(x, y - Math.max(7, radius * 0.14));

  const detail = new Text(
    marker.detail,
    new TextStyle({
      fill: 0xf7d8c9,
      fontFamily: "Avenir Next, Helvetica Neue, sans-serif",
      fontSize: Math.max(7, radius * 0.14),
      fontWeight: "600",
    }),
  );
  detail.anchor.set(0.5, 0.5);
  detail.position.set(x, y + Math.max(7, radius * 0.14));

  const width = Math.max(title.width, detail.width) + 14;
  const height = title.height + detail.height + 10;
  const top = y - height / 2;

  const bg = new Graphics();
  bg.eventMode = "none";
  bg.beginFill(0x2a0d0a, 0.88);
  bg.lineStyle({
    color: accent,
    alpha: 0.72,
    width: 1.2,
  });
  bg.drawRoundedRect(x - width / 2, top, width, height, 8);
  bg.endFill();

  layer.addChild(bg, title, detail);
  return layer;
}

// hpRingColor 根据单位存活状态与 HP 返回外圈血条颜色。
function hpRingColor(hp: number, alive: boolean): number {
  if (!alive || hp <= 0) {
    return palette.ruin;
  }
  if (hp <= 25) {
    return palette.ember;
  }
  if (hp <= 55) {
    return palette.brass;
  }
  return palette.success;
}

// phaseLabel 格式化阶段中文名称。
function phaseLabel(phase: string): string {
  switch (phase) {
    case "deployment":
      return "部署阶段";
    default:
      return "执行阶段";
  }
}

// outcomeLabel 格式化胜负状态中文名称。
function outcomeLabel(outcome: string): string {
  switch (outcome) {
    case "victory":
      return "己方胜利";
    case "defeat":
      return "己方失败";
    case "draw":
      return "平局收束";
    default:
      return "进行中";
  }
}

// terrainVisualFor 返回地形对应的颜色与贴图配置。
function terrainVisualFor(terrain: string): TerrainVisual {
  return terrainVisuals[terrain] ?? terrainVisuals.plains;
}

// tileCenter 计算六边格在画布中的中心坐标。
function tileCenter(q: number, r: number, placement: BoardPlacement) {
  return {
    x: placement.originX + q * placement.horizontalStep + (r % 2) * (placement.horizontalStep / 2),
    y: placement.originY + r * placement.verticalStep,
  };
}

// createHexPoints 生成六边形绘制点集。
function createHexPoints(centerX: number, centerY: number, radius: number): number[] {
  const points: number[] = [];
  for (let side = 0; side < 6; side += 1) {
    const angle = ((Math.PI / 180) * 60 * side) + Math.PI / 6;
    points.push(centerX + radius * Math.cos(angle), centerY + radius * Math.sin(angle));
  }
  return points;
}

// clamp 把数值限制在给定区间。
function clamp(value: number, min: number, max: number): number {
  if (value < min) {
    return min;
  }
  if (value > max) {
    return max;
  }
  return value;
}

// destroyChildren 递归销毁容器子节点，避免图形对象泄漏。
function destroyChildren(container: Container): void {
  for (const child of container.removeChildren()) {
    child.destroy();
  }
}
