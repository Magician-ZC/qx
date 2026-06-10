/* 文件说明：世界编年史面板浮层（分区大世界阶段4 §7「世界编年史」的命运客户端露出）。
   角色史=一人之命途（CharacterSheet/ChroniclePanel），世界史=共享世界众生交织的洪流——boss 讨平 / 英雄诞生陨落 /
   区域解锁 / 阵营之战，条目以各角色真实名字（DisplayName）入史而非「主角」占位（共享世界群像史，方向B Phase0）。
   本面板纯只读观察态：挂载即 getWorldChronicle 拉一页（倒序），按纪元(Era)分段展示。
   全屏遮罩 overlay，墨色宣纸 + 史官古卷质感（全内联样式，不碰 fate.css/styles.css，仿 WorldMap/QuestPanel 范式）。
   每条：category 图标（⚔ boss 讨平 / ✦ 英雄诞生 / 🕯 英雄陨落 / 🗝 区域解锁 / ⚑ 阵营之战）+ 标题 + 史官叙事 + 世界纪。
   空态「史册尚未落笔」（旧单图档 world_id 空 / 大世界尚无大事）。遮罩空白 / Esc 关闭。 */

import { useCallback, useEffect, useMemo, useState } from "react";
import { zIndex } from "../zindex-tokens";
import { getWorldChronicle, type WorldChronicleEntry } from "../session/api";

type Props = {
  sessionId: string;
  onClose: () => void;
};

// CATEGORY_META 把后端 category 映射成图标 + 中文标签 + 主题色（仿 QuestPanel 的 QUEST_TYPE_META）。
//   - boss_slain：区域/世界 boss 被讨平（暗红，杀伐）。
//   - hero_born：传奇角色诞生（暖金，新生）。
//   - hero_died：传奇角色陨落（青灰，挽歌）。
//   - zone_unlocked：新区域解锁/主线推进（暖蓝，开拓）。
//   - faction_war / town_fall：阵营冲突 / 城镇易主（赭红，烽烟）。
//   - cataclysm：天灾巨变（紫，异变）。
const CATEGORY_META: Record<string, { icon: string; label: string; color: string }> = {
  boss_slain: { icon: "⚔", label: "霸主讨平", color: "#a3433f" },
  hero_born: { icon: "✦", label: "英雄诞生", color: "#c79a3a" },
  hero_died: { icon: "🕯", label: "英雄陨落", color: "#6b7a82" },
  zone_unlocked: { icon: "🗝", label: "新土开辟", color: "#3f7fb0" },
  faction_war: { icon: "⚑", label: "阵营之战", color: "#9a5a2a" },
  town_fall: { icon: "⚑", label: "城镇易主", color: "#9a5a2a" },
  cataclysm: { icon: "☄", label: "天地巨变", color: "#7a5cae" },
};

function categoryMeta(category: string) {
  return CATEGORY_META[category] ?? { icon: "◈", label: category || "世事", color: "#7a7268" };
}

// EraGroup 是一个纪元段：纪元名 + 该纪元内的诸条大事（已倒序）。
type EraGroup = {
  era: string;
  entries: WorldChronicleEntry[];
};

// groupByEra 把倒序的 entries 按相邻 era 折成段（保持后端给的倒序，不重排）。
// 后端按 world_tick DESC 倒序返回，同一纪元的条目相邻，故顺序扫描即可分段——遇到 era 变化就开新段。
// era 缺省（空串）的条目归入「纪元未名」段，避免漏显。
function groupByEra(entries: WorldChronicleEntry[]): EraGroup[] {
  const groups: EraGroup[] = [];
  for (const e of entries) {
    const era = e.era && e.era.trim() ? e.era.trim() : "纪元未名";
    const last = groups[groups.length - 1];
    if (last && last.era === era) {
      last.entries.push(e);
    } else {
      groups.push({ era, entries: [e] });
    }
  }
  return groups;
}

export function WorldChroniclePanel({ sessionId, onClose }: Props) {
  const [entries, setEntries] = useState<WorldChronicleEntry[]>([]);
  const [loading, setLoading] = useState(true);
  // worldLinked：是否已与大世界相连（world_id 非空）。false → 旧单图档，空态文案略有不同。
  const [worldLinked, setWorldLinked] = useState(true);

  // 挂载即拉一页世界编年史（best-effort：getWorldChronicle 内部已把失败收敛为空 feed，故这里无需 try/catch）。
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    void getWorldChronicle(sessionId).then((feed) => {
      if (cancelled) return;
      setEntries(feed.entries);
      setWorldLinked(Boolean(feed.world_id && feed.world_id.trim()));
      setLoading(false);
    });
    return () => {
      cancelled = true;
    };
  }, [sessionId]);

  // Esc 关闭。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const groups = useMemo(() => groupByEra(entries), [entries]);

  const onOverlayClick = useCallback(
    (e: React.MouseEvent) => {
      if (e.target === e.currentTarget) onClose();
    },
    [onClose],
  );

  return (
    <div
      style={overlayStyle}
      role="dialog"
      aria-label="世界编年史"
      aria-modal="true"
      onClick={onOverlayClick}
    >
      <div style={panelStyle}>
        <div style={headerStyle}>
          <div>
            <div style={titleStyle}>📖 世界编年史</div>
            <div style={subtitleStyle}>这方世界的纪元史·众生交织——史官执笔，纪元为序。</div>
          </div>
          <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭世界编年史">
            ×
          </button>
        </div>

        <div style={bodyStyle}>
          {loading ? (
            <div style={hintStyle}>正在翻阅史册…</div>
          ) : groups.length === 0 ? (
            <div style={emptyStyle}>
              <div style={emptyMarkStyle}>𐤟</div>
              <div style={emptyTitleStyle}>史册尚未落笔</div>
              <div style={emptyHintStyle}>
                {worldLinked
                  ? "这方世界众生方兴，尚无惊动史官之事。霸主、英雄、烽烟，皆待诸位来日共同书写。"
                  : "这方世界尚未与大世界相连，史册无从落笔。"}
              </div>
            </div>
          ) : (
            <div style={scrollStyle}>
              {groups.map((g, gi) => (
                <section key={`${g.era}-${gi}`} style={eraSectionStyle}>
                  <div style={eraHeadStyle}>
                    <span style={eraOrnamentStyle}>❧</span>
                    <span style={eraNameStyle}>{g.era}</span>
                    <span style={eraRuleStyle} />
                  </div>
                  <ol style={entryListStyle}>
                    {g.entries.map((e) => (
                      <ChronicleRow key={e.id} entry={e} />
                    ))}
                  </ol>
                </section>
              ))}
            </div>
          )}
        </div>

        <div style={footerStyle}>
          <span style={footerNoteStyle}>角色史载一人之命途，世界史载众生交织的洪流。</span>
        </div>
      </div>
    </div>
  );
}

// ChronicleRow 渲染一条世界大事：图标 + 类别签 + 标题 + 史官叙事 + 世界纪（world_tick）。
function ChronicleRow({ entry }: { entry: WorldChronicleEntry }) {
  const meta = categoryMeta(entry.category);
  return (
    <li style={rowStyle}>
      {/* 左侧：类别图标 + 竖向连线（古卷条目的视觉锚） */}
      <div style={railStyle}>
        <span style={{ ...iconBadgeStyle, color: meta.color, borderColor: meta.color }} aria-hidden="true">
          {meta.icon}
        </span>
      </div>
      <div style={rowBodyStyle}>
        <div style={rowTopStyle}>
          <span style={{ ...catChipStyle, color: meta.color, borderColor: meta.color }}>{meta.label}</span>
          <span style={rowTitleStyle}>{entry.title_zh || "无题之事"}</span>
        </div>
        {entry.narrative_zh ? <p style={narrativeStyle}>{entry.narrative_zh}</p> : null}
        <div style={rowMetaStyle}>
          <span style={tickStyle}>世界纪 · 第 {entry.world_tick} 刻</span>
          {entry.importance >= 7 ? <span style={epicBadgeStyle}>重大</span> : null}
        </div>
      </div>
    </li>
  );
}

// ── 内联样式（墨色宣纸 + 史官古卷质感，仿 WorldMap/QuestPanel，不引用任何外部 CSS） ──

const overlayStyle: React.CSSProperties = {
  position: "fixed",
  inset: 0,
  zIndex: zIndex.fullscreenModal,
  display: "flex",
  alignItems: "center",
  justifyContent: "center",
  background: "rgba(20, 14, 8, 0.55)",
  backdropFilter: "blur(2px)",
  padding: 24,
  boxSizing: "border-box",
};

const panelStyle: React.CSSProperties = {
  width: "min(720px, 96vw)",
  maxHeight: "90vh",
  display: "flex",
  flexDirection: "column",
  // 古卷质感：暖宣纸底叠一层极淡的纵向纹理渐变（纸纤维感），不依赖外部图。
  background:
    "linear-gradient(180deg, rgba(247, 238, 220, 0.99) 0%, rgba(242, 230, 207, 0.99) 100%)",
  border: "1px solid rgba(140, 100, 50, 0.5)",
  borderRadius: 14,
  boxShadow: "0 18px 48px rgba(40, 28, 14, 0.46), inset 0 0 60px rgba(150, 110, 60, 0.08)",
  color: "#3a2c1b",
  fontFamily: "'Noto Serif SC', 'Songti SC', serif",
  overflow: "hidden",
};

const headerStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "flex-start",
  justifyContent: "space-between",
  padding: "18px 22px 14px",
  borderBottom: "2px double rgba(140, 100, 50, 0.4)",
};

const titleStyle: React.CSSProperties = {
  fontSize: 22,
  fontWeight: 700,
  letterSpacing: 3,
  color: "#5a3f1c",
};

const subtitleStyle: React.CSSProperties = {
  marginTop: 4,
  fontSize: 13,
  color: "#8a7458",
  letterSpacing: 1,
};

const closeBtnStyle: React.CSSProperties = {
  appearance: "none",
  border: "none",
  background: "transparent",
  fontSize: 26,
  lineHeight: 1,
  color: "#8a7458",
  cursor: "pointer",
  padding: "0 4px",
};

const bodyStyle: React.CSSProperties = {
  flex: 1,
  minHeight: 0,
  display: "flex",
  flexDirection: "column",
  overflow: "hidden",
};

const scrollStyle: React.CSSProperties = {
  padding: "18px 22px 10px",
  overflowY: "auto",
};

const hintStyle: React.CSSProperties = {
  padding: "56px 0",
  textAlign: "center",
  color: "#7a6a52",
  fontSize: 14,
};

const emptyStyle: React.CSSProperties = {
  padding: "52px 28px",
  textAlign: "center",
};

const emptyMarkStyle: React.CSSProperties = {
  fontSize: 34,
  color: "rgba(140, 100, 50, 0.4)",
  marginBottom: 12,
};

const emptyTitleStyle: React.CSSProperties = {
  fontSize: 18,
  fontWeight: 700,
  letterSpacing: 2,
  color: "#6b4a22",
  marginBottom: 8,
};

const emptyHintStyle: React.CSSProperties = {
  fontSize: 13,
  lineHeight: 1.8,
  color: "#9a8a72",
  maxWidth: 380,
  margin: "0 auto",
};

const eraSectionStyle: React.CSSProperties = {
  marginBottom: 22,
};

const eraHeadStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  gap: 10,
  marginBottom: 14,
};

const eraOrnamentStyle: React.CSSProperties = {
  fontSize: 16,
  color: "#a3793f",
  flexShrink: 0,
};

const eraNameStyle: React.CSSProperties = {
  fontSize: 16,
  fontWeight: 700,
  letterSpacing: 3,
  color: "#6b4a22",
  flexShrink: 0,
};

const eraRuleStyle: React.CSSProperties = {
  flex: 1,
  height: 1,
  background:
    "linear-gradient(90deg, rgba(140, 100, 50, 0.5) 0%, rgba(140, 100, 50, 0.08) 100%)",
};

const entryListStyle: React.CSSProperties = {
  listStyle: "none",
  margin: 0,
  padding: 0,
  display: "flex",
  flexDirection: "column",
  gap: 12,
};

const rowStyle: React.CSSProperties = {
  display: "flex",
  gap: 12,
  padding: "12px 14px",
  borderRadius: 10,
  border: "1px solid rgba(120, 95, 60, 0.3)",
  background: "rgba(250, 243, 229, 0.7)",
  boxShadow: "0 3px 10px rgba(60, 44, 27, 0.08)",
  boxSizing: "border-box",
};

const railStyle: React.CSSProperties = {
  flexShrink: 0,
  display: "flex",
  flexDirection: "column",
  alignItems: "center",
  paddingTop: 2,
};

const iconBadgeStyle: React.CSSProperties = {
  width: 34,
  height: 34,
  borderRadius: "50%",
  border: "1px solid",
  display: "flex",
  alignItems: "center",
  justifyContent: "center",
  fontSize: 17,
  background: "rgba(255, 252, 245, 0.85)",
};

const rowBodyStyle: React.CSSProperties = {
  flex: 1,
  minWidth: 0,
  display: "flex",
  flexDirection: "column",
  gap: 6,
};

const rowTopStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  gap: 8,
  flexWrap: "wrap",
};

const catChipStyle: React.CSSProperties = {
  fontSize: 11,
  fontWeight: 700,
  border: "1px solid",
  borderRadius: 999,
  padding: "1px 8px",
  letterSpacing: 1,
  flexShrink: 0,
};

const rowTitleStyle: React.CSSProperties = {
  fontSize: 16,
  fontWeight: 700,
  letterSpacing: 1,
  color: "#3a2c1b",
};

const narrativeStyle: React.CSSProperties = {
  margin: 0,
  fontSize: 13.5,
  lineHeight: 1.85,
  color: "#5a4a36",
  // 史官叙事略带古意：首行缩进两字。
  textIndent: "2em",
};

const rowMetaStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  gap: 10,
  marginTop: 2,
};

const tickStyle: React.CSSProperties = {
  fontSize: 12,
  color: "#8a6a3a",
  letterSpacing: 0.5,
};

const epicBadgeStyle: React.CSSProperties = {
  fontSize: 11,
  fontWeight: 700,
  color: "#a3433f",
  border: "1px solid rgba(163, 67, 63, 0.5)",
  borderRadius: 4,
  padding: "0 6px",
  letterSpacing: 1,
};

const footerStyle: React.CSSProperties = {
  padding: "10px 22px 14px",
  borderTop: "1px solid rgba(140, 100, 50, 0.22)",
  textAlign: "center",
};

const footerNoteStyle: React.CSSProperties = {
  fontSize: 12,
  color: "#9a8a72",
  letterSpacing: 1,
  fontStyle: "italic",
};

export default WorldChroniclePanel;
