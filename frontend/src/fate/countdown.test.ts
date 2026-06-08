/* 文件说明：命运待决策倒计时 computeFateCountdown 的聚焦回归测试（C-FATE）。
   锁定「expires_at 优先于 countdown_hours」的来源优先级——这是修复『倒计时被 countdown_hours 冻结、
   不随实时递减』这一 load-bearing 功能破坏的关键：只有 expires_at 减 now 才能逐秒走动，
   而 countdown_hours 是后端查询那一刻 int(remaining.Hours()) 的整小时冻结快照。
   注意：本文件被 tsconfig.json 的 exclude 排除，不参与 tsc --noEmit；仅 `npm test`（vitest run）执行。*/

import { describe, it, expect } from "vitest";
import { computeFateCountdown, formatFateCountdown } from "./countdown";
import type { FateCard } from "../session/api";

// 构造一张 pending 命运卡的最小骨架。
function card(partial: Partial<FateCard>): FateCard {
  return {
    kind: "pending",
    narrative: "测试卡",
    ...partial,
  } as FateCard;
}

// 固定基准时刻，便于断言绝对截止时刻的相对剩余。
const NOW = Date.parse("2026-06-08T12:00:00.000Z");

describe("computeFateCountdown 来源优先级（expires_at > countdown_hours > occurred_at+48h）", () => {
  it("有 expires_at 时按 expires_at 减 now 计算——随实时递减，不被 countdown_hours 冻结", () => {
    // 后端 feed 同时给 expires_at（精确）与 countdown_hours（整小时冻结快照）。
    // 此处 expires_at 距 now 2.5h；若错误地走 countdown_hours=10 会得 10h。
    const expiresAt = new Date(NOW + 2.5 * 3600 * 1000).toISOString();
    const cd = computeFateCountdown(card({ expires_at: expiresAt, countdown_hours: 10 }), NOW);
    expect(cd.available).toBe(true);
    expect(cd.expired).toBe(false);
    expect(cd.hours).toBe(2);
    expect(cd.minutes).toBe(30);
  });

  it("expires_at 分支随 now 推进而逐秒递减（同一张卡、不同 now 得到不同剩余）", () => {
    const expiresAt = new Date(NOW + 3600 * 1000).toISOString(); // 距基准 1h
    const c = card({ expires_at: expiresAt, countdown_hours: 1 });
    const early = computeFateCountdown(c, NOW); // 剩 3600s
    const later = computeFateCountdown(c, NOW + 1000); // 1 秒后，剩 3599s
    expect(early.totalSeconds).toBe(3600);
    expect(later.totalSeconds).toBe(3599);
    // 关键：结果与 now 相关，不再是冻结值。
    expect(later.totalSeconds).toBeLessThan(early.totalSeconds);
  });

  it("expires_at 带分钟精度时分钟非零（修复前 countdown_hours*3600 必致 minutes 恒为 0）", () => {
    const expiresAt = new Date(NOW + (2 * 3600 + 17 * 60) * 1000).toISOString(); // 2h17m
    const cd = computeFateCountdown(card({ expires_at: expiresAt, countdown_hours: 2 }), NOW);
    expect(cd.hours).toBe(2);
    expect(cd.minutes).toBe(17);
    expect(formatFateCountdown(cd)).toBe("还剩 2 小时 17 分拿主意");
  });

  it("无 expires_at 时退回 countdown_hours（WS 兜底场景，整小时快照）", () => {
    const cd = computeFateCountdown(card({ countdown_hours: 5 }), NOW);
    expect(cd.available).toBe(true);
    expect(cd.hours).toBe(5);
    expect(cd.minutes).toBe(0);
  });

  it("既无 expires_at 也无 countdown_hours 时按 occurred_at + 48h 兜底", () => {
    const occurredAt = new Date(NOW - 10 * 3600 * 1000).toISOString(); // 10h 前发生
    const cd = computeFateCountdown(card({ occurred_at: occurredAt }), NOW);
    // 48h 窗口已过 10h，应剩 38h。
    expect(cd.available).toBe(true);
    expect(cd.hours).toBe(38);
  });

  it("expires_at 已过期返回 expired", () => {
    const expiresAt = new Date(NOW - 1000).toISOString();
    const cd = computeFateCountdown(card({ expires_at: expiresAt, countdown_hours: 10 }), NOW);
    expect(cd.expired).toBe(true);
    expect(formatFateCountdown(cd)).toBe("已自行决断");
  });

  it("无任何时间锚返回 unavailable", () => {
    const cd = computeFateCountdown(card({}), NOW);
    expect(cd.available).toBe(false);
  });

  it("剩余不足 6h 时标红 urgent（按 expires_at 实时判定）", () => {
    const expiresAt = new Date(NOW + 5 * 3600 * 1000).toISOString();
    const cd = computeFateCountdown(card({ expires_at: expiresAt }), NOW);
    expect(cd.urgent).toBe(true);
    const far = new Date(NOW + 12 * 3600 * 1000).toISOString();
    expect(computeFateCountdown(card({ expires_at: far }), NOW).urgent).toBe(false);
  });
});
