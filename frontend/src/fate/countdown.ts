/* 文件说明：命运待决策卡的「拿主意倒计时」计算工具（C-FATE）。
   后端 OpenFateFeed 已为 pending 卡返回 expires_at（精确绝对截止）与 countdown_hours（查询那一刻
   向下取整冻结的整小时快照）；WS fate_inbox 推送只给 occurred_at（甚至更少），不含倒计时。
   本工具把三种来源统一折算成「剩余时分」，**优先用绝对截止时刻**以保证逐秒走动：
     1) 优先 expires_at（ISO 绝对截止时刻）减去当前时刻——唯一能随实时递减的精确来源；
     2) 否则按 countdown_hours（feed 直给的整小时快照，仅 WS 兜底/无 expires 时用，查询时刻冻结）；
     3) 再否则按 occurred_at + 48h 兜底（无任何时间锚时返回 null）。
   注意：countdown_hours 是后端查询那一刻 int(remaining.Hours()) 的冻结快照，不随时间递减；
   只有 expires_at 减 now 才能逐秒走动，故必须 expires_at 优先。
   倒数随实时时间漂移，调用方用 setInterval 周期性重算即可（本函数纯计算、无副作用）。
   FateView / FatePanel 两处待决策渲染共用本工具，避免倒计时逻辑漂移。*/

import type { FateCard } from "../session/api";

// 默认兜底窗口：仅有 occurred_at 时，按发生时刻起 48 小时内须拿主意。
const DEFAULT_WINDOW_HOURS = 48;
// 紧迫阈值：剩余不足 6 小时标红催促。
const URGENT_THRESHOLD_HOURS = 6;

export type FateCountdown = {
  // 是否能算出倒计时（无任何时间锚时为 false，调用方据此决定是否渲染倒计时条）。
  available: boolean;
  // 是否已过期（剩余 ≤ 0）。过期态文案为「已自行决断」。
  expired: boolean;
  // 是否进入紧迫区间（剩余 < 6h 且未过期），用于标红。
  urgent: boolean;
  // 剩余整小时数（已对分钟向下取整后的小时部分，≥ 0）。
  hours: number;
  // 剩余分钟数（0-59，已扣除整小时部分，≥ 0）。
  minutes: number;
  // 剩余总秒数（≥ 0），供需要更细粒度的调用方使用。
  totalSeconds: number;
};

const EXPIRED: FateCountdown = {
  available: true,
  expired: true,
  urgent: false,
  hours: 0,
  minutes: 0,
  totalSeconds: 0,
};

const UNAVAILABLE: FateCountdown = {
  available: false,
  expired: false,
  urgent: false,
  hours: 0,
  minutes: 0,
  totalSeconds: 0,
};

// parseEpoch 把 ISO 字符串解析成毫秒时间戳；非法/空返回 NaN。
function parseEpoch(value: string | undefined): number {
  if (!value) return Number.NaN;
  const ms = Date.parse(value);
  return Number.isNaN(ms) ? Number.NaN : ms;
}

// computeFateCountdown 折算一张命运卡的剩余拿主意时间。
// nowMs 默认取当前时刻；测试或固定基准时可显式传入。
export function computeFateCountdown(card: FateCard, nowMs: number = Date.now()): FateCountdown {
  let remainingMs = Number.NaN;

  // 来源一：expires_at 绝对截止时刻——唯一能随实时逐秒递减的精确来源，故优先。
  const expiresAt = parseEpoch(card.expires_at);
  if (!Number.isNaN(expiresAt)) {
    remainingMs = expiresAt - nowMs;
  } else if (typeof card.countdown_hours === "number" && Number.isFinite(card.countdown_hours)) {
    // 来源二：countdown_hours（feed 直给的整小时快照，仅在无 expires_at 时兜底；查询时刻冻结、不递减）。
    remainingMs = card.countdown_hours * 3600 * 1000;
  } else {
    // 来源三：occurred_at + 48h 兜底。
    const occurredAt = parseEpoch(card.occurred_at);
    if (!Number.isNaN(occurredAt)) {
      remainingMs = occurredAt + DEFAULT_WINDOW_HOURS * 3600 * 1000 - nowMs;
    }
  }

  if (Number.isNaN(remainingMs)) {
    return UNAVAILABLE;
  }
  if (remainingMs <= 0) {
    return EXPIRED;
  }

  const totalSeconds = Math.floor(remainingMs / 1000);
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const urgent = remainingMs < URGENT_THRESHOLD_HOURS * 3600 * 1000;

  return { available: true, expired: false, urgent, hours, minutes, totalSeconds };
}

// formatFateCountdown 把倒计时折算成祖魂语气的展示文案。
export function formatFateCountdown(cd: FateCountdown): string {
  if (!cd.available) return "";
  if (cd.expired) return "已自行决断";
  if (cd.hours > 0) {
    return `还剩 ${cd.hours} 小时 ${cd.minutes} 分拿主意`;
  }
  if (cd.minutes > 0) {
    return `还剩 ${cd.minutes} 分拿主意`;
  }
  return "只剩片刻拿主意";
}
