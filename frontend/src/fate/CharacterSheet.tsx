/* 文件说明：MMORPG 式「角色档案」浮层（命运客户端，混合模型：观察为主 + 在线可操作）。把后端
   整条角色数据（getUnitStatus 返回的 unit.Record）露给玩家：顶部 5 个 tab——状态 / 技能 / 背包 /
   关系 / 编年史。背包 tab 支持在线操作：穿上装备（equipItem）/ 使用消耗品（useItem，A10，后端
   只开 ration/healing_potion）/ 卸下装备（unequipItem，A11，slot=weapon|armor|shoes|accessory）；
   其余只读。仿 CharterEditor 的浮层定位/遮罩/关闭。
   依赖注入风格沿用既有 components：本组件直接从 ../session/api 取 getUnitStatus/getUnitRelations/getItemCatalog
   （这些是本波新增/既有的纯读 wrapper），编年史 tab 复用 ChroniclePanel（注入 getChronicleFeed）。
   后端键名契约：unit.Record 有 json tag 小写——identity/stats{primary,derived}/skills{weapons,survival,social,specialties}
   /personality(8 维)/status/inventory{equipment,backpack}；关系四轴 clamp[-10,10]。墨色宣纸调，与 .fate-* 同口径。*/

import { useCallback, useEffect, useMemo, useState } from "react";
import { zIndex } from "../zindex-tokens";
import { ChroniclePanel } from "../components/ChroniclePanel";
import {
  equipItem,
  getChronicleFeed,
  getItemCatalog,
  getUnitRelations,
  getUnitStatus,
  unequipItem,
  // useItem 起别名（不以 use 开头）：api 函数 `use` 前缀会被 eslint react-hooks/rules-of-hooks 误判为 React Hook。
  useItem as apiUseItem,
  type UnitRelationView,
} from "../session/api";

type Props = {
  sessionId: string;
  unitId: string;
  // fallbackName：getUnitStatus 未返回 identity.name 时的兜底显示名（取 FateApp 的 saved.name）。
  fallbackName?: string;
  onClose: () => void;
};

type TabKey = "status" | "skills" | "inventory" | "relations" | "chronicle";
const TABS: { key: TabKey; label: string }[] = [
  { key: "status", label: "状态" },
  { key: "skills", label: "技能" },
  { key: "inventory", label: "背包" },
  { key: "relations", label: "关系" },
  { key: "chronicle", label: "编年史" },
];

// ── 安全取值：unit 是 Record<string,unknown>，按需收窄，缺字段优雅降级 ──
function asRecord(v: unknown): Record<string, unknown> {
  return v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
}
function asNum(v: unknown, fallback = 0): number {
  return typeof v === "number" && Number.isFinite(v) ? v : fallback;
}
function asStr(v: unknown, fallback = ""): string {
  return typeof v === "string" ? v : fallback;
}
function asBool(v: unknown): boolean {
  return v === true;
}

// 人格 8 维（后端 personality 各字段，normalizedTrait → [0,1]）：中文名。条宽走 personaWidth（[0,1]→满条），数值原样并列。
const PERSONA_AXES: { key: string; label: string }[] = [
  { key: "courage", label: "勇武" },
  { key: "loyalty", label: "忠诚" },
  { key: "aggression", label: "好斗" },
  { key: "prudence", label: "审慎" },
  { key: "sociability", label: "合群" },
  { key: "integrity", label: "正直" },
  { key: "stability", label: "稳重" },
  { key: "ambition", label: "野心" },
];

const PRIMARY_AXES: { key: string; label: string }[] = [
  { key: "strength", label: "力量" },
  { key: "dexterity", label: "敏捷" },
  { key: "constitution", label: "体质" },
  { key: "wisdom", label: "悟性" },
  { key: "perception", label: "感知" },
  { key: "charisma", label: "魅力" },
];

const DERIVED_AXES: { key: string; label: string }[] = [
  { key: "attack", label: "攻击" },
  { key: "defense", label: "防御" },
  { key: "accuracy", label: "命中" },
  { key: "evasion", label: "闪避" },
  { key: "vision", label: "视野" },
  { key: "carry_weight", label: "负重" },
];

const WEAPON_SKILLS: { key: string; label: string }[] = [
  { key: "sword", label: "刀剑" },
  { key: "bow", label: "弓弩" },
  { key: "blunt", label: "钝器" },
  { key: "shield", label: "盾防" },
  { key: "medical", label: "战地医" },
];
const SURVIVAL_SKILLS: { key: string; label: string }[] = [
  { key: "scouting", label: "侦察" },
  { key: "stealth", label: "潜行" },
  { key: "medicine", label: "医术" },
  { key: "gathering", label: "采集" },
];
const SOCIAL_SKILLS: { key: string; label: string }[] = [
  { key: "negotiation", label: "斡旋" },
  { key: "intimidation", label: "威慑" },
  { key: "charm", label: "魅惑" },
  { key: "trade", label: "经商" },
];

// USABLE_ITEM_IDS：后端 PlayerUseItem 当前只开通这两条使用结算（ration 口粮补饥饿 +35 /
// healing_potion 药剂补 HP +25，满血拒绝）；其余消耗品（药草包/解毒药/复活石）尚无玩家
// 直驱用法，后端会中文报错，故不显示「使用」按钮。
const USABLE_ITEM_IDS = new Set(["ration", "healing_potion"]);

// 装备槽中文名（后端 inventory.equipment 的 slot 键，对齐 item.Slot 常量）。
const SLOT_LABELS: Record<string, string> = {
  weapon: "武器",
  armor: "护甲",
  shoes: "鞋履",
  accessory: "饰物",
  offhand: "副手",
  helmet: "头盔",
};
function slotLabel(slot: string): string {
  return SLOT_LABELS[slot] ?? slot;
}

// lifeStateLabel 把 status.life_state 译成中文（与后端 unit life_state 字面量对齐，未知退原值）。
function lifeStateLabel(state: string): string {
  const map: Record<string, string> = {
    alive: "在世",
    healthy: "康健",
    wounded: "负伤",
    downed: "倒地",
    recovering: "休养",
    dead: "已逝",
    retired: "退隐",
  };
  return map[state] ?? (state || "在世");
}

// personaWidth 把人格维度数值折算成进度条占比 [0,1]。后端人格是 normalizedTrait → [0,1]（如 courage 0.39）。
function personaWidth(v: number): number {
  return Math.max(0, Math.min(1, v));
}
// skillWidth 把技能熟练度折算成进度条占比 [0,1]。后端技能 clampIntWithFallback(0,5) → [0,5]（武器/生存/社交各项）。
function skillWidth(v: number): number {
  return Math.max(0, Math.min(1, v / 5));
}

export function CharacterSheet({ sessionId, unitId, fallbackName, onClose }: Props) {
  const [tab, setTab] = useState<TabKey>("status");
  const [unit, setUnit] = useState<Record<string, unknown> | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  // 关系 tab：懒加载（首次切到该 tab 才拉）。
  const [relations, setRelations] = useState<UnitRelationView[] | null>(null);
  const [relLoading, setRelLoading] = useState(false);
  const [relError, setRelError] = useState("");

  // 物品目录：id→中文名（背包 tab 译名用，best-effort，失败回空 Map 全退原 id）。
  const [catalog, setCatalog] = useState<Map<string, string>>(new Map());

  // 挂载即拉整条 unit + 物品目录（目录失败回空 Map 不影响）。
  useEffect(() => {
    let alive = true;
    setLoading(true);
    setError("");
    void (async () => {
      try {
        const [rec, cat] = await Promise.all([getUnitStatus(unitId), getItemCatalog()]);
        if (!alive) return;
        setCatalog(cat);
        if (rec) {
          setUnit(rec);
        } else {
          setError("没能找到她的档案。");
        }
      } catch (err) {
        if (alive) setError(err instanceof Error ? err.message : String(err));
      } finally {
        if (alive) setLoading(false);
      }
    })();
    return () => {
      alive = false;
    };
  }, [unitId]);

  // refreshUnit：在线操作（穿上/使用/卸下）成功后重拉整条档案，让数值/装备/行囊就地刷新。
  const refreshUnit = useCallback(async () => {
    const rec = await getUnitStatus(unitId);
    if (rec) setUnit(rec);
  }, [unitId]);

  // equipBusy：正在穿的物品 id（防重复点）。onEquip：玩家在线给她穿装备（混合模型可操作），成功后重拉档案。
  const [equipBusy, setEquipBusy] = useState("");
  const onEquip = useCallback(
    async (itemID: string) => {
      setEquipBusy(itemID);
      try {
        await equipItem(sessionId, unitId, itemID);
        await refreshUnit();
      } catch (e) {
        window.alert(e instanceof Error ? e.message : "穿不上这件");
      } finally {
        setEquipBusy("");
      }
    },
    [sessionId, unitId, refreshUnit],
  );

  // useBusy：正在使用的消耗品 id（防重复点）。onUse：玩家在线让她吃口粮/喝药（A10），
  // 成功后重拉档案（气血/饥饿/行囊数量就地更新）；失败 alert 后端中文提示兜底。
  const [useBusy, setUseBusy] = useState("");
  const onUse = useCallback(
    async (itemID: string) => {
      setUseBusy(itemID);
      try {
        await apiUseItem(sessionId, unitId, itemID);
        await refreshUnit();
      } catch (e) {
        window.alert(e instanceof Error ? e.message : "这东西她用不了");
      } finally {
        setUseBusy("");
      }
    },
    [sessionId, unitId, refreshUnit],
  );

  // unequipBusy：正在卸下的槽位（防重复点）。onUnequip：玩家在线让她卸下某槽装备回行囊（A11），
  // slot 即后端 inventory.equipment 的键（weapon|armor|shoes|accessory）；成功后重拉档案。
  const [unequipBusy, setUnequipBusy] = useState("");
  const onUnequip = useCallback(
    async (slot: string) => {
      setUnequipBusy(slot);
      try {
        await unequipItem(sessionId, unitId, slot);
        await refreshUnit();
      } catch (e) {
        // 后端最常见失败是「她的行囊已满，腾不出地方放这件装备」（卸下回包失败时装备保持原样）。
        window.alert(e instanceof Error ? e.message : "这件卸不下来");
      } finally {
        setUnequipBusy("");
      }
    },
    [sessionId, unitId, refreshUnit],
  );

  // 关系 tab 首次切入时懒加载。
  const loadRelations = useCallback(async () => {
    setRelLoading(true);
    setRelError("");
    try {
      const rows = await getUnitRelations(unitId);
      setRelations(rows);
    } catch (err) {
      setRelError(err instanceof Error ? err.message : String(err));
      setRelations([]);
    } finally {
      setRelLoading(false);
    }
  }, [unitId]);

  useEffect(() => {
    if (tab === "relations" && relations === null && !relLoading) {
      void loadRelations();
    }
  }, [tab, relations, relLoading, loadRelations]);

  const identity = useMemo(() => asRecord(unit?.identity), [unit]);
  const stats = useMemo(() => asRecord(unit?.stats), [unit]);
  const skills = useMemo(() => asRecord(unit?.skills), [unit]);
  const personality = useMemo(() => asRecord(unit?.personality), [unit]);
  const status = useMemo(() => asRecord(unit?.status), [unit]);
  const inventory = useMemo(() => asRecord(unit?.inventory), [unit]);

  const name = asStr(identity.name) || fallbackName || "她";
  const nickname = asStr(identity.nickname);
  const lineage = asStr(identity.lineage);
  const age = asNum(identity.age, 0);
  const biography = asStr(identity.biography);

  return (
    <aside style={panelStyle} role="dialog" aria-label="角色档案">
      <div style={headerStyle}>
        <div>
          <div style={brandStyle}>
            {name}
            {nickname ? <span style={nickStyle}>「{nickname}」</span> : null}
          </div>
          <div style={subStyle}>
            {lineage ? `${lineage} · ` : ""}
            {age > 0 ? `${age} 岁` : ""}
            {!lineage && age <= 0 ? "她的一身履历，尽在于此。" : ""}
          </div>
        </div>
        <button type="button" style={closeBtnStyle} onClick={onClose} aria-label="关闭角色档案">
          ×
        </button>
      </div>

      {/* tab 条 */}
      <div className="cs-tabs" role="tablist">
        {TABS.map((t) => (
          <button
            key={t.key}
            type="button"
            role="tab"
            aria-selected={tab === t.key}
            className={`cs-tab${tab === t.key ? " cs-tab-active" : ""}`}
            onClick={() => setTab(t.key)}
          >
            {t.label}
          </button>
        ))}
      </div>

      {loading ? (
        <div className="cs-empty">正在翻阅她的档案…</div>
      ) : error ? (
        <div className="cs-notice">读取档案失败：{error}</div>
      ) : (
        <div className="cs-body">
          {tab === "status" && (
            <StatusTab status={status} personality={personality} stats={stats} biography={biography} />
          )}
          {tab === "skills" && <SkillsTab skills={skills} />}
          {tab === "inventory" && (
            <InventoryTab
              inventory={inventory}
              status={status}
              catalog={catalog}
              onEquip={onEquip}
              equipBusy={equipBusy}
              onUse={onUse}
              useBusy={useBusy}
              onUnequip={onUnequip}
              unequipBusy={unequipBusy}
            />
          )}
          {tab === "relations" && (
            <RelationsTab loading={relLoading} error={relError} relations={relations} />
          )}
          {tab === "chronicle" && (
            // 编年史 tab：复用 ChroniclePanel（依赖注入 getChronicleFeed）。它自身是 position:absolute 浮层，
            // 这里包一层 relative 容器并就地内联覆写定位，使其内嵌进本 tab（而非另飞到屏幕右上）。
            <div className="cs-chronicle-host">
              <ChroniclePanel
                sessionID={sessionId}
                unitID={unitId}
                unitName={name}
                fetchChronicle={getChronicleFeed}
                onClose={onClose}
              />
            </div>
          )}
        </div>
      )}
    </aside>
  );
}

// ── 状态 tab ──
function StatusTab(props: {
  status: Record<string, unknown>;
  personality: Record<string, unknown>;
  stats: Record<string, unknown>;
  biography: string;
}) {
  const { status, personality, stats, biography } = props;
  const lifeState = asStr(status.life_state, "alive");
  const hp = asNum(status.hp);
  const hunger = asNum(status.hunger);
  const wallet = asNum(status.wallet);
  const lives = asNum(status.lives_remaining);
  const q = asNum(status.position_q);
  const r = asNum(status.position_r);
  const inCombat = asBool(status.in_combat);

  const primary = asRecord(stats.primary);
  const derived = asRecord(stats.derived);

  return (
    <div className="cs-section">
      {/* 生命徽章 */}
      <div className="cs-badges">
        <span className={`cs-badge cs-badge-life cs-life-${lifeState}`}>{lifeStateLabel(lifeState)}</span>
        <span className="cs-badge">气血 {hp}</span>
        <span className="cs-badge">余命 {lives}</span>
        {inCombat ? <span className="cs-badge cs-badge-combat">交战中</span> : null}
        <span className="cs-badge">位置 ({q}, {r})</span>
      </div>

      {/* 气血 / 饥饿 / 钱袋 */}
      <div className="cs-bars">
        <CsBar label="气血" value={hp} max={100} variant="hp" />
        <CsBar label="饥饿" value={hunger} max={100} variant="hunger" />
        <div className="cs-kv">
          <span className="cs-kv-k">钱袋</span>
          <span className="cs-kv-v">{wallet} G</span>
        </div>
      </div>

      {/* 人格 8 维 */}
      <div className="cs-slot-title">心性 · 八维</div>
      <div className="cs-grid">
        {PERSONA_AXES.map((a) => {
          const v = asNum(personality[a.key]);
          return (
            <div className="cs-stat-row" key={a.key}>
              <span className="cs-stat-label">{a.label}</span>
              <span className="cs-stat-track">
                <span className="cs-stat-fill cs-fill-persona" style={{ width: `${personaWidth(v) * 100}%` }} />
              </span>
              <span className="cs-stat-num">{v}</span>
            </div>
          );
        })}
      </div>

      {/* 基础属性六维 */}
      <div className="cs-slot-title">本相 · 六维</div>
      <div className="cs-grid">
        {PRIMARY_AXES.map((a) => (
          <div className="cs-stat-row cs-stat-plain" key={a.key}>
            <span className="cs-stat-label">{a.label}</span>
            <span className="cs-stat-num cs-stat-num-strong">{asNum(primary[a.key])}</span>
          </div>
        ))}
      </div>

      {/* 衍生属性 */}
      <div className="cs-slot-title">战力 · 衍生</div>
      <div className="cs-grid">
        {DERIVED_AXES.map((a) => (
          <div className="cs-stat-row cs-stat-plain" key={a.key}>
            <span className="cs-stat-label">{a.label}</span>
            <span className="cs-stat-num cs-stat-num-strong">{asNum(derived[a.key])}</span>
          </div>
        ))}
      </div>

      {biography ? (
        <>
          <div className="cs-slot-title">生平</div>
          <p className="cs-bio">{biography}</p>
        </>
      ) : null}
    </div>
  );
}

// ── 技能 tab ──
function SkillsTab(props: { skills: Record<string, unknown> }) {
  const { skills } = props;
  const weapons = asRecord(skills.weapons);
  const survival = asRecord(skills.survival);
  const social = asRecord(skills.social);
  const specialties = Array.isArray(skills.specialties) ? (skills.specialties as unknown[]) : [];

  const groups: { title: string; axes: { key: string; label: string }[]; src: Record<string, unknown> }[] = [
    { title: "武技", axes: WEAPON_SKILLS, src: weapons },
    { title: "生存", axes: SURVIVAL_SKILLS, src: survival },
    { title: "社交", axes: SOCIAL_SKILLS, src: social },
  ];

  return (
    <div className="cs-section">
      {groups.map((g) => (
        <div key={g.title}>
          <div className="cs-slot-title">{g.title}</div>
          <div className="cs-grid">
            {g.axes.map((a) => {
              const v = asNum(g.src[a.key]);
              return (
                <div className="cs-stat-row" key={a.key}>
                  <span className="cs-stat-label">{a.label}</span>
                  <span className="cs-stat-track">
                    <span className="cs-stat-fill cs-fill-skill" style={{ width: `${skillWidth(v) * 100}%` }} />
                  </span>
                  <span className="cs-stat-num">{v}</span>
                </div>
              );
            })}
          </div>
        </div>
      ))}

      <div className="cs-slot-title">专长</div>
      {specialties.length === 0 ? (
        <div className="cs-empty-inline">她还没有立身的绝活。</div>
      ) : (
        <div className="cs-tags">
          {specialties.map((s, i) => (
            <span className="cs-tag" key={`${String(s)}-${i}`}>
              {String(s)}
            </span>
          ))}
        </div>
      )}
    </div>
  );
}

// ── 背包 tab ──
function InventoryTab(props: {
  inventory: Record<string, unknown>;
  status: Record<string, unknown>;
  catalog: Map<string, string>;
  onEquip?: (itemID: string) => void;
  equipBusy?: string;
  onUse?: (itemID: string) => void;
  useBusy?: string;
  onUnequip?: (slot: string) => void;
  unequipBusy?: string;
}) {
  const { inventory, status, catalog, onEquip, equipBusy, onUse, useBusy, onUnequip, unequipBusy } = props;
  const wallet = asNum(status.wallet);
  const equipment = asRecord(inventory.equipment);
  const backpack = Array.isArray(inventory.backpack) ? (inventory.backpack as unknown[]) : [];

  const itemName = useCallback(
    (it: Record<string, unknown>): string => {
      const custom = asStr(it.custom_name);
      if (custom) return custom;
      const id = asStr(it.item_id);
      return catalog.get(id) || id || "未知物品";
    },
    [catalog],
  );

  const renderBadges = (it: Record<string, unknown>) => (
    <>
      {asBool(it.is_legacy) ? <span className="cs-item-badge cs-badge-legacy">传家</span> : null}
      {asBool(it.soul_bound) ? <span className="cs-item-badge cs-badge-soul">魂绑</span> : null}
      {asBool(it.pinned) ? <span className="cs-item-badge cs-badge-pinned">珍藏</span> : null}
    </>
  );

  const equipSlots = Object.keys(equipment);

  return (
    <div className="cs-section">
      <div className="cs-kv cs-wallet-row">
        <span className="cs-kv-k">钱袋</span>
        <span className="cs-kv-v cs-wallet-v">{wallet} G</span>
      </div>

      <div className="cs-slot-title">装备</div>
      {equipSlots.length === 0 ? (
        <div className="cs-empty-inline">她身上空无长物。</div>
      ) : (
        <div className="cs-equip-grid">
          {equipSlots.map((slot) => {
            const it = asRecord(equipment[slot]);
            const lvl = asNum(it.level);
            return (
              <div className="cs-equip-cell" key={slot}>
                <span className="cs-equip-slot">{slotLabel(slot)}</span>
                <span className="cs-equip-name">
                  {itemName(it)}
                  {lvl > 0 ? <span className="cs-equip-lvl"> +{lvl}</span> : null}
                  {renderBadges(it)}
                </span>
                {/* 玩家在线操作：卸下该槽位装备回行囊（A11）。slot 键本身即后端 unequip 契约
                    字符串（weapon|armor|shoes|accessory）；item_id 为空视作空槽不给按钮。
                    行囊满时后端拒绝且装备保持原样，alert 中文兜底。 */}
                {onUnequip && asStr(it.item_id) ? (
                  <button
                    type="button"
                    className="cs-equip-btn"
                    // cs-equip-cell 是横向 flex（gap 8px），按钮收尾靠右；清掉 class 自带的 margin-left 免得间距叠成 16px。
                    style={{ flex: "0 0 auto", marginLeft: 0 }}
                    disabled={unequipBusy === slot}
                    onClick={() => onUnequip(slot)}
                  >
                    {unequipBusy === slot ? "…" : "卸下"}
                  </button>
                ) : null}
              </div>
            );
          })}
        </div>
      )}

      <div className="cs-slot-title">行囊</div>
      {backpack.length === 0 ? (
        <div className="cs-empty-inline">行囊空空。</div>
      ) : (
        <ul className="cs-backpack">
          {backpack.map((raw, i) => {
            const it = asRecord(raw);
            const qty = asNum(it.quantity, 1);
            const lvl = asNum(it.level);
            const itemID = asStr(it.item_id);
            // 可直接使用的消耗品（后端只开 ration/healing_potion 两条结算）：显示「使用」；
            // 消耗品穿不上（后端必拒「这件东西不是用来吃的」），故这两件不再显示死按钮「穿上」。
            const usable = USABLE_ITEM_IDS.has(itemID);
            return (
              <li className="cs-backpack-row" key={`${itemID}-${i}`}>
                <span className="cs-backpack-name">
                  {itemName(it)}
                  {lvl > 0 ? <span className="cs-equip-lvl"> +{lvl}</span> : null}
                  {renderBadges(it)}
                </span>
                <span className="cs-backpack-qty">×{qty}</span>
                {/* 玩家在线操作：使用消耗品（A10，吃口粮/喝药，成功后数值与数量就地刷新）。 */}
                {usable && onUse ? (
                  <button
                    type="button"
                    className="cs-equip-btn"
                    disabled={useBusy === itemID}
                    onClick={() => onUse(itemID)}
                  >
                    {useBusy === itemID ? "…" : "使用"}
                  </button>
                ) : null}
                {/* 玩家在线操作：从行囊把这件穿上（非装备类后端会拒，alert 兜底）。 */}
                {!usable && onEquip ? (
                  <button
                    type="button"
                    className="cs-equip-btn"
                    disabled={equipBusy === itemID}
                    onClick={() => onEquip(itemID)}
                  >
                    {equipBusy === itemID ? "…" : "穿上"}
                  </button>
                ) : null}
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}

// ── 关系 tab ──
function RelationsTab(props: {
  loading: boolean;
  error: string;
  relations: UnitRelationView[] | null;
}) {
  const { loading, error, relations } = props;

  // 按四轴绝对值和降序排（与后端口径一致，仍前端兜一次保证强度排序）。
  const sorted = useMemo(() => {
    const rows = relations ?? [];
    return [...rows].sort(
      (a, b) =>
        Math.abs(b.trust) + Math.abs(b.fear) + Math.abs(b.affection) + Math.abs(b.rivalry) -
        (Math.abs(a.trust) + Math.abs(a.fear) + Math.abs(a.affection) + Math.abs(a.rivalry)),
    );
  }, [relations]);

  if (loading) return <div className="cs-empty">正在打听她身边的人…</div>;
  if (error) return <div className="cs-notice">读取关系失败：{error}</div>;
  if (sorted.length === 0) return <div className="cs-empty">她身边，还没有结下深浅的人。</div>;

  return (
    <div className="cs-section">
      {sorted.map((r) => (
        <div className="cs-rel-card" key={r.target_unit_id}>
          <div className="cs-rel-name">{r.target_name || "无名之人"}</div>
          <div className="cs-rel-axes">
            <RelAxis label="信任" value={r.trust} />
            <RelAxis label="亲昵" value={r.affection} />
            <RelAxis label="忌惮" value={r.fear} />
            <RelAxis label="仇怨" value={r.rivalry} />
          </div>
        </div>
      ))}
    </div>
  );
}

// RelAxis 单条四轴可视化：以中线为 0，正向右暖色、负向左冷色（clamp[-10,10]）。
function RelAxis(props: { label: string; value: number }) {
  const { label, value } = props;
  const clamped = Math.max(-10, Math.min(10, value));
  const pct = (Math.abs(clamped) / 10) * 50; // 半轴宽 50%
  const positive = clamped >= 0;
  return (
    <div className="cs-rel-axis">
      <span className="cs-rel-axis-label">{label}</span>
      <span className="cs-rel-axis-track">
        <span className="cs-rel-axis-mid" />
        <span
          className={`cs-rel-axis-fill ${positive ? "cs-rel-pos" : "cs-rel-neg"}`}
          style={positive ? { left: "50%", width: `${pct}%` } : { right: "50%", width: `${pct}%` }}
        />
      </span>
      <span className="cs-rel-axis-num">{clamped}</span>
    </div>
  );
}

// CsBar 进度条（气血/饥饿）。
function CsBar(props: { label: string; value: number; max: number; variant: "hp" | "hunger" }) {
  const { label, value, max, variant } = props;
  const w = Math.max(0, Math.min(1, max > 0 ? value / max : 0)) * 100;
  return (
    <div className="cs-bar">
      <span className="cs-bar-label">{label}</span>
      <span className="cs-bar-track">
        <span className={`cs-bar-fill cs-bar-${variant}`} style={{ width: `${w}%` }} />
      </span>
      <span className="cs-bar-num">{value}</span>
    </div>
  );
}

// ── 浮层定位（仿 CharterEditor：右侧抽屉式，墨色宣纸调以本组件 className 走 fate.css，定位仍内联） ──
const panelStyle: React.CSSProperties = {
  position: "absolute",
  top: 64,
  right: 12,
  width: 380,
  maxWidth: "94vw",
  maxHeight: "calc(100vh - 96px)",
  overflowY: "auto",
  zIndex: zIndex.rightPanel,
  background: "rgba(245, 236, 220, 0.98)",
  border: "1px solid rgba(140, 100, 50, 0.4)",
  borderRadius: 12,
  boxShadow: "0 10px 30px rgba(60, 44, 27, 0.3)",
  color: "#4a3417",
  padding: 14,
  fontFamily: '"Noto Serif SC", "Songti SC", serif',
  fontSize: 13,
};
const headerStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "flex-start",
  justifyContent: "space-between",
  marginBottom: 8,
};
const brandStyle: React.CSSProperties = { color: "#6b4a22", fontWeight: 700, fontSize: 18 };
const nickStyle: React.CSSProperties = { color: "#97825f", fontSize: 13, marginLeft: 6 };
const subStyle: React.CSSProperties = { color: "#97825f", fontSize: 11, marginTop: 3 };
const closeBtnStyle: React.CSSProperties = {
  cursor: "pointer",
  background: "transparent",
  border: "none",
  color: "#97825f",
  fontSize: 20,
  lineHeight: 1,
};

export default CharacterSheet;
