/* 文件说明：世界 Boss 前端面板（跨玩家 A——后端机制完整，此为前端接入）。
   后端能力：投放世界 Boss（spawnWorldBoss）→ 多人出手累计伤害（strikeWorldBoss）→
   抢到结算闩锁者（Defeated && SettledByMe）执行全员分赃（Participants/Awards）并播祖魂广播卡（BroadcastCard）。
   无 list 端点，故本面板在「本会话内」记住自己投放过的 Boss（spawn 返回的 bossID 存 local state）。
   props 契约（供 App.tsx 挂载对齐）：
     - worldID: string —— 当前世界 ID（spawn/strike 都要）。
     - attackerCandidates: { id; name }[] —— 本局可出手单位（由 App 传入，用作出手下拉）。
     - onClose: () => void —— 关闭面板。
   自包含内联样式，参照 BillingPanel.tsx 的右侧浮层 panelStyle + GovernancePanel.tsx 的 toast/section 范式，
   仅 import api.ts/types.ts，不依赖其它并行组件。*/

import { useCallback, useMemo, useState } from "react";
import { APIError, spawnWorldBoss, strikeWorldBoss } from "../session/api";
import type { EncounterAward, WorldBossStrikeResult } from "../session/types";

type Props = {
  // worldID 当前世界 ID（spawn/strike 必需）。
  worldID: string;
  // attackerCandidates 本局可出手单位，由 App 传入用作出手下拉。
  attackerCandidates: { id: string; name: string }[];
  // onClose 关闭面板。
  onClose: () => void;
};

// SpawnedBoss 是本会话内记住的一个已投放 Boss（无 list 端点，仅前端 local state）。
type SpawnedBoss = {
  id: string;
  name: string;
  hp: number; // 投放时的初始 HP
  regionID?: string;
  // hpRemaining 最近一次 strike 回填的剩余血量（未出手过为 null）。
  hpRemaining: number | null;
  defeated: boolean;
};

const DEFAULT_BOSS_HP = 5000;

// errText 把错误归一为可展示文案，合规/鉴权类错误透出 status/reason（参照 GovernancePanel）。
function errText(err: unknown): string {
  if (err instanceof APIError) {
    const parts = [err.message];
    if (typeof err.status === "number") parts.push(`(HTTP ${err.status})`);
    if (err.reason) parts.push(`原因：${err.reason}`);
    return parts.join(" ");
  }
  return err instanceof Error ? err.message : String(err);
}

// ============ 内联样式（参照 BillingPanel.tsx 右侧浮层 + GovernancePanel.tsx 范式） ============

const panelStyle: React.CSSProperties = {
  position: "absolute",
  top: 64,
  right: 12,
  width: 380,
  maxHeight: "calc(100vh - 96px)",
  overflowY: "auto",
  zIndex: 41,
  background: "rgba(18, 20, 28, 0.95)",
  border: "1px solid rgba(217, 188, 115, 0.35)",
  borderRadius: 10,
  boxShadow: "0 8px 28px rgba(0,0,0,0.45)",
  color: "#e8e2d2",
  padding: 12,
  fontSize: 13,
};

const headerStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  justifyContent: "space-between",
  marginBottom: 8,
};

const brandStyle: React.CSSProperties = { color: "#f2d98f", fontWeight: 700, fontSize: 14 };
const subStyle: React.CSSProperties = { color: "#9aa0ad", fontSize: 11, marginTop: 2 };
const slotTitleStyle: React.CSSProperties = {
  color: "#cdb98a",
  fontSize: 11,
  letterSpacing: 0.5,
  margin: "14px 0 4px",
  textTransform: "uppercase",
};
const labelStyle: React.CSSProperties = {
  display: "block",
  color: "#cdb98a",
  fontSize: 11,
  letterSpacing: 0.4,
  margin: "10px 0 4px",
};
const inputStyle: React.CSSProperties = {
  width: "100%",
  boxSizing: "border-box",
  background: "rgba(32, 36, 48, 0.9)",
  color: "#e8e2d2",
  border: "1px solid rgba(255,255,255,0.12)",
  borderRadius: 6,
  padding: "7px 8px",
  fontSize: 13,
};
const selectStyle: React.CSSProperties = { ...inputStyle };
const sectionCardStyle: React.CSSProperties = {
  background: "rgba(32, 36, 48, 0.7)",
  border: "1px solid rgba(255,255,255,0.06)",
  borderRadius: 8,
  padding: "8px 10px",
  margin: "6px 0",
};
const bossCardStyle: React.CSSProperties = {
  ...sectionCardStyle,
  cursor: "pointer",
};
const bossCardSelectedStyle: React.CSSProperties = {
  ...bossCardStyle,
  border: "1px solid rgba(217, 188, 115, 0.6)",
  background: "rgba(217, 188, 115, 0.1)",
};
const primaryBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "rgba(217, 188, 115, 0.18)",
  border: "1px solid rgba(217, 188, 115, 0.6)",
  color: "#f2d98f",
  borderRadius: 6,
  padding: "8px 14px",
  fontSize: 13,
  fontWeight: 600,
};
const closeBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "transparent",
  border: "none",
  color: "#9aa0ad",
  fontSize: 18,
  lineHeight: 1,
};
const miniPillStyle: React.CSSProperties = {
  display: "inline-block",
  fontSize: 10,
  padding: "1px 7px",
  borderRadius: 999,
  background: "rgba(217, 188, 115, 0.16)",
  border: "1px solid rgba(217, 188, 115, 0.4)",
  color: "#e6d3a0",
  marginLeft: 6,
};
const defeatedPillStyle: React.CSSProperties = {
  ...miniPillStyle,
  color: "#bfe6c8",
  background: "rgba(111, 181, 130, 0.16)",
  border: "1px solid rgba(111, 181, 130, 0.5)",
};
const toastOkStyle: React.CSSProperties = {
  marginTop: 10,
  padding: "8px 10px",
  borderRadius: 6,
  background: "rgba(111, 181, 130, 0.16)",
  border: "1px solid rgba(111, 181, 130, 0.5)",
  color: "#bfe6c8",
  fontSize: 12,
};
const toastErrStyle: React.CSSProperties = {
  ...toastOkStyle,
  background: "rgba(196, 84, 74, 0.16)",
  border: "1px solid rgba(196, 84, 74, 0.5)",
  color: "#f0b0a6",
};
const broadcastStyle: React.CSSProperties = {
  ...sectionCardStyle,
  borderColor: "rgba(217, 188, 115, 0.45)",
  background: "rgba(217, 188, 115, 0.08)",
  color: "#f0ead8",
  fontStyle: "italic",
};
const mutedStyle: React.CSSProperties = { color: "#9aa0ad" };

// hpBar 渲染一个简易血条（剩余/初始）。
function hpBar(remaining: number, total: number): React.ReactNode {
  const pct = total > 0 ? Math.max(0, Math.min(100, (remaining / total) * 100)) : 0;
  return (
    <div style={{ marginTop: 4, height: 6, borderRadius: 999, background: "rgba(0,0,0,0.35)", overflow: "hidden" }}>
      <div
        style={{
          height: "100%",
          width: `${pct}%`,
          background: pct > 30 ? "rgba(196, 84, 74, 0.85)" : "rgba(217, 188, 115, 0.85)",
          transition: "width 0.25s ease",
        }}
      />
    </div>
  );
}

// WorldBossPanel 是接进 App 的世界 Boss 浮层面板（投放 + 出手 + 结算展示）。
export function WorldBossPanel({ worldID, attackerCandidates, onClose }: Props) {
  // 本会话内记住的已投放 Boss（无 list 端点）。
  const [bosses, setBosses] = useState<SpawnedBoss[]>([]);
  const [selectedBossID, setSelectedBossID] = useState<string>("");

  // 投放表单。
  const [spawnName, setSpawnName] = useState("");
  const [spawnHP, setSpawnHP] = useState<string>(String(DEFAULT_BOSS_HP));
  const [spawnRegion, setSpawnRegion] = useState("");
  const [spawning, setSpawning] = useState(false);

  // 出手表单。
  const [attackerID, setAttackerID] = useState<string>(attackerCandidates[0]?.id ?? "");
  const [striking, setStriking] = useState(false);
  const [lastStrike, setLastStrike] = useState<WorldBossStrikeResult | null>(null);

  const [err, setErr] = useState("");
  const [ok, setOk] = useState("");

  const selectedBoss = useMemo(
    () => bosses.find((b) => b.id === selectedBossID) ?? null,
    [bosses, selectedBossID],
  );

  // doSpawn 投放一个新 Boss，成功后把返回 bossID 记入本地列表并选中。
  const doSpawn = useCallback(async () => {
    const name = spawnName.trim();
    if (name === "") {
      setErr("请填写 Boss 名称。");
      setOk("");
      return;
    }
    const hp = Number.parseInt(spawnHP, 10);
    if (!Number.isFinite(hp) || hp <= 0) {
      setErr("血量需为正整数。");
      setOk("");
      return;
    }
    setSpawning(true);
    setErr("");
    setOk("");
    try {
      const region = spawnRegion.trim() || undefined;
      const bossID = await spawnWorldBoss(worldID, name, hp, region);
      const created: SpawnedBoss = {
        id: bossID,
        name,
        hp,
        regionID: region,
        hpRemaining: hp,
        defeated: false,
      };
      setBosses((prev) => [created, ...prev]);
      setSelectedBossID(bossID);
      setOk(`已投放世界 Boss「${name}」（HP ${hp}）。`);
      setSpawnName("");
      setSpawnRegion("");
    } catch (e) {
      setErr(`投放失败：${errText(e)}`);
    } finally {
      setSpawning(false);
    }
  }, [spawnHP, spawnName, spawnRegion, worldID]);

  // doStrike 用选定 attacker 对选定 Boss 出手，回填剩余血量/击杀态/结算结果。
  const doStrike = useCallback(async () => {
    if (selectedBossID === "") {
      setErr("请先选择一个已投放的 Boss。");
      setOk("");
      return;
    }
    if (attackerID === "") {
      setErr("请选择出手单位。");
      setOk("");
      return;
    }
    setStriking(true);
    setErr("");
    setOk("");
    try {
      const res = await strikeWorldBoss(worldID, selectedBossID, attackerID);
      setLastStrike(res);
      // 回填该 Boss 的最新血量/击杀态。
      setBosses((prev) =>
        prev.map((b) =>
          b.id === selectedBossID
            ? { ...b, hpRemaining: res.HPRemaining, defeated: b.defeated || res.Defeated }
            : b,
        ),
      );
      if (res.Defeated) {
        setOk(res.SettledByMe ? "致命一击！本次出手抢到结算，已全员分赃。" : "Boss 已被讨平（结算由他人完成）。");
      } else {
        setOk(`命中，造成 ${res.Damage} 伤害，剩余 ${res.HPRemaining}。`);
      }
    } catch (e) {
      setErr(`出手失败：${errText(e)}`);
    } finally {
      setStriking(false);
    }
  }, [attackerID, selectedBossID, worldID]);

  // 仅当 Defeated && SettledByMe 时展示结算明细。
  const showSettlement = Boolean(lastStrike && lastStrike.Defeated && lastStrike.SettledByMe);
  const awards: EncounterAward[] = lastStrike?.Awards ?? [];

  return (
    <aside style={panelStyle} role="dialog" aria-label="世界 Boss 面板">
      <div style={headerStyle}>
        <div>
          <div style={brandStyle}>世界 Boss</div>
          <div style={subStyle}>投放强敌 · 多人累计出手 · 致命者结算分赃 · 祖魂广播</div>
        </div>
        <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭世界 Boss 面板">
          ×
        </button>
      </div>

      {/* ---- 投放表单 ---- */}
      <div style={slotTitleStyle}>投放新 Boss</div>
      <div style={sectionCardStyle}>
        <label style={labelStyle} htmlFor="boss-name">
          名称
        </label>
        <input
          id="boss-name"
          style={inputStyle}
          value={spawnName}
          onChange={(e) => setSpawnName(e.target.value)}
          placeholder="如：盘踞北境的赤蛟"
        />

        <label style={labelStyle} htmlFor="boss-hp">
          血量（HP）
        </label>
        <input
          id="boss-hp"
          type="number"
          min={1}
          style={inputStyle}
          value={spawnHP}
          onChange={(e) => setSpawnHP(e.target.value)}
          placeholder={String(DEFAULT_BOSS_HP)}
        />

        <label style={labelStyle} htmlFor="boss-region">
          所在区域 ID（可选）
        </label>
        <input
          id="boss-region"
          style={inputStyle}
          value={spawnRegion}
          onChange={(e) => setSpawnRegion(e.target.value)}
          placeholder="留空表示无区域归属"
        />

        <div style={{ display: "flex", justifyContent: "flex-end", marginTop: 10 }}>
          <button type="button" style={primaryBtnStyle} onClick={() => void doSpawn()} disabled={spawning}>
            {spawning ? "投放中…" : "投放 Boss"}
          </button>
        </div>
      </div>

      {/* ---- 已投放 Boss 列表（本会话内记住） ---- */}
      <div style={slotTitleStyle}>本局已投放（{bosses.length}）</div>
      {bosses.length === 0 ? (
        <div style={{ ...sectionCardStyle, ...mutedStyle }}>暂无。先投放一个 Boss，再选中它出手。</div>
      ) : (
        bosses.map((b) => {
          const selected = b.id === selectedBossID;
          return (
            <div
              key={b.id}
              style={selected ? bossCardSelectedStyle : bossCardStyle}
              onClick={() => setSelectedBossID(b.id)}
              role="button"
              tabIndex={0}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") setSelectedBossID(b.id);
              }}
            >
              <div style={{ display: "flex", justifyContent: "space-between", gap: 8 }}>
                <span style={{ fontWeight: 600, color: "#f0ead8" }}>
                  {b.name}
                  {b.defeated ? <span style={defeatedPillStyle}>已讨平</span> : null}
                  {b.regionID ? <span style={miniPillStyle}>区域 {b.regionID}</span> : null}
                </span>
                <span style={{ ...mutedStyle, fontSize: 11, whiteSpace: "nowrap" }}>
                  {b.hpRemaining ?? b.hp} / {b.hp}
                </span>
              </div>
              {hpBar(b.hpRemaining ?? b.hp, b.hp)}
              <div style={{ ...mutedStyle, fontSize: 10, marginTop: 4 }}>ID {b.id}</div>
            </div>
          );
        })
      )}

      {/* ---- 出手 ---- */}
      <div style={slotTitleStyle}>出手</div>
      <div style={sectionCardStyle}>
        <label style={labelStyle} htmlFor="boss-attacker">
          出手单位
        </label>
        {attackerCandidates.length === 0 ? (
          <div style={{ ...mutedStyle, fontSize: 12 }}>本局暂无可出手单位。</div>
        ) : (
          <select
            id="boss-attacker"
            style={selectStyle}
            value={attackerID}
            onChange={(e) => setAttackerID(e.target.value)}
          >
            {attackerCandidates.map((c) => (
              <option key={c.id} value={c.id}>
                {c.name}（{c.id}）
              </option>
            ))}
          </select>
        )}

        <div style={{ ...mutedStyle, fontSize: 11, marginTop: 8 }}>
          目标：{selectedBoss ? `「${selectedBoss.name}」` : "（请先在上方选中一个 Boss）"}
        </div>

        <div style={{ display: "flex", justifyContent: "flex-end", marginTop: 10 }}>
          <button
            type="button"
            style={{ ...primaryBtnStyle, opacity: striking || !selectedBoss || attackerCandidates.length === 0 ? 0.6 : 1 }}
            onClick={() => void doStrike()}
            disabled={striking || !selectedBoss || attackerCandidates.length === 0}
          >
            {striking ? "出手中…" : "对 Boss 出手"}
          </button>
        </div>
      </div>

      {/* ---- 本次出手结果 ---- */}
      {lastStrike ? (
        <>
          <div style={slotTitleStyle}>本次出手结果</div>
          <div style={sectionCardStyle}>
            <div style={{ display: "flex", justifyContent: "space-between", fontSize: 12, padding: "2px 0" }}>
              <span style={mutedStyle}>造成伤害</span>
              <span style={{ color: "#f2d98f", fontWeight: 600 }}>{lastStrike.Damage}</span>
            </div>
            <div style={{ display: "flex", justifyContent: "space-between", fontSize: 12, padding: "2px 0" }}>
              <span style={mutedStyle}>Boss 剩余血量</span>
              <span style={{ color: lastStrike.HPRemaining > 0 ? "#e8e2d2" : "#bfe6c8", fontWeight: 600 }}>
                {lastStrike.HPRemaining}
              </span>
            </div>
            <div style={{ display: "flex", justifyContent: "space-between", fontSize: 12, padding: "2px 0" }}>
              <span style={mutedStyle}>是否讨平</span>
              <span style={{ color: lastStrike.Defeated ? "#bfe6c8" : "#9aa0ad", fontWeight: 600 }}>
                {lastStrike.Defeated ? "是" : "否"}
              </span>
            </div>
          </div>

          {/* 仅结算者（Defeated && SettledByMe）可见分赃 + 广播 */}
          {showSettlement ? (
            <>
              <div style={slotTitleStyle}>结算分赃（参战 {lastStrike?.Participants ?? 0} 人）</div>
              {awards.length === 0 ? (
                <div style={{ ...sectionCardStyle, ...mutedStyle }}>本次无可分战利品。</div>
              ) : (
                awards.map((a, i) => (
                  <div key={`${a.UnitID}-${a.ItemID}-${i}`} style={sectionCardStyle}>
                    <div style={{ display: "flex", justifyContent: "space-between", gap: 8 }}>
                      <span style={{ color: "#f0ead8" }}>
                        {a.ItemID}
                        <span style={miniPillStyle}>×{a.Quantity}</span>
                      </span>
                      <span style={{ ...mutedStyle, fontSize: 11, whiteSpace: "nowrap" }}>归 {a.UnitID}</span>
                    </div>
                    {a.Reason ? (
                      <div style={{ ...mutedStyle, fontSize: 11, marginTop: 4 }}>{a.Reason}</div>
                    ) : null}
                  </div>
                ))
              )}

              {lastStrike?.BroadcastCard ? (
                <>
                  <div style={slotTitleStyle}>祖魂广播</div>
                  <div style={broadcastStyle}>{lastStrike.BroadcastCard}</div>
                </>
              ) : null}
            </>
          ) : lastStrike.Defeated ? (
            <div style={{ ...sectionCardStyle, ...mutedStyle }}>
              Boss 已被讨平，但本次出手未抢到结算闩锁，分赃与广播由结算者执行。
            </div>
          ) : null}
        </>
      ) : null}

      {ok ? <div style={toastOkStyle}>{ok}</div> : null}
      {err ? <div style={toastErrStyle}>{err}</div> : null}
    </aside>
  );
}

export default WorldBossPanel;
