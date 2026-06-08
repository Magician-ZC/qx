/* 文件说明：FatePanel/DefianceCard 抗命溯源卡解析的聚焦测试（前缀 fatePanel.defiance 避免与他人测试撞名）。
   正式收编为 vitest 用例：直接对 DefianceCard.tsx 导出的纯函数（parseDefianceTrace/hasDefianceTrace/
   stripDefianceTrace）写 it/expect 断言。断言与后端 obedience.go 的 composeDefianceTrace 编码逐字对齐。
   注意：本文件被 tsconfig.json 的 exclude 排除，不参与 `npm run build` 的 tsc --noEmit——生产构建与 vitest
   是否安装彻底解耦；仅 `npm test`（vitest run）会编译并执行它。*/

import { describe, it, expect } from "vitest";
import {
  parseDefianceTrace,
  hasDefianceTrace,
  stripDefianceTrace,
  DEFIANCE_TRACE_MARKER,
} from "./DefianceCard";

// 与后端 obedience.go composeDefianceTrace 逐字一致的成长旁白。
const GROWTH_NARRATION = "她第一次没有照你说的做。她在变成她自己。";

// 后端实际编码：marker + " " + "source=人格|phrase=...|narration=..."。
function encodeTrace(source: string, phrase: string): string {
  return `${DEFIANCE_TRACE_MARKER} source=${source}|phrase=${phrase}|narration=${GROWTH_NARRATION}`;
}

describe("DefianceCard marker 常量", () => {
  it("与后端 obedience.go defianceTraceMarker 逐字一致（防整合期被悄悄改动）", () => {
    expect(DEFIANCE_TRACE_MARKER).toBe("「她为什么没听你」溯源卡 ::");
  });
});

describe("parseDefianceTrace", () => {
  it("标准编码行可被完整解析为 source/phrase/narration 三字段", () => {
    const parsed = parseDefianceTrace(encodeTrace("人格", "这不是她会做的选择。"));
    expect(parsed).not.toBeNull();
    expect(parsed?.source).toBe("人格");
    expect(parsed?.phrase).toBe("这不是她会做的选择。");
    expect(parsed?.narration).toBe(GROWTH_NARRATION);
  });

  it("marker 嵌在原拒绝理由之后（前置整行 + 换行）仍能切出溯源卡那一行", () => {
    const embedded = `她拒绝了这道命令，因为这越过了她的底线。\n${encodeTrace(
      "红线",
      "这越过了她不肯让步的底线。",
    )}`;
    const parsed = parseDefianceTrace(embedded);
    expect(parsed).not.toBeNull();
    expect(parsed?.source).toBe("红线");
    expect(parsed?.phrase).toBe("这越过了她不肯让步的底线。");
  });

  it("marker 之后还有别的行时只取 marker 那一行（narration 不被后续行污染）", () => {
    const trailing = `${encodeTrace("关系", "她放不下身边那个人。")}\n后面还有别的日志行，不应混入溯源卡。`;
    const parsed = parseDefianceTrace(trailing);
    expect(parsed?.narration).toBe(GROWTH_NARRATION);
  });

  it("不含 marker 的文本解析返回 null（活动流据此回退渲染原始文本）", () => {
    expect(parseDefianceTrace("毫无关系的一句日志")).toBeNull();
  });

  it("null/undefined 输入解析为 null", () => {
    expect(parseDefianceTrace(null)).toBeNull();
    expect(parseDefianceTrace(undefined)).toBeNull();
  });

  it("仅 marker 前缀、缺键值（被截断的活动流文本）应解析为 null（防空卡）", () => {
    expect(parseDefianceTrace(`${DEFIANCE_TRACE_MARKER}`)).toBeNull();
  });
});

describe("hasDefianceTrace", () => {
  it("含 marker 文本判 true", () => {
    expect(hasDefianceTrace(encodeTrace("压力", "她已经被现实压得喘不过气。"))).toBe(true);
  });

  it("普通文本判 false", () => {
    expect(hasDefianceTrace("她照常执行了命令，没有任何异样。")).toBe(false);
  });

  it("空文本 / null / undefined 判 false", () => {
    expect(hasDefianceTrace("")).toBe(false);
    expect(hasDefianceTrace(null)).toBe(false);
    expect(hasDefianceTrace(undefined)).toBe(false);
  });
});

describe("stripDefianceTrace", () => {
  it("去掉 marker 段只留 marker 之前的人类可读原始理由（杜绝机器编码串裸泄给玩家）", () => {
    const text = `她拒绝了这道命令，因为这越过了她的底线。\n${encodeTrace(
      "红线",
      "这越过了她不肯让步的底线。",
    )}`;
    expect(stripDefianceTrace(text)).toBe("她拒绝了这道命令，因为这越过了她的底线。");
  });

  it("无 marker 时原样返回；空输入返回空串", () => {
    expect(stripDefianceTrace("普通理由文本")).toBe("普通理由文本");
    expect(stripDefianceTrace("")).toBe("");
    expect(stripDefianceTrace(null)).toBe("");
    expect(stripDefianceTrace(undefined)).toBe("");
  });
});
