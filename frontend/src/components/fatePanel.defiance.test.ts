/* 文件说明：FatePanel/DefianceCard 抗命溯源卡解析的聚焦测试（前缀 fatePanel.defiance 避免与他人测试撞名）。
   本仓库前端尚未接入 vitest/jest，且 tsc --noEmit 会编译 src 下全部文件、且未安装 @types/node——故本测试刻意
   「零外部依赖、不引用 process」：只用纯 TS 断言（抛错即失败），既能通过严格类型检查，又可在日后接入 vitest/tsx
   时直接 import 调用 runDefianceCardTests()。断言聚焦 DefianceCard 的 marker 解析这段「载荷逻辑」，与后端
   obedience.go 的编码逐字对齐。*/

import { parseDefianceTrace, hasDefianceTrace, DEFIANCE_TRACE_MARKER } from "./DefianceCard";

function assert(condition: boolean, message: string): void {
  if (!condition) {
    throw new Error(`断言失败：${message}`);
  }
}

function assertEqual<T>(actual: T, expected: T, message: string): void {
  if (actual !== expected) {
    throw new Error(`断言失败：${message}（期望 ${String(expected)}，实得 ${String(actual)}）`);
  }
}

// 与后端 obedience.go composeDefianceTrace 逐字一致的成长旁白与一行编码样例。
const GROWTH_NARRATION = "她第一次没有照你说的做。她在变成她自己。";

// 后端实际编码：marker + " " + "source=人格|phrase=...|narration=..."。
function encodeTrace(source: string, phrase: string): string {
  return `${DEFIANCE_TRACE_MARKER} source=${source}|phrase=${phrase}|narration=${GROWTH_NARRATION}`;
}

export function runDefianceCardTests(): void {
  // 1) marker 常量与后端约定逐字一致（防止整合期被悄悄改动）。
  assertEqual(DEFIANCE_TRACE_MARKER, "「她为什么没听你」溯源卡 ::", "marker 常量必须与后端逐字一致");

  // 2) 纯 marker 行可被完整解析为三字段。
  const parsed = parseDefianceTrace(encodeTrace("人格", "这不是她会做的选择。"));
  assert(parsed !== null, "标准编码行应解析成功");
  assertEqual(parsed?.source ?? "", "人格", "source 应解析为「人格」");
  assertEqual(parsed?.phrase ?? "", "这不是她会做的选择。", "phrase 应解析正确");
  assertEqual(parsed?.narration ?? "", GROWTH_NARRATION, "narration 应解析为钦定成长旁白原文");

  // 3) marker 嵌在原拒绝理由之后（前面有别的整行 + 换行）仍能切出溯源卡那一行。
  const embedded = `她拒绝了这道命令，因为这越过了她的底线。\n${encodeTrace("红线", "这越过了她不肯让步的底线。")}`;
  const parsedEmbedded = parseDefianceTrace(embedded);
  assert(parsedEmbedded !== null, "前置整行 + 换行后的 marker 行应仍可解析");
  assertEqual(parsedEmbedded?.source ?? "", "红线", "嵌入场景 source 应为「红线」");
  assertEqual(parsedEmbedded?.phrase ?? "", "这越过了她不肯让步的底线。", "嵌入场景 phrase 应正确");

  // 4) marker 之后还有别的行时，只取 marker 那一行（narration 不被后续行污染）。
  const trailing = `${encodeTrace("关系", "她放不下身边那个人。")}\n后面还有别的日志行，不应混入溯源卡。`;
  const parsedTrailing = parseDefianceTrace(trailing);
  assertEqual(parsedTrailing?.narration ?? "", GROWTH_NARRATION, "marker 行后的多余行不应污染 narration");

  // 5) hasDefianceTrace 对含/不含 marker 的文本判定正确。
  assert(hasDefianceTrace(encodeTrace("压力", "她已经被现实压得喘不过气。")), "含 marker 文本应判 true");
  assert(!hasDefianceTrace("她照常执行了命令，没有任何异样。"), "普通文本应判 false");
  assert(!hasDefianceTrace(""), "空文本应判 false");
  assert(!hasDefianceTrace(null), "null 应判 false");
  assert(!hasDefianceTrace(undefined), "undefined 应判 false");

  // 6) 不含 marker 的文本解析返回 null（活动流据此回退渲染原始文本，不会显示空卡）。
  assertEqual(parseDefianceTrace("毫无关系的一句日志"), null, "无 marker 文本应解析为 null");
  assertEqual(parseDefianceTrace(null), null, "null 应解析为 null");

  // 7) 只有 marker 前缀、缺键值（被 24 字截断的活动流文本）应解析为 null —— 避免渲染空白溯源卡。
  assertEqual(parseDefianceTrace(`${DEFIANCE_TRACE_MARKER}`), null, "仅 marker 无键值应解析为 null（防空卡）");

  // eslint-disable-next-line no-console
  console.log("runDefianceCardTests: 全部断言通过");
}

export default runDefianceCardTests;
