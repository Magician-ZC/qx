/* 文件说明：PixiBoard React 桥接层，负责把会话模型推送到 Pixi 场景并管理挂载销毁。 */

import { useEffect, useRef } from "react";
import type { SessionSnapshot } from "../session/types";
import { mountPixiBoard } from "./bootstrapPixi";

type Props = {
  session: SessionSnapshot | null;
  commanderFactionID?: string;
  fogPerspectiveUnitID?: string;
  selectedTileCoord: { q: number; r: number } | null;
  onTileClick: (q: number, r: number) => void;
  onOpenDialogues?: () => void;
  onOpenUnitChat?: (unitID: string) => void;
  nowMs?: number;
  zoom?: number;
  executionMarkers?: Array<{
    unitID: string;
    status: "started" | "completed";
    turn: number;
    startedUnits?: number;
    completedUnits?: number;
    totalUnits?: number;
  }>;
};

// PixiBoard 只负责桥接 React 状态与 Pixi 渲染器，渲染细节在 bootstrapPixi.ts。
export function PixiBoard({
  session,
  commanderFactionID = "player",
  fogPerspectiveUnitID = "",
  selectedTileCoord,
  onTileClick,
  onOpenDialogues = () => undefined,
  onOpenUnitChat = () => undefined,
  nowMs = Date.now(),
  zoom = 1,
  executionMarkers = [],
}: Props) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const sceneRef = useRef<Awaited<ReturnType<typeof mountPixiBoard>> | null>(null);
  const latestModelRef = useRef({
    session,
    commanderFactionID,
    fogPerspectiveUnitID,
    selectedTileCoord,
    onTileClick,
    onOpenDialogues,
    onOpenUnitChat,
    nowMs,
    zoom,
    executionMarkers,
  });

  latestModelRef.current = {
    session,
    commanderFactionID,
    fogPerspectiveUnitID,
    selectedTileCoord,
    onTileClick,
    onOpenDialogues,
    onOpenUnitChat,
    nowMs,
    zoom,
    executionMarkers,
  };

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    let disposed = false;

    void mountPixiBoard(container).then((scene) => {
      if (disposed) {
        scene.destroy();
        return;
      }

      sceneRef.current = scene;
      // 首次挂载后立刻用最新模型渲染，避免首帧拿到旧闭包状态。
      scene.render(latestModelRef.current);
    });

    return () => {
      disposed = true;
      sceneRef.current?.destroy();
      sceneRef.current = null;
    };
  }, []);

  useEffect(() => {
    // 会话数据变更时只触发 scene.render，不重新挂载 Pixi 应用。
    sceneRef.current?.render(latestModelRef.current);
  }, [session, commanderFactionID, fogPerspectiveUnitID, selectedTileCoord, onTileClick, onOpenDialogues, onOpenUnitChat, nowMs, zoom, executionMarkers]);

  return <div ref={containerRef} className="pixi-board" aria-label="群像单局战场" />;
}
