// 文件说明：全前端 z-index 设计令牌（single source of truth）。
// 背景：右侧浮层面板/工具栏/全屏模态曾各自硬编码魔法数字，叠层关系靠肉眼对齐、极易打架。
// 此处把分层语义集中为一组只读令牌：TS（组件内联 style）与 CSS（styles.css 的 :root --z-* 变量）
// 双轨引用同一组数值，保证「同一语义层在任何引用处取到同一个 z」。
// 约定：右侧浮层面板（FatePanel/BloodFeud/Chronicle/Consent/WorldBoss/Dungeon/DungeonSegment/Billing/Charter）
// 经 usePanelStack 互斥后一次只显一个，故统一用 rightPanel 同层即可；
// 全屏模态（Governance/LiveOps/Admin）用 fullscreenModal；新手引导用 tour。
export const zIndex = {
  base: 1,
  sticky: 2,
  hud: 14,
  topBar: 50,
  rightPanel: 55,
  phaseButton: 96,
  toolbar: 80,
  selectMenu: 120,
  fullscreenModal: 200,
  tour: 9000,
} as const;

export type ZIndexToken = keyof typeof zIndex;
