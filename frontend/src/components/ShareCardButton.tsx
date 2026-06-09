/* 文件说明：「生成分享图」按钮组件（设计 GDD 传播；当前盘点：分享仅文本 clipboard 复制，无图片卡，
   不构成「愿意截图传播」）。点击调 fate/shareCard.ts 的 renderShareCard 把死亡 / 高光 / 传记卡手绘成
   竖版 PNG，提供：①下载（a[download]）②复制图片到剪贴板（navigator.clipboard.write + ClipboardItem，
   失败回退下载）③预览缩略图。

   依赖注入：卡数据经 props（ShareCardOptions）传入；可选 onShared 埋点回调由主控注入（不直接 import
   api.ts，避免与并发改 api.ts 冲突）。供 FateView / FatePanel / ChroniclePanel / 名人堂的死亡 / 高光 /
   传记分享处挂载（主控集成）。自包含内联样式（仿 FatePanel 浮层风格），不依赖外部 css。 */

import { useCallback, useEffect, useRef, useState } from "react";
import {
  renderShareCard,
  renderShareCardDataURL,
  type ShareCardKind,
  type ShareCardOptions,
} from "../fate/shareCard";

type Props = {
  // card 要图片化的卡数据（标题 / 正文 / kind / 强调色…），由主控按数据源构造后注入。
  card: ShareCardOptions;
  // fileName 下载文件名（不含扩展名），缺省按 kind + 标题生成。
  fileName?: string;
  // label 按钮文案，缺省按 kind 取祖魂语气文案。
  label?: string;
  // compact 紧凑模式：仅一个小按钮、不渲染预览缩略图（用于卡片右下角小入口，如高光卡）。
  compact?: boolean;
  // onShared 可选埋点回调（主控注入，best-effort）：method 标识下载还是复制成功。
  onShared?: (info: { kind: ShareCardKind; method: "download" | "clipboard" }) => void;
  // disabled 外部禁用。
  disabled?: boolean;
};

const wrapStyle: React.CSSProperties = {
  display: "flex",
  flexDirection: "column",
  gap: 8,
  marginTop: 8,
};
const rowStyle: React.CSSProperties = {
  display: "flex",
  gap: 6,
  flexWrap: "wrap",
  alignItems: "center",
};
const primaryBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "rgba(217, 188, 115, 0.16)",
  border: "1px solid rgba(217, 188, 115, 0.5)",
  color: "#f2d98f",
  borderRadius: 6,
  padding: "5px 12px",
  fontSize: 12,
};
const ghostBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "transparent",
  border: "1px solid rgba(255,255,255,0.18)",
  color: "#cbd1da",
  borderRadius: 6,
  padding: "5px 10px",
  fontSize: 12,
};
const disabledBtnStyle: React.CSSProperties = {
  opacity: 0.5,
  cursor: "default",
};
const thumbStyle: React.CSSProperties = {
  width: 96,
  borderRadius: 8,
  border: "1px solid rgba(217, 188, 115, 0.35)",
  boxShadow: "0 4px 14px rgba(0,0,0,0.4)",
  cursor: "zoom-in",
};
const noticeStyle: React.CSSProperties = {
  color: "#9aa0ad",
  fontSize: 11,
};
const errStyle: React.CSSProperties = {
  color: "#f0a89a",
  fontSize: 11,
};

// defaultLabel 按 kind 给祖魂语气的按钮文案。
function defaultLabel(kind: ShareCardKind): string {
  switch (kind) {
    case "death":
      return "为她制一张悼卡";
    case "biography":
      return "把她的一生印成图";
    case "highlight":
    default:
      return "生成分享图";
  }
}

// safeName 把标题清成安全文件名片段（去掉路径/空白等）。
function safeName(s: string): string {
  return s.replace(/[\\/:*?"<>|\s]+/g, "_").slice(0, 40) || "card";
}

// triggerDownload 用临时 a[download] 触发 Blob 下载，随后回收 objectURL。
function triggerDownload(blob: Blob, fileName: string): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = fileName;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  // 延迟回收，确保下载已发起。
  window.setTimeout(() => URL.revokeObjectURL(url), 4000);
}

// canClipboardImage 探测是否可向剪贴板写图片（ClipboardItem + clipboard.write 都在）。
function canClipboardImage(): boolean {
  return (
    typeof ClipboardItem !== "undefined" &&
    typeof navigator !== "undefined" &&
    !!navigator.clipboard &&
    typeof navigator.clipboard.write === "function"
  );
}

// ShareCardButton 「生成分享图」入口：先把卡渲成 Blob，再给下载 + 复制图片两个动作（复制失败回退下载）。
export function ShareCardButton({ card, fileName, label, compact, onShared, disabled }: Props) {
  const [busy, setBusy] = useState(false);
  const [thumb, setThumb] = useState<string>("");
  const [toast, setToast] = useState("");
  const [error, setError] = useState("");
  // blobRef 缓存已渲染的 Blob，避免下载 / 复制重复绘制。card 变化时失效。
  const blobRef = useRef<Blob | null>(null);

  // card 变化时清掉缓存的 Blob 与缩略图（数据已不是同一张卡）。
  useEffect(() => {
    blobRef.current = null;
    setThumb("");
    setError("");
  }, [card]);

  const resolvedFileName = `${safeName(fileName ?? `${card.kind}_${card.title}`)}.png`;

  // ensureBlob 惰性渲染并缓存 Blob。
  const ensureBlob = useCallback(async (): Promise<Blob> => {
    if (blobRef.current) return blobRef.current;
    const blob = await renderShareCard(card);
    blobRef.current = blob;
    return blob;
  }, [card]);

  // ensureThumb 惰性生成预览缩略图（DataURL，同步）；失败吞错，不阻断主动作。
  const ensureThumb = useCallback(() => {
    if (compact) return;
    try {
      setThumb(renderShareCardDataURL(card));
    } catch {
      // 预览失败不影响下载 / 复制。
    }
  }, [card, compact]);

  // onCopy 把图片写进剪贴板（ClipboardItem）；不支持 / 失败则回退下载并提示。
  const onCopy = useCallback(async () => {
    if (disabled || busy) return;
    setBusy(true);
    setError("");
    try {
      const blob = await ensureBlob();
      ensureThumb();
      if (canClipboardImage()) {
        try {
          await navigator.clipboard.write([new ClipboardItem({ "image/png": blob })]);
          setToast("分享图已复制，去任意聊天里粘贴吧。");
          onShared?.({ kind: card.kind, method: "clipboard" });
          return;
        } catch {
          // 复制被拒 / 不支持 image/png → 回退下载。
        }
      }
      triggerDownload(blob, resolvedFileName);
      setToast("浏览器不支持复制图片，已为你下载，去分享吧。");
      onShared?.({ kind: card.kind, method: "download" });
    } catch (err) {
      setError(`生成分享图失败：${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setBusy(false);
    }
  }, [disabled, busy, ensureBlob, ensureThumb, resolvedFileName, onShared, card.kind]);

  // onDownload 直接下载 PNG。
  const onDownload = useCallback(async () => {
    if (disabled || busy) return;
    setBusy(true);
    setError("");
    try {
      const blob = await ensureBlob();
      ensureThumb();
      triggerDownload(blob, resolvedFileName);
      setToast("分享图已下载。");
      onShared?.({ kind: card.kind, method: "download" });
    } catch (err) {
      setError(`生成分享图失败：${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setBusy(false);
    }
  }, [disabled, busy, ensureBlob, ensureThumb, resolvedFileName, onShared, card.kind]);

  const btnLabel = label ?? defaultLabel(card.kind);
  const isDisabled = Boolean(disabled) || busy;

  // 紧凑模式：仅一个「复制/下载」按钮（优先复制图片），不渲染缩略图。
  if (compact) {
    return (
      <button
        type="button"
        style={isDisabled ? { ...primaryBtnStyle, ...disabledBtnStyle } : primaryBtnStyle}
        disabled={isDisabled}
        title="把这张卡存成图片，截图传播给别人看"
        onClick={() => void onCopy()}
      >
        {busy ? "正在制图…" : btnLabel}
      </button>
    );
  }

  return (
    <div style={wrapStyle}>
      <div style={rowStyle}>
        <button
          type="button"
          style={isDisabled ? { ...primaryBtnStyle, ...disabledBtnStyle } : primaryBtnStyle}
          disabled={isDisabled}
          title="把这张卡复制成图片，可直接粘贴到聊天里"
          onClick={() => void onCopy()}
        >
          {busy ? "正在制图…" : `${btnLabel}（复制图片）`}
        </button>
        <button
          type="button"
          style={isDisabled ? { ...ghostBtnStyle, ...disabledBtnStyle } : ghostBtnStyle}
          disabled={isDisabled}
          title="把这张卡下载成 PNG 图片"
          onClick={() => void onDownload()}
        >
          下载
        </button>
      </div>
      {thumb ? (
        <a href={thumb} download={resolvedFileName} title="点击下载这张图" style={{ alignSelf: "flex-start" }}>
          <img src={thumb} alt="分享图预览" style={thumbStyle} />
        </a>
      ) : null}
      {error ? <div style={errStyle}>{error}</div> : null}
      {!error && toast ? <div style={noticeStyle}>{toast}</div> : null}
    </div>
  );
}

export default ShareCardButton;
