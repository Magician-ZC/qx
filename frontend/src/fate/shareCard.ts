/* 文件说明：可分享卡的「图片化」工具——纯无依赖 <canvas> 手绘（设计 GDD 传播；当前盘点：
   分享仅文本 clipboard 复制，无图片卡，不构成「愿意截图传播」的视觉物）。
   renderShareCard 把一张死亡 / 高光 / 传记卡手绘成精美竖版 PNG（背景渐变 + 边框纹饰 + 标题 +
   正文自动换行 + 祖魂语气落款 + 水印），导出为 Blob。deathCard / highlightCard / biographyCard
   是三种风格预设（沿用 fate.css 的墨色宣纸 / 金描边气质）。

   零 npm 依赖：只用 document.createElement('canvas') + CanvasRenderingContext2D + toBlob。
   被 ShareCardButton 调用（依赖注入，不直接接 api.ts）。供 FateView / FatePanel / ChroniclePanel /
   名人堂的死亡 / 高光 / 传记分享处挂载（主控集成）。 */

// ShareCardKind 三种分享卡风格。
export type ShareCardKind = "death" | "highlight" | "biography";

// ShareCardOptions 渲染一张分享卡所需的全部数据（纯数据，组件经 props 注入）。
export type ShareCardOptions = {
  // title 主标题（角色名 / 事件名）。
  title: string;
  // body 正文叙事（自动换行）。
  body: string;
  // subtitle 副标题（血脉 / 回合 / 类型徽标文案），可空。
  subtitle?: string;
  // kind 卡片风格预设。
  kind: ShareCardKind;
  // accent 可选强调色（十六进制，如 "#b4543a"）；不传则按 kind 取预设主色。
  accent?: string;
  // footer 可选落款（祖魂语气的一句话）；不传则按 kind 取预设落款。
  footer?: string;
  // watermark 可选右下角水印；不传默认「一念 · 命运」。
  watermark?: string;
};

// CARD_WIDTH / CARD_HEIGHT 竖版卡逻辑尺寸（按 2x 像素密度导出，保证清晰）。
const CARD_WIDTH = 600;
const CARD_HEIGHT = 800;
const SCALE = 2;
const PADDING = 48;

// kindPreset 每种 kind 的视觉预设：背景双色渐变、主强调色、徽标文案、默认落款。
// 取自 fate.css 的墨色宣纸感与各 kind 既有强调色（death 紫、highlight 金、biography 暖金）。
type Preset = {
  bgTop: string;
  bgBottom: string;
  accent: string;
  ink: string;
  faintInk: string;
  badge: string;
  footer: string;
};

const kindPreset: Record<ShareCardKind, Preset> = {
  // 死亡卡：低饱和墨绿到深褐，紫调强调（与 ChroniclePanel「陨落」#8d6fb5 呼应），肃穆。
  death: {
    bgTop: "#211b26",
    bgBottom: "#12101a",
    accent: "#9d83c4",
    ink: "#ece6f2",
    faintInk: "#9a93ad",
    badge: "斯人已逝",
    footer: "她的故事，到这里。但有人会记着。",
  },
  // 高光卡：暖墨到深金，金调强调（与命运面板金描边一致），有宿命的高光感。
  highlight: {
    bgTop: "#2a2415",
    bgBottom: "#16130c",
    accent: "#e0c789",
    ink: "#f3ecd8",
    faintInk: "#b1a584",
    badge: "她经历的",
    footer: "你垂看着她，却不能替她活。",
  },
  // 传记卡：温润宣纸暗调到深褐，暖金强调（与编年史 #c9a227 呼应），厚重。
  biography: {
    bgTop: "#241f17",
    bgBottom: "#14110b",
    accent: "#c9a227",
    ink: "#f0ead8",
    faintInk: "#a89a73",
    badge: "她走过的路",
    footer: "一笔一笔，都记着。",
  },
};

// wrapText 把一段文本按可用宽度逐字断行（中文无空格，按字符逐个累加测量）。
// 返回行数组；保留原文中的显式换行符 \n。
function wrapText(ctx: CanvasRenderingContext2D, text: string, maxWidth: number): string[] {
  const lines: string[] = [];
  const paragraphs = text.split(/\n/);
  for (const para of paragraphs) {
    if (para.length === 0) {
      lines.push("");
      continue;
    }
    let current = "";
    for (const ch of para) {
      const candidate = current + ch;
      if (ctx.measureText(candidate).width > maxWidth && current.length > 0) {
        lines.push(current);
        current = ch;
      } else {
        current = candidate;
      }
    }
    if (current.length > 0) {
      lines.push(current);
    }
  }
  return lines;
}

// roundRectPath 在 ctx 上勾出一条圆角矩形路径（不依赖 ctx.roundRect，兼容旧实现）。
function roundRectPath(
  ctx: CanvasRenderingContext2D,
  x: number,
  y: number,
  w: number,
  h: number,
  r: number,
): void {
  const radius = Math.min(r, w / 2, h / 2);
  ctx.beginPath();
  ctx.moveTo(x + radius, y);
  ctx.arcTo(x + w, y, x + w, y + h, radius);
  ctx.arcTo(x + w, y + h, x, y + h, radius);
  ctx.arcTo(x, y + h, x, y, radius);
  ctx.arcTo(x, y, x + w, y, radius);
  ctx.closePath();
}

// drawCard 把整张卡画进给定 2D 上下文（坐标系已按 SCALE 缩放，逻辑坐标作画）。
function drawCard(ctx: CanvasRenderingContext2D, opts: ShareCardOptions): void {
  const preset = kindPreset[opts.kind];
  const accent = opts.accent ?? preset.accent;
  const cjkFont = '"Noto Serif SC", "Songti SC", "STSong", serif';

  // 背景：竖向双色渐变。
  const grad = ctx.createLinearGradient(0, 0, 0, CARD_HEIGHT);
  grad.addColorStop(0, preset.bgTop);
  grad.addColorStop(1, preset.bgBottom);
  ctx.fillStyle = grad;
  ctx.fillRect(0, 0, CARD_WIDTH, CARD_HEIGHT);

  // 背景纹饰：极淡的强调色对角光晕，增强质感、不抢正文。
  const glow = ctx.createRadialGradient(
    CARD_WIDTH * 0.5,
    CARD_HEIGHT * 0.18,
    20,
    CARD_WIDTH * 0.5,
    CARD_HEIGHT * 0.18,
    CARD_WIDTH * 0.8,
  );
  glow.addColorStop(0, hexWithAlpha(accent, 0.16));
  glow.addColorStop(1, hexWithAlpha(accent, 0));
  ctx.fillStyle = glow;
  ctx.fillRect(0, 0, CARD_WIDTH, CARD_HEIGHT);

  // 外描边纹饰：双层圆角框（外细内更细），金/紫描边气质。
  ctx.lineWidth = 2;
  ctx.strokeStyle = hexWithAlpha(accent, 0.55);
  roundRectPath(ctx, 18, 18, CARD_WIDTH - 36, CARD_HEIGHT - 36, 18);
  ctx.stroke();

  ctx.lineWidth = 1;
  ctx.strokeStyle = hexWithAlpha(accent, 0.25);
  roundRectPath(ctx, 28, 28, CARD_WIDTH - 56, CARD_HEIGHT - 56, 14);
  ctx.stroke();

  // 顶部徽标（小圆角标签）：kind 文案。
  ctx.font = `600 16px ${cjkFont}`;
  const badgeText = preset.badge;
  const badgeMetrics = ctx.measureText(badgeText);
  const badgeW = badgeMetrics.width + 28;
  const badgeH = 30;
  const badgeX = PADDING;
  const badgeY = PADDING + 4;
  ctx.fillStyle = hexWithAlpha(accent, 0.16);
  roundRectPath(ctx, badgeX, badgeY, badgeW, badgeH, 15);
  ctx.fill();
  ctx.strokeStyle = hexWithAlpha(accent, 0.5);
  ctx.lineWidth = 1;
  roundRectPath(ctx, badgeX, badgeY, badgeW, badgeH, 15);
  ctx.stroke();
  ctx.fillStyle = accent;
  ctx.textBaseline = "middle";
  ctx.textAlign = "left";
  ctx.fillText(badgeText, badgeX + 14, badgeY + badgeH / 2 + 1);

  // 主标题（角色名 / 事件名）：大字号，必要时截断。
  ctx.textBaseline = "alphabetic";
  ctx.fillStyle = preset.ink;
  ctx.font = `700 44px ${cjkFont}`;
  const titleMaxWidth = CARD_WIDTH - PADDING * 2;
  const titleLines = wrapText(ctx, opts.title.trim() || "无名", titleMaxWidth).slice(0, 2);
  let cursorY = badgeY + badgeH + 56;
  for (const line of titleLines) {
    ctx.fillText(line, PADDING, cursorY);
    cursorY += 52;
  }

  // 副标题（血脉 / 回合 / 类型）：淡墨小字。
  if (opts.subtitle && opts.subtitle.trim()) {
    ctx.fillStyle = preset.faintInk;
    ctx.font = `400 18px ${cjkFont}`;
    const subLines = wrapText(ctx, opts.subtitle.trim(), titleMaxWidth).slice(0, 2);
    for (const line of subLines) {
      ctx.fillText(line, PADDING, cursorY);
      cursorY += 26;
    }
  }

  // 标题与正文之间的分隔金线。
  cursorY += 18;
  ctx.strokeStyle = hexWithAlpha(accent, 0.4);
  ctx.lineWidth = 1.5;
  ctx.beginPath();
  ctx.moveTo(PADDING, cursorY);
  ctx.lineTo(PADDING + 64, cursorY);
  ctx.stroke();
  cursorY += 34;

  // 正文：自动换行，行高宽松，留出底部落款空间，溢出以省略号收束。
  ctx.fillStyle = preset.ink;
  ctx.font = `400 22px ${cjkFont}`;
  const bodyMaxWidth = CARD_WIDTH - PADDING * 2;
  const lineHeight = 36;
  const footerReserve = 110;
  const maxBodyBottom = CARD_HEIGHT - footerReserve;
  const bodyLines = wrapText(ctx, opts.body.trim() || "（无）", bodyMaxWidth);
  const maxLines = Math.max(1, Math.floor((maxBodyBottom - cursorY) / lineHeight));
  const shown = bodyLines.slice(0, maxLines);
  if (bodyLines.length > maxLines && shown.length > 0) {
    const last = shown[shown.length - 1];
    shown[shown.length - 1] = truncateToWidth(ctx, last + "……", bodyMaxWidth);
  }
  for (const line of shown) {
    ctx.fillText(line, PADDING, cursorY);
    cursorY += lineHeight;
  }

  // 底部落款（祖魂语气）：靠左斜体淡墨。
  const footerText = opts.footer ?? preset.footer;
  ctx.fillStyle = preset.faintInk;
  ctx.font = `italic 400 18px ${cjkFont}`;
  ctx.fillText(`— ${footerText}`, PADDING, CARD_HEIGHT - 64);

  // 右下角水印。
  const watermark = opts.watermark ?? "一念 · 命运";
  ctx.fillStyle = hexWithAlpha(accent, 0.7);
  ctx.font = `600 16px ${cjkFont}`;
  ctx.textAlign = "right";
  ctx.fillText(watermark, CARD_WIDTH - PADDING, CARD_HEIGHT - 40);
  ctx.textAlign = "left";
}

// truncateToWidth 把一行尾部裁到不超过 maxWidth（用于最后一行加省略号后再保险收束）。
function truncateToWidth(ctx: CanvasRenderingContext2D, text: string, maxWidth: number): string {
  if (ctx.measureText(text).width <= maxWidth) return text;
  let s = text;
  while (s.length > 1 && ctx.measureText(s).width > maxWidth) {
    s = s.slice(0, -2) + "…";
  }
  return s;
}

// hexWithAlpha 把 "#rrggbb" / "#rgb" 叠加 alpha（0~1），返回 rgba()。非法输入回退强调金。
function hexWithAlpha(hex: string, alpha: number): string {
  let h = hex.trim().replace(/^#/, "");
  if (h.length === 3) {
    h = h
      .split("")
      .map((c) => c + c)
      .join("");
  }
  if (h.length !== 6 || /[^0-9a-fA-F]/.test(h)) {
    return `rgba(217, 188, 115, ${clampAlpha(alpha)})`;
  }
  const r = parseInt(h.slice(0, 2), 16);
  const g = parseInt(h.slice(2, 4), 16);
  const b = parseInt(h.slice(4, 6), 16);
  return `rgba(${r}, ${g}, ${b}, ${clampAlpha(alpha)})`;
}

function clampAlpha(a: number): number {
  if (!Number.isFinite(a)) return 1;
  return Math.max(0, Math.min(1, a));
}

// createCardCanvas 新建一张离屏画布并把卡画好，返回 canvas（供导出 Blob / DataURL）。
function createCardCanvas(opts: ShareCardOptions): HTMLCanvasElement {
  const canvas = document.createElement("canvas");
  canvas.width = CARD_WIDTH * SCALE;
  canvas.height = CARD_HEIGHT * SCALE;
  const ctx = canvas.getContext("2d");
  if (!ctx) {
    throw new Error("当前环境不支持 canvas 2D，无法生成分享图。");
  }
  ctx.scale(SCALE, SCALE);
  drawCard(ctx, opts);
  return canvas;
}

// renderShareCard 渲染一张分享卡并导出为 PNG Blob（核心入口）。
// 失败（无 canvas / toBlob 不可用）抛错，由调用方兜底回退。
export function renderShareCard(opts: ShareCardOptions): Promise<Blob> {
  const canvas = createCardCanvas(opts);
  return new Promise<Blob>((resolve, reject) => {
    try {
      canvas.toBlob((blob) => {
        if (blob) {
          resolve(blob);
        } else {
          reject(new Error("生成分享图失败（toBlob 返回空）。"));
        }
      }, "image/png");
    } catch (err) {
      reject(err instanceof Error ? err : new Error(String(err)));
    }
  });
}

// renderShareCardDataURL 同步导出 DataURL（用于预览缩略图，无需 await Blob）。
export function renderShareCardDataURL(opts: ShareCardOptions): string {
  const canvas = createCardCanvas(opts);
  return canvas.toDataURL("image/png");
}

// ── 三种风格预设的便捷构造器（调用方只给最小字段，其余按 kind 兜底）──

// deathCard 死亡卡预设：标题=角色名，正文=临终叙事 / 遗言，副标题=血脉 / 死于第几回合。
export function deathCard(input: {
  name: string;
  epitaph: string;
  lineage?: string;
  accent?: string;
}): ShareCardOptions {
  return {
    kind: "death",
    title: input.name,
    body: input.epitaph,
    subtitle: input.lineage,
    accent: input.accent,
  };
}

// highlightCard 高光卡预设：标题=角色名 / 高光题，正文=高光叙事，副标题可空。
export function highlightCard(input: {
  title: string;
  narrative: string;
  subtitle?: string;
  accent?: string;
}): ShareCardOptions {
  return {
    kind: "highlight",
    title: input.title,
    body: input.narrative,
    subtitle: input.subtitle,
    accent: input.accent,
  };
}

// biographyCard 传记卡预设：标题=角色名，正文=传记 / 编年史摘录，副标题=血脉 / 跨越回合。
export function biographyCard(input: {
  name: string;
  biography: string;
  subtitle?: string;
  accent?: string;
}): ShareCardOptions {
  return {
    kind: "biography",
    title: input.name,
    body: input.biography,
    subtitle: input.subtitle,
    accent: input.accent,
  };
}
