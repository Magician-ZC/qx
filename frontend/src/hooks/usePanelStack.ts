// 文件说明：右侧浮层面板互斥栈 Hook（usePanelStack）。
// 背景（根因）：约 10 个右侧浮层面板（命运/血仇/编年史/来意/世界Boss/副本/副本分段/充值/立约）原本各持
// 一个独立的 open 布尔 state，且全都 position 固定在 top:64 right:12 同一处——同时打开多个就直接叠在一起重叠。
// 修法：把这些独立布尔收敛为「单一当前打开面板 ID（或 null）」，同一时刻右侧最多只显示一个面板，
// 打开新面板自动关掉旧的，从根上消除重叠。各面板的渲染条件改为「openPanelId === 对应 id」，
// onClose 改为 closePanel()，工具栏按钮改为 togglePanel(对应 id)。
import { useCallback, useState } from "react";

// PanelID 是所有受互斥管理的右侧浮层面板的联合类型。
// 新增右侧面板时在此登记其 id，并在 App 里用 togglePanel/openPanelId 接线即可。
export type PanelID =
  | "fate"
  | "consent"
  | "bloodFeud"
  | "chronicle"
  | "charter"
  | "billing"
  | "worldBoss"
  | "dungeon"
  | "dungeonSegment";

export interface PanelStack {
  // 当前打开的右侧面板 id；null 表示右侧无面板。
  openPanelId: PanelID | null;
  // 打开指定面板（自动顶掉当前已开的任何面板，保证互斥）。
  openPanel: (id: PanelID) => void;
  // 关闭右侧面板（无论当前开的是哪个）。
  closePanel: () => void;
  // 切换指定面板：当前开的就是它则关闭，否则打开它（同样互斥）。
  togglePanel: (id: PanelID) => void;
  // 便捷判断：某面板当前是否处于打开态。
  isOpen: (id: PanelID) => boolean;
}

// usePanelStack 维护「单活」的右侧面板栈：任意时刻 openPanelId 至多一个。
export function usePanelStack(initial: PanelID | null = null): PanelStack {
  const [openPanelId, setOpenPanelId] = useState<PanelID | null>(initial);

  const openPanel = useCallback((id: PanelID) => {
    setOpenPanelId(id);
  }, []);

  const closePanel = useCallback(() => {
    setOpenPanelId(null);
  }, []);

  const togglePanel = useCallback((id: PanelID) => {
    setOpenPanelId((current) => (current === id ? null : id));
  }, []);

  const isOpen = useCallback(
    (id: PanelID) => openPanelId === id,
    [openPanelId],
  );

  return { openPanelId, openPanel, closePanel, togglePanel, isOpen };
}
