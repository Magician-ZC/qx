/* 文件说明：世界地图浮层（分区大世界三层导航 §8.1 的「世界地图」层）。
   全屏遮罩 overlay，墨色宣纸调（全内联样式，不碰 fate.css/styles.css）。挂载即拉 getZones，
   把 7 区按「中立新手区 + 三阵营列」分组画成区域块：每块显名字/阵营(中文+配色)/等级带/kind。
   当前区高亮「★所在」；可达区给「前往」按钮（travelToZone→成功 onClose+onTraveled）；
   不可达：portal_kind=="portal" 画「🔒未开通」、portal_kind=="" 画「路不通」，均置灰。
   busy 态防重复点。不依赖任何外部 CSS。 */

import { useCallback, useEffect, useMemo, useState } from "react";
import { zIndex } from "../zindex-tokens";
import { getZones, travelToZone, type ZoneSummary } from "../session/api";

type Props = {
  sessionId: string;
  unitId: string;
  // playerLevel 主角当前等级（用于「等级护栏」难度色：区域 boss 等级 - 主角等级 越大越凶险）。
  // 缺省 1（旧调用方/未知时按新手处理）。设计 §3：region 等级带 > 角色等级时此地凶险。
  playerLevel?: number;
  onClose: () => void;
  // onTraveled 成功前往后回调（让父级刷新快照/区域地图）。
  onTraveled?: () => void;
};

// 阵营中文名 + 配色（freedom 暖蓝 / order 金 / chaos 暗红 / neutral 灰）。
const FACTION_META: Record<string, { label: string; color: string; soft: string }> = {
  freedom: { label: "自由", color: "#3f7fb0", soft: "rgba(63, 127, 176, 0.16)" },
  order: { label: "秩序", color: "#c79a3a", soft: "rgba(199, 154, 58, 0.16)" },
  chaos: { label: "混乱", color: "#a3433f", soft: "rgba(163, 67, 63, 0.16)" },
  neutral: { label: "中立", color: "#7a7268", soft: "rgba(122, 114, 104, 0.14)" },
};

function factionMeta(factionID: string) {
  return FACTION_META[factionID] ?? FACTION_META.neutral;
}

// kind 中文（starter 新手 / capital 主城 / wild 野外）。
function kindLabel(kind: string): string {
  switch (kind) {
    case "starter":
      return "新手";
    case "capital":
      return "主城";
    case "wild":
      return "野外";
    default:
      return kind || "区域";
  }
}

// 难度色（等级护栏，设计 §3）：以「区域等级带上限 - 主角等级」的差值分四档，
// 给区域块的等级带文字上色 + 标签，引导玩家按等级带推进（魔兽式分级体验）。
//   - gap ≤ 0   ：可轻取（绿）——区域等级带不高于主角。
//   - gap 1-4   ：有挑战（橙）。
//   - gap 5-9   ：凶险（暗红）——与后端 zoneBossLevelGuardGap=5 软门对齐，挑战 boss 会被拒。
//   - gap ≥ 10  ：致命（深红）。
// 中立新手区（无 boss）不显难度档（返回 null）。
function zonePeril(zone: ZoneSummary, playerLevel: number): { label: string; color: string } | null {
  if ((zone.faction_id ?? "") === "neutral" || zone.kind === "starter") {
    return null;
  }
  const gap = zone.level_max - playerLevel;
  if (gap <= 0) return { label: "可轻取", color: "#3f7a4a" };
  if (gap < 5) return { label: "有挑战", color: "#b87a2a" };
  if (gap < 10) return { label: "此地凶险", color: "#a3433f" };
  return { label: "致命凶地", color: "#7a1c14" };
}

// 阵营分列顺序（中立单独居顶；三阵营左中右）。
const FACTION_COLUMNS: { id: string; label: string }[] = [
  { id: "freedom", label: "自由" },
  { id: "order", label: "秩序" },
  { id: "chaos", label: "混乱" },
];

export function WorldMap({ sessionId, unitId, playerLevel = 1, onClose, onTraveled }: Props) {
  const [zones, setZones] = useState<ZoneSummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  // busyZoneID：正在前往的目标区 id（非空时整图禁点，防重复提交）。
  const [busyZoneID, setBusyZoneID] = useState("");
  const [travelError, setTravelError] = useState("");

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setLoadError("");
    getZones(sessionId)
      .then((res) => {
        if (cancelled) {
          return;
        }
        setZones(res.zones);
        if (res.zones.length === 0) {
          setLoadError("舆图未能展开——此间尚无可往之处。");
        }
      })
      .catch((err) => {
        if (!cancelled) {
          setLoadError(err instanceof Error ? err.message : String(err));
        }
      })
      .finally(() => {
        if (!cancelled) {
          setLoading(false);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [sessionId]);

  // Esc 关闭世界地图（前往中不关，避免打断正在进行的穿行反馈）。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !busyZoneID) {
        onClose();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [busyZoneID, onClose]);

  const handleTravel = useCallback(
    async (zone: ZoneSummary) => {
      if (busyZoneID) {
        return;
      }
      setTravelError("");
      setBusyZoneID(zone.id);
      try {
        await travelToZone(sessionId, unitId, zone.id);
        onTraveled?.();
        onClose();
      } catch (err) {
        setTravelError(err instanceof Error ? err.message : String(err));
        setBusyZoneID("");
      }
    },
    [busyZoneID, sessionId, unitId, onClose, onTraveled],
  );

  // 把区域按「中立（居顶）」+「三阵营分列」归组。
  const grouped = useMemo(() => {
    const neutral: ZoneSummary[] = [];
    const byFaction: Record<string, ZoneSummary[]> = { freedom: [], order: [], chaos: [] };
    for (const z of zones) {
      if (z.faction_id === "neutral" || !byFaction[z.faction_id]) {
        if (z.faction_id === "neutral") {
          neutral.push(z);
        } else if (byFaction[z.faction_id]) {
          byFaction[z.faction_id].push(z);
        } else {
          // 未知阵营兜底归入中立列，避免漏画。
          neutral.push(z);
        }
      } else {
        byFaction[z.faction_id].push(z);
      }
    }
    return { neutral, byFaction };
  }, [zones]);

  return (
    <div
      style={overlayStyle}
      role="dialog"
      aria-label="世界地图"
      aria-modal="true"
      onClick={(e) => {
        // 点遮罩空白处关闭（点面板内不冒泡到这里）；前往中不关，避免打断反馈。
        if (e.target === e.currentTarget && !busyZoneID) {
          onClose();
        }
      }}
    >
      <div style={panelStyle}>
        <div style={headerStyle}>
          <div>
            <div style={titleStyle}>舆图 · 天下</div>
            <div style={subtitleStyle}>九州分疆，择一处前往。</div>
          </div>
          <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭世界地图">
            ×
          </button>
        </div>

        {travelError ? <div style={errorBannerStyle}>{travelError}</div> : null}

        <div style={bodyStyle}>
          {loading ? (
            <div style={hintStyle}>正在展开舆图…</div>
          ) : loadError ? (
            <div style={hintStyle}>{loadError}</div>
          ) : (
            <>
              {grouped.neutral.length > 0 ? (
                <div style={sectionStyle}>
                  <div style={sectionTitleStyle}>中州 · 无主之地</div>
                  <div style={zoneRowStyle}>
                    {grouped.neutral.map((z) => (
                      <ZoneCard
                        key={z.id}
                        zone={z}
                        playerLevel={playerLevel}
                        busy={Boolean(busyZoneID)}
                        busyHere={busyZoneID === z.id}
                        onTravel={handleTravel}
                      />
                    ))}
                  </div>
                </div>
              ) : null}

              <div style={columnsStyle}>
                {FACTION_COLUMNS.map((col) => {
                  const meta = factionMeta(col.id);
                  const list = grouped.byFaction[col.id] ?? [];
                  return (
                    <div key={col.id} style={columnStyle}>
                      <div style={{ ...columnTitleStyle, color: meta.color }}>{meta.label}领</div>
                      {list.length === 0 ? (
                        <div style={emptyColStyle}>—</div>
                      ) : (
                        list.map((z) => (
                          <ZoneCard
                            key={z.id}
                            zone={z}
                            playerLevel={playerLevel}
                            busy={Boolean(busyZoneID)}
                            busyHere={busyZoneID === z.id}
                            onTravel={handleTravel}
                          />
                        ))
                      )}
                    </div>
                  );
                })}
              </div>
            </>
          )}
        </div>
      </div>
    </div>
  );
}

// ZoneCard 渲染单个区域块。
function ZoneCard({
  zone,
  playerLevel,
  busy,
  busyHere,
  onTravel,
}: {
  zone: ZoneSummary;
  playerLevel: number;
  busy: boolean;
  busyHere: boolean;
  onTravel: (z: ZoneSummary) => void;
}) {
  const meta = factionMeta(zone.faction_id);
  const isCurrent = zone.is_current;
  // 难度档（等级护栏）：据「区域等级带上限 - 主角等级」分档上色（中立新手区无 boss → null）。
  const peril = zonePeril(zone, playerLevel);
  // 不可达类型：portal=>未开通锁，""=>路不通；border/可达正常。
  const lockedPortal = !zone.reachable && zone.portal_kind === "portal";
  const noRoad = !zone.reachable && zone.portal_kind === "";
  const dimmed = !zone.reachable && !isCurrent;

  const cardStyle: React.CSSProperties = {
    ...zoneCardStyle,
    borderColor: isCurrent ? meta.color : "rgba(120, 95, 60, 0.35)",
    background: isCurrent ? meta.soft : "rgba(248, 241, 228, 0.92)",
    boxShadow: isCurrent ? `0 0 0 1px ${meta.color} inset, 0 6px 16px rgba(60, 44, 27, 0.18)` : zoneCardStyle.boxShadow,
    opacity: dimmed ? 0.62 : 1,
  };

  return (
    <div style={cardStyle}>
      <div style={zoneCardTopStyle}>
        <span style={{ ...factionChipStyle, color: meta.color, borderColor: meta.color }}>
          {meta.label}
        </span>
        <span style={kindChipStyle}>{kindLabel(zone.kind)}</span>
      </div>
      <div style={zoneNameStyle}>{zone.name || zone.id}</div>
      <div style={levelBandRowStyle}>
        <span style={levelBandStyle}>Lv {zone.level_min}-{zone.level_max}</span>
        {peril ? (
          <span style={{ ...perilChipStyle, color: peril.color, borderColor: peril.color }}>
            {peril.label}
          </span>
        ) : null}
      </div>

      <div style={zoneActionStyle}>
        {isCurrent ? (
          <span style={{ ...currentBadgeStyle, color: meta.color }}>★ 所在</span>
        ) : zone.reachable ? (
          <button
            type="button"
            style={{ ...travelBtnStyle, borderColor: meta.color, color: meta.color }}
            disabled={busy}
            onClick={() => onTravel(zone)}
          >
            {busyHere ? "前往中…" : "前往 →"}
          </button>
        ) : lockedPortal ? (
          <span style={lockBadgeStyle}>🔒 未开通</span>
        ) : noRoad ? (
          <span style={lockBadgeStyle}>路不通</span>
        ) : (
          <span style={lockBadgeStyle}>不可达</span>
        )}
      </div>
    </div>
  );
}

// ── 内联样式（墨色宣纸调，不引用任何外部 CSS） ──

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
  width: "min(960px, 96vw)",
  maxHeight: "90vh",
  display: "flex",
  flexDirection: "column",
  background: "rgba(245, 236, 220, 0.98)",
  border: "1px solid rgba(140, 100, 50, 0.45)",
  borderRadius: 14,
  boxShadow: "0 18px 48px rgba(40, 28, 14, 0.42)",
  color: "#3a2c1b",
  fontFamily: "'Noto Serif SC', 'Songti SC', serif",
  overflow: "hidden",
};

const headerStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "flex-start",
  justifyContent: "space-between",
  padding: "18px 22px 12px",
  borderBottom: "1px solid rgba(140, 100, 50, 0.24)",
};

const titleStyle: React.CSSProperties = {
  fontSize: 22,
  fontWeight: 700,
  letterSpacing: 2,
};

const subtitleStyle: React.CSSProperties = {
  marginTop: 4,
  fontSize: 13,
  color: "#7a6a52",
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

const errorBannerStyle: React.CSSProperties = {
  margin: "10px 22px 0",
  padding: "8px 12px",
  borderRadius: 8,
  background: "rgba(163, 67, 63, 0.12)",
  border: "1px solid rgba(163, 67, 63, 0.4)",
  color: "#8c3a36",
  fontSize: 13,
};

const bodyStyle: React.CSSProperties = {
  padding: "16px 22px 22px",
  overflowY: "auto",
};

const hintStyle: React.CSSProperties = {
  padding: "40px 0",
  textAlign: "center",
  color: "#7a6a52",
  fontSize: 14,
};

const sectionStyle: React.CSSProperties = {
  marginBottom: 18,
};

const sectionTitleStyle: React.CSSProperties = {
  fontSize: 13,
  fontWeight: 700,
  color: "#7a7268",
  letterSpacing: 1,
  marginBottom: 8,
};

const zoneRowStyle: React.CSSProperties = {
  display: "flex",
  flexWrap: "wrap",
  gap: 12,
  justifyContent: "center",
};

const columnsStyle: React.CSSProperties = {
  display: "grid",
  // 自适应列：宽屏三列、窄屏自动退化为单列堆叠（minmax 允许子项收缩，杜绝横向溢出）。
  gridTemplateColumns: "repeat(auto-fit, minmax(180px, 1fr))",
  gap: 14,
  minWidth: 0,
};

const columnStyle: React.CSSProperties = {
  display: "flex",
  flexDirection: "column",
  gap: 12,
};

const columnTitleStyle: React.CSSProperties = {
  fontSize: 14,
  fontWeight: 700,
  letterSpacing: 2,
  textAlign: "center",
  paddingBottom: 6,
  borderBottom: "1px dashed rgba(140, 100, 50, 0.3)",
};

const emptyColStyle: React.CSSProperties = {
  textAlign: "center",
  color: "#b3a791",
  fontSize: 13,
  padding: "12px 0",
};

const zoneCardStyle: React.CSSProperties = {
  display: "flex",
  flexDirection: "column",
  gap: 6,
  minWidth: 150,
  flex: "1 1 150px",
  maxWidth: 220,
  padding: "12px 14px",
  borderRadius: 10,
  border: "1px solid rgba(120, 95, 60, 0.35)",
  background: "rgba(248, 241, 228, 0.92)",
  boxShadow: "0 4px 12px rgba(60, 44, 27, 0.1)",
  boxSizing: "border-box",
};

const zoneCardTopStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  justifyContent: "space-between",
  gap: 6,
};

const factionChipStyle: React.CSSProperties = {
  fontSize: 11,
  fontWeight: 700,
  border: "1px solid",
  borderRadius: 999,
  padding: "1px 8px",
  letterSpacing: 1,
};

const kindChipStyle: React.CSSProperties = {
  fontSize: 11,
  color: "#8a7458",
  background: "rgba(140, 100, 50, 0.12)",
  borderRadius: 6,
  padding: "1px 7px",
};

const zoneNameStyle: React.CSSProperties = {
  fontSize: 16,
  fontWeight: 700,
  letterSpacing: 1,
};

const levelBandRowStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  gap: 6,
};

const levelBandStyle: React.CSSProperties = {
  fontSize: 12,
  color: "#6f5f48",
  fontFamily: "ui-monospace, SFMono-Regular, monospace",
};

// 难度档徽标（等级护栏色，文字色 + 边框由 zonePeril 给）。
const perilChipStyle: React.CSSProperties = {
  fontSize: 11,
  border: "1px solid",
  borderRadius: 6,
  padding: "0px 6px",
  fontWeight: 600,
};

const zoneActionStyle: React.CSSProperties = {
  marginTop: 4,
};

const currentBadgeStyle: React.CSSProperties = {
  fontSize: 13,
  fontWeight: 700,
  letterSpacing: 1,
};

const travelBtnStyle: React.CSSProperties = {
  appearance: "none",
  border: "1px solid",
  background: "transparent",
  borderRadius: 8,
  padding: "5px 12px",
  fontSize: 13,
  fontWeight: 700,
  cursor: "pointer",
  fontFamily: "inherit",
};

const lockBadgeStyle: React.CSSProperties = {
  fontSize: 12,
  color: "#9a8a72",
  fontWeight: 600,
};

export default WorldMap;
