/* 文件说明：shouldCloseUnitScopedPanel 的聚焦回归测试（L2 修复）。
   锁定「来意(consent)/血仇(bloodFeud) 面板在取消选中单位后应被自动关闭」这一触发路径——修复前这两个面板
   渲染条件含 selectedUnitID、工具栏按钮在未选中时 disabled，取消选中后面板卡成「打不开也关不掉」的幽灵态。
   非单位作用域面板（fate/chronicle/charter/billing/...）不受此约束、即便未选中也不应被这条逻辑关闭。
   注意：本文件被 tsconfig.json 的 exclude 排除，不参与 `npm run build` 的 tsc --noEmit；仅 `npm test`（vitest run）执行。*/

import { describe, it, expect } from "vitest";
import { shouldCloseUnitScopedPanel } from "./App";

describe("shouldCloseUnitScopedPanel（L2：取消选中单位后关闭单位作用域面板）", () => {
  it("consent 面板打开且无选中单位 → 应关闭", () => {
    expect(shouldCloseUnitScopedPanel("consent", null)).toBe(true);
  });

  it("bloodFeud 面板打开且无选中单位 → 应关闭", () => {
    expect(shouldCloseUnitScopedPanel("bloodFeud", null)).toBe(true);
  });

  it("consent/bloodFeud 面板打开但仍有选中单位 → 不关闭", () => {
    expect(shouldCloseUnitScopedPanel("consent", "u1")).toBe(false);
    expect(shouldCloseUnitScopedPanel("bloodFeud", "u1")).toBe(false);
  });

  it("非单位作用域面板（fate/chronicle/charter/billing）即便无选中单位也不被此逻辑关闭", () => {
    expect(shouldCloseUnitScopedPanel("fate", null)).toBe(false);
    expect(shouldCloseUnitScopedPanel("chronicle", null)).toBe(false);
    expect(shouldCloseUnitScopedPanel("charter", null)).toBe(false);
    expect(shouldCloseUnitScopedPanel("billing", null)).toBe(false);
  });

  it("无面板打开（null）→ 不关闭（无副作用）", () => {
    expect(shouldCloseUnitScopedPanel(null, null)).toBe(false);
    expect(shouldCloseUnitScopedPanel(null, "u1")).toBe(false);
  });
});
