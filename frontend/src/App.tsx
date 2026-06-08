/* 文件说明：前端主页面容器，负责会话初始化、双人房间流程、指令操作区与战场视图联动。 */

import { Suspense, lazy, useEffect, useMemo, useRef, useState } from "react";
import {
  APIError,
  advancePhase,
  confirmOpeningDraft,
  createDuelSession,
  createSinglePlayerSession,
  getCurrentAccount,
  getSession,
  joinDuelByRoomCodeWithRole,
  listTerrainCatalog,
  loginAccount,
  logoutAccount,
  registerAccount,
  resolveEliteEncounter,
  resolveFieldBoss,
  setAccountToken,
  setImmediateOrder,
  setSessionRoleToken,
  setGlobalDirective,
  setTaskDirective,
  subscribeSessionStream,
  talkToUnit,
  trackFunnel,
  emitClientAnalytics,
} from "./session/api";
import type { BattleMapSizeID, EliteEncounterResult } from "./session/api";
import type { FieldBossResult } from "./session/types";
import { FatePanel } from "./components/FatePanel";
import GovernancePanel, { ReportDialog, PrivacyEraseDialog } from "./components/GovernancePanel";
import BillingPanel from "./components/BillingPanel";
import ComplianceGatePanel, { ComplianceBlockedBanner } from "./components/ComplianceGate";
import ConsentInbox from "./components/ConsentInbox";
import WorldBossPanel from "./components/WorldBossPanel";
import { BloodFeudPanel } from "./components/BloodFeudPanel";
import { OpsDashboard } from "./components/OpsDashboard";
import DungeonPanel from "./components/DungeonPanel";
import { DefianceCard, hasDefianceTrace, parseDefianceTrace, stripDefianceTrace } from "./components/DefianceCard";
import type {
  CompletionAttempt,
  AccountUser,
  BattleUnit,
  DecisionTrace,
  DialogueMessage,
  DuelRoomStatus,
  LLMInteraction,
  Outcome,
  Phase,
  SessionLog,
  SessionSnapshot,
  TerrainDefinition,
} from "./session/types";

type LoadState = "idle" | "loading" | "ready" | "error";
type StartMode = "landing" | "single" | "multiplayer";
type AccountFormMode = "login" | "register";
type ExecutionFeedEntry = {
  id: string;
  turn: number;
  phase: Phase | "";
  unitID: string;
  unitName: string;
  reason?: "execution_unit_started" | "execution_action_completed" | "execution_unit_completed";
  status: "started" | "completed";
  startedUnits?: number;
  completedUnits?: number;
  totalUnits?: number;
  aiText?: string;
};
type ActivityFeedItem = {
  id: string;
  turn: number;
  phase: Phase | "";
  tone: "started" | "completed" | "event";
  text: string;
};
type UnitItemGainToast = {
  id: string;
  unitID: string;
  unitName: string;
  resource: "item" | "gold";
  direction: "gain" | "loss";
  itemLabel: string;
  quantity: number;
  createdAt: number;
};
type UnitDialogueThreadLine = {
  id: string;
  unitID: string;
  speaker: string;
  message: string;
  turn: number;
  phase: Phase;
  occurredAt: string;
};
type UnitDialogueThread = {
  id: string;
  turn: number;
  phase: Phase;
  occurredAt: string;
  actorUnitID: string;
  targetUnitID: string;
  participantUnitIDs: string[];
  summary: string;
  lines: UnitDialogueThreadLine[];
};
type UnitDialogueModalPair = {
  unitID: string;
  partnerUnitID: string;
};
type GameGuideSection = {
  title: string;
  body: string[];
  tips?: string[];
};
type BoardExecutionMarker = Pick<
  ExecutionFeedEntry,
  "unitID" | "status" | "turn" | "startedUnits" | "completedUnits" | "totalUnits"
>;
type FloatingPanelID =
  | "overview"
  | "command"
  | "chat"
  | "unit"
  | "inventory"
  | "thoughts"
  | "roster"
  | "structures"
  | "combat"
  | "dialogues"
  | "battleReport"
  | "rawEvent"
  | "llmTrace";

const phaseLabels: Record<Phase, string> = {
  deployment: "部署阶段",
  execution: "执行阶段",
};

const outcomeLabels: Record<Outcome, string> = {
  ongoing: "进行中",
  victory: "胜利",
  defeat: "失败",
  draw: "平局",
};

const personalityTraitLabels: Array<{ key: keyof BattleUnit["personality"]; label: string }> = [
  { key: "courage", label: "勇气" },
  { key: "loyalty", label: "忠诚" },
  { key: "aggression", label: "进攻" },
  { key: "prudence", label: "谨慎" },
  { key: "sociability", label: "社交" },
  { key: "integrity", label: "正直" },
  { key: "stability", label: "稳定" },
  { key: "ambition", label: "野心" },
];
const primaryStatLabels: Array<{ key: "strength" | "dexterity" | "constitution" | "wisdom" | "perception" | "charisma"; label: string }> = [
  { key: "strength", label: "力量" },
  { key: "dexterity", label: "敏捷" },
  { key: "constitution", label: "体质" },
  { key: "wisdom", label: "智慧" },
  { key: "perception", label: "感知" },
  { key: "charisma", label: "魅力" },
];
const derivedStatLabels: Array<{ key: "attack" | "defense" | "accuracy" | "evasion" | "vision" | "carry_weight"; label: string }> = [
  { key: "attack", label: "攻击" },
  { key: "defense", label: "防御" },
  { key: "accuracy", label: "命中" },
  { key: "evasion", label: "闪避" },
  { key: "vision", label: "视野" },
  { key: "carry_weight", label: "负重" },
];
const duelResumeStorageKey = "qunxiang.duel.resume.v1";
const accountAuthStorageKey = "qunxiang.account.auth.v1";
const hudVisibilityStorageKey = "qunxiang.map.hud.visible.v1";
const deploymentIntroSkipStorageKey = "qunxiang.deployment.intro.skip.v1";
const developerModeStorageKey = "qunxiang.developer.mode.v1";
// returnVisitStorageKey 标记「这台浏览器之前来过」——存在即视为回访（return_visit 漏斗）。
const returnVisitStorageKey = "qunxiang.last.visit.v1";
const gameGuideSections: GameGuideSection[] = [
  {
    title: "这是一个怎样的世界",
    body: [
      "《群像》发生在一片被城镇、村庄、森林、河谷、废墟和荒地切开的六边形战场。每个单位都有姓名、性格、属性、背包、装备、饥饿、记忆和人际关系。",
      "你不是逐个点击技能的传统指挥官，而是给阵营发布自然语言方针；单位会结合地形、敌我距离、装备、伤势、饥饿、性格、记忆和最近对话，自主选择行动。",
      "胜利目标通常是击败全部敌方单位；失败条件是己方全灭。战斗中会留下尸体、墓碑、掉落物、战报和单位自己的记忆。",
    ],
  },
  {
    title: "一回合怎么玩",
    body: [
      "每回合分为“部署阶段”和“执行阶段”。部署阶段适合写总方针、点名任务、和单位聊天、确认装备与背包；执行阶段单位会按行动点和候选动作依次自主行动。",
      "部署阶段点击左侧“方针”，写一句你希望全队遵守的战略，例如：稳住正面，优先集火落单敌人；或先采集食物，避免饥饿归零。",
      "确认后点击“执行/开始执行”。执行阶段不要频繁打断，观察头顶气泡、活动流、AI事件和战报，下一回合再修正方针。",
    ],
    tips: ["方针越具体越好：谁优先、做什么、避开什么。", "如果局势混乱，先用“稳住、抱团、防御、观察”比盲目冲锋安全。"],
  },
  {
    title: "自然语言指挥技巧",
    body: [
      "总方针影响全队，例如“弓手保持距离，近战保护伤员，优先攻击最近的敌方输出”。",
      "点名任务适合给单个单位更明确的目标，例如“望舒向北靠近森林，别离队超过两格”。",
      "交谈可以改变单位理解、补充局势、安抚恐惧，也能让单位记住你的意图。单位不是绝对傀儡，胆怯、重伤、饥饿或关系变化都可能影响执行。",
    ],
    tips: ["避免只写“随便打”，模型会缺少优先级。", "可以写条件式命令：若血量低于一半就撤退，否则协助集火。"],
  },
  {
    title: "探索性原则",
    body: [
      "单位是独立个体，不是玩家的鼠标指针。他们有自己的意识、性格、好奇心、风险判断和关系记忆。",
      "方针很重要，是单位理解阵营意图的强信号；但单位仍会结合自身性格、近期记忆、亲疏关系、附近局势、背包装备和生存风险做判断。",
      "在不违背生存、关键目标与自身判断的前提下，单位会倾向于主动尝试不同可能性：探索新地形、观察未知机会、尝试可用候选动作、推进人际关系，并把结果写入记忆或世界规律。",
      "当生存、战斗和探索压力下降后，单位可以尽量发展伴侣关系、结婚叙事并共同养育孩子；这仍取决于双方性格、记忆、关系和当场同意。",
      "如果你希望单位保守行动，就在方针里明确写“不要探索、不要离队、优先固守/补给/撤退”；如果你希望他们主动发现世界，就写“侦察、探路、尝试新地块、记录发现”。",
    ],
    tips: ["探索不会覆盖生存规则：饥饿、重伤、贴身威胁仍会优先处理。", "探索也不会允许单位自造非法动作，只会在合法候选动作里选择。"],
  },
  {
    title: "地图、地形与视野",
    body: [
      "地图由六边形地块组成，不同地形影响移动、视野和可执行事项。森林适合隐蔽与采集，河流和山地会限制推进，村庄和城市更适合据守或修建铁匠铺。",
      "迷雾开启时，只能看到己方视野内的敌人、设施和事件。单位的感知、地形视野、天气和位置都会影响侦察结果。",
      "双击地块或点击单位卡可以查看地块详情：地形规则、建筑、墓碑、地面遗落物和可拾取资源。",
    ],
  },
  {
    title: "地块产出",
    body: [
      "平原 plains：可开垦农田；可建陷阱、炮台、瞭望塔；农田成熟后收口粮 2。",
      "森林 forest：可采集木材 2、药草 1、口粮 1；有武器可打猎，随机获得口粮或皮革；可建陷阱。",
      "山地 mountain：可挖铁矿 1、石料 1，也可采石料/药草；可建铁匠铺、陷阱、炮台、瞭望塔。",
      "河流 river：可钓鱼获得口粮 1；不适合建造主要设施。",
      "河谷 river_valley：可钓鱼获得口粮 2；可开垦农田，成熟后收口粮 3；可建陷阱、瞭望塔。",
      "草原 grassland：有武器可打猎，随机获得口粮或皮革；可建陷阱、瞭望塔。",
      "雪原 snowfield：有武器可打猎，皮革概率更高；可建陷阱。",
      "沼泽 swamp：可采集药草 2；可建陷阱。",
      "废墟 ruins：可采集木材 1、口粮 1；可建铁匠铺、陷阱、炮台、瞭望塔。",
      "村庄 village：可建铁匠铺、陷阱、炮台；不会自动产商品，交易必须通过相邻单位 trade。",
      "城市 city：可建铁匠铺；不会自动产商品，交易必须通过相邻单位 trade。",
      "沙漠 desert：当前没有稳定采集或建造收益，不适合补给。",
      "道路 road：适合移动和布防；可建陷阱、炮台、瞭望塔；当前无采集产出。",
    ],
    tips: ["采集、建造、锻造、强化都需要 2 AP；只有 1 AP 时，站在关键地块上通常应该等下回合再动。", "村庄和城市不会自动产出商品；交易必须通过相邻单位的 trade 候选完成。"],
  },
  {
    title: "建筑功能",
    body: [
      "当前可建建筑只有农田、铁匠铺、陷阱、炮台和瞭望塔；没有小屋、房子、营地或婚房建筑。单位可以谈未来，但真正执行时只能选择这些合法建筑。",
      "农田 farmland：建在平原或河谷，无材料消耗，施工 1 回合；成熟后可收口粮，平原产 2，河谷产 3，适合长期补给。",
      "铁匠铺 forge：建在城市、村庄、废墟或山地，需要木材 2、石料 2、铁矿 1，施工 2 回合；完工后驻守 ATK+4/DEF+3，并支持锻造和强化装备。",
      "陷阱 trap：不能建在河流、沙漠、城市，需要木材 1，施工 1 回合；敌方踩中会受伤并停顿，适合封路和保护后排。",
      "炮台 turret：建在平原、山地、道路、废墟或村庄，需要木材 2、铁矿 1，施工 2 回合；驻守者 ATK+8，攻击距离至少 3 格，适合守点和远程压制。",
      "瞭望塔 watchtower：建在平原、草原、山地、道路、废墟或河谷，需要木材 2，施工 1 回合；驻守者 ATK+2，攻击距离至少 2 格，并改善侦察视野。",
    ],
    tips: ["想升级装备时，常见链路是先采木材/石料/铁矿，再去合法地块建铁匠铺，完工后 forge 或 upgrade。", "建筑和材料都归具体单位与地块，不是全队共享。"],
  },
  {
    title: "单位、属性与行动",
    body: [
      "单位有 HP、攻击、防御、移动、命中、闪避、视野、负重、金币和饥饿。力量、敏捷、体质、智慧、感知、魅力会间接影响战斗和事件表现。",
      "常见行动包括移动、防御、观察、普通攻击、重击、冲锋、技能、援助、交谈、交易、采集、建造、锻造、装备、进食、拾取和拆除。",
      "行动会消耗 AP。不是每个动作每回合都可用：必须满足距离、地形、装备、背包、设施、目标可见和行动点等条件。",
    ],
    tips: ["观察能提高后续攻击稳定性；防御能降低受伤风险。", "冲锋和重击更激进，适合收割或打开局面，但不要让脆弱单位孤军深入。"],
  },
  {
    title: "生存、物资与长期运营",
    body: [
      "饥饿会随回合推进消耗，归零会造成严重后果甚至死亡。食物、药剂和补给要及时使用或分配。",
      "背包和装备会影响战斗表现。武器、护甲、道具、金币可以通过交易、拾取、生产、战斗掉落和设施互动流动。",
      "道路、农田、铁匠铺、陷阱、炮台和瞭望塔会改变局势：有的提供补给，有的支持锻造和强化，有的改善侦察或防线。",
    ],
  },
  {
    title: "对话与贸易",
    body: [
      "部署阶段可以点名和单位对话，用自然语言说明交易意图，例如“靠近敌方后先喊话，尝试用金币示好”或“把多余口粮交给前排”。这些对话会进入单位记忆，影响后续自主判断。",
      "执行阶段的直接交易只会在双方相邻时成为合法 trade 候选；敌方可见但不相邻时，可以先让单位 move 靠近，或用 say/dialogue 先表达交易意图。",
      "当前显式 trade 支持三类：把自己的物品赠送给目标、向目标调拨金币、把自己的物品卖给目标。敌我双方可以交易，但目标单位会单独判断是否接受，敌意、风险、补给不足或不信任都可能导致拒绝。",
      "面对敌方或关系紧张的目标，可以先给一点好处再谈交易：相邻后先赠送低价值物品或少量金币示好，改善关系、降低戒备，再尝试出售物品或推进后续交易。",
      "如果你想让单位与敌方交易，方针要同时写清目标和方式，例如“红泥搬山向东靠近纸甲阿梨，相邻后先提出赠送 10 金示好；若不能交易就先喊话沟通”。",
    ],
    tips: ["没有 trade 候选时，AI 不能凭空交易，只能先移动或交谈。", "赠送金币或物品不保证对方一定接受后续交易，但会给关系和信任创造更好的条件。", "材料和金币在具体单位身上，不是全队共享；交易是把资源转到正确单位身上的主要方式。"],
  },
  {
    title: "恋爱、伴侣与孩子",
    body: [
      "单位互相交谈、关系升温后，执行阶段可能出现 romance 候选；选择后还要由双方 LLM 再次判断是否真心接受，接受后会互为伴侣。",
      "互为伴侣后，至少要到下一回合，双方仍相邻且 family 候选出现时，单位才能商量共同养育孩子；双方再次同意后会进入 5 回合孕期，到期才会生成孩子单位。",
      "怀孕中的单位不能参与战斗和建筑，应该优先保命、移动、进食、交谈或交易。",
      "孩子会作为新单位加入战场，单位界面会显示伴侣、父母和小孩关系。若你想推进这条线，可以点名写清“先交流培养关系，相邻后表白；已是伴侣后再共同养育孩子”。",
    ],
    tips: ["玩家方针不能强制恋爱或生育，只能提高单位考虑这件事的权重。", "同回合刚确认伴侣时不会立刻生孩子，至少需要等到下一回合。"],
  },
  {
    title: "装备锻造与强化",
    body: [
      "单位不是手动点装备按钮，而是在执行阶段从合法候选动作中自行选择 forge、upgrade 或 equip。你可以在方针里写“先整备装备、优先强化主武器、材料够就去铁匠铺强化”。",
      "强化装备需要同时满足：单位站在己方已完工铁匠铺上；目标装备已经穿戴或在背包中；背包材料足够；本回合还有足够 AP。只有这些条件都满足时，AI 决策候选里才会出现 upgrade。",
      "每次 upgrade 只提升 1 级。升级到 +N 会消耗铁矿 N + 石料 N；护甲和鞋履还要皮革 N；饰品要宝石 max(1,N-1)；远程武器、弓、弩还要木材 N。材料不足时不会出现强化候选。",
      "强化收益：武器每级攻击 +4、防御 +1；护甲每级防御 +3；鞋履每级防御 +1、每 2 级移动 +1；饰品每级攻击 +1、防御 +2。通常进攻方针优先强化主武器，防守/保命方针优先护甲或鞋履。",
    ],
    tips: ["如果单位不主动强化，先确认它是否站在己方已完工铁匠铺上，并且材料在该单位自己的背包里。", "材料和金币不全局共享，需要通过相邻交易或击杀继承流到具体单位身上。"],
  },
  {
    title: "战斗阅读与复盘",
    body: [
      "右侧浮窗可以查看概览、指令、聊天、单位、背包、情报、编组、设施、AI事件、交谈、战报、事件流和设置。",
      "活动流会显示执行阶段的关键事件；战报适合回合后复盘；事件流更底层，适合排查“为什么单位这样做”。",
      "单位会记录记忆，包括谁造成伤害、用了什么武器、移动到哪里、发现了什么、谁倒下以及死因。长期战斗中旧记忆会被压缩成 memory2，避免后期遗忘关键历史。",
    ],
    tips: ["看不懂局势时，先打开“战报”和“AI事件”。", "如果单位行为不符合预期，下回合把方针写得更具体，并点名关键单位。"],
  },
];

// LazyPixiBoard 懒加载 Pixi 战场，降低首屏包体与初始化成本。
const LazyPixiBoard = lazy(() =>
  import("./game/PixiBoard").then((module) => ({
    default: module.PixiBoard,
  })),
);

type GameSelectOption = {
  value: string;
  label: string;
  disabled?: boolean;
};

type GameSelectProps = {
  value: string;
  options: GameSelectOption[];
  onChange: (value: string) => void;
  disabled?: boolean;
  ariaLabel?: string;
  className?: string;
};

// GameSelect 用自定义弹层替换浏览器原生 select，统一游戏内 HUD/面板风格。
function GameSelect({ value, options, onChange, disabled = false, ariaLabel, className = "" }: GameSelectProps) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement | null>(null);
  const selectedOption = options.find((option) => option.value === value) ?? options[0];
  const handleSelect = (option: GameSelectOption) => {
    if (option.disabled) {
      return;
    }
    setOpen(false);
    onChange(option.value);
  };

  useEffect(() => {
    if (!open) {
      return;
    }
    const handlePointerDown = (event: PointerEvent) => {
      if (!rootRef.current?.contains(event.target as Node)) {
        setOpen(false);
      }
    };
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setOpen(false);
      }
    };
    window.addEventListener("pointerdown", handlePointerDown);
    window.addEventListener("keydown", handleKeyDown);
    return () => {
      window.removeEventListener("pointerdown", handlePointerDown);
      window.removeEventListener("keydown", handleKeyDown);
    };
  }, [open]);

  useEffect(() => {
    if (disabled) {
      setOpen(false);
    }
  }, [disabled]);

  return (
    <div
      ref={rootRef}
      className={`game-select ${open ? "game-select-open" : ""} ${disabled ? "game-select-disabled" : ""} ${className}`.trim()}
      onClick={(event) => event.stopPropagation()}
    >
      <button
        type="button"
        className="game-select-trigger"
        disabled={disabled}
        onClick={() => setOpen((current) => !current)}
        onKeyDown={(event) => {
          if (event.key === "ArrowDown" || event.key === "Enter" || event.key === " ") {
            event.preventDefault();
            setOpen(true);
          }
        }}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-label={ariaLabel}
      >
        <span className="game-select-current">{selectedOption?.label ?? "请选择"}</span>
        <span className="game-select-caret" aria-hidden="true">▾</span>
      </button>
      {open ? (
        <div className="game-select-menu" role="listbox" aria-label={ariaLabel}>
          {options.map((option) => {
            const active = option.value === value;
            return (
              <button
                key={option.value}
                type="button"
                className={`game-select-option ${active ? "game-select-option-active" : ""}`}
                disabled={option.disabled}
                onPointerDown={(event) => {
                  if (option.disabled) {
                    return;
                  }
                  event.preventDefault();
                  event.stopPropagation();
                  handleSelect(option);
                }}
                onClick={() => handleSelect(option)}
                role="option"
                aria-selected={active}
              >
                <span className="game-select-option-mark">✓</span>
                <span>{option.label}</span>
              </button>
            );
          })}
        </div>
      ) : null}
    </div>
  );
}

// App 是前端主容器：负责会话订阅、指令面板与战场联动渲染。
export function App() {
  const [session, setSession] = useState<SessionSnapshot | null>(null);
  // sessionRef 用于在 SSE 回调里读取最新快照，避免闭包拿到旧状态。
  const sessionRef = useRef<SessionSnapshot | null>(null);
  const [loadState, setLoadState] = useState<LoadState>("idle");
  const [startMode, setStartMode] = useState<StartMode>("landing");
  const [selectedTileCoord, setSelectedTileCoord] = useState<{ q: number; r: number } | null>(null);
  const [lastClickedCoord, setLastClickedCoord] = useState<{ q: number; r: number; time: number } | null>(null);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("请选择单人模式或多人模式。");
  const [accountUser, setAccountUser] = useState<AccountUser | null>(null);
  const [accountAuthToken, setAccountAuthToken] = useState(() => readAccountAuthFromStorage()?.token ?? "");
  const [accountFormMode, setAccountFormMode] = useState<AccountFormMode>("login");
  const [accountUsername, setAccountUsername] = useState("");
  const [accountDisplayName, setAccountDisplayName] = useState("");
  const [accountPassword, setAccountPassword] = useState("");
  const [directiveDraft, setDirectiveDraft] = useState("");
  const [taskDraft, setTaskDraft] = useState("");
  const [orderDraft, setOrderDraft] = useState("");
  const [taskTargetUnitID, setTaskTargetUnitID] = useState("");
  const [orderTargetUnitID, setOrderTargetUnitID] = useState("");
  const [deploymentTaskModalOpen, setDeploymentTaskModalOpen] = useState(false);
  const [deploymentDoctrineConfirmed, setDeploymentDoctrineConfirmed] = useState(false);
  const [deploymentIntroSkip, setDeploymentIntroSkip] = useState<boolean>(() =>
    readDeploymentIntroSkipFromStorage(),
  );
  const deploymentIntroShownKeyRef = useRef("");
  const [unitDetailPopoverOpen, setUnitDetailPopoverOpen] = useState(false);
  const [tileDetailPopoverOpen, setTileDetailPopoverOpen] = useState(false);
  // 命运四槽面板：开关 + 聚焦单位（默认跟随当前选中单位）。
  const [fatePanelOpen, setFatePanelOpen] = useState(false);
  // elite/PvE 遭遇：进行中的单位与最近一次结果（用于展示在事件流/弹层）。
  const [eliteEncounterBusyUnitID, setEliteEncounterBusyUnitID] = useState("");
  const [eliteEncounterResult, setEliteEncounterResult] = useState<EliteEncounterResult | null>(null);
  // 组队 PvE（野外 Boss）：选中的队员、进行中标志与最近一次结果。
  const [fieldBossSelectionIDs, setFieldBossSelectionIDs] = useState<string[]>([]);
  const [fieldBossBusy, setFieldBossBusy] = useState(false);
  const [fieldBossResult, setFieldBossResult] = useState<FieldBossResult | null>(null);
  const [fieldBossModalOpen, setFieldBossModalOpen] = useState(false);
  // 玩家侧治理：举报弹窗（不受 developer 门，所有玩家可见）。
  const [reportDialogOpen, setReportDialogOpen] = useState(false);
  // 运营/dev 治理台 + 隐私擦除（developer 门控）。
  const [governancePanelOpen, setGovernancePanelOpen] = useState(false);
  const [privacyEraseOpen, setPrivacyEraseOpen] = useState(false);
  // 商业化/合规浮层（玩家可见，常驻入口触发）。
  const [billingPanelOpen, setBillingPanelOpen] = useState(false);
  const [compliancePanelOpen, setCompliancePanelOpen] = useState(false);
  // 合规 403 拦截横幅：被门拦时存后端 reason，渲染 ComplianceBlockedBanner 引导实名/告知宵禁/防沉迷。
  const [complianceBlockReason, setComplianceBlockReason] = useState<string | null>(null);
  // 跨玩家同意收件箱浮层（针对当前选中角色）。
  const [consentInboxOpen, setConsentInboxOpen] = useState(false);
  // 血仇网络面板（针对当前选中角色，玩家可见——让 blood_feud 传播可感知）。
  const [bloodFeudOpen, setBloodFeudOpen] = useState(false);
  // 世界 Boss 协作面板（需本局已接入世界 world_id；developer 门控的进阶玩法）。
  const [worldBossOpen, setWorldBossOpen] = useState(false);
  // 运营看板（cost-dashboard + leads-funnel，developer 门控）。
  const [opsDashboardOpen, setOpsDashboardOpen] = useState(false);
  // 副本面板（多层 PvE，QUNXIANG_DUNGEON 默认关时后端报错→面板提示未启用）。
  const [dungeonOpen, setDungeonOpen] = useState(false);
  const [dialogueDraft, setDialogueDraft] = useState("");
  const [latestDialogueReply, setLatestDialogueReply] = useState("");
  const [terrainCatalog, setTerrainCatalog] = useState<TerrainDefinition[]>([]);
  const [commanderFactionID, setCommanderFactionID] = useState<string>("player");
  const [duelRoomCode, setDuelRoomCode] = useState("");
  const [duelJoinRoomCode, setDuelJoinRoomCode] = useState("");
  const [duelJoinPreferredRole, setDuelJoinPreferredRole] = useState<"player" | "enemy">("enemy");
  const [duelCreatorRole, setDuelCreatorRole] = useState<"player" | "enemy">("player");
  const [openingUnitCount, setOpeningUnitCount] = useState(3);
  const [battleMapSize, setBattleMapSize] = useState<BattleMapSizeID>("small");
  const [fogOfWarEnabled, setFogOfWarEnabled] = useState(false);
  const [randomEventsEnabled, setRandomEventsEnabled] = useState(false);
  const [duelJoinSessionID, setDuelJoinSessionID] = useState("");
  const [duelJoinRoleToken, setDuelJoinRoleToken] = useState("");
  const [duelRoomStatus, setDuelRoomStatus] = useState<DuelRoomStatus | null>(null);
  const [showHUD, setShowHUD] = useState<boolean>(() => readHUDVisibilityFromStorage());
  const [developerMode, setDeveloperMode] = useState<boolean>(() => readDeveloperModeFromStorage());
  const [showShortcutHelp, setShowShortcutHelp] = useState(false);
  const [gameGuideOpen, setGameGuideOpen] = useState(false);
  const [hallArchiveModalOpen, setHallArchiveModalOpen] = useState(false);
  const shownHallArchiveSessionRef = useRef("");
  const [activePanelID, setActivePanelID] = useState<FloatingPanelID | null>(null);
  const [chatTargetUnitID, setChatTargetUnitID] = useState("");
  const [unitDialogueModalPair, setUnitDialogueModalPair] = useState<UnitDialogueModalPair | null>(null);
  const [executionFeed, setExecutionFeed] = useState<ExecutionFeedEntry[]>([]);
  const [itemGainToasts, setItemGainToasts] = useState<UnitItemGainToast[]>([]);
  const previousInventorySnapshotRef = useRef<Map<string, number> | null>(null);
  const previousInventorySessionIDRef = useRef("");
  const [mapZoom, setMapZoom] = useState(1);
  const [fogVisionMode, setFogVisionMode] = useState("merged");
  const autoAdvancedPhaseKeyRef = useRef("");
  const phaseTransitionPollTimerRef = useRef<number | undefined>();
  // returnVisitTrackedRef 守卫 return_visit 漏斗每次启动至多上报一条（防 StrictMode 双挂载/重渲重复）。
  const returnVisitTrackedRef = useRef(false);
  const [nowMs, setNowMs] = useState(() => Date.now());
  const [openingDraftUnits, setOpeningDraftUnits] = useState<BattleUnit[]>([]);
  const [openingDraftSelectedIDs, setOpeningDraftSelectedIDs] = useState<string[]>([]);
  const [openingDraftSecondsLeft, setOpeningDraftSecondsLeft] = useState(60);
  const openingDraftDirtyFieldsRef = useRef<Map<string, Set<"name" | "biography" | "gender" | "portrait_url">>>(new Map());
  const duelRoomReady = session?.mode !== "duel" || Boolean(duelRoomStatus?.player_joined && duelRoomStatus?.enemy_joined);
  const duelWaitingForOpponent = Boolean(session && session.mode === "duel" && !duelRoomReady);

  const applySessionSnapshot = (nextSession: SessionSnapshot) => {
    sessionRef.current = nextSession;
    setSession(nextSession);
  };

  const stopPhaseTransitionPolling = () => {
    if (phaseTransitionPollTimerRef.current !== undefined) {
      window.clearInterval(phaseTransitionPollTimerRef.current);
      phaseTransitionPollTimerRef.current = undefined;
    }
  };

  const startPhaseTransitionPolling = (baseline: SessionSnapshot) => {
    stopPhaseTransitionPolling();
    const startedAt = Date.now();
    let inFlight = false;
    phaseTransitionPollTimerRef.current = window.setInterval(async () => {
      if (inFlight) {
        return;
      }
      if (Date.now() - startedAt > 90_000) {
        stopPhaseTransitionPolling();
        return;
      }
      const current = sessionRef.current;
      if (!current || current.id !== baseline.id) {
        stopPhaseTransitionPolling();
        return;
      }

      inFlight = true;
      try {
        const latest = await getSession(baseline.id);
        const nextSession = latest.session;
        const active = sessionRef.current;
        if (!active || active.id !== baseline.id) {
          stopPhaseTransitionPolling();
          return;
        }
        if (!isSnapshotOlder(nextSession, active) && !isSnapshotEquivalent(nextSession, active)) {
          applySessionSnapshot(nextSession);
        }
        const phaseChanged =
          nextSession.turn_state.turn !== baseline.turn_state.turn ||
          nextSession.turn_state.phase !== baseline.turn_state.phase ||
          nextSession.execution_in_progress !== baseline.execution_in_progress;
        if (phaseChanged) {
          stopPhaseTransitionPolling();
        }
      } catch {
        // 阶段切换中的短轮询只做兜底；下一轮或 websocket 仍可继续同步。
      } finally {
        inFlight = false;
      }
    }, 1000);
  };

  useEffect(() => {
    const target = window as Window & {
      qxdev?: () => string;
      qxnodev?: () => string;
    };
    target.qxdev = () => {
      setDeveloperMode(true);
      writeDeveloperModeToStorage(true);
      return "Qunxiang developer mode enabled.";
    };
    target.qxnodev = () => {
      setDeveloperMode(false);
      writeDeveloperModeToStorage(false);
      return "Qunxiang developer mode disabled.";
    };
    return () => {
      delete target.qxdev;
      delete target.qxnodev;
    };
  }, []);

  // return_visit：启动时若本浏览器存在上次访问标记，则视为回访上报一条漏斗（once 守卫，每次启动至多一条）；
  // 随后写入本次访问标记。全程 best-effort（吞 localStorage/网络错），绝不影响首屏与 UX。
  useEffect(() => {
    if (returnVisitTrackedRef.current) {
      return;
    }
    returnVisitTrackedRef.current = true;
    let returning = false;
    try {
      returning = (window.localStorage.getItem(returnVisitStorageKey) ?? "") !== "";
    } catch {
      // localStorage 不可用（隐私模式等）：当作首次访问，不上报。
    }
    if (returning) {
      void trackFunnel("return_visit");
    }
    try {
      window.localStorage.setItem(returnVisitStorageKey, String(Date.now()));
    } catch {
      // 忽略持久化失败——下次启动仍按首次处理，至多漏一条回访。
    }
  }, []);

  useEffect(() => {
    if (session?.setup_phase !== "drafting") {
      setOpeningDraftUnits([]);
      setOpeningDraftSelectedIDs([]);
      openingDraftDirtyFieldsRef.current = new Map();
      return;
    }
    const pool = session.player_draft_pool ?? [];
    setOpeningDraftUnits((current) => mergeDraftUnitsPreservingLocalEdits(current, pool, openingDraftDirtyFieldsRef.current));
    setOpeningDraftSelectedIDs((current) => {
      const valid = current.filter((id) => pool.some((unit) => unit.id === id));
      if (valid.length > 0) {
        return valid.slice(0, session.draft_required_pick ?? 10);
      }
      return pool.slice(0, session.draft_required_pick ?? 10).map((unit) => unit.id);
    });
  }, [session?.id, session?.setup_phase, session?.player_draft_pool, session?.draft_required_pick]);

  useEffect(() => {
    if (session?.setup_phase !== "drafting" || !session.setup_deadline_at) {
      return;
    }
    const update = () => {
      const left = Math.max(0, Math.ceil((new Date(session.setup_deadline_at ?? "").getTime() - Date.now()) / 1000));
      setOpeningDraftSecondsLeft(left);
    };
    update();
    const timer = window.setInterval(update, 250);
    return () => window.clearInterval(timer);
  }, [session?.setup_phase, session?.setup_deadline_at]);

  useEffect(() => {
    if (session?.setup_phase !== "drafting" || busy || openingDraftSecondsLeft > 0) {
      return;
    }
    void handleConfirmOpeningDraft();
  }, [session?.setup_phase, busy, openingDraftSecondsLeft]);

  useEffect(() => {
    if (!session || session.outcome === "ongoing") {
      return;
    }
    if ((session.hall_archive_entries ?? []).length === 0) {
      return;
    }
    if (shownHallArchiveSessionRef.current === session.id) {
      return;
    }
    shownHallArchiveSessionRef.current = session.id;
    setHallArchiveModalOpen(true);
  }, [session?.id, session?.outcome, session?.hall_archive_entries?.length]);

  useEffect(() => {
    let cancelled = false;

    async function bootstrap() {
      setLoadState("loading");
      setMessage("正在检查是否需要恢复房间。");

      try {
        const search = new URLSearchParams(window.location.search);
        const linkedSessionID = search.get("session_id")?.trim() ?? "";
        const linkedRoleToken = search.get("role_token")?.trim() ?? "";
        const linkedRoomCode = normalizeRoomCodeInput(search.get("room_code") ?? "");
        const localResume = readDuelResumeFromStorage();

        if (linkedSessionID !== "" && linkedRoleToken !== "") {
          setSessionRoleToken(linkedRoleToken);
          const linked = await getSession(linkedSessionID);
          if (cancelled) {
            return;
          }

          const nextSession = linked.session;
          const nextCommanderFactionID = linked.commander_faction_id?.trim() || nextSession.player_faction_id;
          const controlled = controlledUnitsByFaction(nextSession, nextCommanderFactionID);
          const firstUnitID = controlled[0]?.id ?? "";

          setSession(nextSession);
          setCommanderFactionID(nextCommanderFactionID);
          if (controlled[0]) { setSelectedTileCoord({ q: controlled[0].status.position_q, r: controlled[0].status.position_r }); } else { setSelectedTileCoord(null); }
          setTaskTargetUnitID(firstUnitID);
          setOrderTargetUnitID(firstUnitID);
          setDirectiveDraft(factionDoctrineDraft(nextSession, nextCommanderFactionID));
          const restoredRoomCode = normalizeRoomCodeInput(linked.room_code ?? "");
          setDuelRoomCode(restoredRoomCode);
          setDuelJoinRoomCode(restoredRoomCode);
          setDuelJoinPreferredRole(
            nextCommanderFactionID === nextSession.player_faction_id ? "player" : "enemy",
          );
          setDuelJoinSessionID(linkedSessionID);
          setDuelJoinRoleToken(linkedRoleToken);
          setDuelRoomStatus(normalizeDuelRoomStatus(linked.room_status));
          writeDuelResumeToStorage({
            session_id: linkedSessionID,
            role_token: linkedRoleToken,
            room_code: restoredRoomCode,
            preferred_role: nextCommanderFactionID === nextSession.player_faction_id ? "player" : "enemy",
          });
          setLoadState("ready");
          setMessage(
            nextCommanderFactionID === nextSession.player_faction_id
              ? "已加入双人对局（我方=己方阵营）。"
              : "已加入双人对局（我方=敌方阵营）。",
          );
          return;
        }

        if (linkedRoomCode !== "") {
          const linkedRoomRoleRaw = search.get("room_role")?.trim().toLowerCase() ?? "";
          const linkedRoomRole: "player" | "enemy" =
            linkedRoomRoleRaw === "player" ? "player" : "enemy";
          const joined = await joinDuelByRoomCodeWithRole(linkedRoomCode, linkedRoomRole);
          if (cancelled) {
            return;
          }
          const nextSession = joined.session;
          const nextCommanderFactionID = joined.commander_faction_id?.trim() || nextSession.player_faction_id;
          const joinedRoleToken = joined.role_token.trim();
          const controlled = controlledUnitsByFaction(nextSession, nextCommanderFactionID);
          const firstUnitID = controlled[0]?.id ?? "";

          setSessionRoleToken(joinedRoleToken);
          setSession(nextSession);
          setCommanderFactionID(nextCommanderFactionID);
          if (controlled[0]) { setSelectedTileCoord({ q: controlled[0].status.position_q, r: controlled[0].status.position_r }); } else { setSelectedTileCoord(null); }
          setTaskTargetUnitID(firstUnitID);
          setOrderTargetUnitID(firstUnitID);
          setDirectiveDraft(factionDoctrineDraft(nextSession, nextCommanderFactionID));
          const joinedRoomCode = normalizeRoomCodeInput(joined.room_code ?? linkedRoomCode);
          setDuelRoomCode(joinedRoomCode);
          setDuelJoinRoomCode(joinedRoomCode);
          setDuelJoinPreferredRole(linkedRoomRole);
          setDuelJoinSessionID(nextSession.id);
          setDuelJoinRoleToken(joinedRoleToken);
          setDuelRoomStatus(normalizeDuelRoomStatus(joined.room_status));
          writeDuelResumeToStorage({
            session_id: nextSession.id,
            role_token: joinedRoleToken,
            room_code: joinedRoomCode,
            preferred_role: linkedRoomRole,
          });

          const selfLink = `${window.location.pathname}?session_id=${encodeURIComponent(
            nextSession.id,
          )}&role_token=${encodeURIComponent(joinedRoleToken)}`;
          window.history.replaceState(null, "", selfLink);
          setLoadState("ready");
          setMessage("已通过房间号加入双人对局。");
          return;
        }

        if (localResume?.session_id && localResume.role_token) {
          setStartMode("multiplayer");
          setDuelJoinSessionID(localResume.session_id);
          setDuelJoinRoleToken(localResume.role_token);
          setDuelJoinRoomCode(normalizeRoomCodeInput(localResume.room_code ?? ""));
          setDuelJoinPreferredRole(
            localResume.preferred_role === "player" || localResume.preferred_role === "enemy"
              ? localResume.preferred_role
              : "enemy",
          );
          setLoadState("idle");
          setMessage("检测到本地双人房记录，可点击“恢复房间”重进。也可以选择单人模式或重新创建房间。");
          return;
        }

        setSessionRoleToken("");
        setLoadState("idle");
        setMessage("请选择单人模式或多人模式。多人模式需先注册/登录账号，再创建房间或恢复房间。");
      } catch (error) {
        if (cancelled) {
          return;
        }

        setLoadState("error");
        setMessage(getErrorMessage(error, "建局失败"));
      }
    }

    void bootstrap();

    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    const token = accountAuthToken.trim();
    if (!token) {
      // 登出/无 token：同步清掉 api.ts 模块级 Bearer，billing/compliance 不再误带空 token。
      setAccountToken("");
      setAccountUser(null);
      return;
    }

    // 关键：把恢复/登录得到的 token 同步进 api.ts 模块级 Bearer，
    // 否则刷新后恢复登录态时 billing/compliance 的强制 Bearer 端点会发空 token 被 401。
    setAccountToken(token);

    let cancelled = false;
    void getCurrentAccount(token)
      .then((user) => {
        if (cancelled) {
          return;
        }
        setAccountUser(user);
        writeAccountAuthToStorage({ token });
      })
      .catch(() => {
        if (cancelled) {
          return;
        }
        setAccountToken("");
        setAccountUser(null);
        setAccountAuthToken("");
        clearAccountAuthFromStorage();
      });

    return () => {
      cancelled = true;
    };
  }, [accountAuthToken]);

  useEffect(() => {
    let cancelled = false;
    void listTerrainCatalog()
      .then((catalog) => {
        if (!cancelled) {
          setTerrainCatalog(catalog);
        }
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    sessionRef.current = session;
  }, [session]);

  useEffect(() => () => stopPhaseTransitionPolling(), []);

  useEffect(() => {
    if (!session?.id || activePanelID !== "llmTrace") {
      return;
    }

    let cancelled = false;
    let inFlight = false;
    const refreshActiveLLMCalls = async () => {
      if (inFlight) {
        return;
      }
      inFlight = true;
      try {
        const response = await getSession(session.id);
        if (cancelled) {
          return;
        }
        const current = sessionRef.current;
        if (current?.id === session.id) {
          applySessionSnapshot(response.session);
          setDuelRoomStatus(normalizeDuelRoomStatus(response.room_status));
        }
      } catch {
        // 调试轮询失败不打断正在执行的对局，后续 websocket/轮询会继续刷新。
      } finally {
        inFlight = false;
      }
    };

    void refreshActiveLLMCalls();
    const timer = window.setInterval(() => void refreshActiveLLMCalls(), 1500);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [session?.id, activePanelID]);

  useEffect(() => {
    if (!session) {
      previousInventorySnapshotRef.current = null;
      previousInventorySessionIDRef.current = "";
      setItemGainToasts([]);
      return;
    }

    const nextInventory = buildInventoryQuantitySnapshot(session);
    const previousInventory = previousInventorySnapshotRef.current;
    const previousSessionID = previousInventorySessionIDRef.current;
    previousInventorySnapshotRef.current = nextInventory;
    previousInventorySessionIDRef.current = session.id;
    if (!previousInventory || previousSessionID !== session.id) {
      return;
    }

    const gainCommanderFactionID = commanderFactionID || session.player_faction_id;
    const visibleGainUnitIDs = inventoryGainToastVisibleUnitIDs(session, gainCommanderFactionID);
    const gains = detectInventoryChanges(session, previousInventory, nextInventory, visibleGainUnitIDs);
    if (gains.length === 0) {
      return;
    }

    const now = Date.now();
    setItemGainToasts((current) => [
      ...gains.map((gain, index) => ({
        ...gain,
        id: `${session.id}:${session.turn_state.turn}:${now}:${index}:${gain.unitID}:${gain.itemLabel}`,
        createdAt: now,
      })),
      ...current,
    ].slice(0, 4));
  }, [session, commanderFactionID]);

  useEffect(() => {
    if (itemGainToasts.length === 0) {
      return;
    }

    const timer = window.setTimeout(() => {
      const cutoff = Date.now() - 4200;
      setItemGainToasts((current) => current.filter((toast) => toast.createdAt > cutoff));
    }, 4200);
    return () => window.clearTimeout(timer);
  }, [itemGainToasts]);

  useEffect(() => {
    const timer = window.setInterval(() => setNowMs(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, []);

  useEffect(() => {
    if (!session) {
      return;
    }
    const controlledUnits = controlledUnitsByFaction(session, commanderFactionID);
    const defaultDirectiveDraft = factionDoctrineDraft(session, commanderFactionID);

    setDirectiveDraft((current) => (session.turn_state.phase === "deployment" && current.trim() === "" ? defaultDirectiveDraft : current));

    const hasTaskTarget = controlledUnits.some((unit) => unit.id === taskTargetUnitID);
    if (!hasTaskTarget) {
      setTaskTargetUnitID(controlledUnits[0]?.id ?? "");
    }
    const hasOrderTarget = controlledUnits.some((unit) => unit.id === orderTargetUnitID);
    if (!hasOrderTarget) {
      setOrderTargetUnitID(controlledUnits[0]?.id ?? "");
    }
    if (chatTargetUnitID && ![...session.player_units, ...session.enemy_units, ...(session.wild_units ?? [])].some((unit) => unit.id === chatTargetUnitID)) {
      setChatTargetUnitID("");
    }

    // 取消自动选中第一个单位，如果有记录位置则不重置
    // 改由用户主动点击地块
  }, [session, commanderFactionID, taskTargetUnitID, orderTargetUnitID, chatTargetUnitID]);

  useEffect(() => {
    const sessionID = session?.id;
    if (!sessionID) {
      return;
    }

    const unsubscribe = subscribeSessionStream(sessionID, {
      onSnapshot: (nextSession, meta) => {
        const normalizedMeta = meta ?? {};
        const nextRoomStatus = normalizeDuelRoomStatus(normalizedMeta.room_status);
        if (nextRoomStatus) {
          setDuelRoomStatus(nextRoomStatus);
        }
        // 执行流事件优先入侧栏，保证单位“开始/完成”即时可见。
        const executionEntry = parseExecutionFeedEntry(normalizedMeta, nextSession);
        if (executionEntry) {
          setExecutionFeed((current) => [executionEntry, ...current].slice(0, 24));
        }
        const progressMessage = formatExecutionProgressMessage(
          normalizedMeta,
          nextSession,
          commanderFactionID || nextSession.player_faction_id,
        );
        if (progressMessage) {
          setMessage(progressMessage);
        }
        const current = sessionRef.current;
        if (!current || current.id !== nextSession.id) {
          applySessionSnapshot(nextSession);
          return;
        }
        if (isSnapshotOlder(nextSession, current)) {
          return;
        }
        // 执行阶段逐单位事件强制刷新，避免被等价签名误判后“整轮才更新”。
        const reason = typeof normalizedMeta.reason === "string" ? normalizedMeta.reason : "";
        const isExecutionProgressEvent =
          reason === "execution_unit_started" ||
          reason === "execution_action_completed" ||
          reason === "execution_unit_completed";
        if (isExecutionProgressEvent || !isSnapshotEquivalent(nextSession, current)) {
          applySessionSnapshot(nextSession);
        }
      },
      onError: () => undefined,
    });

    return () => {
      unsubscribe();
    };
  }, [commanderFactionID, session?.id]);

  useEffect(() => {
    if (!session?.id || session.turn_state.phase !== "execution") {
      return;
    }

    let cancelled = false;
    let inFlight = false;
    const refreshExecutionSnapshot = async () => {
      if (inFlight) {
        return;
      }
      inFlight = true;
      try {
        const response = await getSession(session.id);
        if (cancelled) {
          return;
        }
        const nextSession = response.session;
        const current = sessionRef.current;
        if (!current || current.id !== nextSession.id) {
          applySessionSnapshot(nextSession);
          return;
        }
        if (!isSnapshotOlder(nextSession, current) && !isSnapshotEquivalent(nextSession, current)) {
          applySessionSnapshot(nextSession);
        }
      } catch {
        // WebSocket 仍是主链路；轮询只是执行阶段兜底，短暂失败不打断操作。
      } finally {
        inFlight = false;
      }
    };

    const timer = window.setInterval(() => void refreshExecutionSnapshot(), 5000);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [session?.id, session?.turn_state.phase]);

  useEffect(() => {
    if (!session?.id || !duelWaitingForOpponent) {
      return;
    }
    let cancelled = false;
    let inFlight = false;
    const refreshRoomStatus = async () => {
      if (inFlight) {
        return;
      }
      inFlight = true;
      try {
        const response = await getSession(session.id);
        if (cancelled) {
          return;
        }
        setDuelRoomStatus(normalizeDuelRoomStatus(response.room_status));
        if (response.session.id === session.id) {
          setSession(response.session);
        }
      } catch {
        // 等待页只需要持续刷新加入状态；短暂失败不打断邀请流程。
      } finally {
        inFlight = false;
      }
    };
    void refreshRoomStatus();
    const timer = window.setInterval(() => void refreshRoomStatus(), 2000);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [session?.id, duelWaitingForOpponent]);

  useEffect(() => {
    if (!session) {
      setExecutionFeed([]);
      return;
    }
    if (session.turn_state.phase !== "execution") {
      setExecutionFeed([]);
      return;
    }
    setExecutionFeed((current) => current.filter((entry) => entry.turn === session.turn_state.turn));
  }, [session?.id, session?.turn_state.turn, session?.turn_state.phase]);

  useEffect(() => {
    if (!session || session.outcome !== "ongoing") {
      return;
    }
    if (session.turn_state.phase !== "deployment" && session.turn_state.phase !== "execution") {
      // 离开部署/执行阶段时自动关闭弹窗。
      setDeploymentTaskModalOpen(false);
      return;
    }
    if (session.turn_state.phase === "execution") {
      return;
    }
    if (deploymentIntroSkip) {
      return;
    }
    const introKey = `${session.id}:${session.turn_state.turn}`;
    if (deploymentIntroShownKeyRef.current === introKey) {
      return;
    }
    deploymentIntroShownKeyRef.current = introKey;
    setDeploymentTaskModalOpen(true);
  }, [
    session?.id,
    session?.turn_state.turn,
    session?.turn_state.phase,
    session?.outcome,
    deploymentIntroSkip,
  ]);

  useEffect(() => {
    setDeploymentDoctrineConfirmed(false);
  }, [session?.id, session?.turn_state.turn, session?.turn_state.phase, commanderFactionID]);

  useEffect(() => {
    if (!session || session.outcome !== "ongoing" || session.execution_in_progress || busy || duelWaitingForOpponent) {
      return;
    }
    const phase = session.turn_state.phase;
    if (phase !== "deployment") {
      return;
    }

    const deadline = new Date(session.turn_state.phase_ends_at).getTime();
    if (!Number.isFinite(deadline)) {
      return;
    }
    const autoKey = `${session.id}:${session.turn_state.turn}:${session.turn_state.phase}:${session.turn_state.phase_ends_at}`;
    if (autoAdvancedPhaseKeyRef.current === autoKey) {
      return;
    }

    const delayMs = Math.max(0, deadline - Date.now()) + 250;
    const timer = window.setTimeout(() => {
      const current = sessionRef.current;
      if (
        !current ||
        current.id !== session.id ||
        current.turn_state.turn !== session.turn_state.turn ||
        current.turn_state.phase !== phase ||
        current.turn_state.phase_ends_at !== session.turn_state.phase_ends_at ||
        current.outcome !== "ongoing" ||
        current.execution_in_progress
      ) {
        return;
      }
      autoAdvancedPhaseKeyRef.current = autoKey;
      setMessage("部署阶段倒计时结束，正在自动进入执行阶段。");
      void handleAdvancePhase();
    }, delayMs);

    return () => window.clearTimeout(timer);
  }, [
    session?.id,
    session?.turn_state.turn,
    session?.turn_state.phase,
    session?.turn_state.phase_ends_at,
    session?.outcome,
    session?.execution_in_progress,
    directiveDraft,
    commanderFactionID,
    busy,
    duelWaitingForOpponent,
  ]);

  const effectiveCommanderFactionID = session
    ? commanderFactionID || session.player_faction_id
    : commanderFactionID || "player";
  const controlledUnits = useMemo(
    () => (session ? controlledUnitsByFaction(session, effectiveCommanderFactionID) : []),
    [session, effectiveCommanderFactionID],
  );
  const displayExecutionFeed = useMemo(
    () => sanitizeExecutionFeedForCommander(executionFeed, session, effectiveCommanderFactionID),
    [executionFeed, session, effectiveCommanderFactionID],
  );
  const boardExecutionMarkers = useMemo<BoardExecutionMarker[]>(() => {
    if (!session) {
      return [];
    }
    return displayExecutionFeed.filter((entry) => entry.turn === session.turn_state.turn).slice(0, 24);
  }, [displayExecutionFeed, session]);
  const fogVisionUnits = useMemo(
    () => controlledUnits.filter((unit) => unit.status.life_state === "active"),
    [controlledUnits],
  );
  useEffect(() => {
    if (fogVisionMode === "merged") {
      return;
    }
    if (!fogVisionUnits.some((unit) => unit.id === fogVisionMode)) {
      setFogVisionMode("merged");
    }
  }, [fogVisionMode, fogVisionUnits]);
  const fogPerspectiveUnitID = useMemo(() => {
    if (!session || !session.fog_of_war_enabled) {
      return "";
    }
    if (fogVisionMode === "merged") {
      return "";
    }
    return fogVisionUnits.some((unit) => unit.id === fogVisionMode) ? fogVisionMode : "";
  }, [fogVisionMode, fogVisionUnits, session]);
  const fogVisionModeLabel = fogVisionMode === "merged"
    ? "合并视野"
    : fogVisionUnits.find((unit) => unit.id === fogVisionMode)?.identity.name ?? "合并视野";
  const visibleSession = useMemo(
    () => visibleSessionForCommander(session, effectiveCommanderFactionID, fogPerspectiveUnitID),
    [session, effectiveCommanderFactionID, fogPerspectiveUnitID],
  );
  const allUnits = useMemo(
    () => (visibleSession ? [...visibleSession.player_units, ...visibleSession.enemy_units] : []),
    [visibleSession],
  );
  const currentFactionReady = session?.phase_ready?.[effectiveCommanderFactionID] ?? false;
  const currentFactionDoctrineReady = session?.turn_state.phase === "deployment"
    ? session.directive_history.some((directive) =>
      directive.turn === session.turn_state.turn &&
      directive.phase === session.turn_state.phase &&
      directive.kind === "doctrine" &&
      (directive.issued_by === effectiveCommanderFactionID || directive.applies_to === effectiveCommanderFactionID) &&
      directive.text.trim() !== "",
    )
    : true;
  const opponentFactionID = session
    ? effectiveCommanderFactionID === session.enemy_faction_id
      ? session.player_faction_id
      : session.enemy_faction_id
    : "";
  const singlePlayerEnemyDoctrineReady = session?.mode === "single_player" && session.turn_state.phase === "deployment"
    ? session.directive_history.some((directive) =>
      directive.turn === session.turn_state.turn &&
      directive.phase === session.turn_state.phase &&
      directive.kind === "doctrine" &&
      directive.applies_to === session.enemy_faction_id &&
      directive.text.trim() !== "",
    )
    : true;
  const opponentFactionReady = session?.mode === "duel" && opponentFactionID !== ""
    ? session.phase_ready?.[opponentFactionID] ?? false
    : singlePlayerEnemyDoctrineReady;
  const deploymentDeadlineReached = !!session && session.turn_state.phase === "deployment" && phaseDeadlineReachedForClient(session);
  const canRequestAdvancePhase = !!session &&
    !busy &&
    !session.execution_in_progress &&
    (session.outcome === "ongoing" || session.turn_state.phase !== "execution") &&
    (session.turn_state.phase !== "deployment" || deploymentDeadlineReached || (currentFactionDoctrineReady && opponentFactionReady));
  const advancePhaseButtonTitle = !session
    ? "等待会话加载"
    : busy
      ? "正在处理上一条指令，请稍候"
      : session.turn_state.phase === "deployment" && deploymentDeadlineReached
        ? "部署倒计时已结束，将沿用最近一条方针进入执行阶段"
        : session.turn_state.phase === "deployment" && !currentFactionDoctrineReady
        ? "请先保存本回合总方针"
        : session.turn_state.phase === "deployment" && !opponentFactionReady
          ? session.mode === "duel"
            ? "等待对方准备完成后才能开始执行"
            : "敌方部署方针尚未准备好，请稍后再开始执行"
          : session.turn_state.phase === "deployment"
            ? "开始执行阶段"
            : "进入下一回合";
  const opponentUnits = allUnits.filter((unit) => unit.faction_id !== effectiveCommanderFactionID);
  const isDuelJoined = duelJoinSessionID.trim() !== "" && duelJoinRoleToken.trim() !== "";
  const opponentJoinRole: "player" | "enemy" = session && effectiveCommanderFactionID === session.enemy_faction_id ? "player" : "enemy";
  const opponentJoinLink = useMemo(() => {
    const roomCode = normalizeRoomCodeInput(duelRoomCode);
    if (!roomCode) {
      return "";
    }
    return `${window.location.origin}${window.location.pathname}?room_code=${encodeURIComponent(
      roomCode,
    )}&room_role=${opponentJoinRole}`;
  }, [duelRoomCode, opponentJoinRole]);
  const selfResumeLink = useMemo(() => {
    const sessionID = duelJoinSessionID.trim();
    const roleToken = duelJoinRoleToken.trim();
    if (!sessionID || !roleToken) {
      return "";
    }
    return `${window.location.origin}${window.location.pathname}?session_id=${encodeURIComponent(
      sessionID,
    )}&role_token=${encodeURIComponent(roleToken)}`;
  }, [duelJoinSessionID, duelJoinRoleToken]);
  const selectedUnit = useMemo(() => {
    if (!selectedTileCoord || !visibleSession) return null;
    return allUnits.find((unit) => unit.status.position_q === selectedTileCoord.q && unit.status.position_r === selectedTileCoord.r) ?? null;
  }, [allUnits, selectedTileCoord, visibleSession]);
  const selectedUnitID = selectedUnit?.id ?? null;
  const selectedUnitFactionLabel = selectedUnit
    ? selectedUnit.faction_id === effectiveCommanderFactionID
      ? "我方"
      : "对方"
    : "";
  const selectedUnitRestrictedByFog = !!(
    session?.fog_of_war_enabled &&
    selectedUnit &&
    selectedUnit.faction_id !== effectiveCommanderFactionID
  );
  const selectedUnitPortraitURL = portraitURLForUnit(selectedUnit);
  const selectedUnitPortraitFallbackURL = portraitFallbackURLForUnit(selectedUnit);
  const phaseRemainingText = formatPhaseRemaining(session, nowMs);
  const recentAIEvents = useMemo(() => {
    if (!session) {
      return [];
    }
    return [...session.logs]
      .filter((entry) => aiTurnLineFromLog(entry) !== null)
      .slice(-16)
      .reverse();
  }, [session]);
  const liveActivityFeed = useMemo<ActivityFeedItem[]>(() => {
    if (!session) {
      return [];
    }
    const seen = new Set<string>();
    const items: ActivityFeedItem[] = [];

    for (const entry of displayExecutionFeed) {
      const text = formatExecutionFeedLine(entry).trim();
      if (!text) {
        continue;
      }
      const dedupeKey = `exec:${entry.turn}:${entry.unitID}:${entry.status}:${text}`;
      if (seen.has(dedupeKey)) {
        continue;
      }
      seen.add(dedupeKey);
      items.push({
        id: entry.id,
        turn: entry.turn,
        phase: entry.phase,
        tone: entry.status,
        text,
      });
      if (items.length >= 10) {
        return items;
      }
    }

    for (let index = session.logs.length - 1; index >= 0; index -= 1) {
      const log = session.logs[index];
      if (session.fog_of_war_enabled && !isLogVisibleForCommander(session, log, effectiveCommanderFactionID)) {
        continue;
      }
      const line = aiTurnLineFromLog(log);
      if (!line) {
        continue;
      }
      const message = line.text.trim();
      if (!message) {
        continue;
      }
      const unitID = lineTargetUnitIDs(log)[0];
      const prefix = unitID ? `${findUnitName(session, unitID)}：` : "";
      const text = `${prefix}${truncateAIBrief(message, 24)}`;
      const dedupeKey = `log:${log.turn}:${log.kind}:${unitID ?? ""}:${text}`;
      if (seen.has(dedupeKey)) {
        continue;
      }
      seen.add(dedupeKey);
      items.push({
        id: `log:${log.id}`,
        turn: log.turn,
        phase: log.phase,
        tone: "event",
        text,
      });
      if (items.length >= 10) {
        break;
      }
    }

    return items;
  }, [displayExecutionFeed, effectiveCommanderFactionID, session]);
  const crossFactionInteractions = session?.metrics?.cross_faction_interactions ?? 0;
  const llmCostUSD = session?.metrics?.llm_estimated_cost_usd ?? 0;
  const llmTotalTokens = session?.metrics?.llm_total_tokens ?? 0;
  const llmGuardrailActive = llmCostUSD >= 2.85;

  const selectedDialogue = useMemo(() => {
    if (!session || !selectedUnitID) {
      return [];
    }
    return session.dialogue_history.filter((entry) => entry.unit_id === selectedUnitID).slice(-8);
  }, [session, selectedUnitID]);
  const visibleUnitDialogueThreads = useMemo(() => {
    if (!session) {
      return [];
    }
    const threads = buildUnitDialogueThreads(session).reverse();
    if (!selectedUnitID) {
      return threads.slice(0, 18);
    }
    const relevant: UnitDialogueThread[] = [];
    const others: UnitDialogueThread[] = [];
    for (const thread of threads) {
      if (thread.participantUnitIDs.includes(selectedUnitID)) {
        relevant.push(thread);
      } else {
        others.push(thread);
      }
    }
    return [...relevant, ...others].slice(0, 18);
  }, [session, selectedUnitID]);
  const unitDialogueModalThreads = useMemo(() => {
    if (!session || !unitDialogueModalPair) {
      return [];
    }
    return buildUnitDialogueThreads(session)
      .filter(
        (thread) =>
          thread.participantUnitIDs.includes(unitDialogueModalPair.unitID) &&
          thread.participantUnitIDs.includes(unitDialogueModalPair.partnerUnitID),
      )
      .reverse();
  }, [session, unitDialogueModalPair]);
  const unitDialogueModalUnit = useMemo(() => {
    if (!session || !unitDialogueModalPair) {
      return null;
    }
    return allSnapshotUnits(session).find((unit) => unit.id === unitDialogueModalPair.unitID) ?? null;
  }, [session, unitDialogueModalPair]);
  const unitDialogueModalPartnerUnit = useMemo(() => {
    if (!session || !unitDialogueModalPair) {
      return null;
    }
    return allSnapshotUnits(session).find((unit) => unit.id === unitDialogueModalPair.partnerUnitID) ?? null;
  }, [session, unitDialogueModalPair]);

  const latestDecisions = useMemo(() => {
    const map = new Map<string, DecisionTrace>();
    for (const trace of session?.decision_traces ?? []) {
      map.set(trace.unit_id, trace);
    }
    return map;
  }, [session]);

  const latestInteractionsByUnit = useMemo(() => {
    const map = new Map<string, LLMInteraction>();
    for (const interaction of session?.llm_interactions ?? []) {
      map.set(interaction.unit_id, interaction);
    }
    return map;
  }, [session]);

  const latestDecisionInteractions = useMemo(() => {
    const map = new Map<string, LLMInteraction>();
    for (const interaction of session?.llm_interactions ?? []) {
      if (interaction.kind === "decision") {
        map.set(interaction.unit_id, interaction);
      }
    }
    return map;
  }, [session]);

  const bubbleLinesByUnit = useMemo(() => (session ? unitBubbleLinesByUnit(session) : new Map<string, string[]>()), [session]);
  const latestAITurnLines = useMemo(
    () => latestAITurnLineByUnit(session?.logs ?? [], session?.turn_state.turn ?? -1),
    [session],
  );

  const llmInteractionsForSettings = useMemo(
    () => [
      ...(session?.active_llm_calls ?? []),
      ...(session?.llm_interactions ?? []).slice().reverse(),
    ],
    [session],
  );
  const recentRawEvents = useMemo(
    () => [...(session?.raw_event_log ?? [])].slice(-24).reverse(),
    [session],
  );
  const recentBattleReports = useMemo(
    () => [...(session?.battle_reports ?? [])].slice(-4).reverse(),
    [session],
  );
  const hallArchiveEntries = useMemo(
    () => [...(session?.hall_archive_entries ?? [])],
    [session],
  );
  const recentStructures = useMemo(
    () =>
      [...(session?.structures ?? [])].sort((left, right) => {
        if (left.q !== right.q) {
          return left.q - right.q;
        }
        return left.r - right.r;
      }),
    [session],
  );

  const selectedDecision = selectedUnitID ? latestDecisions.get(selectedUnitID) ?? null : null;
  const selectedThought = selectedUnitID
    ? latestDecisionInteractions.get(selectedUnitID) ?? latestInteractionsByUnit.get(selectedUnitID) ?? null
    : null;
  const selectedBubbleLines = selectedUnitID ? bubbleLinesByUnit.get(selectedUnitID) ?? [] : [];
  const selectedTile = useMemo(() => {
    if (!visibleSession || !selectedTileCoord) {
      return null;
    }
    return (
      visibleSession.map.tiles.find(
        (tile) => tile.coord.q === selectedTileCoord.q && tile.coord.r === selectedTileCoord.r,
      ) ?? null
    );
  }, [visibleSession, selectedTileCoord]);
  const selectedTerrain = useMemo(() => {
    if (!selectedTile) {
      return null;
    }
    return terrainCatalog.find((entry) => entry.id === selectedTile.terrain) ?? null;
  }, [terrainCatalog, selectedTile]);
  const selectedStructure = useMemo(() => {
    if (!visibleSession || !selectedTileCoord) {
      return null;
    }
    return (
      visibleSession.structures.find(
        (structure) => structure.q === selectedTileCoord.q && structure.r === selectedTileCoord.r,
      ) ?? null
    );
  }, [visibleSession, selectedTileCoord]);
  const selectedGraveMarkers = useMemo(() => {
    if (!visibleSession || !selectedTileCoord) {
      return [];
    }
    return (visibleSession.grave_markers ?? []).filter(
      (marker) => marker.q === selectedTileCoord.q && marker.r === selectedTileCoord.r,
    );
  }, [visibleSession, selectedTileCoord]);
  const selectedGroundLootDrops = useMemo(() => {
    if (!visibleSession || !selectedTileCoord) {
      return [];
    }
    return (visibleSession.ground_loot_drops ?? []).filter(
      (drop) => drop.q === selectedTileCoord.q && drop.r === selectedTileCoord.r,
    );
  }, [visibleSession, selectedTileCoord]);
  const selectedUnitChangeTimeline = useMemo(
    () => buildUnitChangeTimeline(session, selectedUnitID),
    [session, selectedUnitID],
  );
  const selectedUnitStatusChanges = selectedUnitChangeTimeline.filter((entry) => entry.kind === "status").slice(0, 12);
  const selectedUnitInventoryChanges = selectedUnitChangeTimeline.filter((entry) => entry.kind === "inventory").slice(0, 12);
  const selectedUnitDecisionChanges = selectedUnitChangeTimeline.filter((entry) => entry.kind === "decision").slice(0, 8);
  const chatUnits = useMemo(
    () =>
      session
        ? allUnits.filter(
          (unit) => !session.fog_of_war_enabled || unit.faction_id === effectiveCommanderFactionID,
        )
        : [],
    [allUnits, effectiveCommanderFactionID, session],
  );
  const chatTargetUnit = useMemo(() => {
    if (!chatUnits.length) {
      return null;
    }
    const selectedChatUnit = selectedUnit ? chatUnits.find((unit) => unit.id === selectedUnit.id) ?? null : null;
    return chatUnits.find((unit) => unit.id === chatTargetUnitID) ?? selectedChatUnit ?? chatUnits[0] ?? null;
  }, [chatTargetUnitID, chatUnits, selectedUnit]);
  const canChatInCurrentPhase = session?.outcome === "ongoing" && session.turn_state.phase === "deployment";
  const chatTargetDead = chatTargetUnit?.status.life_state === "dead";
  const canSendChat = !!chatTargetUnit && !chatTargetDead && !busy && canChatInCurrentPhase;
  const chatMessages = useMemo(() => {
    if (!session || !chatTargetUnit) {
      return [];
    }
    return session.dialogue_history.filter((entry) => entry.unit_id === chatTargetUnit.id).slice(-18);
  }, [session, chatTargetUnit]);
  const floatingPanels = useMemo<Array<{ id: FloatingPanelID; label: string; short: string; hotkey: string }>>(
    () => [
      { id: "overview", label: "概览", short: "OV", hotkey: "1" },
      { id: "command", label: "指令", short: "CMD", hotkey: "2" },
      { id: "chat", label: "聊天", short: "CHAT", hotkey: "c" },
      { id: "unit", label: "单位", short: "UN", hotkey: "3" },
      { id: "inventory", label: "背包", short: "BAG", hotkey: "b" },
      { id: "thoughts", label: "情报", short: "INT", hotkey: "4" },
      { id: "roster", label: "编组", short: "RO", hotkey: "5" },
      { id: "structures", label: "设施", short: "ST", hotkey: "6" },
      { id: "combat", label: "AI事件", short: "LOG", hotkey: "7" },
      { id: "dialogues", label: "交谈", short: "DIA", hotkey: "d" },
      { id: "battleReport", label: "战报", short: "REP", hotkey: "8" },
      { id: "rawEvent", label: "事件流", short: "RAW", hotkey: "9" },
      { id: "llmTrace", label: "设置", short: "SET", hotkey: "0" },
    ],
    [],
  );
  const panelHotkeyMap = useMemo(() => {
    const map = new Map<string, FloatingPanelID>();
    for (const panel of floatingPanels) {
      map.set(panel.hotkey, panel.id);
    }
    return map;
  }, [floatingPanels]);

  useEffect(() => {
    const handleKeydown = (event: KeyboardEvent) => {
      const key = event.key.toLowerCase();
      if (key === "?") {
        if (isTypingTarget(event.target)) {
          return;
        }
        event.preventDefault();
        setShowShortcutHelp((current) => !current);
        return;
      }
      if (key === "h") {
        if (isTypingTarget(event.target)) {
          return;
        }
        event.preventDefault();
        setShowHUD((current) => !current);
        return;
      }
      if (key === "escape") {
        setShowShortcutHelp(false);
        setGameGuideOpen(false);
        setActivePanelID(null);
        setUnitDialogueModalPair(null);
        return;
      }
      if (event.ctrlKey || event.metaKey || event.altKey) {
        return;
      }
      if (isTypingTarget(event.target)) {
        return;
      }
      const targetPanelID = panelHotkeyMap.get(key);
      if (!targetPanelID) {
        return;
      }
      event.preventDefault();
      handleFloatingPanelToggle(targetPanelID);
    };
    window.addEventListener("keydown", handleKeydown);
    return () => window.removeEventListener("keydown", handleKeydown);
  }, [panelHotkeyMap, selectedUnit, session, allUnits, developerMode]);

  useEffect(() => {
    writeHUDVisibilityToStorage(showHUD);
  }, [showHUD]);

  useEffect(() => {
    writeDeveloperModeToStorage(developerMode);
  }, [developerMode]);

  function handleTileClick(q: number, r: number) {
    const now = Date.now();
    if (session?.fog_of_war_enabled && !fogVisibleCoordSet(session, effectiveCommanderFactionID, fogPerspectiveUnitID).has(coordKey(q, r))) {
      setSelectedTileCoord(null);
      setUnitDetailPopoverOpen(false);
      setTileDetailPopoverOpen(false);
      setLastClickedCoord(null);
      setMessage("有雾模式下不能查看视野外地块。请先选择能看见该区域的己方单位或移动侦察。");
      return;
    }
    const sameAsSelected = selectedTileCoord?.q === q && selectedTileCoord?.r === r;
    const sameAsLast = lastClickedCoord?.q === q && lastClickedCoord?.r === r;
    const isDoubleClick = sameAsLast && now - lastClickedCoord.time <= 420;
    const clickedUnit = allUnits.find((unit) => unit.status.position_q === q && unit.status.position_r === r) ?? null;
    const reopen = sameAsSelected || isDoubleClick;

    setSelectedTileCoord({ q, r });
    setLastClickedCoord({ q, r, time: now });

    if (clickedUnit) {
      const restrictedEnemy = session?.fog_of_war_enabled && clickedUnit.faction_id !== effectiveCommanderFactionID;
      // 有单位：双击/重选 → 打开单位详情（其中也含地形/设施）；首次单击仅选中。
      setTileDetailPopoverOpen(false);
      setUnitDetailPopoverOpen(restrictedEnemy ? false : reopen);
      if (restrictedEnemy && reopen) {
        setMessage("有雾模式下只能确认敌方位置，不能查看敌方单位面板、背包或情报。");
      }
      return;
    }
    // 空地块：单击即可打开地块详情，方便快速查看地形与建筑效果。
    setUnitDetailPopoverOpen(false);
    setTileDetailPopoverOpen(true);
  }

  function handleOpenUnitChat(unitID: string) {
    const unit = allUnits.find((entry) => entry.id === unitID);
    if (unit) {
      setSelectedTileCoord({ q: unit.status.position_q, r: unit.status.position_r });
      setUnitDetailPopoverOpen(false);
      setTileDetailPopoverOpen(false);
    }
    setChatTargetUnitID(unitID);
    setActivePanelID("chat");
  }

  function latestDialoguePartnerUnitID(unitID: string): string {
    if (!session || !unitID) {
      return "";
    }
    const latestThread = buildUnitDialogueThreads(session)
      .reverse()
      .find((thread) => thread.participantUnitIDs.includes(unitID));
    if (!latestThread) {
      return "";
    }
    return latestThread.participantUnitIDs.find((participantID) => participantID !== unitID) ?? "";
  }

  function handleOpenUnitDialogueModal(unitID: string, partnerUnitID = "") {
    const unit = allUnits.find((entry) => entry.id === unitID);
    if (unit) {
      setSelectedTileCoord({ q: unit.status.position_q, r: unit.status.position_r });
      setUnitDetailPopoverOpen(false);
      setTileDetailPopoverOpen(false);
    }
    const resolvedPartnerUnitID = partnerUnitID || latestDialoguePartnerUnitID(unitID);
    if (!resolvedPartnerUnitID) {
      setMessage("这个单位暂时没有可查看的单位间对话记录。");
      return;
    }
    setUnitDialogueModalPair({ unitID, partnerUnitID: resolvedPartnerUnitID });
  }

  function handleFloatingPanelToggle(panelID: FloatingPanelID) {
    if (!developerMode) {
      setMessage("开发者面板已锁定。打开浏览器控制台输入 qxdev() 后可使用右侧面板。");
      return;
    }
    if (panelID === "dialogues") {
      if (selectedUnit) {
        handleOpenUnitDialogueModal(selectedUnit.id);
      } else {
        setMessage("请先在地图上选择一个有单位间对话记录的单位。");
      }
      setActivePanelID(null);
      return;
    }
    setActivePanelID((current) => (current === panelID ? null : panelID));
  }

  // handleEliteEncounter 触发一次单人 elite/PvE 遭遇（真实动作：改 HP/士气/钱包并落命运收件箱卡）。
  async function handleEliteEncounter(unitID: string) {
    const activeSession = sessionRef.current;
    if (!activeSession || !unitID || eliteEncounterBusyUnitID) {
      return;
    }
    setEliteEncounterBusyUnitID(unitID);
    setMessage("她出门历练去了，前路未卜…");
    try {
      const result = await resolveEliteEncounter(activeSession.id, unitID);
      setEliteEncounterResult(result);
      const outcomeLabel =
        result.Outcome === "defeated" ? "全身而退" : result.Outcome === "fled" ? "且战且退" : "负伤而归";
      setMessage(`历练结束：${outcomeLabel}（${result.Rounds} 回合）。`);
      // 刷新一次快照，让 HP/钱包等结算落地反映到界面。
      try {
        const latest = await getSession(activeSession.id);
        if (latest.session.id === activeSession.id) {
          applySessionSnapshot(latest.session);
        }
      } catch {
        // 快照刷新失败不影响遭遇结果展示。
      }
    } catch (err) {
      setMessage(`历练未能成行：${err instanceof APIError ? err.message : String(err)}`);
    } finally {
      setEliteEncounterBusyUnitID("");
    }
  }

  // toggleFieldBossMember 在组队 PvE 选人列表里加/减一个队员。
  function toggleFieldBossMember(unitID: string) {
    setFieldBossSelectionIDs((current) =>
      current.includes(unitID) ? current.filter((id) => id !== unitID) : [...current, unitID],
    );
  }

  // handleFieldBoss 触发一次组队 PvE（野外 Boss）遭遇（真实动作：按贡献分赃/分级惩罚并落命运收件箱）。
  async function handleFieldBoss() {
    const activeSession = sessionRef.current;
    if (!activeSession || fieldBossBusy) {
      return;
    }
    const unitIDs = fieldBossSelectionIDs.filter((id) => id);
    if (unitIDs.length === 0) {
      setMessage("请先勾选要组队出战的队员。");
      return;
    }
    setFieldBossBusy(true);
    setMessage(`${unitIDs.length} 人结伴前去挑战野外强敌，胜负未卜…`);
    try {
      const result = await resolveFieldBoss(activeSession.id, unitIDs);
      setFieldBossResult(result);
      setMessage(result.Victory ? `组队告捷（${result.Rounds} 回合），按贡献分赃。` : `组队折戟（${result.Rounds} 回合），各自承担后果。`);
      void trackFunnel("field_boss_resolved", { source: result.Victory ? "victory" : "defeat" });
      // 刷新一次快照，让 HP/钱包等结算落地反映到界面。
      try {
        const latest = await getSession(activeSession.id);
        if (latest.session.id === activeSession.id) {
          applySessionSnapshot(latest.session);
        }
      } catch {
        // 快照刷新失败不影响遭遇结果展示。
      }
    } catch (err) {
      setMessage(`组队出战未成行：${err instanceof APIError ? err.message : String(err)}`);
    } finally {
      setFieldBossBusy(false);
    }
  }

  function handleReturnToMainMenu() {
    setSession(null);
    sessionRef.current = null;
    setLoadState("idle");
    setStartMode("landing");
    setActivePanelID(null);
    setSelectedTileCoord(null);
    setUnitDetailPopoverOpen(false);
    setTileDetailPopoverOpen(false);
    setDeploymentTaskModalOpen(false);
    setUnitDialogueModalPair(null);
    setExecutionFeed([]);
    window.history.replaceState(null, "", window.location.pathname);
    setMessage("已返回主菜单。多人房可用本地记录或恢复链接重新进入。");
  }

  async function handleStartSinglePlayer() {
    if (busy) {
      return;
    }
    setBusy(true);
    setLoadState("loading");
    setStartMode("single");
    setMessage("正在创建单人对局。");
    try {
      setSessionRoleToken("");
      clearDuelResumeFromStorage();
      const nextSession = await createSinglePlayerSession(Date.now(), openingUnitCount, battleMapSize, fogOfWarEnabled, randomEventsEnabled);
      const controlled = controlledUnitsByFaction(nextSession, nextSession.player_faction_id);
      const firstUnitID = controlled[0]?.id ?? "";
      setSession(nextSession);
      setCommanderFactionID(nextSession.player_faction_id);
      if (controlled[0]) { setSelectedTileCoord({ q: controlled[0].status.position_q, r: controlled[0].status.position_r }); } else { setSelectedTileCoord(null); }
      setTaskTargetUnitID(firstUnitID);
      setOrderTargetUnitID(firstUnitID);
      setDirectiveDraft(nextSession.global_directive.text);
      setTaskDraft("");
      setOrderDraft("");
      setDialogueDraft("");
      setDuelRoomCode("");
      setDuelJoinRoomCode("");
      setDuelJoinPreferredRole("enemy");
      setDuelJoinSessionID("");
      setDuelJoinRoleToken("");
      setDuelRoomStatus(null);
      setFieldBossSelectionIDs([]);
      window.history.replaceState(null, "", window.location.pathname);
      setLoadState("ready");
      setMessage(`单人战场已生成（${nextSession.fog_of_war_enabled ? "有雾" : "无雾"}，随机事件${nextSession.random_events_enabled ? "开启" : "关闭"}）。玩家只做自然语言指挥；进食、交易、采集、建造与战斗都由 AI 单位自己执行。`);
      // 漏斗埋点：建局成功是核心转化点（best-effort 吞错，绝不影响 UX）。
      void trackFunnel("single_player_session_started", { source: nextSession.fog_of_war_enabled ? "fog" : "clear" });
    } catch (error) {
      // 合规门 403：登录态被宵禁/未实名/防沉迷超限拦截——渲染拦截横幅引导，不当作普通报错。
      if (error instanceof APIError && error.status === 403) {
        setLoadState("idle");
        setStartMode("landing");
        setComplianceBlockReason(error.reason ?? "");
        setMessage("");
      } else {
        setLoadState("error");
        setMessage(getErrorMessage(error, "创建单人对局失败"));
      }
    } finally {
      setBusy(false);
    }
  }

  async function handleAccountSubmit() {
    if (busy) {
      return;
    }
    const username = accountUsername.trim();
    const password = accountPassword;
    if (!username || !password) {
      setMessage("请输入账号和密码。");
      return;
    }
    setBusy(true);
    setMessage(accountFormMode === "register" ? "正在注册并登录账号。" : "正在登录账号。");
    try {
      const response = accountFormMode === "register"
        ? await registerAccount({ username, display_name: accountDisplayName.trim(), password })
        : await loginAccount({ username, password });
      const token = response.auth.token.trim();
      setAccountUser(response.user);
      setAccountAuthToken(token);
      writeAccountAuthToStorage({ token });
      setAccountPassword("");
      setStartMode("multiplayer");
      setMessage(`已登录：${response.user.display_name || response.user.username}。现在可以创建房间、选择角色，或恢复房间。`);
    } catch (error) {
      setMessage(getErrorMessage(error, accountFormMode === "register" ? "注册失败" : "登录失败"));
    } finally {
      setBusy(false);
    }
  }

  async function handleAccountLogout() {
    const token = accountAuthToken.trim();
    setBusy(true);
    try {
      if (token) {
        await logoutAccount(token).catch(() => false);
      }
    } finally {
      setAccountUser(null);
      setAccountAuthToken("");
      clearAccountAuthFromStorage();
      setBusy(false);
      setMessage("已退出账号。多人模式需重新登录后才能创建房间。");
    }
  }

  async function handleRestart() {
    setBusy(true);
    setLoadState("loading");
    setMessage("正在重建战场。");

    try {
      setSessionRoleToken("");
      const nextSession = await createSinglePlayerSession(Date.now(), openingUnitCount, battleMapSize, fogOfWarEnabled, randomEventsEnabled);
      const controlled = controlledUnitsByFaction(nextSession, nextSession.player_faction_id);
      const firstUnitID = controlled[0]?.id ?? "";
      setSession(nextSession);
      setCommanderFactionID(nextSession.player_faction_id);
      if (controlled[0]) { setSelectedTileCoord({ q: controlled[0].status.position_q, r: controlled[0].status.position_r }); } else { setSelectedTileCoord(null); }
      setTaskTargetUnitID(firstUnitID);
      setOrderTargetUnitID(firstUnitID);
      setDirectiveDraft(nextSession.global_directive.text);
      setTaskDraft("");
      setOrderDraft("");
      setDialogueDraft("");
      setDuelRoomCode("");
      setDuelJoinRoomCode("");
      setDuelJoinPreferredRole("enemy");
      setDuelJoinSessionID("");
      setDuelJoinRoleToken("");
      clearDuelResumeFromStorage();
      window.history.replaceState(null, "", window.location.pathname);
      setLoadState("ready");
      setMessage(`已开启新的一局（${nextSession.fog_of_war_enabled ? "有雾" : "无雾"}，随机事件${nextSession.random_events_enabled ? "开启" : "关闭"}）。玩家仍然只负责自然语言指挥，其余动作全由 AI 单位自行处理。`);
    } catch (error) {
      setLoadState("error");
      setMessage(getErrorMessage(error, "重开失败"));
    } finally {
      setBusy(false);
    }
  }

  async function handleCreateDuel() {
    if (busy) {
      return;
    }
    if (!accountUser || !accountAuthToken.trim()) {
      setStartMode("multiplayer");
      setMessage("多人模式需要先注册/登录账号，然后才能创建房间。");
      return;
    }
    setBusy(true);
    setLoadState("loading");
    setStartMode("multiplayer");
    setMessage("正在创建多人房间。");
    try {
      const creatorRole = duelCreatorRole === "enemy" ? "enemy" : "player";
      const result = await createDuelSession(Date.now(), openingUnitCount, battleMapSize, fogOfWarEnabled, randomEventsEnabled, creatorRole);
      const nextSession = result.session;
      const playerRoleToken = result.player_role_token.trim();
      const enemyRoleToken = result.enemy_role_token.trim();
      const roomCode = result.room_code.trim();
      const activeRoleToken = creatorRole === "player" ? playerRoleToken : enemyRoleToken;
      const nextCommanderFactionID = result.commander_faction_id?.trim() || (creatorRole === "player" ? nextSession.player_faction_id : nextSession.enemy_faction_id);
      const controlled = controlledUnitsByFaction(nextSession, nextCommanderFactionID);
      const firstUnitID = controlled[0]?.id ?? "";

      setSessionRoleToken(activeRoleToken);
      setSession(nextSession);
      setCommanderFactionID(nextCommanderFactionID);
      if (controlled[0]) { setSelectedTileCoord({ q: controlled[0].status.position_q, r: controlled[0].status.position_r }); } else { setSelectedTileCoord(null); }
      setTaskTargetUnitID(firstUnitID);
      setOrderTargetUnitID(firstUnitID);
      setDirectiveDraft(factionDoctrineDraft(nextSession, nextCommanderFactionID));
      setTaskDraft("");
      setOrderDraft("");
      setDialogueDraft("");
      setDuelRoomCode(roomCode);
      setDuelJoinRoomCode(roomCode);
      setDuelJoinPreferredRole(creatorRole === "player" ? "enemy" : "player");
      setDuelJoinSessionID(nextSession.id);
      setDuelJoinRoleToken(activeRoleToken);
      setDuelRoomStatus(normalizeDuelRoomStatus(result.room_status) ?? {
        room_code: roomCode,
        player_joined: creatorRole === "player",
        enemy_joined: creatorRole === "enemy",
      });
      writeDuelResumeToStorage({
        session_id: nextSession.id,
        role_token: activeRoleToken,
        room_code: roomCode,
        preferred_role: creatorRole,
      });
      setLoadState("ready");

      const playerLink = `${window.location.pathname}?session_id=${encodeURIComponent(
        nextSession.id,
      )}&role_token=${encodeURIComponent(activeRoleToken)}`;
      window.history.replaceState(null, "", playerLink);
      setMessage(`多人房间已创建（房间号 ${roomCode}）。先把房间号或加入链接发给对手，对手进入后再开始游戏。`);
    } catch (error) {
      // 合规门 403：登录态被宵禁/未实名/防沉迷超限拦截——渲染拦截横幅引导，不当作普通报错。
      if (error instanceof APIError && error.status === 403) {
        setLoadState("idle");
        setComplianceBlockReason(error.reason ?? "");
        setMessage("");
      } else {
        setLoadState("error");
        setMessage(getErrorMessage(error, "创建双人房失败"));
      }
    } finally {
      setBusy(false);
    }
  }

  async function handleJoinDuelByInput() {
    if (busy || !duelJoinSessionID.trim() || !duelJoinRoleToken.trim()) {
      return;
    }
    setBusy(true);
    setLoadState("loading");
    setMessage("正在加入双人对局。");
    try {
      const sessionID = duelJoinSessionID.trim();
      const roleToken = duelJoinRoleToken.trim();
      setSessionRoleToken(roleToken);
      const response = await getSession(sessionID);
      const nextSession = response.session;
      const nextCommanderFactionID = response.commander_faction_id?.trim() || nextSession.player_faction_id;
      const controlled = controlledUnitsByFaction(nextSession, nextCommanderFactionID);
      const firstUnitID = controlled[0]?.id ?? "";

      setSession(nextSession);
      setCommanderFactionID(nextCommanderFactionID);
      if (controlled[0]) { setSelectedTileCoord({ q: controlled[0].status.position_q, r: controlled[0].status.position_r }); } else { setSelectedTileCoord(null); }
      setTaskTargetUnitID(firstUnitID);
      setOrderTargetUnitID(firstUnitID);
      setDirectiveDraft(factionDoctrineDraft(nextSession, nextCommanderFactionID));
      const roomCode = normalizeRoomCodeInput(response.room_code ?? "");
      setDuelRoomCode(roomCode);
      setDuelJoinRoomCode(roomCode);
      setDuelRoomStatus(normalizeDuelRoomStatus(response.room_status));
      setDuelJoinPreferredRole(
        nextCommanderFactionID === nextSession.player_faction_id ? "player" : "enemy",
      );
      writeDuelResumeToStorage({
        session_id: nextSession.id,
        role_token: roleToken,
        room_code: roomCode,
        preferred_role: nextCommanderFactionID === nextSession.player_faction_id ? "player" : "enemy",
      });
      const link = `${window.location.pathname}?session_id=${encodeURIComponent(
        nextSession.id,
      )}&role_token=${encodeURIComponent(roleToken)}`;
      window.history.replaceState(null, "", link);
      setLoadState("ready");
      setMessage("已加入双人对局。");
    } catch (error) {
      setLoadState("error");
      setMessage(getErrorMessage(error, "加入双人对局失败"));
    } finally {
      setBusy(false);
    }
  }

  async function handleJoinDuelByRoomCode() {
    const roomCode = normalizeRoomCodeInput(duelJoinRoomCode);
    if (busy || !roomCode) {
      return;
    }
    setBusy(true);
    setLoadState("loading");
    setMessage("正在通过房间号加入双人对局。");
    try {
      const joined = await joinDuelByRoomCodeWithRole(roomCode, duelJoinPreferredRole);
      const nextSession = joined.session;
      const roleToken = joined.role_token.trim();
      const nextCommanderFactionID = joined.commander_faction_id?.trim() || nextSession.player_faction_id;
      const controlled = controlledUnitsByFaction(nextSession, nextCommanderFactionID);
      const firstUnitID = controlled[0]?.id ?? "";

      setSessionRoleToken(roleToken);
      setSession(nextSession);
      setCommanderFactionID(nextCommanderFactionID);
      if (controlled[0]) { setSelectedTileCoord({ q: controlled[0].status.position_q, r: controlled[0].status.position_r }); } else { setSelectedTileCoord(null); }
      setTaskTargetUnitID(firstUnitID);
      setOrderTargetUnitID(firstUnitID);
      setDirectiveDraft(factionDoctrineDraft(nextSession, nextCommanderFactionID));
      const joinedRoomCode = normalizeRoomCodeInput(joined.room_code ?? roomCode);
      setDuelRoomCode(joinedRoomCode);
      setDuelJoinRoomCode(joinedRoomCode);
      setDuelJoinPreferredRole(joined.role === "player" ? "player" : "enemy");
      setDuelJoinSessionID(nextSession.id);
      setDuelJoinRoleToken(roleToken);
      setDuelRoomStatus(normalizeDuelRoomStatus(joined.room_status));
      writeDuelResumeToStorage({
        session_id: nextSession.id,
        role_token: roleToken,
        room_code: joinedRoomCode,
        preferred_role: joined.role === "player" ? "player" : "enemy",
      });
      const link = `${window.location.pathname}?session_id=${encodeURIComponent(
        nextSession.id,
      )}&role_token=${encodeURIComponent(roleToken)}`;
      window.history.replaceState(null, "", link);
      setLoadState("ready");
      setMessage(joined.role === "player" ? "已通过房间号加入双人对局（我方=player）。" : "已通过房间号加入双人对局（我方=enemy）。");
    } catch (error) {
      setLoadState("error");
      setMessage(getErrorMessage(error, "房间号加入失败"));
    } finally {
      setBusy(false);
    }
  }

  async function handleCopyText(value: string, label: string) {
    const text = value.trim();
    if (!text) {
      setMessage(`${label}为空，无法复制。`);
      return;
    }
    try {
      await copyTextToClipboard(text);
      setMessage(`${label}已复制。`);
    } catch {
      setMessage(`${label}复制失败，请手动复制。`);
    }
  }

  async function handleAdvancePhase() {
    if (!session || busy) {
      return;
    }
    if (session.execution_in_progress) {
      setMessage("执行阶段仍在逐单位行动中，请等待当前执行流完成。");
      return;
    }
    const deadlineReached = session.turn_state.phase === "deployment" && phaseDeadlineReachedForClient(session);
    if (session.turn_state.phase === "deployment" && !deadlineReached && !currentFactionDoctrineReady) {
      setMessage("请先填写并保存本回合总方针，再开始执行。");
      return;
    }
    if (session.turn_state.phase === "deployment" && !deadlineReached && !opponentFactionReady) {
      setMessage(session.mode === "duel" ? "需要对方也完成准备后，才能开始执行阶段。" : "敌方部署方针尚未准备好，请稍后再开始执行。");
      return;
    }
    if (session.outcome !== "ongoing" && session.turn_state.phase === "execution") {
      return;
    }

    setBusy(true);
    setMessage(`正在推进到下一阶段：${phaseLabels[session.turn_state.phase]}`);
    startPhaseTransitionPolling(session);

    try {
      const nextSession = await advancePhase(session.id);
      stopPhaseTransitionPolling();
      applySessionSnapshot(nextSession);
      if (nextSession.turn_state.phase !== "deployment") {
        setDeploymentTaskModalOpen(false);
      }
      if (nextSession.turn_state.phase === "deployment") {
        setDirectiveDraft(factionDoctrineDraft(nextSession, effectiveCommanderFactionID));
      }
      if (nextSession.outcome === "ongoing") {
        if (nextSession.turn_state.phase === session.turn_state.phase && nextSession.phase_ready?.[effectiveCommanderFactionID]) {
          setMessage("已选择下一阶段，等待另一方确认。");
        } else {
          setMessage(nextPhaseMessage(nextSession.turn_state.phase));
        }
      } else {
        setMessage(`本局已结束：${outcomeLabels[nextSession.outcome]}。`);
      }
    } catch (error) {
      // 合规门 403：推进被宵禁/未实名/防沉迷超限拦截——渲染拦截横幅引导，不当作普通报错。
      if (error instanceof APIError && error.status === 403) {
        stopPhaseTransitionPolling();
        setComplianceBlockReason(error.reason ?? "");
        setBusy(false);
        return;
      }
      const recovered = recoverSession(error, applySessionSnapshot);
      let recoveredMessage = "";
      if (!recovered) {
        try {
          const latest = await getSession(session.id);
          applySessionSnapshot(latest.session);
          if (latest.session.turn_state.phase === "deployment") {
            setDirectiveDraft(factionDoctrineDraft(latest.session, effectiveCommanderFactionID));
          }
          const phaseChanged =
            latest.session.turn_state.turn !== session.turn_state.turn ||
            latest.session.turn_state.phase !== session.turn_state.phase ||
            latest.session.execution_in_progress !== session.execution_in_progress;
          if (phaseChanged) {
            recoveredMessage = nextPhaseMessage(latest.session.turn_state.phase);
          }
        } catch {
          // Keep the original error visible if recovery also fails.
        }
      }
      setMessage(recoveredMessage || getErrorMessage(error, "阶段推进失败，已尝试同步最新对局状态"));
    } finally {
      setBusy(false);
    }
  }

  async function handleDirectiveSubmit() {
    if (!session) {
      setMessage("当前还没有进入对局，无法提交方针。");
      return;
    }
    if (busy) {
      setMessage("当前仍有操作处理中，请稍等片刻再提交方针。");
      return;
    }
    if (!directiveDraft.trim()) {
      setMessage("方针内容不能为空，请先输入自然语言方针。");
      return;
    }
    if (session.outcome !== "ongoing") {
      setMessage("本局已经结束，不能再提交方针。");
      return;
    }
    if (session.turn_state.phase !== "deployment" && session.turn_state.phase !== "execution") {
      setMessage(
        `当前是${phaseLabels[session.turn_state.phase]}，全局方针只能在部署阶段提交，或在执行阶段预设到下一回合。`,
      );
      return;
    }

    setBusy(true);
    setMessage(session.turn_state.phase === "execution" ? "正在预设下一回合全局方针。" : "正在保存本回合全局方针。");
    try {
      const nextSession = await setGlobalDirective(session.id, directiveDraft);
      if (session.turn_state.phase === "execution") {
        setSession(nextSession);
        setDirectiveDraft("");
        setActivePanelID(null);
        setMessage("下一回合全局方针已预设，会在本次执行结束后生效。");
        return;
      }
      setSession(nextSession);
      setDirectiveDraft(factionDoctrineDraft(nextSession, effectiveCommanderFactionID));
      setMessage("总方针已更新；部署阶段可继续修改，点击开始执行后以最后一次提交为准。");
    } catch (error) {
      recoverSession(error, setSession);
      setMessage(getErrorMessage(error, "更新方针失败"));
    } finally {
      setBusy(false);
    }
  }

  async function handleTaskSubmit() {
    if (!session || busy || !taskDraft.trim()) {
      return;
    }

    setBusy(true);
    try {
      const nextSession = await setTaskDirective(session.id, taskDraft, taskTargetUnitID || undefined);
      setSession(nextSession);
      setTaskDraft("");
      setDeploymentTaskModalOpen(false);
      setMessage(
        session.turn_state.phase === "deployment"
          ? "部署任务已下达；现在可以手动选择下一阶段。"
          : "任务指令已下达；单位会自己评估并执行。",
      );
    } catch (error) {
      recoverSession(error, setSession);
      setMessage(getErrorMessage(error, "发布任务失败"));
    } finally {
      setBusy(false);
    }
  }

  async function handleDeploymentModalSubmit() {
    if (!session || busy || session.outcome !== "ongoing") {
      return;
    }
    const directiveText = directiveDraft.trim();
    if (!directiveText) {
      setMessage("请填写总方针后再提交。");
      return;
    }
    const shouldCloseAfterSave = session.turn_state.phase === "execution";
    if (shouldCloseAfterSave) {
      setDeploymentTaskModalOpen(false);
      setActivePanelID(null);
    }
    setBusy(true);
    try {
      const latest = await setGlobalDirective(session.id, directiveDraft);
      setSession(latest);
      setDirectiveDraft(factionDoctrineDraft(latest, effectiveCommanderFactionID));
      if (session.turn_state.phase === "execution") {
        setMessage("下一回合总方针已预设，会在本次执行结束后生效。");
      } else {
        setDeploymentDoctrineConfirmed(false);
        setMessage("总方针已更新；部署阶段可再次打开方针继续修改。");
      }
    } catch (error) {
      recoverSession(error, setSession);
      setMessage(getErrorMessage(error, "提交总方针失败"));
    } finally {
      setBusy(false);
    }
  }

  async function handleDeploymentDoctrineConfirm() {
    if (!session || busy || session.outcome !== "ongoing") {
      return;
    }
    if (session.turn_state.phase !== "deployment") {
      return;
    }
    if (!directiveDraft.trim()) {
      setMessage("请先填写并保存总方针。")
      return;
    }
    setBusy(true);
    setMessage("正在确认本回合总方针。");
    try {
      const latest = await setGlobalDirective(session.id, directiveDraft);
      setSession(latest);
      setDirectiveDraft(factionDoctrineDraft(latest, effectiveCommanderFactionID));
      setDeploymentDoctrineConfirmed(true);
      setMessage("总方针已确认；对方准备完成后即可开始执行。")
    } catch (error) {
      recoverSession(error, setSession);
      setDeploymentDoctrineConfirmed(false);
      setMessage(getErrorMessage(error, "确认总方针失败"));
    } finally {
      setBusy(false);
    }
  }

  function openDeploymentCommandModal() {
    if (session?.turn_state.phase === "deployment") {
      setDirectiveDraft(factionDoctrineDraft(session, effectiveCommanderFactionID));
      setDeploymentDoctrineConfirmed(currentFactionReady);
    } else if (session && directiveDraft.trim() === "") {
      setDirectiveDraft(factionDoctrineDraft(session, effectiveCommanderFactionID));
    }
    setActivePanelID(null);
    setDeploymentTaskModalOpen(true);
  }

  function handleTaskActionClick() {
    if (!session || busy || session.outcome !== "ongoing") {
      return;
    }
    void handleTaskSubmit();
  }

  async function handleImmediateOrderSubmit() {
    if (!session || busy || !orderDraft.trim() || !orderTargetUnitID) {
      return;
    }

    setBusy(true);
    try {
      const nextSession = await setImmediateOrder(session.id, orderDraft, orderTargetUnitID);
      setSession(nextSession);
      setOrderDraft("");
      setMessage("即时令已下达；已消耗指挥力并注入该单位 AI 决策上下文，具体行为仍由单位自主执行。");
    } catch (error) {
      recoverSession(error, setSession);
      setMessage(getErrorMessage(error, "下达即时令失败"));
    } finally {
      setBusy(false);
    }
  }

  async function handleDialogueSubmit() {
    const target = chatTargetUnit;
    if (!session || !target || busy || session.turn_state.phase !== "deployment") {
      if (session?.turn_state.phase === "execution") {
        setMessage("执行阶段不能即时聊天；可在全局方针里写下回合沟通策略，下一部署阶段再交谈。");
      }
      return;
    }
    if (target.status.life_state === "dead") {
      setMessage(`${target.identity.name} 已死亡，只能查看历史聊天。`);
      return;
    }

    setBusy(true);
    setLatestDialogueReply("等待单位回复中…");
    try {
      const result = await talkToUnit(session.id, target.id, dialogueDraft);
      setSession(result.session);
      setDialogueDraft("");
      setLatestDialogueReply(`${result.reply.speaker}：${result.reply.message}`);
      setMessage(`${target.identity.name} 已回应：${result.reply.message}`);
    } catch (error) {
      recoverSession(error, setSession);
      setLatestDialogueReply("");
      setMessage(getErrorMessage(error, "对话失败"));
    } finally {
      setBusy(false);
    }
  }

  function handleToggleOpeningDraftUnit(unitID: string) {
    const required = session?.draft_required_pick ?? 10;
    setOpeningDraftSelectedIDs((current) => {
      if (current.includes(unitID)) {
        return current.filter((id) => id !== unitID);
      }
      if (current.length >= required) {
        return [...current.slice(1), unitID];
      }
      return [...current, unitID];
    });
  }

  function handleOpeningDraftEdit(unitID: string, field: "name" | "biography" | "gender" | "portrait_url", value: string) {
    const nextDirtyFields = new Set(openingDraftDirtyFieldsRef.current.get(unitID) ?? []);
    nextDirtyFields.add(field);
    openingDraftDirtyFieldsRef.current.set(unitID, nextDirtyFields);
    setOpeningDraftUnits((current) =>
      current.map((unit) => {
        if (unit.id !== unitID) {
          return unit;
        }
        return {
          ...unit,
          identity: {
            ...unit.identity,
            [field]: value,
          },
        };
      }),
    );
  }

  async function handleConfirmOpeningDraft() {
    if (!session || busy) {
      return;
    }
    const required = session.draft_required_pick ?? 10;
    const selected = openingDraftUnits.filter((unit) => openingDraftSelectedIDs.includes(unit.id)).slice(0, required);
    if (selected.length < required) {
      setMessage(`还需要选择 ${required} 名单位。`);
      return;
    }
    setBusy(true);
    setMessage("正在确认开局名单。");
    try {
      const nextSession = await confirmOpeningDraft(session.id, selected);
      const controlled = controlledUnitsByFaction(nextSession, nextSession.player_faction_id);
      const firstUnitID = controlled[0]?.id ?? "";
      openingDraftDirtyFieldsRef.current = new Map();
      setSession(nextSession);
      setCommanderFactionID(nextSession.player_faction_id);
      if (controlled[0]) { setSelectedTileCoord({ q: controlled[0].status.position_q, r: controlled[0].status.position_r }); } else { setSelectedTileCoord(null); }
      setTaskTargetUnitID(firstUnitID);
      setOrderTargetUnitID(firstUnitID);
      setDirectiveDraft(nextSession.global_directive.text);
      setMessage("开局 10 人名单已确认，进入部署阶段。");
    } catch (error) {
      setMessage(getErrorMessage(error, "确认开局名单失败"));
    } finally {
      setBusy(false);
    }
  }

  // complianceBanner 是被合规门 403 拦截时的引导横幅（position:fixed，可在任意 return 内复用）。
  // 仅『需实名』类拦截露出『去实名认证』按钮；宵禁/防沉迷只显示『知道了』。
  const complianceBanner =
    complianceBlockReason != null ? (
      <ComplianceBlockedBanner
        reason={complianceBlockReason}
        onClose={() => setComplianceBlockReason(null)}
        onGoRealname={() => {
          setComplianceBlockReason(null);
          setCompliancePanelOpen(true);
        }}
      />
    ) : null;

  // compliancePanelOverlay 是合规面板浮层（实名/生日登记 + 当前裁决），玩家可见，任意 return 内复用。
  const compliancePanelOverlay = compliancePanelOpen ? (
    <ComplianceGatePanel
      accountId={accountUser?.id ?? ""}
      onClose={() => setCompliancePanelOpen(false)}
      onRequireLogin={() => {
        setCompliancePanelOpen(false);
        setStartMode("multiplayer");
        setMessage("请先注册/登录账号，再进行实名认证。");
      }}
    />
  ) : null;

  // billingPanelOverlay 是商业化面板浮层（充值/会员/权益），玩家可见，任意 return 内复用。
  const billingPanelOverlay = billingPanelOpen ? (
    <BillingPanel
      accountId={accountUser?.id ?? ""}
      onClose={() => setBillingPanelOpen(false)}
      onRequireLogin={() => {
        setBillingPanelOpen(false);
        setStartMode("multiplayer");
        setMessage("请先注册/登录账号，再前往充值/会员。");
      }}
    />
  ) : null;

  if (!session) {
    const hasResumeInput = duelJoinSessionID.trim() !== "" && duelJoinRoleToken.trim() !== "";
    return (
      <div className="app-shell">
        <header className="hero">
          <div className="hero-copy">
            <p className="eyebrow">Qunxiang</p>
            <h1>选择游戏模式</h1>
            <p className="subtitle">
              单人模式会直接创建本地战场；多人模式需要先注册/登录账号，再创建房间、选择角色，或用房间号/恢复链接重进。
            </p>
          </div>
          <div className="hero-tag">{loadState === "loading" || busy ? "处理中" : "待选择"}</div>
        </header>

        <main className="workspace" style={{ minHeight: "auto" }}>
          <section className="panel-card">
            <div className="panel-header">
              <div>
                <p className="card-kicker">Mode</p>
                <h2>单人模式 / 多人模式</h2>
              </div>
              <span className="mini-pill">{message}</span>
            </div>
            <label className="input-block">
              <span className="shop-label">单位数（每方）</span>
              <GameSelect
                value={String(openingUnitCount)}
                onChange={(nextValue) => setOpeningUnitCount(Number(nextValue) || 3)}
                disabled={busy || loadState === "loading"}
                ariaLabel="选择每方单位数"
                options={[1, 2, 3, 4, 5, 6, 7, 8, 9, 10].map((count) => ({
                  value: String(count),
                  label: `${count}${count === 3 ? "（建议）" : ""}`,
                }))}
              />
              <span className="field-hint">建议 3 人；最多 10 个单位。双人游戏由房主创建房间时指定。</span>
            </label>
            <label className="input-block">
              <span className="shop-label">地图大小</span>
              <GameSelect
                value={battleMapSize}
                onChange={(nextValue) => setBattleMapSize(normalizeBattleMapSize(nextValue))}
                disabled={busy || loadState === "loading"}
                ariaLabel="选择地图大小"
                options={[
                  { value: "small", label: "小（9×7，快节奏）" },
                  { value: "medium", label: "中（13×9，标准）" },
                  { value: "large", label: "大（17×11，迂回空间更大）" },
                ]}
              />
              <span className="field-hint">地图会按所选长方形尺寸和随机种子生成地形分布。</span>
            </label>
            <label className="input-block">
              <span className="shop-label">迷雾机制</span>
              <GameSelect
                value={fogOfWarEnabled ? "fog" : "open"}
                onChange={(nextValue) => setFogOfWarEnabled(nextValue === "fog")}
                disabled={busy || loadState === "loading"}
                ariaLabel="选择迷雾机制"
                options={[
                  { value: "open", label: "无雾：可见全图敌对单位" },
                  { value: "fog", label: "有雾：只看视野内敌对单位" },
                ]}
              />
              <span className="field-hint">单人开局由玩家选择；多人游戏由房主创建房间时指定，受邀玩家沿用房主设置。</span>
            </label>
            <label className="input-block">
              <span className="shop-label">随机事件</span>
              <GameSelect
                value={randomEventsEnabled ? "on" : "off"}
                onChange={(nextValue) => setRandomEventsEnabled(nextValue === "on")}
                disabled={busy || loadState === "loading"}
                ariaLabel="选择随机事件开关"
                options={[
                  { value: "off", label: "关闭：回合切换更快，不触发随机事件" },
                  { value: "on", label: "开启：每回合可能触发单位自主事件" },
                ]}
              />
              <span className="field-hint">默认关闭，避免执行阶段收尾时额外等待随机事件叙事。</span>
            </label>
            <div className="command-actions">
              <button
                type="button"
                className={`action-button ${startMode === "single" ? "" : "action-button-secondary"}`}
                disabled={busy || loadState === "loading"}
                onClick={() => void handleStartSinglePlayer()}
              >
                单人模式：立即开始
              </button>
              <button
                type="button"
                className={`action-button ${startMode === "multiplayer" ? "" : "action-button-secondary"}`}
                disabled={busy || loadState === "loading"}
                onClick={() => {
                  setStartMode("multiplayer");
                  setMessage("多人模式：请先注册/登录账号，然后创建房间、选择角色或恢复房间。");
                }}
              >
                多人模式：登录后进入
              </button>
            </div>
          </section>

          {startMode === "multiplayer" ? (
            <section className="panel-card">
              <div className="panel-header">
                <div>
                  <p className="card-kicker">Account</p>
                  <h2>注册 / 登录账号</h2>
                </div>
                <span className="mini-pill">
                  {accountUser ? `已登录 ${accountUser.display_name || accountUser.username}` : "未登录"}
                </span>
              </div>

              {accountUser ? (
                <div className="command-actions">
                  <button type="button" className="action-button action-button-secondary" disabled={busy} onClick={() => void handleAccountLogout()}>
                    退出账号
                  </button>
                </div>
              ) : (
                <>
                  <div className="command-actions">
                    <button
                      type="button"
                      className={`action-button ${accountFormMode === "login" ? "" : "action-button-secondary"}`}
                      disabled={busy}
                      onClick={() => setAccountFormMode("login")}
                    >
                      登录
                    </button>
                    <button
                      type="button"
                      className={`action-button ${accountFormMode === "register" ? "" : "action-button-secondary"}`}
                      disabled={busy}
                      onClick={() => setAccountFormMode("register")}
                    >
                      注册
                    </button>
                  </div>
                  <label className="input-block">
                    <span className="shop-label">账号</span>
                    <input className="text-input" value={accountUsername} onChange={(event) => setAccountUsername(event.target.value)} disabled={busy} placeholder="请输入用户名" />
                  </label>
                  {accountFormMode === "register" ? (
                    <label className="input-block">
                      <span className="shop-label">昵称</span>
                      <input className="text-input" value={accountDisplayName} onChange={(event) => setAccountDisplayName(event.target.value)} disabled={busy} placeholder="可选，用于显示" />
                    </label>
                  ) : null}
                  <label className="input-block">
                    <span className="shop-label">密码</span>
                    <input className="text-input" type="password" value={accountPassword} onChange={(event) => setAccountPassword(event.target.value)} disabled={busy} placeholder="请输入密码" />
                  </label>
                  <button type="button" className="action-button" disabled={busy} onClick={() => void handleAccountSubmit()}>
                    {accountFormMode === "register" ? "注册并登录" : "登录账号"}
                  </button>
                </>
              )}
            </section>
          ) : null}

          {startMode === "multiplayer" ? (
            <section className="panel-card">
              <div className="panel-header">
                <div>
                  <p className="card-kicker">Room</p>
                  <h2>创建房间 / 选择角色 / 恢复房间</h2>
                </div>
                <span className="mini-pill">{accountUser ? "可创建房间" : "需先登录"}</span>
              </div>

              <label className="input-block">
                <span className="shop-label">创建后我的角色</span>
                <GameSelect
                  value={duelCreatorRole}
                  onChange={(nextValue) => setDuelCreatorRole(nextValue === "enemy" ? "enemy" : "player")}
                  disabled={busy || !accountUser}
                  ariaLabel="选择创建房间后的角色"
                  options={[
                    { value: "player", label: "player（房主默认阵营）" },
                    { value: "enemy", label: "enemy（房主选择敌方阵营）" },
                  ]}
                />
              </label>
              <p className="panel-note">房主创建房间时会使用上方“单位数（每方）”和“迷雾机制”设置；受邀玩家加入后沿用房主指定规则。</p>
              <div className="command-actions">
                <button type="button" className="action-button" disabled={busy || !accountUser} onClick={() => void handleCreateDuel()}>
                  创建多人房间
                </button>
              </div>

              <label className="input-block">
                <span className="shop-label">房间号</span>
                <input className="text-input" value={duelJoinRoomCode} onChange={(event) => setDuelJoinRoomCode(normalizeRoomCodeInput(event.target.value))} disabled={busy} placeholder="例如：A7K9Q2" />
              </label>
              <label className="input-block">
                <span className="shop-label">加入时选择角色</span>
                <GameSelect
                  value={duelJoinPreferredRole}
                  onChange={(nextValue) => setDuelJoinPreferredRole(nextValue === "player" ? "player" : "enemy")}
                  disabled={busy}
                  ariaLabel="选择加入房间时的角色"
                  options={[
                    { value: "enemy", label: "enemy（受邀玩家默认）" },
                    { value: "player", label: "player（恢复/接管 player 端）" },
                  ]}
                />
              </label>
              <div className="command-actions">
                <button type="button" className="action-button action-button-secondary" disabled={busy || !duelJoinRoomCode.trim()} onClick={() => void handleJoinDuelByRoomCode()}>
                  房间号加入
                </button>
              </div>

              <label className="input-block">
                <span className="shop-label">恢复 Session ID</span>
                <input className="text-input" value={duelJoinSessionID} onChange={(event) => setDuelJoinSessionID(event.target.value)} disabled={busy} placeholder="从恢复链接或本地记录带入" />
              </label>
              <label className="input-block">
                <span className="shop-label">恢复角色 Token</span>
                <input className="text-input" value={duelJoinRoleToken} onChange={(event) => setDuelJoinRoleToken(event.target.value)} disabled={busy} placeholder="role_token" />
              </label>
              <div className="command-actions">
                <button type="button" className="action-button action-button-secondary" disabled={busy || !hasResumeInput} onClick={() => void handleJoinDuelByInput()}>
                  恢复房间（重进）
                </button>
              </div>
            </section>
          ) : null}
        </main>
        {complianceBanner}
        {compliancePanelOverlay}
        {billingPanelOverlay}
        {reportDialogOpen ? (
          <ReportDialog
            sessionId=""
            reporter={accountUser?.display_name || accountUser?.username || ""}
            onClose={() => setReportDialogOpen(false)}
          />
        ) : null}
      </div>
    );
  }

  if (duelWaitingForOpponent) {
    return (
      <div className="app-shell">
        <header className="hero">
          <div className="hero-copy">
            <p className="eyebrow">Duel Room</p>
            <h1>等待对手加入</h1>
            <p className="subtitle">
              房间已经创建。把房间号或加入链接发给对手；双方进入后会自动打开战场。
            </p>
          </div>
          <div className="hero-tag">{duelRoomCode || "ROOM"}</div>
        </header>

        <main className="workspace" style={{ minHeight: "auto" }}>
          <section className="panel-card room-wait-card">
            <div className="panel-header">
              <div>
                <p className="card-kicker">Invite</p>
                <h2>邀请对手</h2>
              </div>
              <span className="mini-pill">
                {duelRoomStatus?.player_joined ? "player 已进入" : "等待 player"}
                {" / "}
                {duelRoomStatus?.enemy_joined ? "enemy 已进入" : "等待 enemy"}
              </span>
            </div>

            <div className="command-summary room-share-grid">
              <span className="shop-label">房间号</span>
              <div className="inline-input-row">
                <input className="text-input" value={duelRoomCode || ""} readOnly />
                <button
                  type="button"
                  className="action-button inline-action"
                  disabled={!duelRoomCode}
                  onClick={() => void handleCopyText(duelRoomCode, "房间号")}
                >
                  复制
                </button>
              </div>
            </div>

            {opponentJoinLink ? (
              <label className="input-block">
                <span className="shop-label">对手加入链接（{opponentJoinRole}）</span>
                <div className="inline-input-row">
                  <input className="text-input" value={opponentJoinLink} readOnly />
                  <button
                    type="button"
                    className="action-button inline-action"
                    onClick={() => void handleCopyText(opponentJoinLink, "对手加入链接")}
                  >
                    复制
                  </button>
                </div>
              </label>
            ) : null}

            {selfResumeLink ? (
              <label className="input-block">
                <span className="shop-label">我的恢复链接</span>
                <div className="inline-input-row">
                  <input className="text-input" value={selfResumeLink} readOnly />
                  <button
                    type="button"
                    className="action-button inline-action"
                    onClick={() => void handleCopyText(selfResumeLink, "我的恢复链接")}
                  >
                    复制
                  </button>
                </div>
              </label>
            ) : null}

            <p className="panel-note">
              当前你控制 {effectiveCommanderFactionID === session.player_faction_id ? "player" : "enemy"} 阵营。不要把“我的恢复链接”发给对手；对手只需要房间号或对手加入链接。
            </p>

            <div className="command-actions">
              <button type="button" className="action-button action-button-secondary" onClick={handleReturnToMainMenu}>
                返回主菜单
              </button>
            </div>
          </section>
        </main>
      </div>
    );
  }

  if (session?.setup_phase === "drafting") {
    const required = session.draft_required_pick ?? 5;
    return (
      <div className="app-shell">
        <header className="hero">
          <div className="hero-copy">
            <p className="eyebrow">Opening Draft</p>
            <h1>开局组队：选择 {required} 个单位</h1>
            <p className="subtitle">
              系统会直接给出最多 10 个候选单位；当前局优先使用上一次异步生成的名单，同时后台继续生成下一局名单，避免开局卡顿。
            </p>
          </div>
          <div className="hero-tag">{openingDraftSecondsLeft}s</div>
        </header>
        <section className="draft-toolbar panel-card">
          <div>
            <p className="card-kicker">Roster</p>
            <h2>已选择 {openingDraftSelectedIDs.length} / {required}</h2>
            <p className="panel-note">{message}</p>
          </div>
          <button
            type="button"
            className="action-button"
            disabled={busy || openingDraftSelectedIDs.length < required}
            onClick={() => void handleConfirmOpeningDraft()}
          >
            确认名单
          </button>
        </section>
        <main className="draft-grid">
          {openingDraftUnits.map((unit) => {
            const selected = openingDraftSelectedIDs.includes(unit.id);
            return (
              <article key={unit.id} className={`draft-card panel-card ${selected ? "draft-card-selected" : ""}`}>
                <button type="button" className="draft-select" onClick={() => handleToggleOpeningDraftUnit(unit.id)}>
                  {selected ? "已入队" : "选择"}
                </button>
                <div className="draft-avatar-row">
                  <img
                    className="draft-avatar"
                    src={portraitURLForUnit(unit)}
                    alt=""
                    onError={(event) => {
                      event.currentTarget.onerror = null;
                      event.currentTarget.src = portraitFallbackURLForUnit(unit);
                    }}
                  />
                  <label className="input-block draft-portrait-input">
                    <span className="shop-label">头像 URL</span>
                    <input
                      className="text-input"
                      value={unit.identity.portrait_url || ""}
                      onChange={(event) => handleOpeningDraftEdit(unit.id, "portrait_url", event.target.value)}
                    />
                  </label>
                </div>
                <label className="input-block">
                  <span className="shop-label">名字</span>
                  <input
                    className="text-input"
                    value={unit.identity.name}
                    onChange={(event) => handleOpeningDraftEdit(unit.id, "name", event.target.value)}
                  />
                </label>
                <label className="input-block">
                  <span className="shop-label">性别/性格标签</span>
                  <input
                    className="text-input"
                    value={unit.identity.gender || "unknown"}
                    onChange={(event) => handleOpeningDraftEdit(unit.id, "gender", event.target.value)}
                  />
                </label>
                <label className="input-block">
                  <span className="shop-label">生平</span>
                  <textarea
                    className="text-area draft-bio"
                    value={unit.identity.biography}
                    onChange={(event) => handleOpeningDraftEdit(unit.id, "biography", event.target.value)}
                  />
                </label>
                <div className="draft-trait-grid" aria-label={`${unit.identity.name} 的性格数值`}>
                  {personalityTraitLabels.map((trait) => (
                    <span key={trait.key} className="draft-trait-chip">
                      <span>{trait.label}</span>
                      <strong>{unit.personality[trait.key].toFixed(2)}</strong>
                    </span>
                  ))}
                </div>
                <div className="draft-trait-grid" aria-label={`${unit.identity.name} 的基础属性`}>
                  {primaryStatLabels.map((stat) => (
                    <span key={stat.key} className="draft-trait-chip">
                      <span>{stat.label}</span>
                      <strong>{unit.stats?.primary?.[stat.key] ?? "--"}</strong>
                    </span>
                  ))}
                </div>
                <div className="draft-trait-grid" aria-label={`${unit.identity.name} 的战斗属性`}>
                  {derivedStatLabels.map((stat) => (
                    <span key={stat.key} className="draft-trait-chip">
                      <span>{stat.label}</span>
                      <strong>{unit.stats?.derived?.[stat.key] ?? "--"}</strong>
                    </span>
                  ))}
                </div>
              </article>
            );
          })}
        </main>
      </div>
    );
  }

  return (
    <div className="app-shell app-shell-game">
      <main className="workspace">
        <section
          className="viewport-card viewport-card-game"
          onPointerDownCapture={(event) => {
            const target = event.target as HTMLElement | null;
            if (!target) {
              return;
            }
            if (!activePanelID) {
              return;
            }
            if (
              target.closest(".floating-panel-shell") ||
              target.closest(".floating-toolbar") ||
              target.closest(".left-command-dock") ||
              target.closest(".map-vision-controls") ||
              target.closest(".map-zoom-controls") ||
              target.closest(".unit-summary-card") ||
              target.closest(".unit-detail-popover")
            ) {
              return;
            }
            setActivePanelID(null);
          }}
        >
          {showHUD ? (
            <>
              <div className="top-status-bar">
                <div className="top-status-group">
                  <button
                    type="button"
                    className="top-menu-return-button"
                    onClick={handleReturnToMainMenu}
                    title="返回主菜单"
                    aria-label="返回主菜单"
                  >
                    返回
                  </button>
                  <div className="top-status-item">
                    <span className="icon">{(session && effectiveCommanderFactionID === session.player_faction_id) ? "P" : "E"}</span>
                    <span>{session ? (effectiveCommanderFactionID === session.player_faction_id ? "玩家阵营" : "敌方阵营") : "未加入"}</span>
                  </div>
                </div>

                <div className="top-status-center">
                  <div className="top-status-item">
                    <span style={{ color: '#d9bc73' }}>回合</span>
                    <span>{session?.turn_state.turn ?? "--"}</span>
                  </div>
                  <div className="top-status-item">
                    <span style={{ color: '#8cb572' }}>阶段</span>
                    <span>{session ? phaseLabels[session.turn_state.phase] : "--"}</span>
                  </div>
                  <div className="top-status-item top-status-countdown">
                    <span style={{ color: '#f2d98f' }}>倒计时</span>
                    <span>{phaseRemainingText}</span>
                  </div>
                  <div className="top-status-item">
                    <span style={{ color: '#c66d48' }}>指挥力</span>
                    <span>{session ? session.command_power.current : "--"}</span>
                  </div>
                </div>

                <div className="top-status-group">
                  {session?.fog_of_war_enabled ? (
                    <div className="map-vision-controls" title="选择有雾地图使用的视野范围">
                      <span className="map-vision-label">视野</span>
                      <GameSelect
                        className="game-select-compact"
                        value={fogVisionMode}
                        onChange={setFogVisionMode}
                        ariaLabel={`当前视野：${fogVisionModeLabel}`}
                        options={[
                          { value: "merged", label: "合并视野" },
                          ...fogVisionUnits.map((unit) => ({ value: unit.id, label: unit.identity.name })),
                        ]}
                      />
                    </div>
                  ) : null}
                  <div className="map-zoom-controls" aria-label="地图缩放">
                    <button
                      type="button"
                      className="map-zoom-button"
                      onClick={() => setMapZoom((current) => Math.max(0.55, Number((current - 0.1).toFixed(2))))}
                      title="缩小地图"
                      aria-label="缩小地图"
                    >
                      −
                    </button>
                    <span className="map-zoom-value">{Math.round(mapZoom * 100)}%</span>
                    <button
                      type="button"
                      className="map-zoom-button"
                      onClick={() => setMapZoom((current) => Math.min(1.8, Number((current + 0.1).toFixed(2))))}
                      title="放大地图"
                      aria-label="放大地图"
                    >
                      +
                    </button>
                    <button
                      type="button"
                      className="map-zoom-reset"
                      onClick={() => setMapZoom(1)}
                      title="恢复默认缩放"
                      aria-label="恢复默认缩放"
                    >
                      复位
                    </button>
                  </div>
                  <div className="top-status-item">
                    <span style={{ color: session?.outcome === 'ongoing' ? '#b9b194' : '#f2d98f' }}>
                      {session ? outcomeLabels[session.outcome] : "初始化中"}
                    </span>
                  </div>
                  <button className="action-button inline-action" onClick={() => handleFloatingPanelToggle("overview")}>概览</button>
                  <button
                    className={`action-button inline-action ${fatePanelOpen ? "action-button-primary" : ""}`}
                    onClick={() => setFatePanelOpen((open) => !open)}
                    title="命运四槽：看你的人如今怎样、近来经历了什么、有没有事在等你拿主意"
                  >
                    命运
                  </button>
                  <button
                    className={`action-button inline-action ${consentInboxOpen ? "action-button-primary" : ""}`}
                    onClick={() => setConsentInboxOpen((open) => !open)}
                    disabled={!selectedUnitID}
                    title={selectedUnitID ? "来意：看有没有别处的人想与这位角色发生牵连，由你替她拿主意" : "先选中一个角色，再查看其『来意』收件箱"}
                  >
                    来意
                  </button>
                  <button
                    className={`action-button inline-action ${bloodFeudOpen ? "action-button-primary" : ""}`}
                    onClick={() => setBloodFeudOpen((open) => !open)}
                    disabled={!selectedUnitID}
                    title={selectedUnitID ? "血仇：查看这位角色背负的世仇关系网（含因牵连传播而来的间接之恨）" : "先选中一个角色，再查看其血仇网络"}
                  >
                    血仇
                  </button>
                  <button
                    className={`action-button inline-action ${billingPanelOpen ? "action-button-primary" : ""}`}
                    onClick={() => {
                      setBillingPanelOpen((open) => !open);
                      void trackFunnel("open_billing");
                    }}
                    title="充值 / 会员 / 已购权益"
                  >
                    充值
                  </button>
                  <button
                    className={`action-button inline-action ${fieldBossModalOpen ? "action-button-primary" : ""}`}
                    onClick={() => setFieldBossModalOpen((open) => !open)}
                    title="组队挑战野外强敌：勾选队员一同出战，按贡献分赃"
                  >
                    组队
                  </button>
                  <button
                    className={`action-button inline-action ${dungeonOpen ? "action-button-primary" : ""}`}
                    onClick={() => setDungeonOpen((open) => !open)}
                    title="多层副本：勾选队员逐层推进，通关按贡献分赃、败北分级惩罚（需后端开启 QUNXIANG_DUNGEON）"
                  >
                    副本
                  </button>
                  <button
                    className="action-button inline-action"
                    onClick={() => setReportDialogOpen(true)}
                    title="举报不当内容（可针对当前选中角色）"
                  >
                    举报
                  </button>
                  {developerMode ? (
                    <button
                      className={`action-button inline-action ${governancePanelOpen ? "action-button-primary" : ""}`}
                      onClick={() => setGovernancePanelOpen((open) => !open)}
                      title="运营/开发者：审计 · 举报管理台 · 隐私擦除"
                    >
                      治理台
                    </button>
                  ) : null}
                  {developerMode ? (
                    <button
                      className={`action-button inline-action ${opsDashboardOpen ? "action-button-primary" : ""}`}
                      onClick={() => setOpsDashboardOpen((open) => !open)}
                      title="运营看板：跨会话 LLM 成本 / fallback 率 / 假门转化漏斗（需 X-Ops-Token）"
                    >
                      运营看板
                    </button>
                  ) : null}
                  {developerMode ? (
                    <button
                      className={`action-button inline-action ${worldBossOpen ? "action-button-primary" : ""}`}
                      onClick={() => setWorldBossOpen((open) => !open)}
                      disabled={!session?.world_id}
                      title={session?.world_id ? "世界 Boss：跨玩家共享血池协作 PvE（投放 / 出手 / 按贡献分赃）" : "本局未接入世界（world_id 为空），世界 Boss 不可用"}
                    >
                      世界Boss
                    </button>
                  ) : null}
                </div>
              </div>
              <div className="global-params-strip" aria-label="全局参数">
                <span className="global-params-title">全局参数</span>
                <span className="global-param-chip" title={session?.weather?.note || "当前回合天气"}>
                  <span>天气</span>
                  <strong>{session?.weather?.display_name ?? "--"}</strong>
                </span>
                <span className="global-param-chip">
                  <span>地图</span>
                  <strong>{session?.map_script_name ?? "--"}</strong>
                </span>
                <span className="global-param-chip">
                  <span>规模</span>
                  <strong>{session?.map_size_name ?? session?.map_size_id ?? "--"}</strong>
                </span>
                <span className="global-param-chip">
                  <span>迷雾</span>
                  <strong>{session ? (session.fog_of_war_enabled ? "开启" : "关闭") : "--"}</strong>
                </span>
                <span className="global-param-chip">
                  <span>胜利目标</span>
                  <strong>击败全部敌方单位</strong>
                </span>
              </div>
            </>
          ) : null}
          {showHUD ? (
            <nav className="left-command-dock" aria-label="地图左侧快捷命令">
              <button
                type="button"
                className="left-command-button left-command-button-primary"
                onClick={() => void handleAdvancePhase()}
                disabled={!canRequestAdvancePhase}
                title={advancePhaseButtonTitle}
              >
                <span>▶</span>
                <strong>{session?.turn_state.phase === "deployment" ? "执行" : "推进"}</strong>
              </button>
              <button
                type="button"
                className="left-command-button"
                onClick={() => {
                  if (selectedUnit) {
                    setChatTargetUnitID(selectedUnit.id);
                  }
                  setActivePanelID("chat");
                }}
                title="像聊天一样与单位交谈"
              >
                <span>💬</span>
                <strong>交谈</strong>
              </button>
              <button
                type="button"
                className="left-command-button"
                onClick={openDeploymentCommandModal}
                disabled={!session || session.outcome !== "ongoing"}
                title={session?.turn_state.phase === "execution" ? "预设下一回合指令" : "发布部署指令"}
              >
                <span>✎</span>
                <strong>{session?.turn_state.phase === "execution" ? "预设" : "方针"}</strong>
              </button>
              <button
                type="button"
                className="left-command-button left-command-button-guide"
                onClick={() => {
                  setActivePanelID(null);
                  setShowShortcutHelp(false);
                  setGameGuideOpen(true);
                }}
                title="查看游戏攻略、世界观和规则说明"
              >
                <span>📖</span>
                <strong>攻略</strong>
              </button>
            </nav>
          ) : null}
          <button
            type="button"
            className={`hud-toggle-button ${showHUD ? "hud-toggle-button-on" : "hud-toggle-button-off"}`}
            title={`HUD ${showHUD ? "隐藏" : "显示"} (H)`}
            aria-label={`HUD ${showHUD ? "隐藏" : "显示"}（快捷键 H）`}
            onClick={() => setShowHUD((current) => !current)}
          >
            {showHUD ? "HUD ON" : "HUD OFF"}
          </button>
          <button
            type="button"
            className="shortcut-help-toggle"
            title="快捷键帮助 (?)"
            aria-label="快捷键帮助（快捷键 ?）"
            onClick={() => setShowShortcutHelp((current) => !current)}
          >
            ?
          </button>
          <Suspense
            fallback={
              <div className="pixi-board pixi-board-loading">
                <p>战场资源加载中…</p>
              </div>
            }
          >
            <LazyPixiBoard
              session={visibleSession}
              commanderFactionID={effectiveCommanderFactionID}
              fogPerspectiveUnitID={fogPerspectiveUnitID}
              selectedTileCoord={selectedTileCoord}
              onTileClick={handleTileClick}
              onOpenDialogues={() => handleFloatingPanelToggle("dialogues")}
              onOpenUnitChat={handleOpenUnitDialogueModal}
              nowMs={nowMs}
              zoom={mapZoom}
              executionMarkers={boardExecutionMarkers}
            />
          </Suspense>
          {showHUD && itemGainToasts.length > 0 ? (
            <div className="item-gain-toast-stack" aria-live="polite">
              {itemGainToasts.map((toast) => (
                <article key={toast.id} className="item-gain-toast">
                    <span className="item-gain-toast-icon" aria-hidden="true">{toast.resource === "gold" ? "G" : "🎒"}</span>
                    <span className="item-gain-toast-main">
                      <strong>{toast.unitName}</strong>
                    <span>
                      {toast.direction === "gain" ? "获得" : "失去"}{" "}
                      {toast.resource === "gold" ? `${toast.quantity} 金币` : `${toast.itemLabel} x${toast.quantity}`}
                    </span>
                  </span>
                </article>
              ))}
            </div>
          ) : null}
          {showHUD && selectedUnit ? (
            <button
              type="button"
              className="unit-summary-card"
              onClick={() => {
                if (selectedUnitRestrictedByFog) {
                  setMessage("有雾模式下不能查看敌方单位的具体信息、背包和情报。");
                  return;
                }
                setUnitDetailPopoverOpen(true);
              }}
              title={selectedUnitRestrictedByFog ? "有雾模式下敌方详情受限" : "点击展开角色详报"}
              aria-label={selectedUnitRestrictedByFog ? `${selectedUnit.identity.name} 的敌方情报受迷雾遮蔽` : `展开 ${selectedUnit.identity.name} 的情报面板`}
            >
              <img
                className="unit-summary-avatar"
                src={selectedUnitPortraitURL}
                alt=""
                onError={(event) => {
                  event.currentTarget.onerror = null;
                  event.currentTarget.src = selectedUnitPortraitFallbackURL;
                }}
              />
              <span className="unit-summary-main">
                <span className="unit-summary-headline">
                  <strong>{selectedUnit.identity.name}</strong>
                  <span className={`unit-summary-faction ${selectedUnit.faction_id === effectiveCommanderFactionID ? "unit-summary-faction-player" : "unit-summary-faction-enemy"}`}>
                    {selectedUnitFactionLabel}
                  </span>
                </span>
                {selectedUnitRestrictedByFog ? (
                  <span className="unit-summary-stats">
                    <span>位置可见</span>
                    <span>具体情报受迷雾遮蔽</span>
                  </span>
                ) : (
                  <span className="unit-summary-stats">
                    <span>HP {selectedUnit.status.hp}</span>
                    <span>ATK {selectedUnit.status.attack}</span>
                    <span>DEF {selectedUnit.status.defense}</span>
                    <span>MOV {selectedUnit.status.move}</span>
                    <span>G {selectedUnit.status.wallet}</span>
                    <span>饥饿 {selectedUnit.status.hunger}</span>
                  </span>
                )}
                <span className="unit-summary-hint">{selectedUnitRestrictedByFog ? "有雾：不能展开敌方单位面板/背包/情报" : "再次点击单位 / 双击地块 / 点击此卡展开情报"}</span>
              </span>
            </button>
          ) : null}
          {showHUD && selectedUnit && unitDetailPopoverOpen && !selectedUnitRestrictedByFog ? (
            <>
              <div
                className="unit-detail-backdrop"
                onClick={() => setUnitDetailPopoverOpen(false)}
                aria-hidden="true"
              />
              <aside className="unit-detail-popover" role="dialog" aria-label={`${selectedUnit.identity.name} 详细信息`}>
              <button
                type="button"
                className="unit-detail-close"
                onClick={() => setUnitDetailPopoverOpen(false)}
                aria-label="关闭角色详报"
              >
                ×
              </button>
              <div className="unit-detail-hero">
                <img
                  className="unit-detail-avatar"
                  src={selectedUnitPortraitURL}
                  alt=""
                  onError={(event) => {
                    event.currentTarget.onerror = null;
                    event.currentTarget.src = selectedUnitPortraitFallbackURL;
                  }}
                />
                <div>
                  <p className="card-kicker">角色详报</p>
                  <h3>{selectedUnit.identity.name}</h3>
                  <p className="unit-detail-meta">
                    {selectedUnitFactionLabel} · {selectedUnit.identity.gender || "unknown"} · {selectedUnit.identity.age ? `${selectedUnit.identity.age}岁` : "年龄未知"}
                  </p>
                </div>
              </div>
              <div className="unit-detail-stat-grid">
                <span><strong>{selectedUnit.status.hp}</strong>HP</span>
                <span><strong>{selectedUnit.status.attack}</strong>ATK</span>
                <span><strong>{selectedUnit.status.defense}</strong>DEF</span>
                <span><strong>{selectedUnit.status.move}</strong>MOV</span>
                <span><strong>{selectedUnit.status.wallet}</strong>G</span>
                <span><strong>{selectedUnit.status.hunger}</strong>饥饿</span>
              </div>
              <div className="unit-detail-section">
                <span className="shop-label">伴侣 / 父母 / 小孩</span>
                <p>{formatUnitSocialTies(selectedUnit, session)}</p>
              </div>
              <div className="unit-detail-section">
                <span className="shop-label">性格</span>
                <div className="unit-detail-traits">
                  {personalityTraitLabels.map((trait) => (
                    <span key={trait.key} className="unit-detail-trait">
                      <span>{trait.label}</span>
                      <strong>{selectedUnit.personality[trait.key].toFixed(2)}</strong>
                      <i style={{ width: `${Math.round(selectedUnit.personality[trait.key] * 100)}%` }} />
                    </span>
                  ))}
                </div>
              </div>
              <div className="unit-detail-section">
                <span className="shop-label">最近想法 / 决策</span>
                <p>{formatThoughtSummary(selectedThought, selectedDecision) || stripDefianceTrace(selectedDecision?.reasoning) || "暂无可见想法。"}</p>
                {hasDefianceTrace(selectedDecision?.reasoning) ? (
                  <DefianceCard reasoning={selectedDecision?.reasoning} />
                ) : null}
              </div>
              <div className="unit-detail-section">
                <span className="shop-label">装备与背包</span>
                <div className="inventory-detail-grid">
                  <div>
                    <strong className="inventory-detail-title">装备栏</strong>
                    <div className="inventory-chip-list">
                      {formatEquipmentEntries(selectedUnit).length > 0 ? formatEquipmentEntries(selectedUnit).map((entry) => (
                        <span key={entry} className="inventory-chip">{entry}</span>
                      )) : <span className="inventory-chip inventory-chip-empty">无装备</span>}
                    </div>
                  </div>
                  <div>
                    <strong className="inventory-detail-title">背包栏</strong>
                    <div className="inventory-chip-list">
                      {formatBackpackEntries(selectedUnit).length > 0 ? formatBackpackEntries(selectedUnit).map((entry) => (
                        <span key={entry} className="inventory-chip">{entry}</span>
                      )) : <span className="inventory-chip inventory-chip-empty">背包为空</span>}
                    </div>
                  </div>
                </div>
              </div>
              <div className="unit-detail-section">
                <span className="shop-label">生平</span>
                <p>{selectedUnit.identity.biography || "暂无生平。"}</p>
              </div>
              <div className="unit-detail-section">
                <span className="shop-label">脚下地块</span>
                <p>
                  <strong>
                    {terrainEmojiFor(selectedTerrain?.id)}{" "}
                    {selectedTerrain?.display_name ?? selectedTile?.terrain ?? "未知地形"}
                  </strong>
                  {selectedTerrain ? ` · 移动成本 ${selectedTerrain.move_cost} · 视野 ${selectedTerrain.vision_range}` : ""}
                </p>
                <p>{formatTerrainRuleSummary(selectedTerrain) || "暂无特殊规则。"}</p>
                {selectedStructure ? (
                  <>
                    <p>
                      <strong>
                        {structureEmojiFor(selectedStructure.type)} {formatStructureType(selectedStructure.type)}
                      </strong>
                      {" · "}
                      {selectedStructure.faction_id === (session?.player_faction_id ?? "")
                        ? "玩家阵营"
                        : "敌方阵营"}
                      {" · "}
                      {selectedStructure.completed
                        ? "已完成"
                        : `施工中 ${selectedStructure.build_progress}/${selectedStructure.build_required}`}
                    </p>
                    <p>{formatStructureEffect(selectedStructure.type)}</p>
                  </>
                ) : (
                  <p>该地块尚未建造任何设施。</p>
                )}
                {selectedGraveMarkers.length > 0 ? (
                  <p>🪦 {formatGraveMarkers(selectedGraveMarkers)}</p>
                ) : null}
                {selectedGroundLootDrops.length > 0 ? (
                  <p>🎒 地面遗落：{formatGroundLootDrops(selectedGroundLootDrops)}</p>
                ) : null}
              </div>
              <div className="command-actions">
                <button type="button" className="action-button" onClick={() => setActivePanelID("unit")}>打开单位面板</button>
                <button type="button" className="action-button action-button-secondary" onClick={() => handleOpenUnitDialogueModal(selectedUnit.id)}>打开交谈面板</button>
                <button type="button" className="action-button action-button-secondary" onClick={() => setActivePanelID("inventory")}>打开背包面板</button>
                <button type="button" className="action-button action-button-secondary" onClick={() => setActivePanelID("thoughts")}>打开情报面板</button>
                {selectedUnit.faction_id === effectiveCommanderFactionID && selectedUnit.status.life_state === "active" ? (
                  <button
                    type="button"
                    className="action-button action-button-secondary"
                    disabled={eliteEncounterBusyUnitID === selectedUnit.id}
                    onClick={() => void handleEliteEncounter(selectedUnit.id)}
                    title="让她出门历练一次：多回合消耗战，胜则得战利品，败则负伤而归，结果落入命运收件箱"
                  >
                    {eliteEncounterBusyUnitID === selectedUnit.id ? "历练中…" : "遭遇 / 历练"}
                  </button>
                ) : null}
              </div>
            </aside>
            </>
          ) : null}
          {showHUD && !selectedUnit && tileDetailPopoverOpen && selectedTileCoord ? (
            <>
              <div
                className="unit-detail-backdrop"
                onClick={() => setTileDetailPopoverOpen(false)}
                aria-hidden="true"
              />
              <aside
                className="unit-detail-popover tile-detail-popover"
                role="dialog"
                aria-label={`地块 ${selectedTileCoord.q},${selectedTileCoord.r} 详细信息`}
            >
              <button
                type="button"
                className="unit-detail-close"
                onClick={() => setTileDetailPopoverOpen(false)}
                aria-label="关闭地块详情"
              >
                ×
              </button>
              <div className="unit-detail-hero">
                <div className="tile-detail-icon" aria-hidden="true">
                  {selectedStructure ? structureEmojiFor(selectedStructure.type) : terrainEmojiFor(selectedTerrain?.id)}
                </div>
                <div>
                  <p className="card-kicker">地块详情</p>
                  <h3>{selectedTerrain?.display_name ?? selectedTile?.terrain ?? "未知地形"}</h3>
                  <p className="unit-detail-meta">
                    坐标 {selectedTileCoord.q}, {selectedTileCoord.r}
                    {selectedTerrain ? ` · 移动成本 ${selectedTerrain.move_cost} · 视野 ${selectedTerrain.vision_range}` : ""}
                  </p>
                </div>
              </div>
              <div className="unit-detail-section">
                <span className="shop-label">地形效果</span>
                <p>{formatTerrainRuleSummary(selectedTerrain) || "暂无特殊规则。"}</p>
              </div>
              {selectedStructure ? (
                <div className="unit-detail-section">
                  <span className="shop-label">建筑信息</span>
                  <p>
                    <strong>
                      {structureEmojiFor(selectedStructure.type)} {formatStructureType(selectedStructure.type)}
                    </strong>
                    {" · "}
                    {selectedStructure.faction_id === (session?.player_faction_id ?? "")
                      ? "玩家阵营"
                      : "敌方阵营"}
                    {" · "}
                    {selectedStructure.completed
                      ? "已完成"
                      : `施工中 ${selectedStructure.build_progress}/${selectedStructure.build_required}`}
                  </p>
                  <p>{formatStructureEffect(selectedStructure.type)}</p>
                </div>
              ) : (
                <div className="unit-detail-section">
                  <span className="shop-label">建筑信息</span>
                  <p>该地块尚未建造任何设施。</p>
                </div>
              )}
              {selectedGraveMarkers.length > 0 ? (
                <div className="unit-detail-section">
                  <span className="shop-label">葬身之地</span>
                  <p>🪦 {formatGraveMarkers(selectedGraveMarkers)}</p>
                </div>
              ) : null}
              {selectedGroundLootDrops.length > 0 ? (
                <div className="unit-detail-section">
                  <span className="shop-label">地面遗落</span>
                  <p>🎒 {formatGroundLootDrops(selectedGroundLootDrops)}</p>
                  <p>单位站到该地块时，LLM 候选动作会出现“拾取”。遗落物 5 回合后自动消失。</p>
                </div>
              ) : null}
            </aside>
            </>
          ) : null}
          {showHUD && !selectedUnit && !activePanelID && liveActivityFeed.length > 0 ? (
            <aside className="activity-feed-overlay" aria-live="polite">
              <header className="activity-feed-header">
                <span>Activity Feed</span>
              </header>
              <div
                className="activity-feed-list"
                role="log"
                aria-label="Activity Feed"
                tabIndex={0}
                onWheel={(event) => event.stopPropagation()}
                onTouchMove={(event) => event.stopPropagation()}
              >
                {liveActivityFeed.map((entry) => (
                  <article
                    key={entry.id}
                    className={`activity-feed-entry activity-feed-entry-${entry.tone}`}
                  >
                    {parseDefianceTrace(entry.text) ? (
                      <DefianceCard reasoning={entry.text} />
                    ) : (
                      <p className="activity-feed-text">{entry.text}</p>
                    )}
                    <span className="activity-feed-meta">
                      T{entry.turn} · {entry.phase ? phaseLabels[entry.phase] : "--"}
                    </span>
                  </article>
                ))}
              </div>
            </aside>
          ) : null}
          {showHUD && fatePanelOpen && session ? (
            <FatePanel
              sessionId={session.id}
              units={controlledUnits.map((unit) => ({ id: unit.id, name: unit.identity.name }))}
              initialUnitID={selectedUnitID}
              onClose={() => setFatePanelOpen(false)}
            />
          ) : null}
          {/* 跨玩家同意收件箱：scoped 到当前选中角色（SessionSnapshot 无 world_id，故不传 worldId——组件会隐藏『惊动世界』按钮，收件箱仍可用）。*/}
          {showHUD && consentInboxOpen && selectedUnitID ? (
            <ConsentInbox
              unitId={selectedUnitID}
              unitName={selectedUnit?.identity.name}
              onClose={() => setConsentInboxOpen(false)}
            />
          ) : null}
          {/* 血仇网络面板：scoped 到当前选中角色，让 blood_feud 多跳传播对玩家可感知。*/}
          {showHUD && bloodFeudOpen && session && selectedUnitID ? (
            <BloodFeudPanel
              sessionID={session.id}
              unitID={selectedUnitID}
              unitName={selectedUnit?.identity.name}
              onClose={() => setBloodFeudOpen(false)}
            />
          ) : null}
          {/* 世界 Boss 协作 PvE（developer 门控；需本局已接入世界 world_id）。*/}
          {showHUD && developerMode && worldBossOpen && session?.world_id ? (
            <WorldBossPanel
              worldID={session.world_id}
              attackerCandidates={controlledUnits.map((unit) => ({ id: unit.id, name: unit.identity.name }))}
              onClose={() => setWorldBossOpen(false)}
            />
          ) : null}
          {/* 运营看板：跨会话成本 + 假门转化漏斗（developer 门控）。*/}
          {developerMode && opsDashboardOpen ? (
            <OpsDashboard onClose={() => setOpsDashboardOpen(false)} />
          ) : null}
          {/* 多层副本（玩家可达；后端 QUNXIANG_DUNGEON 关时面板提示未启用）。*/}
          {showHUD && dungeonOpen && session ? (
            <DungeonPanel
              sessionID={session.id}
              partyCandidates={controlledUnits.map((unit) => ({ id: unit.id, name: unit.identity.name }))}
              onClose={() => setDungeonOpen(false)}
            />
          ) : null}
          {/* 商业化 / 合规浮层（玩家可见，复用顶部入口触发）。*/}
          {billingPanelOverlay}
          {compliancePanelOverlay}
          {/* 合规 403 拦截横幅（建局/推进被门拦时引导实名/告知宵禁/防沉迷）。*/}
          {complianceBanner}
          {/* 举报弹窗（玩家可见，不受 developer 门，可针对当前选中角色）。*/}
          {reportDialogOpen ? (
            <ReportDialog
              sessionId={session.id}
              targetUnitId={selectedUnitID ?? undefined}
              reporter={accountUser?.display_name || accountUser?.username || ""}
              onClose={() => setReportDialogOpen(false)}
            />
          ) : null}
          {/* 运营/开发者：审计·举报管理台 + 隐私擦除（developer 门控）。*/}
          {developerMode && governancePanelOpen ? (
            <GovernancePanel
              sessionId={session.id}
              onClose={() => setGovernancePanelOpen(false)}
              onOpenPrivacyErase={() => setPrivacyEraseOpen(true)}
            />
          ) : null}
          {developerMode && privacyEraseOpen ? (
            <PrivacyEraseDialog sessionId={session.id} onClose={() => setPrivacyEraseOpen(false)} />
          ) : null}
          {/* 组队 PvE（野外 Boss）结果弹层。*/}
          {showHUD && fieldBossResult ? (
            <>
              <div
                className="unit-detail-backdrop"
                onClick={() => setFieldBossResult(null)}
                aria-hidden="true"
              />
              <aside className="unit-detail-popover" role="dialog" aria-label="组队 PvE 结果">
                <button
                  type="button"
                  className="unit-detail-close"
                  onClick={() => setFieldBossResult(null)}
                  aria-label="关闭组队结果"
                >
                  ×
                </button>
                <div className="unit-detail-hero">
                  <div>
                    <p className="card-kicker">组队 / 野外强敌</p>
                    <h3>{fieldBossResult.Victory ? "组队告捷" : "组队折戟"}</h3>
                    <p className="unit-detail-meta">
                      威胁 {fieldBossResult.ThreatID || "未知"} · {fieldBossResult.Rounds} 回合 ·{" "}
                      {(fieldBossResult.Members?.length ?? 0)} 人出战
                    </p>
                  </div>
                </div>
                {fieldBossResult.Members && fieldBossResult.Members.length > 0 ? (
                  <div className="unit-detail-section">
                    <span className="shop-label">各队员结算</span>
                    {fieldBossResult.Members.map((member, index) => {
                      const memberUnit = allUnits.find((u) => u.id === member.UnitID);
                      const memberName = memberUnit?.identity.name ?? member.UnitID;
                      return (
                        <div key={`${member.UnitID}-${index}`} style={{ marginBottom: 8 }}>
                          <p style={{ margin: 0 }}>
                            <strong>{memberName}</strong> · {member.Outcome} · 贡献 {member.Contribution}
                            {member.PenaltyLayer > 0 ? ` · 后果第 ${member.PenaltyLayer} 层` : ""}
                          </p>
                          {member.Awards && member.Awards.length > 0 ? (
                            <div className="inventory-chip-list">
                              {member.Awards.map((award, awardIndex) => (
                                <span key={`${award.ItemID}-${awardIndex}`} className="inventory-chip">
                                  {award.ItemID} ×{award.Quantity}
                                </span>
                              ))}
                            </div>
                          ) : null}
                          {member.InboxCard ? (
                            <p style={{ margin: "2px 0 0", opacity: 0.85 }}>{member.InboxCard}</p>
                          ) : null}
                        </div>
                      );
                    })}
                  </div>
                ) : null}
              </aside>
            </>
          ) : null}
          {/* 组队 PvE 选人弹窗（多选队员→ resolveFieldBoss）。*/}
          {showHUD && fieldBossModalOpen ? (
            <>
              <div
                className="unit-detail-backdrop"
                onClick={() => setFieldBossModalOpen(false)}
                aria-hidden="true"
              />
              <aside className="unit-detail-popover" role="dialog" aria-label="组队出战选人">
                <button
                  type="button"
                  className="unit-detail-close"
                  onClick={() => setFieldBossModalOpen(false)}
                  aria-label="关闭组队选人"
                >
                  ×
                </button>
                <div className="unit-detail-hero">
                  <div>
                    <p className="card-kicker">组队 / PvE</p>
                    <h3>组队挑战野外强敌</h3>
                    <p className="unit-detail-meta">勾选要结伴出战的队员，按贡献分赃；失败各担后果。</p>
                  </div>
                </div>
                <div className="unit-detail-section">
                  {controlledUnits.length === 0 ? (
                    <p>当前没有可出战的队员。</p>
                  ) : (
                    controlledUnits.map((unit) => (
                      <label key={unit.id} style={{ display: "flex", alignItems: "center", gap: 8, padding: "4px 0", cursor: "pointer" }}>
                        <input
                          type="checkbox"
                          checked={fieldBossSelectionIDs.includes(unit.id)}
                          onChange={() => toggleFieldBossMember(unit.id)}
                          disabled={fieldBossBusy}
                        />
                        <span>{unit.identity.name}（HP {unit.status.hp}）</span>
                      </label>
                    ))
                  )}
                </div>
                <button
                  type="button"
                  className="action-button action-button-primary"
                  disabled={fieldBossBusy || fieldBossSelectionIDs.length === 0}
                  onClick={() => {
                    void handleFieldBoss();
                  }}
                >
                  {fieldBossBusy ? "结算中…" : `出战（${fieldBossSelectionIDs.length} 人）`}
                </button>
              </aside>
            </>
          ) : null}
          {showHUD && eliteEncounterResult ? (
            <>
              <div
                className="unit-detail-backdrop"
                onClick={() => setEliteEncounterResult(null)}
                aria-hidden="true"
              />
              <aside className="unit-detail-popover" role="dialog" aria-label="历练结果">
                <button
                  type="button"
                  className="unit-detail-close"
                  onClick={() => setEliteEncounterResult(null)}
                  aria-label="关闭历练结果"
                >
                  ×
                </button>
                <div className="unit-detail-hero">
                  <div>
                    <p className="card-kicker">遭遇 / 历练</p>
                    <h3>
                      {eliteEncounterResult.Outcome === "defeated"
                        ? "全身而退"
                        : eliteEncounterResult.Outcome === "fled"
                          ? "且战且退"
                          : "负伤而归"}
                    </h3>
                    <p className="unit-detail-meta">
                      {eliteEncounterResult.Rounds} 回合 · 造成 {eliteEncounterResult.DamageDealt} · 承受{" "}
                      {eliteEncounterResult.DamageTaken}
                    </p>
                  </div>
                </div>
                {eliteEncounterResult.Awards && eliteEncounterResult.Awards.length > 0 ? (
                  <div className="unit-detail-section">
                    <span className="shop-label">战利品</span>
                    <div className="inventory-chip-list">
                      {eliteEncounterResult.Awards.map((award, index) => (
                        <span key={`${award.ItemID}-${index}`} className="inventory-chip">
                          {award.ItemID} ×{award.Quantity}
                        </span>
                      ))}
                    </div>
                  </div>
                ) : null}
                {eliteEncounterResult.PenaltyLayer > 0 ? (
                  <div className="unit-detail-section">
                    <span className="shop-label">代价</span>
                    <p>她受了挫，后果分级落到第 {eliteEncounterResult.PenaltyLayer} 层。</p>
                  </div>
                ) : null}
                {eliteEncounterResult.InboxCard ? (
                  <div className="unit-detail-section">
                    <span className="shop-label">祖魂托梦</span>
                    <p>{eliteEncounterResult.InboxCard}</p>
                  </div>
                ) : null}
              </aside>
            </>
          ) : null}
          {showHUD && developerMode ? (
            <>
              <div className={`floating-toolbar ${selectedUnit ? "floating-toolbar-with-unit-summary" : ""}`}>
                {floatingPanels.map((panel) => (
                  <button
                    key={panel.id}
                    type="button"
                    className={`floating-toolbar-button ${activePanelID === panel.id ? "floating-toolbar-button-active" : ""}`}
                    onClick={() => handleFloatingPanelToggle(panel.id)}
                    title={`${panel.label} (${panel.hotkey})`}
                    aria-label={`${panel.label}（快捷键 ${panel.hotkey}）`}
                  >
                    <span className="floating-toolbar-button-code">{panel.short}</span>
                    <span className="floating-toolbar-hotkey">{panel.hotkey}</span>
                    <span className="floating-toolbar-tooltip">
                      {panel.label} ({panel.hotkey})
                    </span>
                  </button>
                ))}
              </div>
              <button
                type="button"
                className="action-button action-button-primary phase-advance-button"
                disabled={!canRequestAdvancePhase}
                onClick={() => void handleAdvancePhase()}
                title={advancePhaseButtonTitle}
              >
                {advanceButtonLabel(session, currentFactionReady)}
              </button>
            </>
          ) : null}
          {showShortcutHelp ? (
            <div className="shortcut-help-overlay" onClick={() => setShowShortcutHelp(false)}>
              <section className="shortcut-help-card" onClick={(event) => event.stopPropagation()}>
                <div className="panel-header">
                  <div>
                    <p className="card-kicker">Hotkeys</p>
                    <h2>快捷键</h2>
                  </div>
                </div>
                <ul className="shortcut-help-list">
                  <li>
                    <strong>1-0</strong> 打开/收起右上对应浮窗
                  </li>
                  <li>
                    <strong>H</strong> 显示/隐藏 HUD
                  </li>
                  <li>
                    <strong>?</strong> 显示/隐藏快捷键帮助
                  </li>
                  <li>
                    <strong>ESC</strong> 关闭浮窗和帮助层
                  </li>
                  <li>
                    <strong>点击地图空白</strong> 关闭当前浮窗
                  </li>
                </ul>
                <button type="button" className="action-button" onClick={() => setShowShortcutHelp(false)}>
                  关闭
                </button>
              </section>
            </div>
          ) : null}
          {gameGuideOpen ? (
            <div className="command-modal-overlay game-guide-overlay" onClick={() => setGameGuideOpen(false)}>
              <section
                className="command-modal-card game-guide-card"
                role="dialog"
                aria-modal="true"
                aria-labelledby="game-guide-title"
                onClick={(event) => event.stopPropagation()}
              >
                <button
                  type="button"
                  className="unit-detail-close"
                  onClick={() => setGameGuideOpen(false)}
                  aria-label="关闭游戏攻略"
                >
                  ×
                </button>
                <div className="game-guide-hero">
                  <div>
                    <p className="card-kicker">Game Guide</p>
                    <h2 id="game-guide-title">游戏攻略：怎么玩《群像》</h2>
                    <p className="panel-note">
                      这是一份给新玩家的完整说明：先理解世界、回合、自然语言指挥，再学会看地图、照顾单位、复盘战报。
                    </p>
                  </div>
                  <span className="mini-pill">随时可看</span>
                </div>

                <div className="game-guide-quickstart" aria-label="快速上手">
                  <article>
                    <strong>1. 部署阶段写方针</strong>
                    <span>说明全队优先级：推进、集火、补给、撤退或建设。</span>
                  </article>
                  <article>
                    <strong>2. 点执行推进回合</strong>
                    <span>单位会根据候选动作、性格、记忆和环境自主行动。</span>
                  </article>
                  <article>
                    <strong>3. 看战报再修正</strong>
                    <span>下一回合根据伤亡、位置、饥饿和发现继续下令。</span>
                  </article>
                </div>

                <div className="game-guide-content">
                  {gameGuideSections.map((section) => (
                    <article key={section.title} className="game-guide-section">
                      <h3>{section.title}</h3>
                      {section.body.map((paragraph) => (
                        <p key={paragraph}>{paragraph}</p>
                      ))}
                      {section.tips && section.tips.length > 0 ? (
                        <ul className="game-guide-tip-list">
                          {section.tips.map((tip) => (
                            <li key={tip}>{tip}</li>
                          ))}
                        </ul>
                      ) : null}
                    </article>
                  ))}
                </div>

                <div className="game-guide-footer">
                  <span>快捷入口：左侧“攻略”按钮。按 ESC 或点击空白处关闭。</span>
                  <button type="button" className="action-button action-button-primary" onClick={() => setGameGuideOpen(false)}>
                    开始指挥
                  </button>
                </div>
              </section>
            </div>
          ) : null}
          {hallArchiveModalOpen && session && session.outcome !== "ongoing" ? (
            <div className="command-modal-overlay hall-archive-overlay" onClick={() => setHallArchiveModalOpen(false)}>
              <section
                className="command-modal-card hall-archive-card"
                role="dialog"
                aria-modal="true"
                aria-labelledby="hall-archive-title"
                onClick={(event) => event.stopPropagation()}
              >
                <div className="panel-header">
                  <div>
                    <p className="card-kicker">Archivist</p>
                    <h2 id="hall-archive-title">战后档案官</h2>
                  </div>
                  <span className={`hero-tag hero-tag-${session.outcome}`}>{outcomeLabels[session.outcome]}</span>
                </div>
                <p className="panel-note">
                  战斗结束后统一展示本局写入殿堂的幸存者档案。之后也可以从“战报”面板重新查看。
                </p>
                {hallArchiveEntries.length === 0 ? (
                  <p className="empty-state">战后档案仍在整理中，请稍后刷新快照。</p>
                ) : (
                  <div className="hall-archive-list">
                    {hallArchiveEntries.map((entry) => (
                      <article key={entry.id} className="hall-archive-entry">
                        <div className="hall-archive-entry-header">
                          <div>
                            <span className="log-turn">
                              {entry.faction_id === session.player_faction_id ? "玩家阵营" : "敌方阵营"}
                            </span>
                            <h3>{entry.unit_name}</h3>
                          </div>
                          {entry.used_fallback ? <span className="inventory-chip">本地模板</span> : null}
                        </div>
                        <p>{entry.biography}</p>
                        {entry.top_events && entry.top_events.length > 0 ? (
                          <ul className="hall-archive-events">
                            {entry.top_events.slice(0, 3).map((eventText, index) => (
                              <li key={`${entry.id}-event-${index}`}>{eventText}</li>
                            ))}
                          </ul>
                        ) : null}
                        <button
                          type="button"
                          className="action-button action-button-secondary hall-archive-share-btn"
                          onClick={() => {
                            const lines = [
                              `【${entry.unit_name} 的一生】`,
                              entry.biography,
                              ...(entry.top_events ?? []).slice(0, 3).map((e) => `· ${e}`),
                            ].filter((s) => s && s.trim() !== "");
                            void copyTextToClipboard(lines.join("\n"));
                            void emitClientAnalytics("share_initiated", { source: "hall_archive" });
                            void trackFunnel("share_initiated", { source: "hall_archive" });
                            setMessage(`已复制 ${entry.unit_name} 的传记，可以分享给别人了。`);
                          }}
                        >
                          分享 TA 的一生
                        </button>
                      </article>
                    ))}
                  </div>
                )}
                <div className="command-actions">
                  <button type="button" className="action-button action-button-primary" onClick={() => setHallArchiveModalOpen(false)}>
                    收起档案
                  </button>
                  <button
                    type="button"
                    className="action-button action-button-secondary"
                    onClick={() => {
                      setHallArchiveModalOpen(false);
                      setActivePanelID("battleReport");
                    }}
                  >
                    打开战报面板
                  </button>
                </div>
              </section>
            </div>
          ) : null}
          {unitDialogueModalPair && session ? (
            <div className="command-modal-overlay unit-dialogue-modal-overlay" onClick={() => setUnitDialogueModalPair(null)}>
              <section
                className="command-modal-card unit-dialogue-modal-card"
                role="dialog"
                aria-modal="true"
                aria-labelledby="unit-dialogue-modal-title"
                onClick={(event) => event.stopPropagation()}
              >
                <button
                  type="button"
                  className="unit-detail-close"
                  onClick={() => setUnitDialogueModalPair(null)}
                  aria-label="关闭单位交谈记录"
                >
                  ×
                </button>
                <div className="unit-dialogue-modal-hero">
                  <div className="unit-dialogue-modal-avatar-pair" aria-hidden="true">
                    {unitDialogueModalUnit ? (
                      <img
                        className="unit-dialogue-modal-avatar"
                        src={portraitURLForUnit(unitDialogueModalUnit)}
                        alt=""
                        onError={(event) => {
                          event.currentTarget.onerror = null;
                          event.currentTarget.src = portraitFallbackURLForUnit(unitDialogueModalUnit);
                        }}
                      />
                    ) : (
                      <div className="unit-dialogue-modal-avatar unit-dialogue-modal-avatar-empty">？</div>
                    )}
                    {unitDialogueModalPartnerUnit ? (
                      <img
                        className="unit-dialogue-modal-avatar"
                        src={portraitURLForUnit(unitDialogueModalPartnerUnit)}
                        alt=""
                        onError={(event) => {
                          event.currentTarget.onerror = null;
                          event.currentTarget.src = portraitFallbackURLForUnit(unitDialogueModalPartnerUnit);
                        }}
                      />
                    ) : (
                      <div className="unit-dialogue-modal-avatar unit-dialogue-modal-avatar-empty">？</div>
                    )}
                  </div>
                  <div>
                    <p className="card-kicker">Unit Dialogue</p>
                    <h2 id="unit-dialogue-modal-title">
                      {unitDialogueModalUnit?.identity.name ?? findUnitName(session, unitDialogueModalPair.unitID)} ↔ {unitDialogueModalPartnerUnit?.identity.name ?? findUnitName(session, unitDialogueModalPair.partnerUnitID)}
                    </h2>
                    <p className="panel-note">
                      这里展示这两名单位之间跨多个回合的单位交谈与表白记录；玩家和单位的聊天仍保留在左侧“与单位交谈”面板。
                    </p>
                  </div>
                </div>

                {unitDialogueModalThreads.length === 0 ? (
                  <p className="empty-state">暂未找到这两名单位之间的单位间对话记录。</p>
                ) : (
                  <div className="unit-dialogue-modal-list">
                    {unitDialogueModalThreads.map((thread) => {
                      const leftName = findUnitName(session, thread.actorUnitID);
                      const rightName = findUnitName(session, thread.targetUnitID);
                      return (
                        <article key={`unit-dialogue-modal-${thread.id}`} className="dialogue-entry dialogue-entry-thread unit-dialogue-modal-entry">
                          <div className="thought-head">
                            <strong>{leftName} ↔ {rightName}</strong>
                            <span className="log-turn">
                              T{thread.turn} · {phaseLabels[thread.phase]} · {formatTurnsAgo(thread.turn, session.turn_state.turn)}
                            </span>
                          </div>
                          <p className="dialogue-thread-summary">{thread.summary}</p>
                          {thread.lines.length > 0 ? (
                            <div className="chat-transcript chat-transcript-thread">
                              {thread.lines.map((line) => {
                                const rightSide = line.unitID === unitDialogueModalPair.partnerUnitID;
                                const speakerName = findUnitName(session, line.unitID) || displayDialogueSpeaker(line.speaker);
                                return (
                                  <article
                                    key={`${thread.id}-${line.id}-${line.unitID}`}
                                    className={`chat-message ${rightSide ? "chat-message-right" : "chat-message-left"}`}
                                  >
                                    <div className={`chat-meta ${rightSide ? "chat-meta-right" : "chat-meta-left"}`}>
                                      <span>{speakerName}</span>
                                      <span>{formatDialogueTimestamp(line.occurredAt, line.turn, line.phase, session.turn_state.turn)}</span>
                                    </div>
                                    <div
                                      className={`chat-bubble ${rightSide ? "chat-bubble-right" : "chat-bubble-left"} ${
                                        line.unitID === unitDialogueModalPair.unitID ? "chat-bubble-selected" : ""
                                      }`}
                                    >
                                      <p>{line.message}</p>
                                    </div>
                                  </article>
                                );
                              })}
                            </div>
                          ) : null}
                        </article>
                      );
                    })}
                  </div>
                )}
              </section>
            </div>
          ) : null}
          {deploymentTaskModalOpen ? (
            <div className="command-modal-overlay" onClick={() => setDeploymentTaskModalOpen(false)}>
              <section
                className="command-modal-card"
                role="dialog"
                aria-modal="true"
                aria-labelledby="deployment-command-title"
                onClick={(event) => event.stopPropagation()}
              >
                <div className="panel-header">
                  <div>
                    <p className="card-kicker">Deployment Command</p>
                    <h2 id="deployment-command-title">
                      {session?.turn_state.phase === "execution" ? "预设下一回合方针" : "部署阶段总方针"}
                    </h2>
                  </div>
                </div>
                <p className="panel-note">
                  {session?.turn_state.phase === "execution"
                    ? "执行阶段提交的总方针会预设到下一回合，当前执行中的单位不会被中途改写。"
                    : "部署阶段可多次打开并修改总方针；最后一次提交的内容会作为本回合生效方针。"}
                </p>
                <label className="input-block">
                  <span className="shop-label">
                    {session?.turn_state.phase === "execution" ? "下回合总方针（全队）" : "总方针（全队）"}
                  </span>
                  <textarea
                    className="text-input text-area"
                    value={directiveDraft}
                    onChange={(event) => {
                      setDirectiveDraft(event.target.value);
                      setDeploymentDoctrineConfirmed(false);
                    }}
                    disabled={busy || session?.outcome !== "ongoing"}
                    autoFocus
                    placeholder={
                      session?.turn_state.phase === "execution"
                        ? "例如：下一回合先整备装备，再稳步推进。"
                        : "例如：稳住正面，避免孤军深入。"
                    }
                  />
                </label>
                <div className="command-actions">
                  <button
                    type="button"
                    className="action-button action-button-primary"
                    disabled={
                      busy ||
                      session?.outcome !== "ongoing" ||
                      !directiveDraft.trim()
                    }
                    onClick={() => void handleDeploymentModalSubmit()}
                  >
                    {session?.turn_state.phase === "execution" ? "预设下回合方针" : "保存总方针"}
                  </button>
                  {session?.turn_state.phase === "deployment" ? (
                    deploymentDoctrineConfirmed ? (
                      <button
                        type="button"
                        className="action-button action-button-primary"
                        disabled={!canRequestAdvancePhase}
                        onClick={() => void handleAdvancePhase()}
                        title={advancePhaseButtonTitle}
                      >
                        开始执行
                      </button>
                    ) : (
                      <button
                        type="button"
                        className="action-button action-button-secondary"
                        disabled={busy || session.outcome !== "ongoing" || !directiveDraft.trim()}
                        onClick={() => void handleDeploymentDoctrineConfirm()}
                      >
                        确定方针
                      </button>
                    )
                  ) : null}
                  <button
                    type="button"
                    className="action-button action-button-secondary"
                    disabled={busy}
                    onClick={() => setDeploymentTaskModalOpen(false)}
                  >
                    取消
                  </button>
                </div>
                <label className="deployment-intro-skip">
                  <input
                    type="checkbox"
                    checked={deploymentIntroSkip}
                    onChange={(event) => {
                      const next = event.target.checked;
                      setDeploymentIntroSkip(next);
                      writeDeploymentIntroSkipToStorage(next);
                    }}
                  />
                  <span>下次进入部署阶段不再自动弹出</span>
                </label>
              </section>
            </div>
          ) : null}
          {activePanelID ? (
            <div className="floating-panel-wrap">
              <div className="floating-panel-backdrop" onClick={() => setActivePanelID(null)} />
              <aside className={`floating-panel-shell ${activePanelID === "chat" ? "floating-panel-shell-chat" : ""} ${activePanelID === "llmTrace" ? "floating-panel-shell-settings" : ""}`}>
                <button
                  type="button"
                  className="floating-panel-close"
                  onClick={() => setActivePanelID(null)}
                  aria-label="关闭浮窗"
                >
                  关闭
                </button>
                {activePanelID === "overview" ? (
                  <>
                    <header className="hero">
                      <div className="hero-copy">
                        <p className="eyebrow">Qunxiang Prototype</p>
                        <h1>自然语言指挥原型</h1>
                        <p className="subtitle">
                          玩家现在只通过自然语言做三档指挥：战略方针、回合任务、执行阶段即时令；部署阶段也可点名和 AI 单位对话。装备调拨、金币转移、
                          物资交换、是否进食、生产建造与战斗动作都不再手动点击，而是由单位结合周围环境、性格、记忆、方针和最近对话，
                          自主调用 AI 做决定并执行；头顶气泡和最近记忆也由单位自己产出。
                        </p>
                      </div>
                      <div className={`hero-tag hero-tag-${session?.outcome ?? "ongoing"}`}>
                        {session ? outcomeLabels[session.outcome] : "初始化中"}
                      </div>
                    </header>

                    <section className="global-params-overview" aria-label="全局参数总览">
                      <div className="panel-header">
                        <div>
                          <p className="card-kicker">Global Params</p>
                          <h2>全局参数</h2>
                        </div>
                        <span className="mini-pill">实时快照</span>
                      </div>
                    </section>

                    <section className="phase-strip">
                      <article className="phase-card">
                        <span className="phase-label">当前回合</span>
                        <strong className="phase-value">{session?.turn_state.turn ?? "--"}</strong>
                      </article>
                      <article className="phase-card">
                        <span className="phase-label">当前阶段</span>
                        <strong className="phase-value">{session ? phaseLabels[session.turn_state.phase] : "--"}</strong>
                        {session?.turn_state.phase === "deployment" || session?.turn_state.phase === "execution" ? (
                          <>
                            <button
                              type="button"
                              className="phase-card-button"
                              disabled={session?.outcome !== "ongoing"}
                              onClick={openDeploymentCommandModal}
                            >
                              {session.turn_state.phase === "execution" ? "预设下回合" : "打开发指令"}
                            </button>
                            <button
                              type="button"
                              className="phase-card-button phase-card-button-primary"
                              disabled={!canRequestAdvancePhase}
                              onClick={() => void handleAdvancePhase()}
                              title={advancePhaseButtonTitle}
                            >
                              {session.turn_state.phase === "deployment" ? "开始执行" : "下一回合"}
                            </button>
                          </>
                        ) : null}
                      </article>
                      <article className="phase-card">
                        <span className="phase-label">阶段倒计时</span>
                        <strong className="phase-value">{phaseRemainingText}</strong>
                      </article>
                      <article className="phase-card">
                        <span className="phase-label">胜负状态</span>
                        <strong className="phase-value">{session ? outcomeLabels[session.outcome] : "--"}</strong>
                      </article>
                      <article className="phase-card">
                        <span className="phase-label">胜利条件</span>
                        <strong className="phase-value">敌方全灭</strong>
                        <span className="phase-note">失败条件：己方全灭</span>
                      </article>
                      <article className="phase-card">
                        <span className="phase-label">当前天气</span>
                        <strong className="phase-value">{session?.weather?.display_name ?? "--"}</strong>
                        {session?.weather?.note ? <span className="phase-note">{session.weather.note}</span> : null}
                      </article>
                      <article className="phase-card">
                        <span className="phase-label">地图剧本</span>
                        <strong className="phase-value">{session?.map_script_name ?? "--"}</strong>
                        <span className="phase-note">{session?.map_size_name ?? session?.map_size_id ?? "地图规模未设置"}</span>
                      </article>
                      <article className="phase-card">
                        <span className="phase-label">迷雾机制</span>
                        <strong className="phase-value">{session ? (session.fog_of_war_enabled ? "有雾" : "无雾") : "--"}</strong>
                      </article>
                      <article className="phase-card">
                        <span className="phase-label">跨势力事件</span>
                        <strong className="phase-value">{session ? `${crossFactionInteractions}` : "--"}</strong>
                      </article>
                      <article className="phase-card">
                        <span className="phase-label">LLM 估算成本</span>
                        <strong className="phase-value">
                          {session ? `$${llmCostUSD.toFixed(4)}${llmGuardrailActive ? " · 护栏中" : ""}` : "--"}
                        </strong>
                      </article>
                      <article className="phase-card">
                        <span className="phase-label">LLM Tokens</span>
                        <strong className="phase-value">{session ? `${llmTotalTokens}` : "--"}</strong>
                      </article>
                    </section>
                  </>
                ) : null}
                {activePanelID === "command" ? (
                  <section className="panel-card">
                    <div className="panel-header">
                      <div>
                        <p className="card-kicker">Command</p>
                        <h2>方针与对话</h2>
                      </div>
                      <span className="mini-pill">{busy ? "处理中" : "就绪"}</span>
                    </div>
                    <p className="panel-note">{message}</p>
                    {displayExecutionFeed.length > 0 ? (
                      <div className="execution-feed">
                        {displayExecutionFeed.map((entry) => (
                          <article
                            key={entry.id}
                            className={`execution-feed-entry ${
                              entry.status === "completed"
                                ? "execution-feed-entry-completed"
                                : "execution-feed-entry-started"
                            }`}
                          >
                            <span className="execution-feed-line">{formatExecutionFeedLine(entry)}</span>
                            <span className="execution-feed-meta">
                              T{entry.turn} · {entry.phase ? phaseLabels[entry.phase] : "--"}
                            </span>
                          </article>
                        ))}
                      </div>
                    ) : null}

                    <div className="command-summary">
                      <span className="shop-label">当前控制阵营</span>
                      <strong>
                        {session
                          ? effectiveCommanderFactionID === session.player_faction_id
                            ? "我方（原己方阵营）"
                            : "我方（原敌方阵营）"
                          : "--"}
                      </strong>
                    </div>

                    <label className="input-block">
                      <span className="shop-label">全局方针（部署阶段，作用于我方）</span>
                      <textarea
                        className="text-input text-area"
                        value={directiveDraft}
                        onChange={(event) => setDirectiveDraft(event.target.value)}
                        disabled={busy || session?.outcome !== "ongoing"}
                        placeholder="例如：先保全队伍，再寻找落单目标逐步推进。"
                      />
                      {session?.turn_state.phase !== "deployment" && session?.outcome === "ongoing" ? (
                        <span className="panel-note">
                          {session?.turn_state.phase === "execution"
                            ? "当前是执行阶段，提交后会预设为下一回合部署方针。"
                            : `当前是${session ? phaseLabels[session.turn_state.phase] : "--"}，可以先编辑草稿。`}
                        </span>
                      ) : null}
                    </label>

                    <div className="unit-grid">
                      <div>
                        <span className="shop-label">指挥力</span>
                        <strong>
                          {session ? `${session.command_power.current}/${session.command_power.max}` : "--"}
                        </strong>
                      </div>
                      <div>
                        <span className="shop-label">每回合恢复</span>
                        <strong>{session ? `${session.command_power.regen}` : "--"}</strong>
                      </div>
                    </div>

                    <label className="input-block">
                      <span className="shop-label">任务指令（部署）</span>
                      <GameSelect
                        value={taskTargetUnitID}
                        onChange={setTaskTargetUnitID}
                        disabled={busy || session?.outcome !== "ongoing"}
                        ariaLabel="选择任务指令目标"
                        options={[
                          { value: "", label: "全队" },
                          ...controlledUnits.map((record) => ({ value: record.id, label: record.identity.name })),
                        ]}
                      />
                      <textarea
                        className="text-input text-area-compact"
                        value={taskDraft}
                        onChange={(event) => setTaskDraft(event.target.value)}
                        disabled={
                          busy ||
                          session?.outcome !== "ongoing" ||
                          (session?.turn_state.phase !== "deployment" &&
                            session?.turn_state.phase !== "execution")
                        }
                        placeholder="例如：折棠本回合去东侧侦察，避免硬拼。"
                      />
                    </label>

                    <button
                      type="button"
                      className="action-button"
                      disabled={
                        busy ||
                        !taskDraft.trim() ||
                        session?.outcome !== "ongoing" ||
                        (session?.turn_state.phase !== "deployment" &&
                          session?.turn_state.phase !== "execution")
                      }
                      onClick={handleTaskActionClick}
                    >
                      {session?.turn_state.phase === "execution"
                        ? "预设下回合"
                        : session?.turn_state.phase === "deployment"
                          ? "发布任务"
                          : "发布任务"}
                    </button>

                    <label className="input-block">
                      <span className="shop-label">即时令（执行阶段，消耗指挥力）</span>
                      <GameSelect
                        value={orderTargetUnitID}
                        onChange={setOrderTargetUnitID}
                        disabled={busy || session?.outcome !== "ongoing"}
                        ariaLabel="选择即时令目标"
                        options={controlledUnits.map((record) => ({ value: record.id, label: record.identity.name }))}
                      />
                      <textarea
                        className="text-input text-area-compact"
                        value={orderDraft}
                        onChange={(event) => setOrderDraft(event.target.value)}
                        disabled={
                          busy ||
                          session?.outcome !== "ongoing" ||
                          session?.turn_state.phase !== "execution" ||
                          !orderTargetUnitID
                        }
                        placeholder="例如：立刻后撤到掩体后，保命优先。"
                      />
                    </label>

                    <button
                      type="button"
                      className="action-button"
                      disabled={
                        busy ||
                        !orderDraft.trim() ||
                        session?.outcome !== "ongoing" ||
                        session?.turn_state.phase !== "execution" ||
                        !orderTargetUnitID ||
                        !session ||
                        session.command_power.current < session.command_power.order_cost
                      }
                      onClick={() => void handleImmediateOrderSubmit()}
                    >
                      下达即时令
                    </button>

                    <div className="command-actions">
                    <button
                      type="button"
                      className="action-button action-button-primary"
                        disabled={busy || session?.outcome !== "ongoing" || !directiveDraft.trim()}
                      onClick={() => void handleDirectiveSubmit()}
                    >
                        {session?.turn_state.phase === "execution" ? "预设下回合方针" : "保存方针"}
                    </button>
                      
                      <button
                        type="button"
                        className="action-button action-button-secondary"
                        disabled={busy}
                        onClick={() => void handleRestart()}
                      >
                        重开一局
                      </button>
                    </div>

                    <ul className="legend-list">
                      <li>战场六边格以地形贴图为主，只给城市/村庄/废墟显示文字，右上角图例保留地形计数。</li>
                      <li>玩家可发三档自然语言指令：方针（战略）、任务（战略/部署）、即时令（执行，消耗指挥力）。</li>
                      <li>部署阶段可和任意存活 AI 单位对话；输入文字后“发送对话”才会点亮，邻近单位的调拨、交换、购买都由 AI 自己完成。</li>
                      <li>执行阶段禁止中途打断；单位会自己判断是否进食、采集、种地、施工或交战。</li>
                      <li>回合战报已暂时关闭；设置面板仍会保留并展示本局全部 LLM 交互记录。</li>
                      <li>玩家不是逐单位遥控器，单位会基于环境、性格与记忆自行判断，可能执行、偏离或抗命。</li>
                    </ul>
                  </section>
                ) : null}
                {activePanelID === "chat" ? (
                  <section className="panel-card chat-panel-card">
                    <div className="panel-header">
                      <div>
                        <p className="card-kicker">Unit Chat</p>
                        <h2>与单位交谈</h2>
                      </div>
                      <span className="mini-pill">{busy ? "等待回复" : canChatInCurrentPhase ? "部署聊天" : "仅可查看"}</span>
                    </div>
                    <p className="panel-note">选择一个单位后像聊天一样输入；只能在部署阶段发送。执行阶段可继续编辑全局方针，内容会用于下一回合。</p>

                    <div className="chat-layout">
                      <div className="chat-contact-list" aria-label="可交谈单位列表">
                        {chatUnits.length === 0 ? (
                          <p className="summary-note">暂无可显示单位。</p>
                        ) : (
                          chatUnits.map((unit) => (
                            <button
                              key={`chat-contact-${unit.id}`}
                              type="button"
                              className={`chat-contact ${chatTargetUnit?.id === unit.id ? "chat-contact-active" : ""} ${unit.status.life_state === "dead" ? "chat-contact-dead" : ""}`}
                              onClick={() => setChatTargetUnitID(unit.id)}
                            >
                              <img
                                src={portraitURLForUnit(unit)}
                                alt=""
                                onError={(event) => {
                                  event.currentTarget.onerror = null;
                                  event.currentTarget.src = portraitFallbackURLForUnit(unit);
                                }}
                              />
                              <span>
                                <strong>{unit.identity.name}</strong>
                                <small>
                                  {unit.faction_id === effectiveCommanderFactionID ? "我方" : "对方"} · {formatChatUnitState(unit)}
                                </small>
                              </span>
                            </button>
                          ))
                        )}
                      </div>

                      <div className="chat-window">
                        {chatTargetUnit ? (
                          <div className="chat-window-header">
                            <img
                              src={portraitURLForUnit(chatTargetUnit)}
                              alt=""
                              onError={(event) => {
                                event.currentTarget.onerror = null;
                                event.currentTarget.src = portraitFallbackURLForUnit(chatTargetUnit);
                              }}
                            />
                            <div>
                              <strong>{chatTargetUnit.identity.name}</strong>
                              <span>
                                {chatTargetUnit.faction_id === effectiveCommanderFactionID ? "我方单位" : "其他阵营"} · {formatChatUnitState(chatTargetUnit)}
                              </span>
                            </div>
                          </div>
                        ) : null}

                        <div className="chat-message-list" aria-live="polite">
                          {!chatTargetUnit ? (
                            <p className="empty-state">先从左侧选择一个单位。</p>
                          ) : chatTargetDead && chatMessages.length === 0 ? (
                            <p className="empty-state">这个单位已死亡，只能查看旧聊天记录。</p>
                          ) : chatMessages.length === 0 ? (
                            <p className="empty-state">还没有聊天记录，先打个招呼。</p>
                          ) : (
                            chatMessages.map((entry) => {
                              const mine = entry.speaker === "player" || entry.speaker === "玩家" || entry.speaker === "Commander";
                              return (
                                <article key={entry.id} className={`chat-message ${mine ? "chat-message-mine" : "chat-message-unit"}`}>
                                  <p>{entry.message}</p>
                                  <span>{entry.speaker} · T{entry.turn}</span>
                                </article>
                              );
                            })
                          )}
                        </div>

                        <div className="chat-input-row">
                          <textarea
                            className="text-input chat-input"
                            value={dialogueDraft}
                            onChange={(event) => setDialogueDraft(event.target.value)}
                            disabled={!canSendChat}
                            placeholder={
                              chatTargetDead
                                ? "该单位已死亡，只能查看历史聊天"
                                : canChatInCurrentPhase
                                ? chatTargetUnit
                                  ? `对 ${chatTargetUnit.identity.name} 说点什么`
                                  : "先选择单位"
                                : "执行阶段不能聊天；请在命令里写下回合方针"
                            }
                          />
                          <button
                            type="button"
                            className="action-button action-button-primary chat-send-button"
                            disabled={!canSendChat || !dialogueDraft.trim()}
                            onClick={() => void handleDialogueSubmit()}
                          >
                            {busy && latestDialogueReply === "等待单位回复中…" ? "…" : "发送"}
                          </button>
                        </div>
                      </div>
                    </div>
                  </section>
                ) : null}
                {activePanelID === "unit" ? (
                  <section className="panel-card">
                    <div className="panel-header">
                      <div>
                        <p className="card-kicker">Selected Target</p>
                        <h2>地块与单位详情</h2>
                      </div>
                    </div>

                    {!selectedTileCoord ? (
                      <p className="empty-state">请在战场中点击一个地块。</p>
                    ) : (
                      <>
                        {selectedUnit ? (
                          selectedUnitRestrictedByFog ? (
                            <div className="command-summary">
                              <span className="shop-label">敌方单位情报受限</span>
                              <strong>{selectedUnit.identity.name} · 坐标 {selectedUnit.status.position_q}, {selectedUnit.status.position_r}</strong>
                              <p className="summary-note">
                                有雾模式下，你只能确认该敌方单位出现在当前视野内；单位面板、背包、近期决策、记忆和详细属性不可查看。
                              </p>
                            </div>
                          ) : (
                          <>
                            <div className="unit-grid" style={{ background: "rgba(0,0,0,0.2)", padding: "8px", borderRadius: "4px" }}>
                              <div>
                                <span className="shop-label">单位</span>
                                <strong>{selectedUnit.identity.name}</strong>
                              </div>
                              <div>
                                <span className="shop-label">阵营</span>
                                <strong>{selectedUnit.faction_id === effectiveCommanderFactionID ? "我方" : "对方"}</strong>
                              </div>
                              <div>
                                <span className="shop-label">HP / 命数</span>
                                <strong>{selectedUnit.status.hp} / {selectedUnit.status.lives_remaining}</strong>
                              </div>
                              <div>
                                <span className="shop-label">ATK / DEF / MOV</span>
                                <strong>{selectedUnit.status.attack} / {selectedUnit.status.defense} / {selectedUnit.status.move}</strong>
                              </div>
                            </div>

                            <div className="unit-grid" style={{ marginTop: "16px" }}>
                              <div>
                                <span className="shop-label">钱包</span>
                                <strong>{selectedUnit.status.wallet} G</strong>
                              </div>
                              <div>
                                <span className="shop-label">饥饿</span>
                                <strong>
                                  {selectedUnit.status.hunger}
                                  {selectedUnit.status.starvation_turns
                                    ? ` / 断粮 ${selectedUnit.status.starvation_turns} 回合`
                                    : ""}
                                </strong>
                              </div>
                              <div>
                                <span className="shop-label">性格</span>
                                <strong>
                                  勇 {selectedUnit.personality.courage.toFixed(2)} / 谨{" "}
                                  {selectedUnit.personality.prudence.toFixed(2)}
                                </strong>
                              </div>
                              <div style={{ gridColumn: "span 2" }}>
                                <span className="shop-label">伴侣 / 父母 / 小孩</span>
                                <strong>{formatUnitSocialTies(selectedUnit, session)}</strong>
                              </div>
                            </div>

                            <div className="unit-grid" style={{ background: "rgba(0,0,0,0.2)", padding: "8px", borderRadius: "4px", marginTop: "16px" }}>
                              <div>
                                <span className="shop-label">脚下坐标</span>
                                <strong>{selectedTileCoord.q}, {selectedTileCoord.r}</strong>
                              </div>
                              <div>
                                <span className="shop-label">脚下地形</span>
                                <strong>{selectedTerrain?.display_name ?? selectedTile?.terrain ?? ""}</strong>
                              </div>
                              <div style={{ gridColumn: "span 2" }}>
                                <span className="shop-label">脚下设施</span>
                                <strong>{selectedStructure ? formatCurrentStructureSummary(selectedStructure, selectedUnit, session) : "无设施"}</strong>
                              </div>
                              <div style={{ gridColumn: "span 2" }}>
                                <span className="shop-label">地形规则</span>
                                <p className="summary-note" style={{ margin: 0 }}>{formatTerrainRuleSummary(selectedTerrain)}</p>
                              </div>
                            </div>

                            <div className="command-summary">
                              <span className="shop-label">头顶气泡 / 本回合自白</span>
                              {selectedBubbleLines.length > 0 ? (
                                <div className="bubble-line-list">
                                  {selectedBubbleLines.map((line, index) => (
                                    <strong key={`${line}-${index}`}>{line}</strong>
                                  ))}
                                </div>
                              ) : (
                                <strong>{selectedDecision?.speak || ""}</strong>
                              )}
                            </div>

                            <div className="command-summary">
                              <span className="shop-label">当前属性影响</span>
                              <p className="summary-note">
                                {formatCurrentInfluenceSummary(
                                  session,
                                  selectedUnit,
                                  selectedTerrain,
                                  selectedStructure,
                                  selectedDecision,
                                )}
                              </p>
                            </div>

                            <div className="command-summary">
                              <span className="shop-label">记忆中的知识</span>
                              <p className="summary-note">{formatKnowledgeHighlights(selectedUnit, selectedDecision)}</p>
                            </div>

                            <div className="command-summary">
                              <span className="shop-label">最近 AI 决策</span>
                              <strong>{selectedDecision ? formatDecision(selectedDecision) : ""}</strong>
                              {selectedDecision ? (
                                hasDefianceTrace(selectedDecision.reasoning) ? (
                                  <>
                                    {stripDefianceTrace(selectedDecision.reasoning) ? (
                                      <p className="summary-note">{stripDefianceTrace(selectedDecision.reasoning)}</p>
                                    ) : null}
                                    <DefianceCard reasoning={selectedDecision.reasoning} />
                                  </>
                                ) : (
                                  <p className="summary-note">{selectedDecision.reasoning}</p>
                                )
                              ) : null}
                            </div>

                            <div className="command-summary">
                              <span className="shop-label">最近想法 / 交互</span>
                              <strong>{formatThoughtSummary(selectedThought, selectedDecision)}</strong>
                              {selectedThought?.error_message ? (
                                <p className="summary-note summary-error">{selectedThought.error_message}</p>
                              ) : selectedThought?.summary ? (
                                <p className="summary-note">{selectedThought.summary}</p>
                              ) : null}
                            </div>

                            <div className="command-summary">
                              <span className="shop-label">装备与背包</span>
                              <strong>{formatInventorySummary(selectedUnit)}</strong>
                            </div>

                            <div className="command-summary">
                              <span className="shop-label">状态变动明细</span>
                              {selectedUnitStatusChanges.length === 0 ? (
                                <p className="summary-note">暂无 HP、饥饿、士气、钱包等数值变动记录。</p>
                              ) : (
                                <div className="unit-change-list">
                                  {selectedUnitStatusChanges.map((entry) => (
                                    <article key={entry.id} className="unit-change-entry unit-change-entry-status">
                                      <strong>{entry.title}</strong>
                                      <p>{entry.detail}</p>
                                      <span>{entry.meta}</span>
                                    </article>
                                  ))}
                                </div>
                              )}
                            </div>

                            <div className="command-summary">
                              <span className="shop-label">物资 / 装备变动</span>
                              {selectedUnitInventoryChanges.length === 0 ? (
                                <p className="summary-note">暂无拾取、掉落、交易、采集、锻造、装备或进食记录。</p>
                              ) : (
                                <div className="unit-change-list">
                                  {selectedUnitInventoryChanges.map((entry) => (
                                    <article key={entry.id} className="unit-change-entry unit-change-entry-inventory">
                                      <strong>{entry.title}</strong>
                                      <p>{entry.detail}</p>
                                      <span>{entry.meta}</span>
                                    </article>
                                  ))}
                                </div>
                              )}
                            </div>

                            <div className="command-summary">
                              <span className="shop-label">决策 / 行动轨迹</span>
                              {selectedUnitDecisionChanges.length === 0 ? (
                                <p className="summary-note">暂无可展示的 AI 决策轨迹。</p>
                              ) : (
                                <div className="unit-change-list">
                                  {selectedUnitDecisionChanges.map((entry) => (
                                    <article key={entry.id} className="unit-change-entry">
                                      <strong>{entry.title}</strong>
                                      <p>{entry.detail}</p>
                                      <span>{entry.meta}</span>
                                    </article>
                                  ))}
                                </div>
                              )}
                            </div>

                            <div className="command-summary">
                              <span className="shop-label">最近记忆</span>
                              <strong>{formatMemorySummary(selectedUnit, session?.turn_state.turn)}</strong>
                            </div>

                            <div className="command-summary">
                              <span className="shop-label">单位传记（AI）</span>
                              <p className="summary-note">{selectedUnit.identity.biography || ""}</p>
                            </div>

                            <div className="command-summary">
                              <span className="shop-label">招募词（AI）</span>
                              <strong>{selectedUnit.identity.recruitment_pitch || ""}</strong>
                            </div>

                            <div className="dialogue-thread">
                              <div className="section-title-row">
                                <span className="shop-label">最近对话</span>
                                <button
                                  type="button"
                                  className="tiny-link-button"
                                  onClick={() => handleOpenUnitChat(selectedUnit.id)}
                                >
                                  点开聊天
                                </button>
                              </div>
                              {selectedDialogue.length === 0 ? (
                                <p className="empty-state">还没有和这个单位对话。</p>
                              ) : (
                                <div className="chat-transcript">
                                  {selectedDialogue.map((entry) => {
                                    const isPlayerLine = entry.speaker === "player";
                                    return (
                                      <article
                                        key={entry.id}
                                        className={`chat-message ${isPlayerLine ? "chat-message-right" : "chat-message-left"}`}
                                      >
                                        <div className={`chat-meta ${isPlayerLine ? "chat-meta-right" : "chat-meta-left"}`}>
                                          <span>{displayDialogueSpeaker(entry.speaker)}</span>
                                          <span>{formatDialogueTimestamp(entry.occurred_at, entry.turn, entry.phase, session?.turn_state.turn)}</span>
                                        </div>
                                        <div className={`chat-bubble ${isPlayerLine ? "chat-bubble-right" : "chat-bubble-left"}`}>
                                          <p>{entry.message}</p>
                                        </div>
                                      </article>
                                    );
                                  })}
                                </div>
                              )}
                            </div>
                          </>
                          )
                        ) : (
                          <div className="unit-grid" style={{ background: "rgba(0,0,0,0.2)", padding: "8px", borderRadius: "4px" }}>
                            <div>
                              <span className="shop-label">坐标</span>
                              <strong>{selectedTileCoord.q}, {selectedTileCoord.r}</strong>
                            </div>
                            <div>
                              <span className="shop-label">地形</span>
                              <strong>{selectedTerrain?.display_name ?? selectedTile?.terrain ?? ""}</strong>
                            </div>
                            <div style={{ gridColumn: "span 2" }}>
                              <span className="shop-label">地块设施</span>
                              <strong>{selectedStructure ? formatCurrentStructureSummary(selectedStructure, null, session) : "无设施"}</strong>
                            </div>
                            <div style={{ gridColumn: "span 2" }}>
                              <span className="shop-label">地形规则</span>
                              <p className="summary-note" style={{ margin: 0 }}>{formatTerrainRuleSummary(selectedTerrain)}</p>
                            </div>
                          </div>
                        )}
                      </>
                    )}
                  </section>




                ) : null}
                {activePanelID === "inventory" ? (
                  <section className="panel-card inventory-panel-card">
                    <div className="panel-header">
                      <div>
                        <p className="card-kicker">Inventory</p>
                        <h2>装备与背包</h2>
                      </div>
                    </div>

                    {!selectedUnit ? (
                      <p className="empty-state">请先在战场或编组中选择一个单位。</p>
                    ) : selectedUnitRestrictedByFog ? (
                      <p className="empty-state">有雾模式下不能查看敌方单位的装备栏、背包和钱包。</p>
                    ) : (
                      <>
                        <div className="inventory-selected-head">
                          <img
                            src={selectedUnitPortraitURL}
                            alt=""
                            onError={(event) => {
                              event.currentTarget.onerror = null;
                              event.currentTarget.src = selectedUnitPortraitFallbackURL;
                            }}
                          />
                          <div>
                            <strong>{selectedUnit.identity.name}</strong>
                            <p>{selectedUnitFactionLabel} · {selectedUnit.status.wallet} G · 背包 {selectedUnit.inventory.backpack.length}/6</p>
                          </div>
                        </div>

                        <div className="inventory-detail-grid inventory-panel-grid">
                          <section className="inventory-box">
                            <span className="shop-label">装备栏</span>
                            <div className="inventory-chip-list inventory-chip-list-column">
                              {Object.entries(selectedUnit.inventory.equipment).length > 0 ? (
                                Object.entries(selectedUnit.inventory.equipment).map(([slot, stack]) => (
                                  <span key={slot} className="inventory-row-chip">
                                    <span>{displayEquipmentSlotLabel(slot)}</span>
                                    <strong>{displayStackLabel(stack)} x{stack.quantity}</strong>
                                    <small>{displayStackDetails(stack)}</small>
                                  </span>
                                ))
                              ) : (
                                <span className="inventory-chip inventory-chip-empty">无装备</span>
                              )}
                            </div>
                          </section>

                          <section className="inventory-box">
                            <span className="shop-label">背包栏</span>
                            <div className="inventory-chip-list inventory-chip-list-column">
                              {selectedUnit.inventory.backpack.length > 0 ? (
                                selectedUnit.inventory.backpack.map((stack, index) => (
                                  <span key={`${stack.item_id}-${index}`} className="inventory-row-chip">
                                    <span>#{index + 1}</span>
                                    <strong>{displayStackLabel(stack)} x{stack.quantity}</strong>
                                    <small>{displayStackDetails(stack)}</small>
                                  </span>
                                ))
                              ) : (
                                <span className="inventory-chip inventory-chip-empty">背包为空</span>
                              )}
                            </div>
                          </section>
                        </div>

                        <p className="summary-note">
                          当前背包由 AI 单位在采集、交易、进食、建造、锻造、强化、换装、信鸽投递和战利品继承时自动使用；锻造装备会结合单位生平与性格由 LLM 命名。
                        </p>
                      </>
                    )}
                  </section>
                ) : null}
                {activePanelID === "thoughts" ? (
                  <section className="panel-card">
                    <div className="panel-header">
                      <div>
                        <p className="card-kicker">Intel</p>
                        <h2>单位情报</h2>
                      </div>
                    </div>

                    {allUnits.length === 0 ? (
                      <p className="empty-state">会话尚未初始化。</p>
                    ) : (
                      <div className="thought-grid">
                        {allUnits.map((unit) => {
                          const decisionTrace = latestDecisions.get(unit.id);
                          const interaction =
                            latestDecisionInteractions.get(unit.id) ?? latestInteractionsByUnit.get(unit.id) ?? null;
                          const enemy = unit.faction_id !== effectiveCommanderFactionID;
                          const redactedByFog = !!session?.fog_of_war_enabled && enemy;

                          return (
                            <article
                              key={unit.id}
                              className={`thought-card ${enemy ? "thought-card-enemy" : "thought-card-player"} ${
                                selectedUnitID === unit.id ? "thought-card-selected" : ""
                              }`}
                            >
                              <div className="thought-head">
                                <strong>{unit.identity.name}</strong>
                                <span className="thought-kind">{enemy ? "敌方" : "己方"}</span>
                              </div>
                              <p className="thought-summary">
                                {redactedByFog ? "有雾模式下敌方情报不可查看。" : formatThoughtSummary(interaction, decisionTrace)}
                              </p>
                              <span className="thought-meta">
                                {redactedByFog
                                  ? "仅位置可见"
                                  : interaction
                                    ? `${formatInteractionKind(interaction.kind)} · T${interaction.turn} · ${
                                        phaseLabels[interaction.phase]
                                      }`
                                    : ""}
                              </span>
                              {!redactedByFog && interaction?.error_message ? (
                                <p className="thought-error">{interaction.error_message}</p>
                              ) : null}
                            </article>
                          );
                        })}
                      </div>
                    )}
                  </section>
                ) : null}
                {activePanelID === "roster" ? (
                  <section className="panel-card">
                    <div className="panel-header">
                      <div>
                        <p className="card-kicker">Roster</p>
                        <h2>战场编组</h2>
                      </div>
                    </div>

                    <div className="roster-group">
                      <p className="group-title">我方</p>
                      {controlledUnits.map((unit) => (
                        <button
                          key={unit.id}
                          type="button"
                          className={`roster-button ${selectedUnitID === unit.id ? "roster-button-selected" : ""}`}
                          onClick={() => setSelectedTileCoord({ q: unit.status.position_q, r: unit.status.position_r })}
                          style={{ display: 'flex', alignItems: 'center', gap: '8px' }}
                        >
                          <img
                            src={portraitURLForUnit(unit)}
                            style={{ width: '20px', height: '20px' }}
                            alt=""
                            onError={(event) => {
                              event.currentTarget.onerror = null;
                              event.currentTarget.src = portraitFallbackURLForUnit(unit);
                            }}
                          />
                          <span style={{ flex: 1, textAlign: 'left' }}>{unit.identity.name}</span>
                          <span className="roster-meta">
                            {formatRosterMeta(unit.status.hp, formatDecisionShort(latestDecisions.get(unit.id), latestAITurnLines.get(unit.id)))}
                          </span>
                        </button>
                      ))}
                    </div>

                    <div className="roster-group">
                      <p className="group-title">对方</p>
                      {opponentUnits.map((unit) => (
                        <button
                          key={unit.id}
                          type="button"
                          className={`roster-button roster-button-enemy ${selectedUnitID === unit.id ? "roster-button-selected" : ""}`}
                          onClick={() => setSelectedTileCoord({ q: unit.status.position_q, r: unit.status.position_r })}
                          style={{ display: 'flex', alignItems: 'center', gap: '8px' }}
                        >
                          <img
                            src={portraitURLForUnit(unit)}
                            style={{ width: '20px', height: '20px' }}
                            alt=""
                            onError={(event) => {
                              event.currentTarget.onerror = null;
                              event.currentTarget.src = portraitFallbackURLForUnit(unit);
                            }}
                          />
                          <span style={{ flex: 1, textAlign: 'left' }}>{unit.identity.name}</span>
                          <span className="roster-meta">
                            {formatRosterMeta(unit.status.hp, formatDecisionShort(latestDecisions.get(unit.id), latestAITurnLines.get(unit.id)))}
                          </span>
                        </button>
                      ))}
                    </div>
                  </section>
                ) : null}
                {activePanelID === "structures" ? (
                  <section className="panel-card">
                    <div className="panel-header">
                      <div>
                        <p className="card-kicker">Structures</p>
                        <h2>设施与产点</h2>
                      </div>
                    </div>

                    {recentStructures.length === 0 ? (
                      <p className="empty-state">场上还没有设施。单位会在合适地形自行决定是否采集、种地或施工。</p>
                    ) : (
                      <div className="log-list">
                        {recentStructures.map((structure) => (
                          <article key={structure.id} className="log-entry">
                            <span className="log-turn">{formatStructureTag(structure, session, session?.turn_state.turn)}</span>
                            <p>{formatStructureSummary(structure, session, session?.turn_state.turn)}</p>
                          </article>
                        ))}
                      </div>
                    )}
                  </section>
                ) : null}
                {activePanelID === "combat" ? (
                  <section className="panel-card">
                    <div className="panel-header">
                      <div>
                        <p className="card-kicker">Combat Log</p>
                        <h2>最近AI事件</h2>
                      </div>
                    </div>

                    {recentAIEvents.length === 0 ? (
                      <p className="empty-state">执行后会显示单位AI本轮自述。</p>
                    ) : (
                      <div className="log-list">
                        {recentAIEvents.map((entry) => (
                          <article key={entry.id} className="log-entry">
                            <span className="log-turn">
                              T{entry.turn} · {phaseLabels[entry.phase]} · {formatTurnsAgo(entry.turn, session?.turn_state.turn)}
                            </span>
                            <p>{aiTurnLineFromLog(entry)?.text ?? trimSpeakerPrefix(entry.message)}</p>
                          </article>
                        ))}
                      </div>
                    )}
                  </section>
                ) : null}
                {activePanelID === "dialogues" ? (
                  <section className="panel-card">
                    <div className="panel-header">
                      <div>
                        <p className="card-kicker">Dialogue Threads</p>
                        <h2>单位交谈</h2>
                      </div>
                      <span className="mini-pill">{visibleUnitDialogueThreads.length} 条</span>
                    </div>
                    <p className="panel-note">
                      按单位交谈与表白事件聚合成线程卡片，优先展示双方原句；若当前选中了单位，会先把与该单位相关的交谈置顶。
                    </p>

                    {visibleUnitDialogueThreads.length === 0 ? (
                      <p className="empty-state">部署交易、行动间隙交谈或跨阵营短暂对话出现后，这里会自动形成专门线程。</p>
                    ) : (
                      <div className="dialogue-thread dialogue-thread-panel">
                        {visibleUnitDialogueThreads.map((thread) => {
                          const leftName = findUnitName(session, thread.actorUnitID);
                          const rightName = findUnitName(session, thread.targetUnitID);
                          return (
                            <article key={thread.id} className="dialogue-entry dialogue-entry-thread">
                              <div className="thought-head">
                                <strong>{leftName} ↔ {rightName}</strong>
                                <span className="log-turn">
                                  T{thread.turn} · {phaseLabels[thread.phase]} · {formatTurnsAgo(thread.turn, session?.turn_state.turn)}
                                </span>
                              </div>
                              <p className="dialogue-thread-summary">{thread.summary}</p>
                              {thread.lines.length > 0 ? (
                                <div className="chat-transcript chat-transcript-thread">
                                  {thread.lines.map((line) => {
                                    const rightSide = line.unitID === thread.targetUnitID;
                                    return (
                                      <article
                                        key={line.id}
                                        className={`chat-message ${rightSide ? "chat-message-right" : "chat-message-left"}`}
                                      >
                                        <div className={`chat-meta ${rightSide ? "chat-meta-right" : "chat-meta-left"}`}>
                                          <span>{displayDialogueSpeaker(line.speaker)}</span>
                                          <span>{formatDialogueTimestamp(line.occurredAt, line.turn, line.phase, session?.turn_state.turn)}</span>
                                        </div>
                                        <div
                                          className={`chat-bubble ${rightSide ? "chat-bubble-right" : "chat-bubble-left"} ${
                                            selectedUnitID === line.unitID ? "chat-bubble-selected" : ""
                                          }`}
                                        >
                                          <p>{line.message}</p>
                                        </div>
                                      </article>
                                    );
                                  })}
                                </div>
                              ) : null}
                            </article>
                          );
                        })}
                      </div>
                    )}
                  </section>
                ) : null}
                {activePanelID === "battleReport" ? (
                  <section className="panel-card">
                    <div className="panel-header">
                      <div>
                        <p className="card-kicker">Battle Report</p>
                        <h2>战报 / 战后档案</h2>
                      </div>
                    </div>
                    <p className="panel-note">回合战报当前不触发新的生成；战后档案官会在战斗结束时统一弹窗展示，并保留在这里供回看。</p>

                    {session && session.outcome !== "ongoing" ? (
                      <button
                        type="button"
                        className="action-button action-button-secondary"
                        disabled={hallArchiveEntries.length === 0}
                        onClick={() => setHallArchiveModalOpen(true)}
                      >
                        查看战后档案官（{hallArchiveEntries.length}）
                      </button>
                    ) : null}

                    {recentBattleReports.length === 0 ? (
                      <p className="empty-state">回合战报当前已禁用，本局不会自动生成新战报；结束后会显示战后档案。</p>
                    ) : (
                      <div className="log-list">
                        {recentBattleReports.map((report) => (
                          <article key={report.id} className="log-entry">
                            <span className="log-turn">
                              T{report.turn} · {phaseLabels[report.phase]} · {report.narrator}
                            </span>
                            {report.illustration_url ? (
                              <figure className="battle-report-illustration">
                                <img
                                  src={report.illustration_url}
                                  alt={`${report.narrator} 第 ${report.turn} 回合战报配图`}
                                  loading="lazy"
                                />
                              </figure>
                            ) : null}
                            <p>
                              <strong>{report.title || `第 ${report.turn} 回合战场纪事`}</strong>
                            </p>
                            <p>{report.content}</p>
                          </article>
                        ))}
                      </div>
                    )}
                  </section>
                ) : null}
                {activePanelID === "rawEvent" ? (
                  <section className="panel-card">
                    <div className="panel-header">
                      <div>
                        <p className="card-kicker">Raw Event</p>
                        <h2>原始事件流</h2>
                      </div>
                      <span className="mini-pill">{recentRawEvents.length} 条</span>
                    </div>
                    <p className="panel-note">event_log_raw 会完整记录状态变更、对话、决策与日志，不裁剪写入，不直接喂给 LLM。</p>

                    {recentRawEvents.length === 0 ? (
                      <p className="empty-state">执行后会出现原始事件。</p>
                    ) : (
                      <div className="log-list">
                        {recentRawEvents.map((entry) => (
                          <article key={entry.id} className="log-entry">
                            <span className="log-turn">
                              T{entry.turn} · {phaseLabels[entry.phase]} · {formatTurnsAgo(entry.turn, session?.turn_state.turn)} · {entry.source}/{entry.kind}
                            </span>
                            <p>{entry.summary || "（无摘要）"}</p>
                          </article>
                        ))}
                      </div>
                    )}
                  </section>
                ) : null}
                {activePanelID === "llmTrace" ? (
                  <section className="panel-card llm-trace-card">
                    <div className="panel-header">
                      <div>
                        <p className="card-kicker">Settings</p>
                        <h2>设置与调试</h2>
                      </div>
                      <span className="mini-pill">
                        {selectedUnit ? `当前高亮：${selectedUnit.identity.name}` : `${llmInteractionsForSettings.length} 条`}
                      </span>
                    </div>
                    <p className="panel-note">房间与恢复链接等局外信息放在这里；CMD 面板只保留对局内指挥、任务与对话。</p>

                    <div className="settings-section">
                      <div className="panel-header panel-header-compact">
                        <div>
                          <p className="card-kicker">Room</p>
                          <h3>房间设置</h3>
                        </div>
                      </div>

                      {isDuelJoined || startMode === "multiplayer" || duelRoomCode || duelJoinRoomCode || duelJoinSessionID || duelJoinRoleToken ? (
                        <>
                          <p className="panel-note">
                            多人房信息仅用于创建、加入、邀请和恢复；进入正式对局后不占用指令区。
                          </p>

                          <div className="command-actions">
                            {!isDuelJoined ? (
                              <>
                                <button type="button" className="action-button" disabled={busy} onClick={() => void handleCreateDuel()}>
                                  创建双人房
                                </button>
                                <button
                                  type="button"
                                  className="action-button"
                                  disabled={busy || !duelJoinRoomCode.trim()}
                                  onClick={() => void handleJoinDuelByRoomCode()}
                                >
                                  房间号加入
                                </button>
                                <button
                                  type="button"
                                  className="action-button action-button-secondary"
                                  disabled={busy || !duelJoinSessionID.trim() || !duelJoinRoleToken.trim()}
                                  onClick={() => void handleJoinDuelByInput()}
                                >
                                  令牌恢复
                                </button>
                              </>
                            ) : null}
                          </div>

                          {!isDuelJoined ? (
                            <>
                              <label className="input-block">
                                <span className="shop-label">房间号</span>
                                <input
                                  className="text-input"
                                  value={duelJoinRoomCode}
                                  onChange={(event) => setDuelJoinRoomCode(normalizeRoomCodeInput(event.target.value))}
                                  disabled={busy}
                                  placeholder="例如：A7K9Q2"
                                />
                              </label>

                              <label className="input-block">
                                <span className="shop-label">房间号加入阵营</span>
                                <GameSelect
                                  value={duelJoinPreferredRole}
                                  onChange={(nextValue) => setDuelJoinPreferredRole(nextValue === "player" ? "player" : "enemy")}
                                  disabled={busy}
                                  ariaLabel="选择房间号加入阵营"
                                  options={[
                                    { value: "enemy", label: "enemy（默认，适合受邀加入）" },
                                    { value: "player", label: "player（用于恢复 player 端）" },
                                  ]}
                                />
                              </label>

                              <label className="input-block">
                                <span className="shop-label">双人房 Session ID</span>
                                <input
                                  className="text-input"
                                  value={duelJoinSessionID}
                                  onChange={(event) => setDuelJoinSessionID(event.target.value)}
                                  disabled={busy}
                                  placeholder="输入 session_id 或创建后自动填入"
                                />
                              </label>

                              <label className="input-block">
                                <span className="shop-label">双人房角色 Token</span>
                                <input
                                  className="text-input"
                                  value={duelJoinRoleToken}
                                  onChange={(event) => setDuelJoinRoleToken(event.target.value)}
                                  disabled={busy}
                                  placeholder="输入 role_token（创建后己方自动填入）"
                                />
                              </label>
                            </>
                          ) : null}

                          {isDuelJoined ? (
                            <>
                              <div className="command-summary room-share-grid">
                                <span className="shop-label">当前房间号</span>
                                <div className="inline-input-row">
                                  <input className="text-input" value={duelRoomCode || ""} readOnly placeholder="未创建/未加入房间" />
                                  <button
                                    type="button"
                                    className="action-button inline-action"
                                    disabled={!duelRoomCode}
                                    onClick={() => void handleCopyText(duelRoomCode, "房间号")}
                                  >
                                    复制
                                  </button>
                                </div>
                              </div>

                              {opponentJoinLink ? (
                                <label className="input-block">
                                  <span className="shop-label">对方加入链接（{opponentJoinRole}）</span>
                                  <div className="inline-input-row">
                                    <input className="text-input" value={opponentJoinLink} readOnly />
                                    <button
                                      type="button"
                                      className="action-button inline-action"
                                      onClick={() => void handleCopyText(opponentJoinLink, "对方加入链接")}
                                    >
                                      复制
                                    </button>
                                  </div>
                                </label>
                              ) : null}

                              {selfResumeLink ? (
                                <label className="input-block">
                                  <span className="shop-label">我的恢复链接</span>
                                  <div className="inline-input-row">
                                    <input className="text-input" value={selfResumeLink} readOnly />
                                    <button
                                      type="button"
                                      className="action-button inline-action"
                                      onClick={() => void handleCopyText(selfResumeLink, "我的恢复链接")}
                                    >
                                      复制
                                    </button>
                                  </div>
                                </label>
                              ) : null}
                            </>
                          ) : null}
                        </>
                      ) : (
                        <p className="empty-state">当前是单人对局，不需要房间设置。</p>
                      )}
                    </div>

                    <div className="settings-section">
                      <div className="panel-header panel-header-compact">
                        <div>
                          <p className="card-kicker">LLM Trace</p>
                          <h3>调试日志</h3>
                        </div>
                      </div>
                      <p className="panel-note">展示本局已保存和调用中的 LLM 交互、模型输出和请求尝试，便于跨回合定位错误。</p>

                      {llmInteractionsForSettings.length === 0 ? (
                        <p className="empty-state">执行或对话后，这里会出现 LLM 交互记录。</p>
                      ) : (
                        <div className="llm-list">
                        {llmInteractionsForSettings.map((interaction) => {
                          const unitName = interaction.in_progress && !interaction.unit_id
                            ? "LLM 调用"
                            : findUnitName(session, interaction.unit_id);
                          const selected = Boolean(interaction.unit_id) && interaction.unit_id === selectedUnitID;

                          return (
                            <article
                              key={interaction.id}
                              className={`llm-entry ${selected ? "llm-entry-selected" : ""} ${
                                interaction.error_message ? "llm-entry-error" : ""
                              } ${interaction.in_progress ? "llm-entry-active" : ""}`}
                            >
                              <div className="llm-head">
                                <div>
                                  <strong>{unitName}</strong>
                                  <p className="llm-meta">
                                    T{interaction.turn} · {phaseLabels[interaction.phase]} ·{" "}
                                    {formatInteractionKind(interaction.kind)}
                                  </p>
                                </div>
                                <div className="llm-badges">
                                  {interaction.in_progress ? <span className="llm-badge llm-badge-active">调用中</span> : null}
                                  {interaction.provider ? <span className="llm-badge">{interaction.provider}</span> : null}
                                  {interaction.model ? <span className="llm-badge">{interaction.model}</span> : null}
                                  {interaction.used_fallback ? <span className="llm-badge">fallback</span> : null}
                                  {interaction.in_progress && typeof interaction.elapsed_ms === "number" ? (
                                    <span className="llm-badge">{formatAttemptDuration(interaction.elapsed_ms)}</span>
                                  ) : null}
                                </div>
                              </div>

                              <p className="llm-summary">{formatThoughtSummary(interaction, latestDecisions.get(interaction.unit_id))}</p>

                              {interaction.error_message ? (
                                <p className="llm-error">错误：{formatLLMError(interaction.error_message)}</p>
                              ) : null}
                              {interaction.fallback_cause ? (
                                <p className="llm-error">失败原因：{formatLLMError(interaction.fallback_cause)}</p>
                              ) : null}

                              <details className="trace-details">
                                <summary>输出</summary>
                                <div className="trace-detail-body">
                                  <span className="trace-label">解析结果</span>
                                  <pre className="trace-code">{interaction.parsed_output || (interaction.in_progress ? "（调用中，尚未返回）" : "（空）")}</pre>
                                  <span className="trace-label">原始结果</span>
                                  <pre className="trace-code">{interaction.raw_output || (interaction.in_progress ? "（调用中，尚未返回）" : "（空）")}</pre>
                                </div>
                              </details>

                              <details className="trace-details">
                                <summary>提示词</summary>
                                <div className="trace-detail-body">
                                  <span className="trace-label">System Prompt</span>
                                  <pre className="trace-code">{interaction.system_prompt}</pre>
                                  <span className="trace-label">User Prompt</span>
                                  <pre className="trace-code">{interaction.user_prompt}</pre>
                                </div>
                              </details>

                              <details className="trace-details">
                                <summary>请求尝试 ({interaction.attempts?.length ?? 0})</summary>
                                <div className="trace-detail-body trace-attempts">
                                  {(interaction.attempts ?? []).length === 0 ? (
                                    <p className="empty-state">
                                      {interaction.in_progress ? "调用已开始，具体请求尝试会在 provider 返回后记录。" : "没有尝试记录。"}
                                    </p>
                                  ) : (
                                    (interaction.attempts ?? []).map((attempt, index) => (
                                      <article key={`${interaction.id}-${index}`} className="trace-attempt">
                                        <strong>{formatAttemptTitle(attempt)}</strong>
                                        <p>{formatAttemptDetails(attempt)}</p>
                                      </article>
                                    ))
                                  )}
                                </div>
                              </details>
                            </article>
                          );
                        })}
                      </div>
                    )}
                    </div>
                  </section>
                ) : null}
              </aside>
            </div>
          ) : null}
        </section>
      </main>
    </div>
  );
}

function portraitURLForUnit(unit: BattleUnit | null | undefined): string {
  const url = unit?.identity.portrait_url?.trim() ?? "";
  return url !== "" ? url : portraitFallbackURLForUnit(unit);
}

function portraitFallbackURLForUnit(unit: BattleUnit | null | undefined): string {
  const name = unit?.identity.name?.trim() || "?";
  const safeName = escapeSVGText(name);
  const initial = escapeSVGText(Array.from(name)[0] ?? "?");
  const female = unit?.identity.gender === "female";
  const backgroundA = female ? "#6f5b7f" : "#5b677f";
  const backgroundB = female ? "#2f2038" : "#1d2c3f";
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 128 128" role="img" aria-label="${safeName}头像占位"><defs><linearGradient id="g" x1="16" y1="12" x2="112" y2="118" gradientUnits="userSpaceOnUse"><stop stop-color="${backgroundA}"/><stop offset="1" stop-color="${backgroundB}"/></linearGradient></defs><path d="M64 8 114 36v56l-50 28-50-28V36Z" fill="url(#g)" stroke="#1d2228" stroke-width="6" stroke-linejoin="round"/><circle cx="64" cy="48" r="20" fill="#d9a77f" stroke="#1d2228" stroke-width="5"/><path d="M34 104c5-24 17-36 30-36s25 12 30 36Z" fill="#f1ead7" stroke="#1d2228" stroke-width="5" stroke-linejoin="round"/><text x="64" y="78" text-anchor="middle" font-family="Avenir Next, Helvetica Neue, sans-serif" font-size="38" font-weight="800" fill="#1d2228">${initial}</text></svg>`;
  return `data:image/svg+xml;charset=UTF-8,${encodeURIComponent(svg)}`;
}

function escapeSVGText(value: string): string {
  return value
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

function formatPhaseRemaining(session: SessionSnapshot | null, nowMs: number): string {
  if (!session || session.outcome !== "ongoing") {
    return "--";
  }
  if (session.execution_in_progress) {
    return "执行中";
  }
  const deadline = new Date(session.turn_state.phase_ends_at).getTime();
  if (!Number.isFinite(deadline)) {
    return "--";
  }
  const totalSeconds = Math.max(0, Math.ceil((deadline - nowMs) / 1000));
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  return `${minutes}:${String(seconds).padStart(2, "0")}`;
}

function phaseDeadlineReachedForClient(session: SessionSnapshot): boolean {
  if (session.outcome !== "ongoing" || session.execution_in_progress) {
    return false;
  }
  const deadline = new Date(session.turn_state.phase_ends_at).getTime();
  return Number.isFinite(deadline) && deadline <= Date.now();
}

// nextPhaseMessage 生成推进阶段后的用户提示文案。
function nextPhaseMessage(phase: Phase): string {
  switch (phase) {
    case "deployment":
      return "进入部署阶段。现在可以点名和单位对话，AI 会在结束前自行处理邻近交接与补给安排。";
    default:
      return "执行阶段已开始。AI 正在根据方针和对话自行处理进食、交易、生产、建造与战斗。";
  }
}


// advanceButtonLabel 生成“推进阶段”按钮文案。
function advanceButtonLabel(session: SessionSnapshot | null, currentFactionReady = false): string {
  if (!session) {
    return "加载中";
  }
  if (session.outcome !== "ongoing" && session.turn_state.phase === "execution") {
    return "本局已结束";
  }

  switch (session.turn_state.phase) {
    case "deployment":
      return currentFactionReady ? "等待\n对方" : "开始\n执行";
    default:
      return "下一\n回合";
  }
}

// phaseRank 把阶段映射为可比较的顺序值。
function phaseRank(phase: Phase): number {
  switch (phase) {
    case "deployment":
      return 1;
    case "execution":
      return 2;
    default:
      return 0;
  }
}

// isSnapshotOlder 判断新快照是否落后于当前快照。
function isSnapshotOlder(next: SessionSnapshot, current: SessionSnapshot): boolean {
  if (next.turn_state.turn !== current.turn_state.turn) {
    return next.turn_state.turn < current.turn_state.turn;
  }
  const nextPhaseRank = phaseRank(next.turn_state.phase);
  const currentPhaseRank = phaseRank(current.turn_state.phase);
  if (nextPhaseRank !== currentPhaseRank) {
    return nextPhaseRank < currentPhaseRank;
  }
  if (next.logs.length !== current.logs.length) {
    return next.logs.length < current.logs.length;
  }
  if (next.decision_traces.length !== current.decision_traces.length) {
    return next.decision_traces.length < current.decision_traces.length;
  }
  return false;
}

// isSnapshotEquivalent 判断两个快照是否可视为等价，避免无效重渲染。
function isSnapshotEquivalent(next: SessionSnapshot, current: SessionSnapshot): boolean {
  return snapshotSignature(next) === snapshotSignature(current);
}

function mergeDraftUnitsPreservingLocalEdits(
  current: BattleUnit[],
  incoming: BattleUnit[],
  dirtyFields: Map<string, Set<"name" | "biography" | "gender" | "portrait_url">>,
): BattleUnit[] {
  if (current.length === 0) {
    return incoming;
  }
  const currentByID = new Map(current.map((unit) => [unit.id, unit]));
  return incoming.map((nextUnit) => {
    const currentUnit = currentByID.get(nextUnit.id);
    if (!currentUnit) {
      return nextUnit;
    }
    const dirty = dirtyFields.get(nextUnit.id);
    if (!dirty || dirty.size === 0) {
      return nextUnit;
    }
    return {
      ...nextUnit,
      identity: {
        ...nextUnit.identity,
        name: dirty.has("name") ? currentUnit.identity.name : nextUnit.identity.name,
        biography: dirty.has("biography") ? currentUnit.identity.biography : nextUnit.identity.biography,
        gender: dirty.has("gender") ? currentUnit.identity.gender : nextUnit.identity.gender,
        portrait_url: dirty.has("portrait_url") ? currentUnit.identity.portrait_url : nextUnit.identity.portrait_url,
      },
    };
  });
}

// snapshotSignature 生成快照签名，用于快速比较内容变化。
function snapshotSignature(snapshot: SessionSnapshot): string {
  const allUnits = [...snapshot.player_units, ...snapshot.enemy_units];
  let hpTotal = 0;
  let hungerTotal = 0;
  let walletTotal = 0;
  let positionChecksum = 0;
  for (const unit of allUnits) {
    hpTotal += unit.status.hp;
    hungerTotal += unit.status.hunger;
    walletTotal += unit.status.wallet;
    positionChecksum += unit.status.position_q * 97 + unit.status.position_r * 131;
  }

  const lastLogID = snapshot.logs[snapshot.logs.length - 1]?.id ?? "";
  const lastDecisionID = snapshot.decision_traces[snapshot.decision_traces.length - 1]?.id ?? "";
  const lastLLMID = snapshot.llm_interactions[snapshot.llm_interactions.length - 1]?.id ?? "";
  const lastDialogueID = snapshot.dialogue_history[snapshot.dialogue_history.length - 1]?.id ?? "";
  const lastRawEventID = snapshot.raw_event_log[snapshot.raw_event_log.length - 1]?.id ?? "";
  const lastBattleReportID = snapshot.battle_reports[snapshot.battle_reports.length - 1]?.id ?? "";
  const lastHallArchiveID = snapshot.hall_archive_entries?.[snapshot.hall_archive_entries.length - 1]?.id ?? "";
  const draftPoolSignature = [...(snapshot.player_draft_pool ?? []), ...(snapshot.enemy_draft_pool ?? [])]
    .map((unit) => `${unit.id}:${unit.identity.name}:${unit.identity.biography}:${unit.identity.recruitment_pitch}`)
    .join("~");

  return [
    snapshot.id,
    snapshot.turn_state.turn,
    snapshot.turn_state.phase,
    snapshot.outcome,
    snapshot.command_power.current,
    allUnits.length,
    hpTotal,
    hungerTotal,
    walletTotal,
    positionChecksum,
    `${snapshot.logs.length}:${lastLogID}`,
    `${snapshot.decision_traces.length}:${lastDecisionID}`,
    `${snapshot.llm_interactions.length}:${lastLLMID}`,
    `${snapshot.dialogue_history.length}:${lastDialogueID}`,
    `${snapshot.raw_event_log.length}:${lastRawEventID}`,
    `${snapshot.battle_reports.length}:${lastBattleReportID}`,
    `${snapshot.hall_archive_entries?.length ?? 0}:${lastHallArchiveID}`,
    draftPoolSignature,
    `${snapshot.metrics?.llm_total_tokens ?? 0}:${snapshot.metrics?.cross_faction_interactions ?? 0}`,
  ].join("|");
}

// formatExecutionProgressMessage 把执行流事件格式化为顶部状态提示；有雾时不暴露对方单位与全局人数。
function formatExecutionProgressMessage(
  meta: Record<string, unknown>,
  snapshot?: SessionSnapshot | null,
  commanderFactionID = "",
): string {
  const reason = typeof meta.reason === "string" ? meta.reason : "";
  if (
    reason !== "execution_unit_completed" &&
    reason !== "execution_action_completed" &&
    reason !== "execution_unit_started"
  ) {
    return "";
  }
  if (snapshot?.fog_of_war_enabled) {
    const entry = parseExecutionFeedEntry(meta, snapshot);
    if (!entry) {
      return "";
    }
    const sanitized = sanitizeExecutionFeedForCommander([entry], snapshot, commanderFactionID)[0];
    if (!sanitized) {
      return "";
    }
    return `执行进行中：${formatExecutionFeedLine(sanitized)}。`;
  }
  const unitName =
    typeof meta.unit_name === "string" && meta.unit_name.trim() !== "" ? meta.unit_name : "某单位";
  const started = typeof meta.started_units === "number" ? meta.started_units : Number(meta.started_units);
  const completed = typeof meta.completed_units === "number" ? meta.completed_units : Number(meta.completed_units);
  const total = typeof meta.total_units === "number" ? meta.total_units : Number(meta.total_units);
  if (reason === "execution_unit_started") {
    if (Number.isFinite(started) && Number.isFinite(total) && total > 0) {
      return `执行进行中：${unitName} 开始思考与行动（${started}/${total}）。`;
    }
    return `执行进行中：${unitName} 开始思考与行动。`;
  }
  if (reason === "execution_action_completed") {
    return `执行进行中：${unitName} 刚完成一步行动。`;
  }
  if (Number.isFinite(completed) && Number.isFinite(total) && total > 0) {
    return `执行进行中：${unitName} 已完成思考与行动（${completed}/${total}）。`;
  }
  return `执行进行中：${unitName} 已完成思考与行动。`;
}

// parseExecutionFeedEntry 解析 SSE 元信息并生成执行侧栏条目。
function parseExecutionFeedEntry(
  meta: Record<string, unknown>,
  snapshot?: SessionSnapshot | null,
): ExecutionFeedEntry | null {
  // 只消费执行阶段逐单位事件；其他事件不进入执行流侧栏。
  const reason = typeof meta.reason === "string" ? meta.reason : "";
  if (
    reason !== "execution_unit_started" &&
    reason !== "execution_action_completed" &&
    reason !== "execution_unit_completed"
  ) {
    return null;
  }
  const unitID = typeof meta.unit_id === "string" ? meta.unit_id.trim() : "";
  const unitName =
    typeof meta.unit_name === "string" && meta.unit_name.trim() !== "" ? meta.unit_name.trim() : "某单位";
  const turn = typeof meta.turn === "number" ? meta.turn : Number(meta.turn);
  const phaseRaw = typeof meta.phase === "string" ? meta.phase : "";
  const phase: Phase | "" = phaseRaw === "deployment" || phaseRaw === "execution" ? phaseRaw : "";
  const started = typeof meta.started_units === "number" ? meta.started_units : Number(meta.started_units);
  const completed = typeof meta.completed_units === "number" ? meta.completed_units : Number(meta.completed_units);
  const total = typeof meta.total_units === "number" ? meta.total_units : Number(meta.total_units);
  const aiText =
    snapshot && (reason === "execution_unit_completed" || reason === "execution_action_completed")
      ? latestExecutionAIText(snapshot, unitID, Number.isFinite(turn) ? turn : 0)
      : "";

  return {
    id: `${reason}:${turn}:${unitID}:${started}:${completed}:${Date.now()}:${Math.random()}`,
    turn: Number.isFinite(turn) ? turn : 0,
    phase,
    unitID,
    unitName,
    reason,
    status: reason === "execution_unit_started" ? "started" : "completed",
    startedUnits: Number.isFinite(started) ? started : undefined,
    completedUnits: Number.isFinite(completed) ? completed : undefined,
    totalUnits: Number.isFinite(total) ? total : undefined,
    aiText: aiText || undefined,
  };
}

// formatExecutionFeedLine 格式化执行侧栏单行文案。
function formatExecutionFeedLine(entry: ExecutionFeedEntry): string {
  // 完成态优先展示 AI 生成文本，保证“所有文本由单位生成”的一致性。
  if (entry.status === "started") {
    if (entry.startedUnits && entry.totalUnits && entry.totalUnits > 0) {
      return `${entry.unitName} 开始思考与行动（${entry.startedUnits}/${entry.totalUnits}）`;
    }
    return `${entry.unitName} 开始思考与行动`;
  }
  if (entry.completedUnits && entry.totalUnits && entry.totalUnits > 0) {
    if (entry.aiText && entry.aiText.trim() !== "") {
      return `${entry.unitName}：${truncateAIBrief(entry.aiText, 18)}（${entry.completedUnits}/${entry.totalUnits}）`;
    }
    return `${entry.unitName} 已完成思考与行动（${entry.completedUnits}/${entry.totalUnits}）`;
  }
  if (entry.aiText && entry.aiText.trim() !== "") {
    return `${entry.unitName}：${truncateAIBrief(entry.aiText, 18)}`;
  }
  return `${entry.unitName} 已完成思考与行动`;
}

// sanitizeExecutionFeedForCommander 在迷雾模式下只保留当前指挥阵营的执行事件，并把进度重算为己方进度。
function sanitizeExecutionFeedForCommander(
  entries: ExecutionFeedEntry[],
  session: SessionSnapshot | null,
  commanderFactionID: string,
): ExecutionFeedEntry[] {
  if (!session?.fog_of_war_enabled) {
    return entries;
  }
  const ownUnitIDs = new Set(
    controlledUnitsByFaction(session, commanderFactionID)
      .filter((unit) => unit.status.life_state === "active")
      .map((unit) => unit.id),
  );
  if (ownUnitIDs.size === 0) {
    return [];
  }
  const chronological = [...entries].reverse();
  const startedOrderByTurnUnit = new Map<string, number>();
  const startedCountByTurn = new Map<number, number>();
  const completedCountByTurn = new Map<number, number>();
  const sanitizedChronological: ExecutionFeedEntry[] = [];

  for (const entry of chronological) {
    if (!ownUnitIDs.has(entry.unitID)) {
      continue;
    }
    const turn = entry.turn;
    const turnUnitKey = `${turn}:${entry.unitID}`;
    const nextEntry: ExecutionFeedEntry = {
      ...entry,
      totalUnits: ownUnitIDs.size,
    };
    if (entry.reason === "execution_unit_started" || entry.status === "started") {
      const nextStarted = (startedCountByTurn.get(turn) ?? 0) + 1;
      startedCountByTurn.set(turn, nextStarted);
      startedOrderByTurnUnit.set(turnUnitKey, nextStarted);
      nextEntry.startedUnits = nextStarted;
      nextEntry.completedUnits = undefined;
    } else if (entry.reason === "execution_unit_completed") {
      const nextCompleted = (completedCountByTurn.get(turn) ?? 0) + 1;
      completedCountByTurn.set(turn, nextCompleted);
      nextEntry.completedUnits = nextCompleted;
      nextEntry.startedUnits = undefined;
    } else {
      nextEntry.completedUnits = startedOrderByTurnUnit.get(turnUnitKey) ?? startedCountByTurn.get(turn) ?? undefined;
      nextEntry.startedUnits = undefined;
    }
    sanitizedChronological.push(nextEntry);
  }

  return sanitizedChronological.reverse();
}

function isLogVisibleForCommander(session: SessionSnapshot, log: SessionLog, commanderFactionID: string): boolean {
  const actorUnitID = log.actor_unit_id?.trim();
  if (!actorUnitID) {
    return false;
  }
  return unitFactionID(session, actorUnitID) === commanderFactionID;
}

function unitFactionID(session: SessionSnapshot, unitID: string): string {
  const unit = [...session.player_units, ...session.enemy_units, ...(session.wild_units ?? [])].find((record) => record.id === unitID);
  return unit?.faction_id ?? "";
}

// latestExecutionAIText 提取单位在当前回合的最新 AI 文本。
function latestExecutionAIText(snapshot: SessionSnapshot, unitID: string, turn: number): string {
  if (!snapshot || !unitID || turn <= 0) {
    return "";
  }
  // 先查日志再查 decision trace，尽量取到单位在该回合的最后一句有效自述。
  for (let index = snapshot.logs.length - 1; index >= 0; index -= 1) {
    const entry = snapshot.logs[index];
    if (entry.turn !== turn) {
      continue;
    }
    const targets = lineTargetUnitIDs(entry);
    if (!targets.includes(unitID)) {
      continue;
    }
    const line = aiTurnLineFromLog(entry);
    if (!line || line.text.trim() === "") {
      continue;
    }
    return line.text.trim();
  }
  for (let index = snapshot.decision_traces.length - 1; index >= 0; index -= 1) {
    const trace = snapshot.decision_traces[index];
    if (trace.unit_id !== unitID || trace.turn !== turn) {
      continue;
    }
    return firstAIDecisionText(trace);
  }
  return "";
}

// formatTurnsAgo 把回合号转换为“本回合/N回合前”文本。
function formatTurnsAgo(eventTurn: number, currentTurn?: number): string {
  if (!Number.isFinite(eventTurn)) {
    return "时间未知";
  }
  if (!Number.isFinite(currentTurn)) {
    return `T${eventTurn}`;
  }
  const delta = Math.max(0, (currentTurn ?? 0) - eventTurn);
  if (delta <= 0) {
    return "本回合";
  }
  return `${delta} 回合前`;
}

// displayDialogueSpeaker 统一聊天视图里的说话人名称。
function displayDialogueSpeaker(speaker: string): string {
  return speaker === "player" ? "你" : speaker;
}

// formatDialogueTimestamp 统一聊天视图里的时间标签。
function formatDialogueTimestamp(
  occurredAt: string,
  turn: number,
  phase: Phase,
  currentTurn?: number,
): string {
  const parts = [`T${turn}`, phaseLabels[phase], formatTurnsAgo(turn, currentTurn)];
  const occurredAtMs = parseOccurredAtMs(occurredAt);
  if (Number.isFinite(occurredAtMs)) {
    parts.push(
      new Intl.DateTimeFormat("zh-CN", {
        hour: "2-digit",
        minute: "2-digit",
        hour12: false,
      }).format(new Date(occurredAtMs)),
    );
  }
  return parts.join(" · ");
}

// formatTurnAwareLine 解析并格式化带 Tn 前缀的记忆/事件文本。
function formatTurnAwareLine(line: string, currentTurn?: number): string {
  const text = line.trim();
  if (!text) {
    return "";
  }
  const matched = text.match(/^T(\d+)\s+(.+)$/);
  if (!matched) {
    return text;
  }
  const turn = Number(matched[1]);
  const content = (matched[2] ?? "").trim();
  if (!content) {
    return text;
  }
  return `${content}（${formatTurnsAgo(turn, currentTurn)}）`;
}

// formatDecision 格式化决策轨迹的人类可读摘要。
function formatDecision(trace: DecisionTrace): string {
  const base = firstAIDecisionText(trace) || firstAIRequestedDecisionText(trace);
  if (!base) {
    return "";
  }
  const ap = formatAPTrace(trace);
  return `${base}${ap}`;
}

// controlledUnitsByFaction 过滤出当前玩家可控制阵营的单位列表。
function controlledUnitsByFaction(
  session: SessionSnapshot,
  factionID: string,
): Array<SessionSnapshot["player_units"][number]> {
  const allUnits = [...session.player_units, ...session.enemy_units];
  return allUnits.filter((unit) => unit.faction_id === factionID);
}

// visibleSessionForCommander 在有雾时生成当前指挥阵营可见的前端展示快照。
function visibleSessionForCommander(session: SessionSnapshot | null, factionID: string, perspectiveUnitID = ""): SessionSnapshot | null {
  if (!session || !session.fog_of_war_enabled) {
    return session;
  }
  const visibleCoords = fogVisibleCoordSet(session, factionID, perspectiveUnitID);
  const filterUnits = (units: SessionSnapshot["player_units"]) =>
    units.filter((unit) => visibleCoords.has(coordKey(unit.status.position_q, unit.status.position_r)));
  return {
    ...session,
    player_units: filterUnits(session.player_units),
    enemy_units: filterUnits(session.enemy_units),
    wild_units: session.wild_units?.filter((unit) => visibleCoords.has(coordKey(unit.status.position_q, unit.status.position_r))),
    structures: session.structures.filter((structure) => visibleCoords.has(coordKey(structure.q, structure.r))),
  };
}

function coordKey(q: number, r: number): string {
  return `${q},${r}`;
}

function fogVisibleCoordSet(session: SessionSnapshot, factionID: string, perspectiveUnitID = ""): Set<string> {
  if (!session.fog_of_war_enabled) {
    return new Set(session.map.tiles.map((tile) => coordKey(tile.coord.q, tile.coord.r)));
  }
  const visible = new Set<string>();
  const activeFriendlyUnits = controlledUnitsByFaction(session, factionID).filter((unit) => unit.status.life_state === "active");
  const viewers = perspectiveUnitID
    ? activeFriendlyUnits.filter((unit) => unit.id === perspectiveUnitID)
    : activeFriendlyUnits;
  for (const viewer of viewers.length > 0 ? viewers : activeFriendlyUnits) {
    for (const key of visibleCoordsFromUnit(session, viewer)) {
      visible.add(key);
    }
  }
  return visible;
}

function visibleCoordsFromUnit(session: SessionSnapshot, unit: SessionSnapshot["player_units"][number]): Set<string> {
  const origin = { q: unit.status.position_q, r: unit.status.position_r };
  const originTile = session.map.tiles.find((tile) => tile.coord.q === origin.q && tile.coord.r === origin.r);
  const baseRange = Math.max(1, unit.stats?.derived?.vision ?? 5);
  const effectiveRange = Math.max(1, baseRange + (terrainVisionRange(originTile?.terrain ?? "plains") - 5));
  const tileByKey = new Map(session.map.tiles.map((tile) => [coordKey(tile.coord.q, tile.coord.r), tile]));
  const bestCost = new Map<string, number>([[coordKey(origin.q, origin.r), 0]]);
  const queue: Array<{ q: number; r: number; cost: number }> = [{ ...origin, cost: 0 }];
  while (queue.length > 0) {
    queue.sort((left, right) => left.cost - right.cost);
    const current = queue.shift();
    if (!current || current.cost > effectiveRange) {
      continue;
    }
    for (const neighbor of axialNeighbors(current.q, current.r)) {
      const tile = tileByKey.get(coordKey(neighbor.q, neighbor.r));
      if (!tile) {
        continue;
      }
      const nextCost = current.cost + 1 + terrainVisionPenalty(tile.terrain);
      if (nextCost > effectiveRange) {
        continue;
      }
      const key = coordKey(neighbor.q, neighbor.r);
      const previous = bestCost.get(key);
      if (previous !== undefined && nextCost >= previous) {
        continue;
      }
      bestCost.set(key, nextCost);
      queue.push({ ...neighbor, cost: nextCost });
    }
  }
  return new Set(bestCost.keys());
}

function axialNeighbors(q: number, r: number): Array<{ q: number; r: number }> {
  return [
    { q: q + 1, r },
    { q: q - 1, r },
    { q, r: r + 1 },
    { q, r: r - 1 },
    { q: q + 1, r: r - 1 },
    { q: q - 1, r: r + 1 },
  ];
}

function terrainVisionRange(terrain: string): number {
  switch (terrain) {
    case "forest":
    case "swamp":
      return 2;
    case "mountain":
      return 8;
    case "grassland":
      return 6;
    case "river":
    case "ruins":
    case "village":
    case "city":
      return 3;
    case "river_valley":
    case "desert":
    case "road":
      return 4;
    default:
      return 5;
  }
}

function terrainVisionPenalty(terrain: string): number {
  return ["forest", "swamp", "river", "ruins", "village", "city", "snowfield"].includes(terrain) ? 1 : 0;
}

// factionDoctrineDraft 生成当前阵营的方针输入初始稿。
function factionDoctrineDraft(session: SessionSnapshot, factionID: string): string {
  const normalizedFactionID = factionID.trim();
  if (normalizedFactionID === "") {
    return session.global_directive.text;
  }
  for (let index = session.directive_history.length - 1; index >= 0; index -= 1) {
    const directive = session.directive_history[index];
    const kind = directive.kind?.trim() ?? "";
    if (kind !== "doctrine") {
      continue;
    }
    const appliesTo = directive.applies_to?.trim() ?? "";
    if (appliesTo === normalizedFactionID) {
      return directive.text;
    }
    if (normalizedFactionID === session.player_faction_id && appliesTo === "") {
      return directive.text;
    }
  }
  if (normalizedFactionID === session.player_faction_id) {
    return session.global_directive.text;
  }
  return "";
}

// formatDecisionShort 输出短版决策摘要，用于列表展示。
function formatDecisionShort(trace: DecisionTrace | undefined, aiTurnLine?: string): string {
  if (aiTurnLine && aiTurnLine.trim() !== "") {
    return truncateAIBrief(aiTurnLine, 14);
  }
  if (!trace) {
    return "";
  }

  const aiText = firstAIDecisionText(trace);
  if (aiText) {
    return truncateAIBrief(aiText, 14);
  }
  const requested = firstAIRequestedDecisionText(trace);
  if (requested) {
    return truncateAIBrief(requested, 14);
  }
  return "";
}

// firstAIDecisionText 从决策轨迹中提取首选 AI 文本字段。
function firstAIDecisionText(trace: DecisionTrace | null | undefined): string {
  if (!trace) {
    return "";
  }
  const candidates = [trace.next_action, trace.speak, trace.memory, trace.reasoning];
  for (const text of candidates) {
    const value = text?.trim() ?? "";
    if (value !== "") {
      return value;
    }
  }
  return "";
}

// firstAIRequestedDecisionText 提取“请求动作”侧的 AI 文本字段。
function firstAIRequestedDecisionText(trace: DecisionTrace | null | undefined): string {
  if (!trace) {
    return "";
  }
  const candidates = [
    trace.requested_next_action,
    trace.requested_speak,
    trace.requested_memory,
    trace.requested_reasoning,
  ];
  for (const text of candidates) {
    const value = text?.trim() ?? "";
    if (value !== "") {
      return value;
    }
  }
  return "";
}

// truncateAIBrief 裁剪 AI 文案长度，避免挤占列表宽度。
function truncateAIBrief(text: string, max: number): string {
  const value = text.trim();
  if (!value) {
    return "";
  }
  const runes = Array.from(value);
  if (runes.length <= max) {
    return value;
  }
  return `${runes.slice(0, max).join("")}…`;
}

// formatAPTrace 格式化 AP 消耗轨迹（前/耗/后）。
function formatAPTrace(trace: DecisionTrace): string {
  if (typeof trace.ap_before !== "number") {
    return "";
  }
  const before = trace.ap_before;
  const cost = trace.ap_cost ?? 0;
  const after = trace.ap_after ?? before;
  return ` · AP ${before}-${cost}=${after}`;
}

// findUnitName 按 unit_id 查询单位展示名。
function findUnitName(session: SessionSnapshot | null, unitID?: string): string {
  if (!session || !unitID) {
    return "目标";
  }
  const unit = [...session.player_units, ...session.enemy_units].find((entry) => entry.id === unitID);
  return unit?.identity.name ?? "目标";
}

// latestAITurnLineByUnit 按单位聚合当前回合最新 AI 日志句。
function latestAITurnLineByUnit(logs: SessionLog[], turn: number): Map<string, string> {
  const ranked = new Map<string, { text: string; priority: number }>();
  for (const entry of logs) {
    if (entry.turn !== turn) {
      continue;
    }
    const targets = lineTargetUnitIDs(entry);
    if (targets.length === 0) {
      continue;
    }
    const candidate = aiTurnLineFromLog(entry);
    if (!candidate) {
      continue;
    }
    for (const unitID of targets) {
      const previous = ranked.get(unitID);
      if (!previous || candidate.priority >= previous.priority) {
        ranked.set(unitID, candidate);
      }
    }
  }
  const result = new Map<string, string>();
  for (const [unitID, value] of ranked) {
    result.set(unitID, value.text);
  }
  return result;
}

// unitBubbleLinesByUnit 汇总当前回合某单位的 speak 气泡，地图上不再混入 reasoning/memory/log 文本。
function unitBubbleLinesByUnit(session: SessionSnapshot): Map<string, string[]> {
  const result = new Map<string, string[]>();
  const seen = new Map<string, Set<string>>();
  const addLine = (unitID: string | undefined, text: string | undefined, maxRunes = 28) => {
    const id = unitID?.trim();
    const value = truncateAIBrief(trimSpeakerPrefix(text ?? ""), maxRunes);
    if (!id || !value) {
      return;
    }
    const unitSeen = seen.get(id) ?? new Set<string>();
    const dedupeKey = normalizeBubbleDedupeKey(value);
    for (const existing of unitSeen) {
      if (isSameBubbleText(existing, dedupeKey)) {
        return;
      }
    }
    if (unitSeen.has(dedupeKey)) {
      return;
    }
    unitSeen.add(dedupeKey);
    seen.set(id, unitSeen);
    const lines = result.get(id) ?? [];
    lines.push(value);
    result.set(id, lines);
  };

  for (const trace of session.decision_traces) {
    if (trace.turn !== session.turn_state.turn) {
      continue;
    }
    addLine(trace.unit_id, trace.speak);
  }

  for (const entry of session.dialogue_history) {
    if (entry.turn !== session.turn_state.turn || entry.speaker === "player") {
      continue;
    }
    addLine(entry.unit_id, entry.message);
  }

  for (const entry of session.logs) {
    if (entry.turn !== session.turn_state.turn || !isTwoPartyDialogueKind(entry.kind)) {
      continue;
    }
    addLine(entry.actor_unit_id, dialogueThreadTextFromLog(entry));
    addLine(entry.target_unit_id, dialogueThreadTextFromLog(entry));
  }

  return result;
}

function normalizeBubbleDedupeKey(value: string): string {
  return value.replace(/…$/u, "").replace(/\s+/g, " ").trim();
}

function isSameBubbleText(left: string, right: string): boolean {
  if (!left || !right) {
    return false;
  }
  if (left === right) {
    return true;
  }
  const minLength = Math.min(Array.from(left).length, Array.from(right).length);
  return minLength >= 8 && (left.startsWith(right) || right.startsWith(left));
}

// lineTargetUnitIDs 解析日志条目的目标单位 ID 集合。
function lineTargetUnitIDs(entry: SessionLog): string[] {
  const ids: string[] = [];
  const actorUnitID = entry.actor_unit_id?.trim();
  if (actorUnitID) {
    ids.push(actorUnitID);
  }
  const targetUnitID = entry.target_unit_id?.trim();
  if (targetUnitID && !ids.includes(targetUnitID)) {
    ids.push(targetUnitID);
  }
  return ids;
}

// aiTurnLineFromLog 从日志文本中提取可用于前端展示的 AI 行动句。
function aiTurnLineFromLog(entry: SessionLog): { text: string; priority: number } | null {
  switch (entry.kind) {
    case "action_narration":
      return { text: trimSpeakerPrefix(entry.message), priority: 6 };
    case "reaction_queue":
      return { text: trimSpeakerPrefix(entry.message), priority: 6 };
    case "unit_dialogue":
    case "romance_proposal":
    case "romance":
    case "romance_hold":
    case "family":
    case "family_hold":
    case "pregnancy":
      return { text: trimSpeakerPrefix(entry.message), priority: 5 };
    case "shake":
    case "emotional_override":
      return { text: trimSpeakerPrefix(entry.message), priority: 5 };
    case "trade":
    case "trade_offer":
    case "trade_accept":
    case "trade_rejected":
    case "trade_hold":
    case "trade_blocked":
      return { text: trimSpeakerPrefix(entry.message), priority: 4 };
    case "knowledge":
      return { text: trimSpeakerPrefix(entry.message), priority: 4 };
    case "eat":
    case "pigeon_send":
    case "pigeon_deliver":
    case "pigeon_intercept":
    case "pigeon_blocked":
    case "pigeon_lost":
    case "pigeon_attachment":
    case "pigeon_attachment_lost":
    case "random_event":
      return { text: trimSpeakerPrefix(entry.message), priority: 3 };
    case "speech":
      return { text: trimSpeakerPrefix(entry.message), priority: 3 };
    case "attack":
    case "attack_miss":
    case "move":
    case "move_blocked":
    case "advance":
    case "defend":
    case "observe":
    case "assist":
    case "hold":
    case "skill":
    case "build":
    case "gather":
      return { text: trimSpeakerPrefix(entry.message), priority: 2 };
    default:
      return null;
  }
}

// trimSpeakerPrefix 去掉日志里的“说话人前缀”噪音。
function trimSpeakerPrefix(message: string): string {
  const value = message.trim();
  if (value === "") {
    return "";
  }
  const parts = value.split("：");
  if (parts.length < 2) {
    return value;
  }
  return parts.slice(1).join("：").trim();
}

// buildUnitDialogueThreads 把单位间对话日志聚合为可读的双单位对话线程。
function buildUnitDialogueThreads(session: SessionSnapshot): UnitDialogueThread[] {
  const dialogueLogs = session.logs.filter((entry) => isUnitDialogueThreadLog(entry));
  if (dialogueLogs.length === 0) {
    return [];
  }

  const aiDialogues = session.dialogue_history.filter((entry) => entry.speaker !== "player");
  const threads: UnitDialogueThread[] = [];
  let dialogueCursor = 0;
  let pendingDialogues: Array<{ entry: DialogueMessage; index: number }> = [];

  for (const log of dialogueLogs) {
    const fallbackLines = parseDialogueSummaryLines(log, session);
    if ((log.kind === "unit_dialogue" || log.kind === "romance_proposal") && fallbackLines.length > 0) {
      const participantUnitIDs = lineTargetUnitIDs(log);
      threads.push({
        id: log.id,
        turn: log.turn,
        phase: log.phase,
        occurredAt: log.occurred_at,
        actorUnitID: log.actor_unit_id?.trim() ?? "",
        targetUnitID: log.target_unit_id?.trim() ?? "",
        participantUnitIDs,
        summary: dialogueThreadTextFromLog(log),
        lines: fallbackLines,
      });
      continue;
    }

    const logOccurredAt = parseOccurredAtMs(log.occurred_at);
    while (dialogueCursor < aiDialogues.length) {
      const entry = aiDialogues[dialogueCursor];
      const entryOccurredAt = parseOccurredAtMs(entry.occurred_at);
      if (Number.isFinite(logOccurredAt) && Number.isFinite(entryOccurredAt) && entryOccurredAt > logOccurredAt) {
        break;
      }
      pendingDialogues.push({ entry, index: dialogueCursor });
      dialogueCursor += 1;
    }

    const participantUnitIDs = lineTargetUnitIDs(log);
    const matchingHistory = pendingDialogues.filter(
      ({ entry }) =>
        entry.turn === log.turn &&
        entry.phase === log.phase &&
        participantUnitIDs.includes(entry.unit_id.trim()),
    );
    const latestHistoryByUnit = new Map<string, { entry: DialogueMessage; index: number }>();
    for (const item of matchingHistory) {
      latestHistoryByUnit.set(item.entry.unit_id.trim(), item);
    }

    const fallbackByUnit = new Map<string, UnitDialogueThreadLine>();
    for (const line of fallbackLines) {
      if (!line.unitID) {
        continue;
      }
      fallbackByUnit.set(line.unitID, line);
    }

    const lines: UnitDialogueThreadLine[] = [];
    for (const unitID of participantUnitIDs) {
      const historyItem = latestHistoryByUnit.get(unitID);
      if (historyItem) {
        lines.push({
          id: historyItem.entry.id,
          unitID,
          speaker: historyItem.entry.speaker,
          message: historyItem.entry.message,
          turn: historyItem.entry.turn,
          phase: historyItem.entry.phase,
          occurredAt: historyItem.entry.occurred_at,
        });
        continue;
      }
      const fallbackItem = fallbackByUnit.get(unitID);
      if (fallbackItem) {
        lines.push(fallbackItem);
      }
    }
    if (lines.length === 0) {
      lines.push(...fallbackLines);
    }

    pendingDialogues = pendingDialogues.filter(
      ({ entry }) =>
        !(
          entry.turn === log.turn &&
          entry.phase === log.phase &&
          participantUnitIDs.includes(entry.unit_id.trim())
        ),
    );

    threads.push({
      id: log.id,
      turn: log.turn,
      phase: log.phase,
      occurredAt: log.occurred_at,
      actorUnitID: log.actor_unit_id?.trim() ?? "",
      targetUnitID: log.target_unit_id?.trim() ?? "",
      participantUnitIDs,
      summary: dialogueThreadTextFromLog(log),
      lines,
    });
  }

  return threads;
}

function isUnitDialogueThreadLog(entry: SessionLog): boolean {
  return isTwoPartyDialogueKind(entry.kind);
}

function isTwoPartyDialogueKind(kind: string): boolean {
  return [
    "unit_dialogue",
    "romance_proposal",
    "romance",
    "romance_hold",
    "family",
    "family_hold",
    "pregnancy",
    "trade_offer",
    "trade_accept",
    "trade_rejected",
    "trade",
  ].includes(kind);
}

function dialogueThreadTextFromLog(log: SessionLog): string {
  const raw = log.message.trim();
  if (isUnitDialogueThreadLog(log)) {
    return raw;
  }
  return aiTurnLineFromLog(log)?.text ?? trimSpeakerPrefix(raw);
}

// parseDialogueSummaryLines 从单位交谈摘要里拆出双方短句，作为历史缺口时的兜底展示。
function parseDialogueSummaryLines(log: SessionLog, session: SessionSnapshot): UnitDialogueThreadLine[] {
  const text = dialogueThreadTextFromLog(log);
  if (!text) {
    return [];
  }

  const participantUnitIDs = lineTargetUnitIDs(log);
  const nameToUnitID = new Map<string, string>();
  for (const unitID of participantUnitIDs) {
    const name = findUnitName(session, unitID).trim();
    if (name) {
      nameToUnitID.set(name, unitID);
      for (const alias of dialogueNameAliases(name)) {
        nameToUnitID.set(alias, unitID);
      }
    }
  }

  const lines: UnitDialogueThreadLine[] = [];
  for (const rawSegment of text.split(/[；;]+/)) {
    const segment = rawSegment.trim();
    if (!segment) {
      continue;
    }
    const dividerIndex = segment.indexOf("：");
    if (dividerIndex <= 0) {
      continue;
    }
    const rawSpeaker = segment.slice(0, dividerIndex).trim();
    const tagMatch = rawSpeaker.match(/^(【[^】]+】)(.+)$/);
    const speaker = (tagMatch?.[2] ?? rawSpeaker).trim();
    const messagePrefix = tagMatch?.[1] ?? "";
    const message = `${messagePrefix}${segment.slice(dividerIndex + 1).trim()}`;
    if (!speaker || !message) {
      continue;
    }
    const fallbackUnitID = participantUnitIDs[lines.length] ?? "";
    lines.push({
      id: `${log.id}:${lines.length}`,
      unitID: nameToUnitID.get(speaker) ?? fallbackUnitID,
      speaker,
      message,
      turn: log.turn,
      phase: log.phase,
      occurredAt: log.occurred_at,
    });
  }
  return lines;
}

function dialogueNameAliases(name: string): string[] {
  const trimmed = name.trim();
  if (!trimmed) {
    return [];
  }
  const aliases = new Set<string>();
  aliases.add(trimmed);
  if (trimmed.length > 2) {
    aliases.add(trimmed.slice(-2));
  }
  if (trimmed.length > 3) {
    aliases.add(trimmed.slice(-3));
  }
  const suffixes = ["先生", "姑娘", "小哥", "大姐", "老周", "军师", "枪客", "刀客", "猎户", "铁匠", "医徒", "斥候"];
  for (const suffix of suffixes) {
    if (trimmed.endsWith(suffix) && trimmed.length > suffix.length) {
      aliases.add(trimmed.slice(0, -suffix.length));
    }
  }
  const parts = trimmed.split(/[·#\s]+/).filter(Boolean);
  for (const part of parts) {
    aliases.add(part);
  }
  return [...aliases].filter((alias) => alias !== trimmed);
}

// parseOccurredAtMs 把 ISO 时间转成毫秒；失败时返回 NaN。
function parseOccurredAtMs(value: string): number {
  const ms = Date.parse(value);
  return Number.isFinite(ms) ? ms : Number.NaN;
}

// formatInteractionKind 格式化 LLM 交互类型标签。
function formatInteractionKind(kind: string): string {
  switch (kind) {
    case "decision":
      return "行动决策";
    case "shake":
      return "行动决策";
    case "unit_decision":
      return "行动决策";
    case "dialogue":
      return "对话回复";
    case "reflection":
      return "即时自白";
    case "intent_parse":
      return "指令解析";
    case "deployment":
      return "部署决策";
    case "upkeep":
      return "回合维护";
    case "backstory":
      return "角色生成";
    case "unit_dialogue":
      return "单位交谈";
    case "romance_proposal":
      return "表白";
    case "battle_report":
      return "回合战报";
    case "downtime":
      return "闲时决策";
    case "strategy":
      return "策略生成";
    default:
      return kind;
  }
}

// formatThoughtSummary 汇总并裁剪思考摘要文案。
function formatThoughtSummary(interaction: LLMInteraction | null, trace: DecisionTrace | null | undefined): string {
  if (interaction?.summary) {
    return interaction.summary;
  }
  if (interaction?.error_message) {
    return interaction.error_message;
  }
  const decisionText = firstAIDecisionText(trace);
  if (decisionText) {
    return decisionText;
  }
  const requestedText = firstAIRequestedDecisionText(trace);
  if (requestedText) {
    return requestedText;
  }
  if (trace?.reasoning) {
    return trace.reasoning;
  }
  return "";
}

// formatRosterMeta 格式化单位面板中的状态元信息。
function formatRosterMeta(hp: number, brief: string): string {
  const text = brief.trim();
  if (!text) {
    return `${hp} HP`;
  }
  return `${hp} HP · ${text}`;
}

// formatInventorySummary 格式化背包与装备摘要。
function formatInventorySummary(unit: SessionSnapshot["player_units"][number]): string {
  const equipped = Object.values(unit.inventory.equipment).map((stack) => formatStackWithDetails(stack));
  const backpack = unit.inventory.backpack.map((stack) => `${formatStackWithDetails(stack)} x${stack.quantity}`);
  const parts: string[] = [];
  parts.push(equipped.length > 0 ? `装备: ${equipped.join(" / ")}` : "装备: 无");
  parts.push(backpack.length > 0 ? `背包: ${backpack.join(" / ")}` : "背包: 空");
  return parts.join(" · ");
}

function formatEquipmentEntries(unit: SessionSnapshot["player_units"][number]): string[] {
  return Object.entries(unit.inventory.equipment).map(
    ([slot, stack]) => `${displayEquipmentSlotLabel(slot)}：${formatStackWithDetails(stack)} x${stack.quantity}`,
  );
}

function formatBackpackEntries(unit: SessionSnapshot["player_units"][number]): string[] {
  return unit.inventory.backpack.map((stack, index) => `#${index + 1} ${formatStackWithDetails(stack)} x${stack.quantity}`);
}

function buildInventoryQuantitySnapshot(session: SessionSnapshot): Map<string, number> {
  const result = new Map<string, number>();
  for (const unit of allSnapshotUnits(session)) {
    result.set(walletChangeKey(unit.id), Math.max(0, unit.status.wallet));
    for (const stack of unitInventoryStacks(unit)) {
      const key = inventoryGainKey(unit.id, stack);
      result.set(key, (result.get(key) ?? 0) + Math.max(0, stack.quantity));
    }
  }
  return result;
}

function detectInventoryChanges(
  session: SessionSnapshot,
  previous: Map<string, number>,
  next: Map<string, number>,
  visibleUnitIDs: Set<string> | null,
): Array<Omit<UnitItemGainToast, "id" | "createdAt">> {
  const infoByKey = buildInventoryGainInfoMap(session);
  const changes: Array<Omit<UnitItemGainToast, "id" | "createdAt">> = [];

  for (const [key, quantity] of next) {
    const delta = quantity - (previous.get(key) ?? 0);
    if (delta === 0) {
      continue;
    }
    const info = infoByKey.get(key);
    if (!info) {
      continue;
    }
    if (visibleUnitIDs && !visibleUnitIDs.has(info.unitID)) {
      continue;
    }
    changes.push({
      unitID: info.unitID,
      unitName: info.unitName,
      resource: key.startsWith("wallet:") ? "gold" : "item",
      direction: delta > 0 ? "gain" : "loss",
      itemLabel: info.itemLabel,
      quantity: Math.abs(delta),
    });
  }

  for (const [key, quantity] of previous) {
    if (next.has(key) || quantity <= 0) {
      continue;
    }
    const info = infoByKey.get(key) ?? inventoryChangeInfoFromKey(key, session);
    if (!info) {
      continue;
    }
    if (visibleUnitIDs && !visibleUnitIDs.has(info.unitID)) {
      continue;
    }
    changes.push({
      unitID: info.unitID,
      unitName: info.unitName,
      resource: key.startsWith("wallet:") ? "gold" : "item",
      direction: "loss",
      itemLabel: info.itemLabel,
      quantity,
    });
  }

  return changes;
}

function inventoryGainToastVisibleUnitIDs(session: SessionSnapshot, commanderFactionID: string): Set<string> | null {
  if (!session.fog_of_war_enabled) {
    return null;
  }
  return new Set(controlledUnitsByFaction(session, commanderFactionID).map((unit) => unit.id));
}

function buildInventoryGainInfoMap(session: SessionSnapshot): Map<string, { unitID: string; unitName: string; itemLabel: string }> {
  const result = new Map<string, { unitID: string; unitName: string; itemLabel: string }>();
  for (const unit of allSnapshotUnits(session)) {
    result.set(walletChangeKey(unit.id), {
      unitID: unit.id,
      unitName: unit.identity.name,
      itemLabel: "金币",
    });
    for (const stack of unitInventoryStacks(unit)) {
      result.set(inventoryGainKey(unit.id, stack), {
        unitID: unit.id,
        unitName: unit.identity.name,
        itemLabel: displayStackLabel(stack),
      });
    }
  }
  return result;
}

function inventoryChangeInfoFromKey(
  key: string,
  session: SessionSnapshot,
): { unitID: string; unitName: string; itemLabel: string } | null {
  const parts = key.split("\u001f");
  const unitID = key.startsWith("wallet:") ? key.slice("wallet:".length) : parts[0];
  if (!unitID) {
    return null;
  }
  const unit = allSnapshotUnits(session).find((candidate) => candidate.id === unitID);
  const unitName = unit?.identity.name ?? "未知单位";
  if (key.startsWith("wallet:")) {
    return { unitID, unitName, itemLabel: "金币" };
  }
  return { unitID, unitName, itemLabel: displayItemLabel(parts[1] ?? "物品") };
}

function allSnapshotUnits(session: SessionSnapshot): BattleUnit[] {
  return [...session.player_units, ...session.enemy_units, ...(session.wild_units ?? [])];
}

function formatChatUnitState(unit: BattleUnit): string {
  switch (unit.status.life_state) {
    case "dead":
      return "已死亡";
    case "down":
      return `倒地 · HP ${unit.status.hp}`;
    case "recovering":
      return `恢复中 · HP ${unit.status.hp}`;
    default:
      return `HP ${unit.status.hp}`;
  }
}

function formatUnitSocialTies(unit: BattleUnit, session: SessionSnapshot | null): string {
  if (!session) {
    return "暂无伴侣、父母或小孩记录。";
  }
  const unitNames = new Map(allSnapshotUnits(session).map((record) => [record.id, record.identity.name]));
  const parts: string[] = [];
  const loverID = unit.social?.lover_unit_id?.trim();
  if (loverID) {
    parts.push(`伴侣：${unitNames.get(loverID) ?? loverID}`);
  }
  const parents = formatUnitNameList(unit.social?.parent_unit_ids, unitNames);
  if (parents) {
    parts.push(`父母：${parents}`);
  }
  const children = formatUnitNameList(unit.social?.child_unit_ids, unitNames);
  if (children) {
    parts.push(`小孩：${children}`);
  }
  const pregnancy = session.pregnancies?.find(
    (entry) => entry.pregnant_unit_id === unit.id || entry.parent_unit_ids?.includes(unit.id),
  );
  if (pregnancy) {
    const pregnantName = unitNames.get(pregnancy.pregnant_unit_id) ?? pregnancy.pregnant_unit_id;
    const remaining = Math.max(0, pregnancy.due_turn - session.turn_state.turn);
    if (pregnancy.pregnant_unit_id === unit.id) {
      parts.push(`怀孕中：预产 T${pregnancy.due_turn}，剩余 ${remaining} 回合`);
    } else {
      parts.push(`${pregnantName} 怀孕中：预产 T${pregnancy.due_turn}`);
    }
  }
  return parts.length > 0 ? parts.join(" / ") : "暂无伴侣、父母或小孩记录。";
}

function formatUnitNameList(unitIDs: string[] | undefined, unitNames: Map<string, string>): string {
  const names = (unitIDs ?? [])
    .map((unitID) => unitID.trim())
    .filter(Boolean)
    .map((unitID) => unitNames.get(unitID) ?? unitID);
  return names.join("、");
}

function unitInventoryStacks(unit: BattleUnit): BattleUnit["inventory"]["backpack"] {
  return [...Object.values(unit.inventory.equipment), ...unit.inventory.backpack];
}

function inventoryGainKey(unitID: string, stack: { item_id: string; custom_name?: string; level?: number }): string {
  return [
    unitID,
    stack.item_id.trim(),
    stack.custom_name?.trim() ?? "",
    stack.level ?? 0,
  ].join("\u001f");
}

function walletChangeKey(unitID: string): string {
  return `wallet:${unitID.trim()}`;
}

type UnitChangeTimelineEntry = {
  id: string;
  kind: "status" | "inventory" | "decision";
  title: string;
  detail: string;
  meta: string;
  occurredAt: string;
  turn: number;
};

type StatusPayload = {
  unit_id?: string;
  field?: string;
  delta?: number;
  before?: number;
  after?: number;
  reason_code?: string;
  reason_text?: string;
  actors?: string[];
  location?: string;
};

function buildUnitChangeTimeline(session: SessionSnapshot | null, unitID: string | null): UnitChangeTimelineEntry[] {
  if (!session || !unitID) {
    return [];
  }
  const unitNames = new Map<string, string>();
  for (const unit of [...session.player_units, ...session.enemy_units, ...(session.wild_units ?? [])]) {
    unitNames.set(unit.id, unit.identity.name);
  }
  const entries: UnitChangeTimelineEntry[] = [];

  for (const event of session.raw_event_log ?? []) {
    const payload = parseStatusPayload(event.payload_json);
    const payloadUnitID = payload?.unit_id ?? event.target_unit_id;
    if (payloadUnitID !== unitID) {
      continue;
    }
    const field = payload?.field ?? event.kind;
    const before = typeof payload?.before === "number" ? payload.before : undefined;
    const after = typeof payload?.after === "number" ? payload.after : undefined;
    const delta = typeof payload?.delta === "number" ? payload.delta : undefined;
    const actorText = formatActorNames(payload?.actors, event.actor_unit_id, unitNames, unitID);
    const reason = payload?.reason_text || event.summary || "状态发生变化。";
    const title = `${formatStatusFieldLabel(field)} ${formatNumberDelta(delta)}${before !== undefined && after !== undefined ? `（${formatNumberValue(before)} → ${formatNumberValue(after)}）` : ""}`;
    const detailParts = [`原因：${reason}`];
    if (actorText) {
      detailParts.push(`来源：${actorText}`);
    }
    if (payload?.location) {
      detailParts.push(`地点：${payload.location.replace(/^hex_/, "").replace("_", ",")}`);
    }
    if (payload?.reason_code) {
      detailParts.push(`代码：${payload.reason_code}`);
    }
    entries.push({
      id: `status-${event.id}`,
      kind: "status",
      title,
      detail: detailParts.join("；"),
      meta: formatTimelineMeta(event.turn, event.phase, event.occurred_at),
      occurredAt: event.occurred_at,
      turn: event.turn,
    });
  }

  const inventoryKinds = new Set([
    "opening_supply",
    "loot",
    "pickup",
    "pickup_blocked",
    "gather",
    "eat",
    "eat_blocked",
    "heal",
    "heal_blocked",
    "equip",
    "forge",
    "upgrade",
    "trade",
    "trade_blocked",
    "pigeon_dispatch",
    "pigeon_delivery",
  ]);
  for (const log of session.logs ?? []) {
    if (!inventoryKinds.has(log.kind)) {
      continue;
    }
    if (log.actor_unit_id !== unitID && log.target_unit_id !== unitID) {
      continue;
    }
    const direction = log.actor_unit_id === unitID ? "主动" : `来自 ${unitNames.get(log.actor_unit_id ?? "") ?? "其他单位"}`;
    entries.push({
      id: `inventory-${log.id}`,
      kind: "inventory",
      title: `${formatLogKindLabel(log.kind)} · ${direction}`,
      detail: log.message || "物资/装备发生变化。",
      meta: formatTimelineMeta(log.turn, log.phase, log.occurred_at),
      occurredAt: log.occurred_at,
      turn: log.turn,
    });
  }

  for (const decision of session.decision_traces ?? []) {
    if (decision.unit_id !== unitID) {
      continue;
    }
    const apText = decision.ap_before !== undefined || decision.ap_after !== undefined
      ? `AP ${decision.ap_before ?? "?"} → ${decision.ap_after ?? "?"}（消耗 ${decision.ap_cost ?? "?"}）`
      : "AP 未记录";
    const target = decision.target_unit_id ? `；目标：${unitNames.get(decision.target_unit_id) ?? decision.target_unit_id}` : "";
    entries.push({
      id: `decision-${decision.id}`,
      kind: "decision",
      title: `AI 决策：${formatDecisionActionLabel(decision.action)}${decision.requested_action && decision.requested_action !== decision.action ? `（原想 ${formatDecisionActionLabel(decision.requested_action)}）` : ""}`,
      detail: `${decision.next_action || decision.speak || "执行动作"}；${apText}${target}；理由：${decision.reasoning || "未记录"}`,
      meta: formatTimelineMeta(decision.turn, decision.phase, decision.occurred_at),
      occurredAt: decision.occurred_at,
      turn: decision.turn,
    });
  }

  return entries.sort((left, right) => {
    if (right.turn !== left.turn) {
      return right.turn - left.turn;
    }
    return new Date(right.occurredAt).getTime() - new Date(left.occurredAt).getTime();
  });
}

function parseStatusPayload(payloadJSON?: string): StatusPayload | null {
  if (!payloadJSON) {
    return null;
  }
  try {
    const parsed = JSON.parse(payloadJSON) as StatusPayload;
    return parsed && typeof parsed === "object" ? parsed : null;
  } catch {
    return null;
  }
}

function formatActorNames(actors: string[] | undefined, fallbackActorID: string | undefined, unitNames: Map<string, string>, selfUnitID: string): string {
  const ids = actors && actors.length > 0 ? actors : fallbackActorID ? [fallbackActorID] : [];
  return ids
    .filter((id) => id && id !== selfUnitID)
    .map((id) => unitNames.get(id) ?? id)
    .join("、");
}

function formatStatusFieldLabel(field?: string): string {
  switch (field) {
    case "hp":
      return "HP";
    case "hunger":
      return "饥饿";
    case "morale":
      return "士气";
    case "loyalty":
      return "忠诚";
    case "wallet":
      return "钱包";
    case "attack":
      return "攻击";
    case "defense":
      return "防御";
    case "move":
      return "移动";
    case "lives_remaining":
      return "命数";
    case "fatigue":
      return "疲劳";
    default:
      return field || "状态";
  }
}

function formatNumberDelta(value?: number): string {
  if (value === undefined || Number.isNaN(value)) {
    return "变动";
  }
  return value > 0 ? `+${formatNumberValue(value)}` : formatNumberValue(value);
}

function formatNumberValue(value: number): string {
  return Number.isInteger(value) ? `${value}` : value.toFixed(2);
}

function formatLogKindLabel(kind: string): string {
  switch (kind) {
    case "opening_supply":
      return "开局补给";
    case "loot":
      return "战利品";
    case "pickup":
      return "拾取";
    case "pickup_blocked":
      return "拾取失败";
    case "gather":
      return "采集/生产";
    case "eat":
      return "进食";
    case "eat_blocked":
      return "进食失败";
    case "heal":
      return "治疗";
    case "heal_blocked":
      return "治疗失败";
    case "equip":
      return "装备";
    case "forge":
      return "锻造";
    case "upgrade":
      return "强化";
    case "trade":
      return "交易";
    case "trade_blocked":
      return "交易失败";
    case "pigeon_dispatch":
      return "信鸽寄送";
    case "pigeon_delivery":
      return "信鸽送达";
    default:
      return kind;
  }
}

function formatDecisionActionLabel(action?: string): string {
  switch (action) {
    case "attack":
      return "攻击";
    case "charge":
      return "冲锋";
    case "heavy_attack":
      return "重击";
    case "skill":
      return "技能";
    case "defend":
      return "防御";
    case "observe":
      return "观察";
    case "assist":
      return "援助";
    case "chat":
      return "聊天";
    case "say":
      return "发言";
    case "dialogue":
      return "交谈";
    case "trade":
      return "交易";
    case "romance":
      return "表白";
    case "family":
      return "养育";
    case "build":
      return "建造";
    case "demolish":
      return "拆除";
    case "gather":
      return "采集";
    case "forge":
      return "锻造";
    case "upgrade":
      return "强化";
    case "equip":
      return "装备";
    case "eat":
      return "进食";
    case "pickup":
      return "拾取";
    case "move":
      return "移动";
    case "hold":
      return "待命";
    default:
      return action || "未知动作";
  }
}

function formatTimelineMeta(turn: number, phase: Phase, occurredAt: string): string {
  const time = occurredAt ? new Date(occurredAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) : "--:--";
  return `T${turn} · ${phaseLabels[phase] ?? phase} · ${time}`;
}

function formatGraveMarkers(markers: NonNullable<SessionSnapshot["grave_markers"]>): string {
  return markers.map((marker) => `${marker.unit_name}葬身之地（T${marker.turn}）`).join("、");
}

function formatGroundLootDrops(drops: NonNullable<SessionSnapshot["ground_loot_drops"]>): string {
  return drops
    .map((drop) => {
      const source = drop.source_unit_name ? `${drop.source_unit_name}遗落` : "遗落物";
      const items = drop.items.map((stack) => `${displayStackLabel(stack)} x${stack.quantity}`).join("、") || "无";
      return `${source}：${items}（T${drop.turn}）`;
    })
    .join("；");
}

// formatMemorySummary 格式化单位记忆摘要列表。
function formatMemorySummary(
  unit: SessionSnapshot["player_units"][number],
  currentTurn?: number,
): string {
  const highlights = unit.memory.highlights.slice(-3);
  if (highlights.length === 0) {
    return "";
  }
  return highlights
    .map((line) => formatTurnAwareLine(line, currentTurn))
    .filter((line) => line !== "")
    .join(" / ");
}

// formatTerrainRuleSummary 格式化地形规则说明。
function formatTerrainRuleSummary(terrain: TerrainDefinition | null): string {
  if (!terrain) {
    return "";
  }
  const parts: string[] = [];
  if (terrain.combat_rules.length > 0) {
    parts.push(`战斗：${terrain.combat_rules.join("、")}`);
  }
  if (terrain.special_rules.length > 0) {
    parts.push(`特殊：${terrain.special_rules.join("、")}`);
  }
  if (terrain.activities.length > 0) {
    parts.push(`可行动作：${terrain.activities.join("、")}`);
  }
  if (terrain.resources.length > 0) {
    parts.push(`资源：${terrain.resources.join("、")}`);
  }
  parts.push(`移动成本：${terrain.move_cost}`);
  parts.push(`视野基准：${terrain.vision_range}`);
  return parts.join("；");
}

// formatCurrentInfluenceSummary 格式化“当前属性受环境影响”说明。
function formatCurrentInfluenceSummary(
  session: SessionSnapshot | null,
  unit: SessionSnapshot["player_units"][number],
  terrain: TerrainDefinition | null,
  structure: SessionSnapshot["structures"][number] | null,
  decision: DecisionTrace | null | undefined,
): string {
  const parts: string[] = [];
  if (terrain) {
    parts.push(`地形=${terrain.display_name}（移动成本 ${terrain.move_cost}）`);
  }
  if (session?.weather?.display_name) {
    parts.push(`天气=${session.weather.display_name}`);
    if (session.weather.note) {
      parts.push(`天气说明：${session.weather.note}`);
    }
    switch (session.weather.type) {
      case "foggy":
        parts.push("浓雾会压缩射程并削弱远距稳定性");
        break;
      case "rainy":
        parts.push("阴雨会降低远距稳定性并增加体力消耗");
        break;
      case "windy":
        parts.push("大风会降低远距稳定性");
        break;
      default:
        break;
    }
  }
  if (unit.status.hunger < 30) {
    parts.push("当前低饥饿阈值：行动效率下降（移速/攻击乘区受压）");
  }
  if ((unit.status.starvation_turns ?? 0) > 0) {
    parts.push(`已断粮 ${unit.status.starvation_turns ?? 0} 回合`);
  }
  if (structure) {
    const friendly = structure.faction_id === unit.faction_id;
    if (!structure.completed) {
      parts.push(
        `${friendly ? "己方" : "对方"}${formatStructureType(structure.type)}施工中：${structure.build_progress}/${structure.build_required}`,
      );
    } else if (friendly) {
      switch (structure.type) {
        case "turret":
          parts.push("炮台增效：攻击修正 +8，且攻击距离至少 3 格");
          break;
        case "watchtower":
          parts.push("瞭望塔增效：攻击修正 +2，且攻击距离至少 2 格");
          break;
        case "forge":
          parts.push("铁匠铺增效：攻击 +4、防御 +3");
          break;
        case "farmland":
          parts.push("农田影响：当前格可周期性收粮，保障补给");
          break;
        default:
          break;
      }
    } else {
      parts.push(`敌方${formatStructureType(structure.type)}占位中：该格对敌方可能有额外增益`);
    }
  }
  const moveMultiplier = decision?.move_multiplier ?? 1;
  if (Number.isFinite(moveMultiplier) && Math.abs(moveMultiplier - 1) >= 0.01) {
    parts.push(`本轮机动修正：x${moveMultiplier.toFixed(2)}（天气/地形/状态综合结果）`);
  }
  const attackMultiplier = decision?.attack_multiplier ?? 1;
  if (Number.isFinite(attackMultiplier) && Math.abs(attackMultiplier - 1) >= 0.01) {
    parts.push(`本轮攻防乘区：x${attackMultiplier.toFixed(2)}（天气/地形/状态综合结果）`);
  }
  if (parts.length === 0) {
    return "";
  }
  return parts.join("；");
}

// formatCurrentStructureSummary 格式化脚下建筑状态说明。
function formatCurrentStructureSummary(
  structure: SessionSnapshot["structures"][number] | null,
  unit: BattleUnit | null,
  session: SessionSnapshot | null,
): string {
  if (!structure) {
    return "无";
  }
  const side =
    unit && structure.faction_id === unit.faction_id
      ? "己方"
      : session && structure.faction_id === session.player_faction_id
        ? "玩家阵营"
        : "敌方";
  if (!structure.completed) {
    return `${side}${formatStructureType(structure.type)}（施工中 ${structure.build_progress}/${structure.build_required}）`;
  }
  return `${side}${formatStructureType(structure.type)}（已完成）`;
}

// formatKnowledgeHighlights 格式化单位知识记忆（世界规律）摘要。
function formatKnowledgeHighlights(
  unit: SessionSnapshot["player_units"][number],
  decision: DecisionTrace | null | undefined,
): string {
  const lines: string[] = [];
  const newest = decision?.knowledge?.trim() ?? "";
  if (newest !== "") {
    lines.push(newest.startsWith("规律：") ? newest : `规律：${newest}`);
  }
  for (const line of unit.memory.highlights) {
    const text = formatTurnAwareLine(line, undefined).trim();
    if (!text.startsWith("规律：")) {
      continue;
    }
    if (lines.includes(text)) {
      continue;
    }
    lines.push(text);
  }
  const result = lines.slice(0, 4);
  if (result.length === 0) {
    return "";
  }
  return result.join(" / ");
}

// formatStructureType 格式化建筑类型名称。
function formatStructureType(structureType?: string): string {
  switch (structureType) {
    case "farmland":
      return "农田";
    case "forge":
      return "铁匠铺";
    case "trap":
      return "陷阱";
    case "turret":
      return "炮台";
    case "watchtower":
      return "瞭望塔";
    default:
      return "工事";
  }
}

// formatStructureEffect 返回某个建筑（不论敌我）核心效果的文本说明。
function formatStructureEffect(structureType?: string): string {
  switch (structureType) {
    case "farmland":
      return "周期性产出粮食，帮助驻守单位补给。";
    case "forge":
      return "驻守单位攻击 +4、防御 +3，可强化武器。";
    case "trap":
      return "敌方踩入会触发伤害，可拖慢推进。";
    case "turret":
      return "驻守单位攻击修正 +8，最低攻击距离 3 格，适合守点。";
    case "watchtower":
      return "驻守单位攻击修正 +2，最低攻击距离 2 格，并提升视野。";
    default:
      return "提供阵地化的辅助加成。";
  }
}

// structureEmojiFor 返回建筑类型对应的 emoji 图标，用于详情面板展示。
function structureEmojiFor(structureType?: string): string {
  switch (structureType) {
    case "farmland":
      // 农田：与平原区分，用拖拉机/犁地相关的稻穗 + 边框文字标识在 UI 文本中说明，emoji 仍用稻穗最直观。
      return "🌾";
    case "forge":
      // 铁匠铺：锤砧。
      return "⚒️";
    case "watchtower":
      // 瞭望塔：高塔（东京塔形）。
      return "🗼";
    case "turret":
      // 炮台：城堡塔楼，强调防守工事。
      return "🏯";
    case "trap":
      // 陷阱：警示标志。
      return "⚠️";
    default:
      // 默认：通用建造。
      return "🏗️";
  }
}

// terrainEmojiFor 返回地形对应的 emoji 图标，用于详情面板展示。
function terrainEmojiFor(terrainID?: string): string {
  switch (terrainID) {
    case "plains":
    case "plain":
      // 平原：开阔耕作地，使用稻穗代表。
      return "🌾";
    case "grassland":
      // 草原：青绿草本。
      return "🌿";
    case "forest":
    case "woods":
      // 森林：阔叶树更通用。
      return "🌳";
    case "mountain":
    case "mountains":
      // 山地：山峰剪影。
      return "⛰️";
    case "hill":
    case "hills":
      return "🪨";
    case "river":
    case "water":
    case "lake":
      // 河流：水波。
      return "🌊";
    case "river_valley":
    case "valley":
      // 河谷：含山水的山谷景观。
      return "🏞️";
    case "swamp":
    case "marsh":
      // 沼泽：水生植物（莲）。
      return "🪷";
    case "desert":
      // 沙漠：沙漠景观。
      return "🏜️";
    case "snowfield":
    case "snow":
    case "tundra":
      // 雪原：飘雪。
      return "🌨️";
    case "ruins":
      // 废墟：破败房屋。
      return "🏚️";
    case "road":
    case "path":
      // 道路：行车道。
      return "🛣️";
    case "village":
    case "town":
      // 村庄：聚落民居。
      return "🏘️";
    case "city":
      // 城市：天际线。
      return "🏙️";
    default:
      return "🗺️";
  }
}

// formatStructureTag 生成建筑卡片短标签。
function formatStructureTag(
  structure: SessionSnapshot["structures"][number],
  session: SessionSnapshot | null,
  currentTurn?: number,
): string {
  const side =
    session && structure.faction_id === session.player_faction_id ? "己方" : "敌方";
  return `${side} · ${formatStructureType(structure.type)} · ${structure.q},${structure.r} · 开工于 ${formatTurnsAgo(
    structure.started_turn,
    currentTurn,
  )}`;
}

// formatStructureSummary 生成建筑列表摘要文本。
function formatStructureSummary(
  structure: SessionSnapshot["structures"][number],
  session: SessionSnapshot | null,
  currentTurn?: number,
): string {
  const builder = findUnitName(session, structure.builder_unit_id);
  if (!structure.completed) {
    return `${builder} 正在施工，当前进度 ${structure.build_progress}/${structure.build_required}。`;
  }
  const completion = structure.completed_turn
    ? `（${formatTurnsAgo(structure.completed_turn, currentTurn)}完工）`
    : "";
  if (structure.type === "farmland") {
    return structure.harvest_ready_turn
      ? `农田已建立${completion}，下次可收割回合：T${structure.harvest_ready_turn}。`
      : `农田已建立${completion}。`;
  }
  if (structure.type === "trap") {
    return structure.charges
      ? `陷阱待触发${completion}，剩余 ${structure.charges} 次。`
      : `陷阱已经耗尽${completion}。`;
  }
  if (structure.type === "forge") {
    return `铁匠铺已投入使用${completion}：驻守单位可获得装备整备加成。`;
  }
  return `${formatStructureType(structure.type)} 已就位${completion}。`;
}

type ItemDisplayMeta = {
  slot?: "weapon" | "armor" | "shoes" | "accessory";
  attack?: number;
  defense?: number;
  move?: number;
  weight?: number;
  use: string;
};

const itemDisplayMeta: Record<string, ItemDisplayMeta> = {
  dagger: { slot: "weapon", attack: 8, weight: 1, use: "轻武器，适合低负重近战" },
  short_sword: { slot: "weapon", attack: 15, weight: 1, use: "均衡近战武器" },
  long_sword: { slot: "weapon", attack: 25, weight: 2, use: "主力近战输出" },
  greatsword: { slot: "weapon", attack: 33, weight: 3, use: "重型高伤武器" },
  spear: { slot: "weapon", attack: 19, weight: 2, use: "长柄近战武器" },
  bow: { slot: "weapon", attack: 20, weight: 1, use: "远程武器，适合保持距离" },
  crossbow: { slot: "weapon", attack: 24, weight: 2, use: "远程弩具，适合稳定输出" },
  battle_axe: { slot: "weapon", attack: 27, weight: 3, use: "重型破阵武器" },
  warhammer: { slot: "weapon", attack: 30, weight: 3, use: "重型钝器，适合正面压制" },
  oak_staff: { slot: "weapon", attack: 17, defense: 4, weight: 2, use: "法杖/专注器，兼顾攻防" },
  cloth_armor: { slot: "armor", defense: 5, weight: 1, use: "轻护甲，基础防护" },
  padded_armor: { slot: "armor", defense: 8, weight: 1, use: "轻护甲，低负重防护" },
  leather_armor: { slot: "armor", defense: 12, weight: 2, use: "常规护甲，均衡防护" },
  chain_mail: { slot: "armor", defense: 20, move: -1, weight: 3, use: "中型护甲，牺牲机动换防御" },
  brigandine: { slot: "armor", defense: 24, move: -1, weight: 3, use: "中型札甲，适合前排抗压" },
  plate_armor: { slot: "armor", defense: 35, move: -1, weight: 4, use: "重甲，高防但拖慢移动" },
  mage_robe: { slot: "armor", defense: 10, move: 1, weight: 1, use: "轻装法袍，兼顾防护与机动" },
  cloth_shoes: { slot: "shoes", move: 1, weight: 0, use: "鞋履，提升移动" },
  leather_boots: { slot: "shoes", move: 2, weight: 1, use: "鞋履，提升机动" },
  war_boots: { slot: "shoes", move: 3, weight: 1, use: "战靴，大幅提升机动" },
  riding_spurs: { slot: "shoes", move: 4, weight: 1, use: "高机动鞋履，适合快速转场" },
  buckler: { slot: "accessory", defense: 5, weight: 1, use: "小盾饰品，补充防御" },
  kite_shield: { slot: "accessory", defense: 8, weight: 2, use: "盾牌饰品，提高抗压" },
  tower_shield: { slot: "accessory", defense: 14, move: -1, weight: 3, use: "重盾，高防但降低机动" },
  scout_charm: { slot: "accessory", move: 1, weight: 0, use: "侦察饰品，提升机动" },
  arcane_focus: { slot: "accessory", attack: 7, weight: 0, use: "奥术饰品，补充攻击" },
  healer_emblem: { slot: "accessory", defense: 6, weight: 0, use: "疗愈饰品，补充防护" },
  ration: { use: "食用恢复 35 点饥饿度" },
  herb_bundle: { use: "战地治疗/随机事件药材" },
  healing_potion: { use: "食用恢复 25 HP" },
  antidote: { use: "应对中毒或瘟疫事件" },
  revive_stone: { use: "稀有复苏物资" },
  rope: { use: "工具物资，可交易或事件消耗" },
  pickaxe: { weight: 1, use: "工具，可交易" },
  fishing_net: { weight: 1, use: "渔获工具，可交易" },
  hatchet: { weight: 1, use: "伐木工具，可交易" },
  torch: { use: "火把，可用于控火/驱兽事件" },
  carrier_pigeon: { use: "可远程传信并附带物品" },
  iron_ore: { weight: 1, use: "锻造武器/建造铁匠铺或炮台" },
  wood: { weight: 1, use: "建造陷阱/炮台/瞭望塔/锻造弓类" },
  stone: { weight: 1, use: "建造铁匠铺/强化装备" },
  leather: { weight: 1, use: "锻造护甲/鞋履/弓类" },
  cloth_roll: { weight: 1, use: "锻造鞋履或轻装材料" },
  gemstone: { weight: 1, use: "锻造/强化饰品" },
};

// displayItemLabel 生成物品显示名称（含兜底）。
function displayStackLabel(stack: { item_id: string; custom_name?: string; level?: number }): string {
  const base = stack.custom_name?.trim() || displayItemLabel(stack.item_id);
  return stack.level && stack.level > 0 ? `${base} +${stack.level}` : base;
}

function formatStackWithDetails(stack: { item_id: string; custom_name?: string; level?: number }): string {
  const details = displayStackDetails(stack);
  return details ? `${displayStackLabel(stack)}（${details}）` : displayStackLabel(stack);
}

function displayStackDetails(stack: { item_id: string; level?: number }): string {
  const meta = itemDisplayMeta[stack.item_id];
  if (!meta) {
    return "可交易物资";
  }
  const parts: string[] = [];
  if (meta.slot) {
    parts.push(displayEquipmentSlotLabel(meta.slot));
  }
  const bonus = stackBonusWithLevel(meta, stack.level ?? 0);
  if (bonus.attack) {
    parts.push(`攻击${formatSignedNumber(bonus.attack)}`);
  }
  if (bonus.defense) {
    parts.push(`防御${formatSignedNumber(bonus.defense)}`);
  }
  if (bonus.move) {
    parts.push(`移动${formatSignedNumber(bonus.move)}`);
  }
  if (typeof meta.weight === "number" && meta.weight > 0) {
    parts.push(`重量${meta.weight}`);
  }
  parts.push(`用途：${meta.use}`);
  return parts.join("，");
}

function stackBonusWithLevel(meta: ItemDisplayMeta, level: number): { attack: number; defense: number; move: number } {
  let attack = meta.attack ?? 0;
  let defense = meta.defense ?? 0;
  let move = meta.move ?? 0;
  if (level > 0) {
    switch (meta.slot) {
      case "weapon":
        attack += level * 4;
        defense += level;
        break;
      case "armor":
        defense += level * 3;
        break;
      case "shoes":
        defense += level;
        move += Math.floor(level / 2);
        break;
      case "accessory":
        attack += level;
        defense += level * 2;
        break;
    }
  }
  return { attack, defense, move };
}

function formatSignedNumber(value: number): string {
  return value > 0 ? `+${value}` : `${value}`;
}

function displayItemLabel(itemID: string): string {
  switch (itemID) {
    case "antidote":
      return "解毒药";
    case "arcane_focus":
      return "奥术焦点";
    case "battle_axe":
      return "战斧";
    case "bow":
      return "弓箭";
    case "brigandine":
      return "札甲";
    case "buckler":
      return "圆盾";
    case "carrier_pigeon":
      return "信鸽";
    case "chain_mail":
      return "锁甲";
    case "cloth_armor":
      return "布甲";
    case "cloth_roll":
      return "布匹";
    case "cloth_shoes":
      return "布鞋";
    case "crossbow":
      return "弩";
    case "dagger":
      return "匕首";
    case "fishing_net":
      return "渔网";
    case "gemstone":
      return "宝石";
    case "greatsword":
      return "巨剑";
    case "hatchet":
      return "斧头";
    case "healer_emblem":
      return "疗愈徽章";
    case "healing_potion":
      return "治疗药剂";
    case "herb_bundle":
      return "药草包";
    case "iron_ore":
      return "铁矿";
    case "leather":
      return "皮革";
    case "leather_armor":
      return "皮甲";
    case "leather_boots":
      return "皮靴";
    case "long_sword":
      return "长剑";
    case "mage_robe":
      return "法袍";
    case "oak_staff":
      return "橡木法杖";
    case "padded_armor":
      return "棉甲";
    case "pickaxe":
      return "铁镐";
    case "plate_armor":
      return "板甲";
    case "ration":
      return "口粮";
    case "revive_stone":
      return "复活石";
    case "riding_spurs":
      return "骑乘马刺";
    case "rope":
      return "绳索";
    case "scout_charm":
      return "侦察护符";
    case "short_sword":
      return "短剑";
    case "spear":
      return "长矛";
    case "stone":
      return "石料";
    case "torch":
      return "火把";
    case "tower_shield":
      return "塔盾";
    case "war_boots":
      return "战靴";
    case "warhammer":
      return "战锤";
    case "wood":
      return "木材";
    default:
      return itemID;
  }
}

function displayEquipmentSlotLabel(slot: string): string {
  switch (slot) {
    case "weapon":
      return "武器";
    case "armor":
      return "护甲";
    case "shoes":
      return "鞋履";
    case "accessory":
    case "trinket":
      return "饰品";
    case "tool":
      return "工具";
    default:
      return slot;
  }
}

// formatAttemptTitle 格式化一次 LLM 尝试的标题行。
function formatAttemptTitle(attempt: CompletionAttempt): string {
  const parts = [attempt.provider];
  if (attempt.wire_api) {
    parts.push(attempt.wire_api);
  } else if (attempt.endpoint) {
    parts.push(attempt.endpoint);
  }
  if (attempt.model) {
    parts.push(attempt.model);
  }
  return parts.filter(Boolean).join(" · ");
}

// formatAttemptDetails 格式化一次 LLM 尝试的明细行。
function formatAttemptDetails(attempt: CompletionAttempt): string {
  const parts: string[] = [];
  if (attempt.base_url) {
    parts.push(`base=${attempt.base_url}`);
  }
  if (attempt.status_code) {
    parts.push(`status=${attempt.status_code}`);
  }
  if (attempt.started_at) {
    parts.push(`开始=${formatAttemptStartedAt(attempt.started_at)}`);
  }
  if (typeof attempt.duration_ms === "number" && Number.isFinite(attempt.duration_ms)) {
    parts.push(`耗时=${formatAttemptDuration(attempt.duration_ms)}`);
  }
  parts.push(attempt.succeeded ? "成功" : "失败");
  if (attempt.error) {
    parts.push(formatLLMError(attempt.error));
  }
  return parts.join(" · ");
}

// formatAttemptStartedAt 把后端记录的调用开始时间转成本地可读时间。
function formatAttemptStartedAt(value: string): string {
  const timestamp = Date.parse(value);
  if (!Number.isFinite(timestamp)) {
    return value;
  }
  return new Date(timestamp).toLocaleTimeString("zh-CN", {
    hour12: false,
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

// formatAttemptDuration 把毫秒耗时压缩成适合调试列表展示的文本。
function formatAttemptDuration(durationMS: number): string {
  if (durationMS >= 1000) {
    return `${(durationMS / 1000).toFixed(1)}s`;
  }
  return `${Math.max(0, Math.round(durationMS))}ms`;
}

function formatLLMError(error: string): string {
  const text = error.trim();
  if (text.toLowerCase().includes("context deadline exceeded")) {
    return `${text}（LLM 请求超时：上游模型在本轮超时时间内没有返回，系统会使用兜底输出继续游戏）`;
  }
  return text;
}

// recoverSession 按 session_id 重新拉取会话并恢复前端状态。
function recoverSession(error: unknown, apply: (session: SessionSnapshot) => void): boolean {
  if (error instanceof APIError && error.session) {
    apply(error.session);
    return true;
  }
  return false;
}

// normalizeRoomCodeInput 规范化房间号输入（大写与字符过滤）。
function normalizeRoomCodeInput(value: string): string {
  return value
    .toUpperCase()
    .replace(/[^23456789ABCDEFGHJKLMNPQRSTUVWXYZ]/g, "")
    .slice(0, 6);
}

function normalizeDuelRoomStatus(value: unknown): DuelRoomStatus | null {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return null;
  }
  const record = value as Record<string, unknown>;
  const roomCode = typeof record.room_code === "string" ? normalizeRoomCodeInput(record.room_code) : "";
  return {
    room_code: roomCode,
    player_joined: record.player_joined === true,
    enemy_joined: record.enemy_joined === true,
  };
}

// normalizeBattleMapSize 规范化战场尺寸选择，防止 select 非预期值进入 API。
function normalizeBattleMapSize(value: string): BattleMapSizeID {
  switch (value) {
    case "medium":
    case "large":
      return value;
    default:
      return "small";
  }
}

function isTypingTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) {
    return false;
  }
  const tag = target.tagName.toLowerCase();
  return target.isContentEditable || tag === "input" || tag === "textarea" || tag === "select";
}

async function copyTextToClipboard(value: string): Promise<void> {
  const text = value.trim();
  if (!text) {
    throw new Error("empty");
  }
  if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(text);
    return;
  }
  if (typeof document === "undefined") {
    throw new Error("clipboard unavailable");
  }
  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.setAttribute("readonly", "true");
  textarea.style.position = "absolute";
  textarea.style.left = "-9999px";
  document.body.appendChild(textarea);
  textarea.select();
  const copied = document.execCommand("copy");
  document.body.removeChild(textarea);
  if (!copied) {
    throw new Error("copy failed");
  }
}

type DuelResumePayload = {
  session_id: string;
  role_token: string;
  room_code?: string;
  preferred_role?: "player" | "enemy";
};

type AccountAuthPayload = {
  token: string;
};

// readAccountAuthFromStorage 读取本地账号登录令牌。
function readAccountAuthFromStorage(): AccountAuthPayload | null {
  if (typeof window === "undefined") {
    return null;
  }
  try {
    const raw = window.localStorage.getItem(accountAuthStorageKey);
    if (!raw) {
      return null;
    }
    const parsed = JSON.parse(raw) as Partial<AccountAuthPayload> | null;
    const token = typeof parsed?.token === "string" ? parsed.token.trim() : "";
    return token ? { token } : null;
  } catch {
    return null;
  }
}

// writeAccountAuthToStorage 写入本地账号登录令牌，用于刷新后保持登录态。
function writeAccountAuthToStorage(payload: AccountAuthPayload): void {
  if (typeof window === "undefined") {
    return;
  }
  const token = payload.token.trim();
  if (!token) {
    return;
  }
  try {
    window.localStorage.setItem(accountAuthStorageKey, JSON.stringify({ token }));
  } catch {
    // ignore storage failures
  }
}

// clearAccountAuthFromStorage 清理本地账号登录令牌。
function clearAccountAuthFromStorage(): void {
  if (typeof window === "undefined") {
    return;
  }
  try {
    window.localStorage.removeItem(accountAuthStorageKey);
  } catch {
    // ignore storage failures
  }
}

// readDuelResumeFromStorage 读取本地双人局续连信息。
function readDuelResumeFromStorage(): DuelResumePayload | null {
  if (typeof window === "undefined") {
    return null;
  }
  try {
    const raw = window.localStorage.getItem(duelResumeStorageKey);
    if (!raw) {
      return null;
    }
    const parsed = JSON.parse(raw) as Partial<DuelResumePayload> | null;
    if (!parsed || typeof parsed !== "object") {
      return null;
    }
    const sessionID = typeof parsed.session_id === "string" ? parsed.session_id.trim() : "";
    const roleToken = typeof parsed.role_token === "string" ? parsed.role_token.trim() : "";
    if (!sessionID || !roleToken) {
      return null;
    }
    const roomCode = typeof parsed.room_code === "string" ? normalizeRoomCodeInput(parsed.room_code) : "";
    const preferredRole = parsed.preferred_role === "player" ? "player" : parsed.preferred_role === "enemy" ? "enemy" : undefined;
    return {
      session_id: sessionID,
      role_token: roleToken,
      room_code: roomCode,
      preferred_role: preferredRole,
    };
  } catch {
    return null;
  }
}

function readHUDVisibilityFromStorage(): boolean {
  if (typeof window === "undefined") {
    return true;
  }
  try {
    const raw = window.localStorage.getItem(hudVisibilityStorageKey);
    if (raw === "0") {
      return false;
    }
    if (raw === "1") {
      return true;
    }
  } catch {
    return true;
  }
  return true;
}

function writeHUDVisibilityToStorage(visible: boolean): void {
  if (typeof window === "undefined") {
    return;
  }
  try {
    window.localStorage.setItem(hudVisibilityStorageKey, visible ? "1" : "0");
  } catch {
    // ignore storage failures
  }
}

function readDeveloperModeFromStorage(): boolean {
  if (typeof window === "undefined") {
    return false;
  }
  try {
    return window.localStorage.getItem(developerModeStorageKey) === "1";
  } catch {
    return false;
  }
}

function writeDeveloperModeToStorage(enabled: boolean): void {
  if (typeof window === "undefined") {
    return;
  }
  try {
    window.localStorage.setItem(developerModeStorageKey, enabled ? "1" : "0");
  } catch {
    // ignore storage failures
  }
}

function readDeploymentIntroSkipFromStorage(): boolean {
  if (typeof window === "undefined") {
    return false;
  }
  try {
    const raw = window.localStorage.getItem(deploymentIntroSkipStorageKey);
    if (raw === "1") {
      return true;
    }
  } catch {
    return false;
  }
  return false;
}

function writeDeploymentIntroSkipToStorage(skip: boolean): void {
  if (typeof window === "undefined") {
    return;
  }
  try {
    window.localStorage.setItem(deploymentIntroSkipStorageKey, skip ? "1" : "0");
  } catch {
    // ignore storage failures
  }
}

// writeDuelResumeToStorage 写入本地双人局续连信息。
function writeDuelResumeToStorage(payload: DuelResumePayload): void {
  if (typeof window === "undefined") {
    return;
  }
  const sessionID = payload.session_id.trim();
  const roleToken = payload.role_token.trim();
  if (!sessionID || !roleToken) {
    return;
  }
  const roomCode = normalizeRoomCodeInput(payload.room_code ?? "");
  const preferredRole =
    payload.preferred_role === "player" || payload.preferred_role === "enemy"
      ? payload.preferred_role
      : undefined;
  const normalized: DuelResumePayload = {
    session_id: sessionID,
    role_token: roleToken,
    room_code: roomCode || undefined,
    preferred_role: preferredRole,
  };
  try {
    window.localStorage.setItem(duelResumeStorageKey, JSON.stringify(normalized));
  } catch {
    // ignore quota/storage failures
  }
}

// clearDuelResumeFromStorage 清理本地双人局续连信息。
function clearDuelResumeFromStorage(): void {
  if (typeof window === "undefined") {
    return;
  }
  try {
    window.localStorage.removeItem(duelResumeStorageKey);
  } catch {
    // ignore storage failures
  }
}

// getErrorMessage 提取并标准化接口错误文案。
function getErrorMessage(error: unknown, fallback: string): string {
  return error instanceof Error ? error.message : fallback;
}
