/* 文件说明：抗命溯源卡（宪法 §3.3「抗命叙事化为她的成长」）。
   后端在抗命时把溯源编码进决策 Reasoning，单行格式（见 obedience.go）：
     「她为什么没听你」溯源卡 :: source=人格|phrase=这不是她会做的选择。|narration=她第一次没有照你说的做。她在变成她自己。
   本组件解析该 marker 行 → 渲染「来源标签 + 短语 + 黄字成长旁白」，把冷冰冰的编码串变成一张可读的成长卡。
   自包含内联样式，不依赖外部 CSS（主指挥客户端没有 fate.css）。*/

// DEFIANCE_TRACE_MARKER 与后端 obedience.go 的 defianceTraceMarker 逐字一致。
export const DEFIANCE_TRACE_MARKER = "「她为什么没听你」溯源卡 ::";

// ParsedDefiance 是从一行 marker 文本里解析出的溯源卡字段。
export type ParsedDefiance = {
  source: string;
  phrase: string;
  narration: string;
};

// hasDefianceTrace 判断一段文本里是否含抗命溯源卡 marker（供活动流分流渲染）。
export function hasDefianceTrace(text: string | null | undefined): boolean {
  if (!text) return false;
  return text.includes(DEFIANCE_TRACE_MARKER);
}

// stripDefianceTrace 去掉溯源卡 marker 段，只留 marker 之前的人类可读原始理由，供纯 <p> 渲染——
// 杜绝把机器编码串（source=..|phrase=..|narration=..）裸泄给玩家。无 marker 时原样返回。
export function stripDefianceTrace(text: string | null | undefined): string {
  if (!text) return "";
  const idx = text.indexOf(DEFIANCE_TRACE_MARKER);
  if (idx < 0) return text;
  return text.slice(0, idx).trim();
}

// parseDefianceTrace 从任意文本（可能多行）里切出溯源卡那一行并解析其键值。
// 解析规则与后端编码逐字对应：去掉 marker 前缀 → 按 "|" 切片 → 每片按首个 "=" 拆 key/value。
export function parseDefianceTrace(text: string | null | undefined): ParsedDefiance | null {
  if (!text) return null;
  const markerIndex = text.indexOf(DEFIANCE_TRACE_MARKER);
  if (markerIndex < 0) return null;
  // 取 marker 起头到行尾（marker 之后只有一行键值，换行即结束）。
  const after = text.slice(markerIndex + DEFIANCE_TRACE_MARKER.length);
  const lineEnd = after.indexOf("\n");
  const segment = (lineEnd >= 0 ? after.slice(0, lineEnd) : after).trim();
  if (!segment) return null;

  const fields: Record<string, string> = {};
  for (const part of segment.split("|")) {
    const eq = part.indexOf("=");
    if (eq < 0) continue;
    const key = part.slice(0, eq).trim();
    const value = part.slice(eq + 1).trim();
    if (key) {
      fields[key] = value;
    }
  }

  const source = fields.source ?? "";
  const phrase = fields.phrase ?? "";
  const narration = fields.narration ?? "";
  if (!source && !phrase && !narration) {
    return null;
  }
  return { source, phrase, narration };
}

const cardStyle: React.CSSProperties = {
  border: "1px solid rgba(217, 188, 115, 0.45)",
  borderLeft: "3px solid #d9bc73",
  borderRadius: 6,
  background: "rgba(28, 24, 16, 0.85)",
  padding: "8px 10px",
  margin: "6px 0",
  color: "#e8e2d2",
  fontSize: 13,
  lineHeight: 1.5,
};

const headerStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  gap: 8,
  marginBottom: 4,
};

const badgeStyle: React.CSSProperties = {
  display: "inline-block",
  padding: "1px 8px",
  borderRadius: 999,
  background: "rgba(217, 188, 115, 0.18)",
  border: "1px solid rgba(217, 188, 115, 0.5)",
  color: "#f2d98f",
  fontSize: 11,
  fontWeight: 600,
  whiteSpace: "nowrap",
};

const titleStyle: React.CSSProperties = {
  color: "#cdb98a",
  fontSize: 11,
  letterSpacing: 0.5,
};

const phraseStyle: React.CSSProperties = {
  margin: "2px 0",
  color: "#e8e2d2",
};

const narrationStyle: React.CSSProperties = {
  margin: "4px 0 0",
  color: "#f2d98f",
  fontStyle: "italic",
};

// DefianceCard 渲染一张抗命溯源卡。可传入已解析的 trace，或传入含 marker 的原始 reasoning 文本。
export function DefianceCard({
  trace,
  reasoning,
}: {
  trace?: ParsedDefiance | null;
  reasoning?: string | null;
}) {
  const parsed = trace ?? parseDefianceTrace(reasoning);
  if (!parsed) {
    return null;
  }
  return (
    <div className="defiance-card" style={cardStyle} role="note" aria-label="抗命溯源卡">
      <div style={headerStyle}>
        <span style={titleStyle}>她为什么没听你</span>
        {parsed.source ? <span style={badgeStyle}>来源 · {parsed.source}</span> : null}
      </div>
      {parsed.phrase ? <p style={phraseStyle}>{parsed.phrase}</p> : null}
      {parsed.narration ? <p style={narrationStyle}>{parsed.narration}</p> : null}
    </div>
  );
}

export default DefianceCard;
