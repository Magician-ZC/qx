/* 文件说明：新手首屏「第一分钟」引导覆盖层（设计 GDD 第一分钟体验 + 盘点缺口
   「新手首屏第一分钟缺显式引导，是功能入口堆叠而非被设计过的首次体验脚本」）。
   一个轻量分步引导浮层，首次进入（localStorage 标记 qx_onboarded 缺失）时显示，
   用 6 步把核心心智模型讲清楚——「你是祖魂不是指挥官」「她有自己的命、会抗命」
   「世界事件会变成她的命运」「你影响她而非遥控她」「命运四槽怎么看」「待决策怎么处理」。
   每步可高亮一处 UI 区域（用 props.steps[].anchor 选择器；命中则 canvas 镂空聚光、
   卡片贴近锚点，否则纯居中卡）。下一步 / 跳过 / 完成；完成或跳过写 localStorage，不再打扰。
   祖魂语气、与 fate.css 暖色宣纸调一致；自包含内联样式，不依赖外部 CSS 加载。
   纯前端、依赖注入：onComplete 与锚点选择器经 props 传入，不直接 import api.ts。 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { zIndex } from "../zindex-tokens";

// 默认 localStorage 键名；可被 props.storageKey 覆盖（便于测试或多套引导并存）。
const DEFAULT_STORAGE_KEY = "qx_onboarded";

// TourStep 是单步引导的内容。anchor 缺省或选择器未命中时退化为纯居中卡。
export type TourStep = {
  // title 短标题（如「你是祖魂，不是指挥官」）。
  title: string;
  // body 一两句正文，祖魂语气，把心智模型讲透。
  body: string;
  // anchor 可选 CSS 选择器，指向首屏要高亮的 UI 区域（document.querySelector）。
  anchor?: string;
  // 锚点存在但当前不在视图时的兜底文案（可选）；缺省退化为纯居中卡，不报错。
};

type Props = {
  // 引导完成或跳过时回调（主控据此可埋点 / 关闭浮层）。参数标记是「完成」还是「跳过」。
  onComplete: (reason: "finished" | "skipped") => void;
  // steps 自定义引导步骤；缺省用内置 6 步核心心智模型脚本。
  steps?: TourStep[];
  // storageKey 自定义持久化键；缺省 qx_onboarded。
  storageKey?: string;
  // forceShow 忽略 localStorage 强制显示（用于「重看引导」入口或预览）。
  forceShow?: boolean;
};

// 内置 6 步：第一分钟要建立的核心心智模型，逐条对应盘点缺口与设计宪法。
// 措辞严守祖魂语气——玩家是垂看后人的先祖，不出现「命令 / 操控 / 控制 / 遥控」字眼。
const DEFAULT_STEPS: TourStep[] = [
  {
    title: "你是祖魂，不是指挥官",
    body: "这一脉的后人在世间行走，你是垂看他们的先祖之魂。你不下达军令，你以记挂与期许，悄悄影响她走的路。",
  },
  {
    title: "她有自己的命，也会抗命",
    body: "她有性子、有记忆、有红线。你递去的话，她未必照做——当她违逆你时，那不是出错，是她在长成她自己。",
  },
  {
    title: "世界之事，会成为她的命运",
    body: "别处的征战、远人的生死、一次相遇，都可能顺着关系的丝线惊动到她。世界在动，她的命也随之流转。",
    anchor: "[data-tour='fate']",
  },
  {
    title: "你影响她，而非遥控她",
    body: "你能托一个梦、递一句叮咛、为她祈愿。话说出去了，听不听、怎么听，仍是她的事。影响是水磨工夫，不是按钮。",
    anchor: "[data-tour='intervene']",
  },
  {
    title: "命运四槽，这样看她",
    body: "「她现在怎样」是近况，「一瞥她经历的事」是高光，「等你拿主意」是待你决断的岔路，「因为你上次……」是你影响的回响。四槽合起来，就是她的一生。",
    anchor: "[data-tour='fate']",
  },
  {
    title: "待决策，慢慢拿主意",
    body: "遇到岔路，她会留一格「等你拿主意」：你可以放手让她自己选、轻轻劝一句、或只是默默知晓。你不点，她也会在时限后自行决断——命不等人。",
    anchor: "[data-tour='fate']",
  },
];

// readOnboarded 安全读取「是否已引导过」；localStorage 不可用（隐私模式等）时当作未引导，best-effort。
function readOnboarded(key: string): boolean {
  try {
    return (window.localStorage.getItem(key) ?? "") !== "";
  } catch {
    return false;
  }
}

// writeOnboarded 安全写入引导完成标记；失败静默（下次仍会弹一次，至多重复一回，不影响 UX）。
function writeOnboarded(key: string): void {
  try {
    window.localStorage.setItem(key, String(Date.now()));
  } catch {
    // 忽略持久化失败。
  }
}

// AnchorRect 是高亮锚点在视口中的矩形（含一点外扩 padding），用于 canvas 镂空与卡片定位。
type AnchorRect = {
  x: number;
  y: number;
  width: number;
  height: number;
};

// 高亮框相对锚点向外撑开的留白，让聚光圈不贴边、更柔和。
const HIGHLIGHT_PADDING = 8;

// measureAnchor 解析某步的锚点选择器，返回其视口矩形；选择器缺省 / 未命中 / 元素不可见时返回 null（退化为居中卡）。
function measureAnchor(selector?: string): AnchorRect | null {
  if (!selector) return null;
  try {
    const el = document.querySelector(selector);
    if (!el) return null;
    const rect = el.getBoundingClientRect();
    // 零尺寸（display:none 或未挂载）视为未命中。
    if (rect.width <= 0 || rect.height <= 0) return null;
    return {
      x: Math.max(0, rect.left - HIGHLIGHT_PADDING),
      y: Math.max(0, rect.top - HIGHLIGHT_PADDING),
      width: rect.width + HIGHLIGHT_PADDING * 2,
      height: rect.height + HIGHLIGHT_PADDING * 2,
    };
  } catch {
    // 非法选择器等：当作未命中。
    return null;
  }
}

// ---- 暖色宣纸调（与 fate.css 同源），自包含内联样式，杜绝外部 CSS 依赖 ----
const overlayStyle: React.CSSProperties = {
  position: "fixed",
  inset: 0,
  zIndex: zIndex.tour,
  display: "flex",
  alignItems: "center",
  justifyContent: "center",
  fontFamily: '"Noto Serif SC", "Songti SC", serif',
};

const backdropCanvasStyle: React.CSSProperties = {
  position: "absolute",
  inset: 0,
  width: "100%",
  height: "100%",
  // 聚光圈外是半透墨色幕布，圈内镂空透出被高亮的 UI；点击穿透由覆盖层统一拦截。
  pointerEvents: "none",
};

const cardBaseStyle: React.CSSProperties = {
  position: "absolute",
  width: "min(440px, calc(100vw - 32px))",
  boxSizing: "border-box",
  background: "rgba(255, 250, 240, 0.97)",
  border: "1px solid rgba(120, 90, 50, 0.3)",
  borderRadius: 16,
  padding: "24px 26px 20px",
  boxShadow: "0 12px 40px rgba(60, 44, 22, 0.35)",
  color: "#3a2c1b",
};

const kickerStyle: React.CSSProperties = {
  fontSize: 11,
  letterSpacing: "0.28em",
  color: "#97825f",
  marginBottom: 8,
};

const titleStyle: React.CSSProperties = {
  fontSize: 21,
  letterSpacing: "0.08em",
  color: "#6b4a22",
  margin: "0 0 10px",
};

const bodyStyle: React.CSSProperties = {
  fontSize: 14,
  lineHeight: 1.85,
  color: "#5a4628",
  margin: "0 0 18px",
};

const dotsRowStyle: React.CSSProperties = {
  display: "flex",
  gap: 6,
  alignItems: "center",
  marginBottom: 16,
};

const footerStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  justifyContent: "space-between",
  gap: 12,
};

const skipBtnStyle: React.CSSProperties = {
  background: "transparent",
  border: "none",
  color: "#97825f",
  fontFamily: "inherit",
  fontSize: 13,
  cursor: "pointer",
  padding: "8px 4px",
};

const navGroupStyle: React.CSSProperties = {
  display: "flex",
  gap: 8,
};

const backBtnStyle: React.CSSProperties = {
  padding: "9px 16px",
  border: "1px solid rgba(140, 95, 45, 0.45)",
  borderRadius: 8,
  background: "rgba(255, 252, 245, 0.9)",
  color: "#5a3f1c",
  fontFamily: "inherit",
  fontSize: 14,
  cursor: "pointer",
};

const nextBtnStyle: React.CSSProperties = {
  padding: "9px 22px",
  border: "none",
  borderRadius: 8,
  background: "#7a5226",
  color: "#fdf6e8",
  fontFamily: "inherit",
  fontSize: 14,
  letterSpacing: "0.08em",
  cursor: "pointer",
};

// OnboardingTour 渲染新手第一分钟引导覆盖层。已引导过（localStorage 命中）且未 forceShow 时不渲染任何内容。
export function OnboardingTour({ onComplete, steps, storageKey, forceShow }: Props) {
  const key = storageKey ?? DEFAULT_STORAGE_KEY;
  const tourSteps = useMemo(() => (steps && steps.length > 0 ? steps : DEFAULT_STEPS), [steps]);

  // 初始可见性：forceShow 强制显示；否则首次进入（未引导过）才显示。只在挂载时判定一次。
  const [visible, setVisible] = useState<boolean>(() => Boolean(forceShow) || !readOnboarded(key));
  const [index, setIndex] = useState(0);
  // 当前步锚点矩形（命中则聚光 + 贴近定位）；随步骤 / 窗口尺寸变化重测。
  const [anchorRect, setAnchorRect] = useState<AnchorRect | null>(null);
  // 视口尺寸，用于 canvas 像素与卡片定位计算。
  const [viewport, setViewport] = useState<{ w: number; h: number }>(() => ({
    w: typeof window === "undefined" ? 0 : window.innerWidth,
    h: typeof window === "undefined" ? 0 : window.innerHeight,
  }));

  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const total = tourSteps.length;
  const step = tourSteps[Math.min(index, total - 1)];
  const isLast = index >= total - 1;

  // 关闭引导：写持久化标记并回调主控（finished / skipped 二态）。
  const finish = useCallback(
    (reason: "finished" | "skipped") => {
      writeOnboarded(key);
      setVisible(false);
      onComplete(reason);
    },
    [key, onComplete],
  );

  const goNext = useCallback(() => {
    if (isLast) {
      finish("finished");
      return;
    }
    setIndex((i) => Math.min(i + 1, total - 1));
  }, [finish, isLast, total]);

  const goBack = useCallback(() => {
    setIndex((i) => Math.max(0, i - 1));
  }, []);

  // 测量当前步锚点 + 跟随窗口 resize / 滚动重测（锚点在主界面会随布局变化移动）。
  useEffect(() => {
    if (!visible) return;
    const sync = () => {
      setViewport({ w: window.innerWidth, h: window.innerHeight });
      setAnchorRect(measureAnchor(step?.anchor));
    };
    sync();
    window.addEventListener("resize", sync);
    window.addEventListener("scroll", sync, true);
    return () => {
      window.removeEventListener("resize", sync);
      window.removeEventListener("scroll", sync, true);
    };
  }, [visible, step?.anchor]);

  // 键盘可达性：Esc 跳过、→/Enter 下一步、← 上一步。
  useEffect(() => {
    if (!visible) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        finish("skipped");
      } else if (e.key === "ArrowRight" || e.key === "Enter") {
        e.preventDefault();
        goNext();
      } else if (e.key === "ArrowLeft") {
        e.preventDefault();
        goBack();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [visible, finish, goNext, goBack]);

  // 用无依赖 canvas 画「半透墨幕 + 镂空聚光圈」：命中锚点时在其矩形处镂空（圆角），否则整屏蒙一层。
  useEffect(() => {
    if (!visible) return;
    const canvas = canvasRef.current;
    if (!canvas) return;
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    const w = viewport.w;
    const h = viewport.h;
    canvas.width = Math.max(1, Math.floor(w * dpr));
    canvas.height = Math.max(1, Math.floor(h * dpr));
    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);
    // 暖墨色半透幕布。
    ctx.fillStyle = "rgba(40, 28, 14, 0.62)";
    ctx.fillRect(0, 0, w, h);
    if (anchorRect) {
      // 在锚点处镂空一个圆角矩形（destination-out 擦除幕布，露出底下被高亮的 UI）。
      ctx.save();
      ctx.globalCompositeOperation = "destination-out";
      roundedRectPath(ctx, anchorRect.x, anchorRect.y, anchorRect.width, anchorRect.height, 12);
      ctx.fill();
      ctx.restore();
      // 在镂空边缘描一圈暖金高光，强调被讲解的区域。
      ctx.save();
      ctx.globalCompositeOperation = "source-over";
      ctx.strokeStyle = "rgba(217, 188, 115, 0.85)";
      ctx.lineWidth = 2;
      roundedRectPath(ctx, anchorRect.x, anchorRect.y, anchorRect.width, anchorRect.height, 12);
      ctx.stroke();
      ctx.restore();
    }
  }, [visible, anchorRect, viewport.w, viewport.h]);

  // 计算卡片位置：有锚点时贴在锚点上 / 下方（取空间更大的一侧），无锚点时居中。
  const cardPosition = useMemo<React.CSSProperties>(() => {
    const cardWidth = Math.min(440, viewport.w - 32);
    if (!anchorRect) {
      // 居中：用 transform 让卡片以自身中心对齐视口中心。
      return {
        left: "50%",
        top: "50%",
        transform: "translate(-50%, -50%)",
      };
    }
    // 水平：尽量与锚点中心对齐，但夹在视口内（留 16px 边距）。
    const anchorCenterX = anchorRect.x + anchorRect.width / 2;
    let left = anchorCenterX - cardWidth / 2;
    left = Math.max(16, Math.min(left, viewport.w - cardWidth - 16));
    // 竖直：锚点下方空间够就放下方，否则放上方；估一个保守卡高用于翻转判断。
    const estCardHeight = 230;
    const spaceBelow = viewport.h - (anchorRect.y + anchorRect.height);
    let top: number;
    if (spaceBelow >= estCardHeight + 16) {
      top = anchorRect.y + anchorRect.height + 14;
    } else {
      top = Math.max(16, anchorRect.y - estCardHeight - 14);
    }
    return { left, top };
  }, [anchorRect, viewport.w, viewport.h]);

  if (!visible || !step) {
    return null;
  }

  return (
    <div
      style={overlayStyle}
      role="dialog"
      aria-modal="true"
      aria-label="新手引导"
      // 点击幕布空白处不误关（避免新手手滑跳过整段引导）；交互只走卡片内按钮。
      onClick={(e) => {
        if (e.target === e.currentTarget) {
          e.stopPropagation();
        }
      }}
    >
      <canvas ref={canvasRef} style={backdropCanvasStyle} aria-hidden="true" />
      <div style={{ ...cardBaseStyle, ...cardPosition }}>
        <div style={kickerStyle}>祖魂的低语 · 第 {index + 1} / {total} 言</div>
        <h2 style={titleStyle}>{step.title}</h2>
        <p style={bodyStyle}>{step.body}</p>
        <div style={dotsRowStyle} aria-hidden="true">
          {tourSteps.map((_, i) => (
            <span
              key={i}
              style={{
                width: i === index ? 18 : 7,
                height: 7,
                borderRadius: 999,
                background: i === index ? "#7a5226" : "rgba(120, 90, 50, 0.3)",
                transition: "width 0.25s ease, background 0.25s ease",
              }}
            />
          ))}
        </div>
        <div style={footerStyle}>
          <button type="button" style={skipBtnStyle} onClick={() => finish("skipped")}>
            跳过
          </button>
          <div style={navGroupStyle}>
            {index > 0 ? (
              <button type="button" style={backBtnStyle} onClick={goBack}>
                上一言
              </button>
            ) : null}
            <button type="button" style={nextBtnStyle} onClick={goNext}>
              {isLast ? "入世" : "下一言"}
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

// roundedRectPath 在 ctx 上铺一个圆角矩形路径（无依赖，兼容不支持 ctx.roundRect 的环境）。
function roundedRectPath(
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

export default OnboardingTour;
