/* 文件说明：角色命运开盒的独立入口（与旧战棋客户端分离，Root.tsx 默认路由到此）。
   鉴权前置由外层 AuthGate（Root.tsx 用 <AuthGate><FateApp/></AuthGate> 包住）独占：FateApp 必在已登录态下挂载，
   故本文件**不再自管登录/注册**（曾经的 auth 相位会与 AuthGate 的登录态脱钩——FateApp 清 token 只 setPhase 自身，
   AuthGate 的空依赖 useEffect 永不再核验，仍停在 authed，导致登出后双登录 UI 并存 + 残留前一用户名）。
   流程（账号绑定主世界版）：进入即 gate → 拉取「我的主世界角色」→
     已有 → 直接进四槽主界面（resume 该账号在主世界的同一角色，多设备一致）；
     未有 → 捏人三步 + 离线宪章（onboarding，宪法 §5.1/GDD §4）→ 即时人格快照（O2 最高 ROI）→ 四槽主界面。
   权威态：账号 Bearer 令牌（api.ts 经 localStorage 持久化、自动随请求发送）。localStorage 另缓存
   当前角色 (sessionId/unitId/name) 仅为减少请求的「乐观缓存」，与令牌取到的权威角色不一致时以后者为准。
   「换个账号登入」：清本地角色缓存 + 整页 reload —— reload 会重跑 AuthGate 的挂载核验（token 已被 logout 清掉→
   AuthGate 落到 anon 干净登录表单），从根上消除双 UI 与用户名错配。*/

import { useCallback, useEffect, useMemo, useState } from "react";
import {
  createMyCharacter,
  deleteCharter,
  getAccountToken,
  getCharter,
  getMyCharacter,
  getUnitStatus,
  logoutAccount,
  putCharter,
  recordPlayerIntervention,
  trackFunnel,
} from "../session/api";
import { FateView } from "./FateView";
import { FateBoard } from "./FateBoard";
import { WorldMap } from "./WorldMap";
import { QuestPanel } from "./QuestPanel";
import { Minimap } from "./Minimap";
import { CharacterSheet } from "./CharacterSheet";
import { AccountSettings } from "./AccountSettings";
import { OnboardingTour } from "../components/OnboardingTour";
import { CharterEditor } from "../components/CharterEditor";
import {
  fromPersonalityBlock,
  optionFit,
  pickChoices,
  summarize,
  type MicroOption,
  type PersonaTraits,
  type SnapshotResult,
} from "./personaSnapshot";
import type { SessionSnapshot } from "../session/types";
import "./fate.css";

// crossFileNeeds（本波 W-B 三人分工，本文件只编辑 FateApp.tsx）：
//   - api.ts（B1 拥有）已提供并被本文件调用：getMyCharacter() → MyCharacter{ has_character, session_id?,
//     unit_id?, name? }；createMyCharacter(MyCharacterInput{name?,origin?,desire?,wound?,redline?}) → MyCharacter。
//     登录/注册已上移到外层 AuthGate，本文件不再调 registerAccount/loginAccount；仅在「换个账号登入」时
//     调 logoutAccount(getAccountToken()) 清后端会话+本地 Bearer，再 window.location.reload() 交还 AuthGate 重核验。
//   - FateView（B3 拥有）当前 props 为 { sessionId, unitId }，已够用。若日后要在四槽内「接管关键战」，
//     需 FateView 新增 onEnterBattle?(sessionId) 回调（点击后由本壳 window.location.hash = `#battle/${sessionId}`
//     切到 App 战棋接管视图）——属 B3 改 FateView + B1 改 Root.tsx 的 #battle 路由，本文件届时只透传回调。
//   - 即时人格快照仍为纯体验层、零持久化；若要把玩家微选择落库/反哺 persona，需 api.ts 新增回执函数。

// Phase 去掉了曾经的 "auth"：鉴权归 AuthGate 独占，FateApp 必在已登录态挂载，进入即 gate 拉角色。
type Phase = "gate" | "onboarding" | "preview" | "snapshot" | "play";

// STORE_KEY 缓存「当前角色」乐观态；账号令牌才是权威（多设备登录拿同一角色）。
const STORE_KEY = "qunxiang.fate.character.v1";

type Saved = { sessionId: string; unitId: string; name: string };

function loadSaved(): Saved | null {
  try {
    const raw = window.localStorage.getItem(STORE_KEY);
    if (!raw) return null;
    const v = JSON.parse(raw) as Saved;
    if (v.sessionId && v.unitId) return v;
  } catch {
    /* ignore */
  }
  return null;
}

function persistSaved(next: Saved | null): void {
  try {
    if (next) {
      window.localStorage.setItem(STORE_KEY, JSON.stringify(next));
    } else {
      window.localStorage.removeItem(STORE_KEY);
    }
  } catch {
    /* localStorage 不可用（隐私模式）时忽略——内存态仍在 */
  }
}

const ORIGINS = ["边境猎户", "铁匠之女", "落魄书生", "行脚商人", "庙祝巫医", "流亡贵族", "采药孤女"];

// FACTIONS 三阵营选项（对齐后端 internal/faction：ID freedom/order/chaos + 中文名 + 道德信条）。
// 玩家捏人时三选一，连同 name/origin/desire/wound/redline 一起 createMyCharacter({...,faction})。
// id 用后端稳定常量；后端 Normalize 也容中文别名，但前端固定传英文 id 最稳。
const FACTIONS: { id: string; nameZH: string; creed: string }[] = [
  { id: "freedom", nameZH: "自由", creed: "不受束缚，各凭本心。" },
  { id: "order", nameZH: "秩序", creed: "守序尽责，敬畏规矩。" },
  { id: "chaos", nameZH: "混乱", creed: "打破桎梏，快意恩仇。" },
];

// 立约入口按钮的内联样式（贴合墨色宣纸调）：抽屉顶部一条低调暖色描边按钮。
const charterToggleBtnStyle: React.CSSProperties = {
  display: "inline-flex",
  alignItems: "center",
  gap: 6,
  marginBottom: 10,
  padding: "8px 14px",
  border: "1px solid rgba(140, 100, 50, 0.4)",
  borderRadius: 8,
  background: "rgba(255, 252, 246, 0.9)",
  color: "#7a5226",
  fontFamily: "inherit",
  fontSize: 13,
  cursor: "pointer",
};

// 舆图入口浮层按钮：顶部居中，与 .fate-restart(左上)/.fate-drawer-toggle(右上) 同款墨色宣纸卡风格、同层级(z 21)，
// 不碰 fate.css 故内联。translateX(-50%) 让它真正水平居中而不被左右两个角浮层挤偏。
const worldMapToggleBtnStyle: React.CSSProperties = {
  position: "fixed",
  top: 14,
  left: "50%",
  transform: "translateX(-50%)",
  zIndex: 21,
  display: "inline-flex",
  alignItems: "center",
  gap: 6,
  padding: "8px 14px",
  borderRadius: 10,
  border: "1px solid rgba(140, 100, 50, 0.5)",
  background: "rgba(250, 244, 232, 0.96)",
  color: "#6b4a22",
  fontFamily: "'Noto Serif SC', 'Songti SC', serif",
  fontSize: 14,
  cursor: "pointer",
  boxShadow: "0 4px 14px rgba(60, 44, 27, 0.2)",
};

// 任务入口浮层按钮：与「🗺 舆图」并排居顶。舆图按钮 translateX(-50%) 真居中，此按钮居中偏右排开，
// 两者共用同款墨色宣纸卡风格 + 同层级(z 21)。calc(50% + 100px) 让它落在舆图按钮右侧不重叠。
const questToggleBtnStyle: React.CSSProperties = {
  position: "fixed",
  top: 14,
  left: "calc(50% + 100px)",
  zIndex: 21,
  display: "inline-flex",
  alignItems: "center",
  gap: 6,
  padding: "8px 14px",
  borderRadius: 10,
  border: "1px solid rgba(140, 100, 50, 0.5)",
  background: "rgba(250, 244, 232, 0.96)",
  color: "#6b4a22",
  fontFamily: "'Noto Serif SC', 'Songti SC', serif",
  fontSize: 14,
  cursor: "pointer",
  boxShadow: "0 4px 14px rgba(60, 44, 27, 0.2)",
};

// 小地图定位容器：Minimap 自身 position:absolute top:12 left:12（贴最近定位祖先左上角）。
// 直接挂在 .fate-play-fullscreen(position:fixed) 下会与「换个账号」按钮(top:14 left:14)叠在一起，
// 故套一个偏下的相对容器(top:52)，让小地图落到 restart 按钮下方。容器尺寸贴合小地图(~172×150)。
const minimapWrapStyle: React.CSSProperties = {
  position: "absolute",
  top: 52,
  left: 0,
  width: 184,
  height: 156,
  zIndex: 14,
  pointerEvents: "none",
};

export function FateApp() {
  // 初始相位恒为 gate：AuthGate 已保证挂载即已登录，进入直接拉「我的主世界角色」。
  const [phase, setPhase] = useState<Phase>("gate");
  // saved 仅作乐观缓存兜底；play 渲染前会被 gate 用权威角色覆盖。
  const [saved, setSaved] = useState<Saved | null>(() => loadSaved());
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // charterOpen：降生后玩家点「立约/改约」按钮，浮层式打开 CharterEditor（读/改/撤她的离线宪章）。
  const [charterOpen, setCharterOpen] = useState(false);
  // sheetOpen：MMORPG 式「角色档案」只读浮层（状态/技能/背包/关系/编年史），观察态、不操作角色。
  const [sheetOpen, setSheetOpen] = useState(false);
  // settingsOpen：账号设置浮层（改密码 + 预留绑定飞书）。改密成功复用 signOut 登出。
  const [settingsOpen, setSettingsOpen] = useState(false);
  // drawerOpen：命运 UI 浮层抽屉是否展开（默认开，让玩家同时看到全屏地图 + 命运卡；可收起独看地图）。
  const [drawerOpen, setDrawerOpen] = useState(true);
  // worldMapOpen：世界地图（舆图）浮层是否展开——点顶部「舆图」按钮打开，选区前往后自动关闭（仿 charterOpen 范式）。
  const [worldMapOpen, setWorldMapOpen] = useState(false);
  // questOpen：任务面板（差遣）浮层是否展开——点顶部「📜 任务」按钮打开（仿 worldMapOpen 范式）。
  const [questOpen, setQuestOpen] = useState(false);
  // boardRefresh：传给 FateBoard 的 refreshSignal——前往新区成功后 bump，使 board 重拉快照切到新区地图(state.map 已投影)+新区NPC。
  const [boardRefresh, setBoardRefresh] = useState(0);
  // boardSnap：FateBoard 经 onSnapshot 上抛的最新整快照——存着喂给小地图 Minimap（与 FateBoard 同源，不另起 getSession 轮询）。
  const [boardSnap, setBoardSnap] = useState<SessionSnapshot | null>(null);
  // guidanceDraft：点地图格子/人生成的「指向型指引草稿」——FateBoard 写、FateView 读并预填进指引框（消费后清空）。
  const [guidanceDraft, setGuidanceDraft] = useState("");

  // 捏人四要素 + 出身。
  const [name, setName] = useState("");
  const [origin, setOrigin] = useState(ORIGINS[0]);
  // faction：玩家选择的阵营（freedom/order/chaos），默认自由。决定她降生的阵营 + 道德基准 + 出生据点。
  const [faction, setFaction] = useState(FACTIONS[0].id);
  const [desire, setDesire] = useState("");
  const [wound, setWound] = useState("");
  const [redline, setRedline] = useState("");
  const [preview, setPreview] = useState<{ name: string; bio: string; traits: PersonaTraits } | null>(
    null,
  );

  // gate：账号令牌就绪后，向后端要「我的主世界角色」——有则 resume 进 play，无则进捏人。
  // 令牌是权威：即便 localStorage 缓存了某角色，也以后端返回为准（多设备一致）。
  const loadCharacter = useCallback(async () => {
    setBusy(true);
    setError("");
    try {
      const mine = await getMyCharacter();
      if (mine?.has_character && mine.session_id && mine.unit_id) {
        const next: Saved = {
          sessionId: mine.session_id,
          unitId: mine.unit_id,
          name: mine.name || saved?.name || "她",
        };
        persistSaved(next);
        setSaved(next);
        setPhase("play");
      } else {
        // 该账号尚未在主世界降生过角色——进捏人。清掉可能残留的他账号缓存。
        persistSaved(null);
        setSaved(null);
        setPhase("onboarding");
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      // 拉取失败（如令牌过期/401）→ 停在 gate 相位展示错误与「重新登入」按钮；点击走 signOut（清 token + reload），
      // reload 后由外层 AuthGate 重新核验落到 anon 登录表单。不再切到已删除的自管 "auth" 相位。
      setPhase("gate");
    } finally {
      setBusy(false);
    }
  }, [saved?.name]);

  // 进入 gate 相位即拉取我的角色（仅在该相位触发一次）。
  useEffect(() => {
    if (phase === "gate") void loadCharacter();
  }, [phase, loadCharacter]);

  // create：账号绑定的幂等降生（非匿名 bootstrap）。保留捏人四要素 + 离线宪章 + 即时人格快照体验，
  // 只把「创建」那步换成账号绑定端点 createMyCharacter。
  const create = useCallback(async () => {
    const trimmed = name.trim() || "无名";
    setBusy(true);
    setError("");
    try {
      // 账号绑定幂等降生：后端据令牌把角色挂到该账号，重复调用返回同一角色（多设备/重试安全）。
      // faction 随捏人入参一起提交，决定她降生的阵营 + 道德基准 + 出生据点（后端 MainWorldCharacterInput.Faction）。
      // crossFileNeeds：api.ts 的 MyCharacterInput 尚无 faction 字段——为不阻塞本文件类型检查，这里在传入处局部
      // 扩展类型（&{faction:string}）。待 B1 给 MyCharacterInput 补 faction?:string 后，可移除此 cast。
      const mine = await createMyCharacter({
        name: trimmed,
        origin,
        desire: desire.trim(),
        wound: wound.trim(),
        redline: redline.trim(),
        faction,
      } as Parameters<typeof createMyCharacter>[0] & { faction: string });
      const sessionId = mine.session_id;
      const unitId = mine.unit_id;
      if (!sessionId || !unitId) throw new Error("未能创建角色");

      // 离线宪章 + 欲望/伤痕作为「家训/指引」落成可被回响引用的玩家动作。
      // （createMyCharacter 已带四要素入库；这里再以指引形式落一条人话版家训，供日后回响引用。）
      const charter = [
        desire.trim() && `她想要的：${desire.trim()}`,
        wound.trim() && `她的伤痕：${wound.trim()}`,
        redline.trim() && `你立下的家训：她绝不能${redline.trim()}`,
        `出身：${origin}`,
      ]
        .filter(Boolean)
        .join("；");
      if (charter) {
        // best-effort：指引失败不挡降生（角色已建）。
        try {
          await recordPlayerIntervention(sessionId, unitId, charter);
        } catch {
          /* ignore */
        }
      }

      // 即时人格快照需要 persona 八轴：createMyCharacter 只回 id/name，故回读一次单位取 personality。
      // 回读失败则安全夹到中性 0.5（fromPersonalityBlock 缺轴默认 0.5），快照仍可进行。
      let traits: PersonaTraits = fromPersonalityBlock(null);
      let bio = "";
      try {
        const unit = await getUnitStatus(unitId);
        if (unit) {
          traits = fromPersonalityBlock(unit.personality);
          const identity = (unit.identity ?? {}) as Record<string, unknown>;
          bio = String(identity.biography ?? "");
        }
      } catch {
        /* 回读失败：用中性 persona + 合成简介兜底 */
      }
      setPreview({
        name: trimmed,
        bio:
          bio ||
          `${origin}出身的${trimmed}。${desire.trim() ? "她心里一直惦记着：" + desire.trim() + "。" : ""}`,
        traits,
      });

      const next: Saved = { sessionId, unitId, name: trimmed };
      persistSaved(next);
      setSaved(next);
      // charter_completed：捏人成功（账号已绑角色、离线宪章已落）即 onboarding→preview 转换达成，进 leads 漏斗。
      // 后端 createMyCharacter 另会 Emit 权威版到 product_events；这条仅供前端漏斗统计，best-effort、不重复。
      void trackFunnel("charter_completed", { source: origin });
      setPhase("preview");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }, [name, origin, faction, desire, wound, redline]);

  // 换个账号登入 / 登出：FateApp 不自管鉴权态，故登出后**不能**只切自身相位（那样会与外层 AuthGate 的
  // authed 脱钩 → 双登录 UI 并存 + 残留前一用户名）。正确做法：清账号令牌（logoutAccount 内部无论成败都清本地
  // Bearer）+ 清本地角色缓存，再整页 reload —— reload 重跑 AuthGate 挂载核验，token 已清→落到 anon 干净登录表单。
  const signOut = useCallback(async () => {
    try {
      // logoutAccount 当前签名收 token；传当前 Bearer，内部无论成败都会清本地令牌（localStorage + 模块级）。
      await logoutAccount(getAccountToken());
    } catch {
      /* 后端登出失败也无妨，本地令牌仍会被清 */
    }
    persistSaved(null);
    // 整页刷新让外层 AuthGate 重新核验登录态（无 token → anon 登录表单），从根上消除双 UI 与用户名错配。
    window.location.reload();
  }, []);

  // ── play：四槽主界面 + 命运地图舞台（账号的主世界角色） ──
  // 布局：PixiBoard 地图作主舞台（宽屏左 / 窄屏上），FateView 文字命运卡作旁白（宽屏右栏 / 窄屏下方）。
  // 地图是观战模式（她自治、玩家不下令）；FateView 的指引输入 + 「让世界往前走」循环照常可用。
  // 本文件不可改 fate.css，故两栏布局用内联样式（贴合 .fate-shell 墨色宣纸调）；地图自身轮询随她移动刷新。
  if (phase === "play" && saved) {
    return (
      <div className="fate-play-fullscreen">
        {/* 全屏世界地图（主舞台）：她生活的天地——拖拽平移、滚轮缩放、观战她与身边二十余人。
            refreshSignal=boardRefresh：前往新区成功后父层 bump 它，board 重拉快照切到新区地图(state.map 已投影)+新区NPC。
            onSnapshot：把 board 已拉的整快照上抛，存进 boardSnap 喂给左上小地图 Minimap（同源，不另起 getSession 轮询）。 */}
        <FateBoard
          sessionId={saved.sessionId}
          unitId={saved.unitId}
          refreshSignal={boardRefresh}
          onSnapshot={setBoardSnap}
          onGuidanceSuggested={(t) => {
            setGuidanceDraft(t);
            setDrawerOpen(true);
          }}
        />

        {/* 小地图：当前区缩略 + 她的红点（左上角，偏下避开「换个账号」按钮）。数据来自 FateBoard 上抛的同一份快照。
            Minimap 自身 position:absolute top:12 left:12，故套一个偏下定位的相对容器，使它落在 restart 按钮下方而非盖住它。 */}
        <div style={minimapWrapStyle}>
          <Minimap snap={boardSnap} unitId={saved.unitId} />
        </div>

        {/* 左上浮层：换个账号。 */}
        <button className="fate-restart" onClick={() => void signOut()}>
          换个账号登入
        </button>

        {/* 顶部中央浮层：舆图 · 世界地图入口。点开 WorldMap 浮层，看全部区域、择一前往（仿 charterOpen/drawerOpen 范式）。 */}
        <button style={worldMapToggleBtnStyle} onClick={() => setWorldMapOpen(true)} aria-haspopup="dialog">
          🗺 舆图 · 天下
        </button>

        {/* 顶部偏右浮层：任务（差遣）入口。点开 QuestPanel 浮层，接取/查看进度/交付（仿 worldMapOpen 范式）。 */}
        <button style={questToggleBtnStyle} onClick={() => setQuestOpen(true)} aria-haspopup="dialog">
          📜 任务
        </button>

        {/* 右上浮层：命运抽屉开关（收起则独看全屏地图）。 */}
        <button
          className="fate-drawer-toggle"
          onClick={() => setDrawerOpen((v) => !v)}
          aria-expanded={drawerOpen}
        >
          {drawerOpen ? "收起命运 ›" : "‹ 展开命运"}
        </button>

        {/* 右侧浮层抽屉：命运卡旁白 + 指引 + 立约，叠在地图之上、可折叠。 */}
        <aside className={`fate-drawer ${drawerOpen ? "fate-drawer-open" : ""}`} aria-hidden={!drawerOpen}>
          {/* 抽屉顶部入口区：立约 / 角色档案 / 账号设置（仿 charterOpen 浮层范式，各自 setState 打开）。 */}
          <div className="fate-drawer-entries">
            {/* 立约入口：降生后玩家可随时查看/改/撤她的结构化离线宪章（红线/长期目标/社交授权）。 */}
            <button style={charterToggleBtnStyle} onClick={() => setCharterOpen(true)}>
              ✦ 立约 · 看看你与她定下的约
            </button>
            {/* 角色档案：MMORPG 式只读面板，把后端早已有的角色数据露给玩家（观察态）。 */}
            <button style={charterToggleBtnStyle} onClick={() => setSheetOpen(true)}>
              📜 角色档案
            </button>
            {/* 账号设置：改密码 + 预留绑定飞书。 */}
            <button style={charterToggleBtnStyle} onClick={() => setSettingsOpen(true)}>
              ⚙ 账号设置
            </button>
          </div>
          <FateView
            sessionId={saved.sessionId}
            unitId={saved.unitId}
            draftGuidance={guidanceDraft}
            onDraftConsumed={() => setGuidanceDraft("")}
          />
        </aside>

        {/* 离线宪章编辑浮层：read=getCharter / save=putCharter / delete=deleteCharter（api.ts 现有）。
            单角色客户端只一名单位，故 units 传单元素；onClose 收起浮层。
            CharterEditor 的 charter 类型与 api 的 *DTO 结构同构，经薄适配闭包透传以对齐 props 名义类型。 */}
        {charterOpen && (
          <CharterEditor
            sessionId={saved.sessionId}
            units={[{ id: saved.unitId, name: saved.name }]}
            initialUnitID={saved.unitId}
            fetchCharter={(sessionID, unitID) => getCharter(sessionID, unitID)}
            saveCharter={(sessionID, unitID, charter) => putCharter(sessionID, unitID, charter)}
            deleteCharter={(sessionID, unitID) => deleteCharter(sessionID, unitID)}
            onClose={() => setCharterOpen(false)}
          />
        )}

        {/* 角色档案浮层（只读观察态）：状态/技能/背包/关系/编年史 5 tab，挂载时拉 getUnitStatus。 */}
        {sheetOpen && (
          <CharacterSheet
            sessionId={saved.sessionId}
            unitId={saved.unitId}
            fallbackName={saved.name}
            onClose={() => setSheetOpen(false)}
          />
        )}

        {/* 账号设置浮层：改密码 + 预留绑定飞书。改密成功复用 signOut 登出（清 Bearer + reload）。 */}
        {settingsOpen && (
          <AccountSettings
            onSignOut={() => void signOut()}
            onClose={() => setSettingsOpen(false)}
          />
        )}

        {/* 世界地图浮层（舆图）：看全部区域、择一前往。onTraveled（travel 成功）→ bump boardRefresh 让 FateBoard
            重拉快照切到新区；WorldMap 内部已在成功后自调 onClose 收起浮层，这里 onClose 再兜底置 worldMapOpen=false。 */}
        {worldMapOpen && (
          <WorldMap
            sessionId={saved.sessionId}
            unitId={saved.unitId}
            // 主角等级（等级护栏难度色）：取 boardSnap 中主角的 stats.growth.level；转场清空/缺字段时兜底 Lv1。
            playerLevel={
              boardSnap?.player_units?.find((u) => u.id === saved.unitId)?.stats?.growth?.level ?? 1
            }
            onClose={() => setWorldMapOpen(false)}
            onTraveled={() => {
              // 先清快照：避免「旧区地形图 + 新区主角坐标」的错位帧——Minimap 在 snap=null 时不渲染，
              // 等 FateBoard 重拉到新区快照再显（boardRefresh bump 触发重拉）。
              setBoardSnap(null);
              setBoardRefresh((v) => v + 1);
            }}
          />
        )}

        {/* 任务面板浮层（差遣）：可接/进行中/可交付三段。接取/交付成功 → bump boardRefresh 让 FateBoard 重拉快照——
            任务进度也喂自治决策上下文、交付解锁传送会改可达区，故重拉以同步。主角不挪位，无需清 boardSnap。
            zoneId 缺省由后端按当前区生成可接任务。 */}
        {questOpen && (
          <QuestPanel
            sessionId={saved.sessionId}
            unitId={saved.unitId}
            onClose={() => setQuestOpen(false)}
            onChanged={() => setBoardRefresh((v) => v + 1)}
          />
        )}

        {/* 新手第一分钟引导：首次进入 play 相位才弹（OnboardingTour 内部用 localStorage 'qx_onboarded' 判重）。
            聚光锚点 [data-tour='fate']/[data-tour='intervene'] 由 FateView 状态卡/指引区的 data-tour 属性提供。
            onComplete 回调埋点 onboarding_tour 漏斗（finished/skipped 作 source）。 */}
        <OnboardingTour
          onComplete={(reason) => void trackFunnel("onboarding_tour", { source: reason })}
        />
      </div>
    );
  }

  // ── preview：降生画面 ──
  if (phase === "preview" && saved && preview) {
    return (
      <div className="fate-shell fate-onboarding">
        <div className="fate-preview">
          <div className="fate-preview-title">她来到了世上</div>
          <div className="fate-preview-name">{preview.name}</div>
          <p className="fate-preview-bio">{preview.bio}</p>
          <p className="fate-preview-hint">她身边，已有二十个有名有姓、有恩有怨的人。从此，她的命运不再由你操控，只由你牵挂。</p>
          <button onClick={() => setPhase("snapshot")}>看看她是个什么样的人 →</button>
        </div>
      </div>
    );
  }

  // ── snapshot：即时人格快照 ──
  if (phase === "snapshot" && preview) {
    return (
      <PersonaSnapshot
        name={preview.name}
        traits={preview.traits}
        seed={saved?.unitId ?? preview.name}
        onDone={() => setPhase("play")}
      />
    );
  }

  // ── gate：拉取我的主世界角色（账号令牌权威） ──
  if (phase === "gate") {
    return (
      <div className="fate-shell fate-onboarding">
        <div className="fate-preview">
          <div className="fate-preview-title">正在唤回你的人</div>
          <p className="fate-preview-hint">
            {error ? error : "翻遍世间名册，找回你牵挂的那一个…"}
          </p>
          {error && (
            <button onClick={() => void signOut()}>重新登入</button>
          )}
        </div>
      </div>
    );
  }

  // 鉴权（登录/注册）由外层 AuthGate 独占，FateApp 不再渲染自管的 auth 相位（已删除）。

  // ── onboarding：捏人（账号已登入、尚未降生角色） ──
  return (
    <div className="fate-shell fate-onboarding">
      <div className="fate-create">
        <h1>一念 · 命运开盒</h1>
        <p className="fate-create-lead">捏一个人，把她丢进世界。她会自己活——你只能指引、疾呼，却不能替她做主。</p>

        <label>
          名字
          <input value={name} placeholder="给她起个名字" onChange={(e) => setName(e.target.value)} />
        </label>
        <label>
          出身
          <select value={origin} onChange={(e) => setOrigin(e.target.value)}>
            {ORIGINS.map((o) => (
              <option key={o} value={o}>
                {o}
              </option>
            ))}
          </select>
        </label>

        {/* 阵营：她降生在哪片天地、心向何方。三选一，各配一句道德信条——这决定她为人处世的底色。 */}
        <div className="fate-faction-pick">
          <div className="fate-faction-pick-label">她心向何方</div>
          <div className="fate-faction-options">
            {FACTIONS.map((f) => (
              <button
                key={f.id}
                type="button"
                className={`fate-faction-option${faction === f.id ? " selected" : ""}`}
                onClick={() => setFaction(f.id)}
                aria-pressed={faction === f.id}
              >
                <span className="fate-faction-name">{f.nameZH}</span>
                <span className="fate-faction-creed">{f.creed}</span>
              </button>
            ))}
          </div>
        </div>

        <label>
          欲望（她真正想要的）
          <input value={desire} placeholder="如：替惨死的父母讨回公道" onChange={(e) => setDesire(e.target.value)} />
        </label>
        <label>
          伤痕（她过不去的那道坎）
          <input value={wound} placeholder="如：那场没能救下的火" onChange={(e) => setWound(e.target.value)} />
        </label>
        <label>
          家训 · 红线（她绝不能…）
          <input value={redline} placeholder="如：伤害无辜的孩子" onChange={(e) => setRedline(e.target.value)} />
        </label>

        {error && <div className="fate-error">{error}</div>}
        <button className="fate-create-btn" disabled={busy} onClick={() => void create()}>
          {busy ? "正在把她带到世上…" : "让她降生"}
        </button>
        <button className="fate-restart" disabled={busy} onClick={() => void signOut()}>
          换个账号登入
        </button>
      </div>
    </div>
  );
}

// 每道微抉择的倒计时（秒）——「15 秒人生快进」：默认 5 秒一题，三题≈15 秒。
const SNAPSHOT_SECONDS = 5;

// SNAPSHOT_STYLE 是即时人格快照的样式。文件所有权所限（本波只可改 FateApp.tsx + 既有 personaSnapshot.ts，
// 不得动 fate.css），故内联注入到 <head>，与既有 .fate-* 同款墨色宣纸调，类名加 -snap 前缀避免撞车。
const SNAPSHOT_STYLE = `
.fate-snap {
  text-align: center;
}
.fate-snap-progress {
  display: flex;
  justify-content: center;
  gap: 8px;
  margin-bottom: 14px;
}
.fate-snap-dot {
  width: 9px;
  height: 9px;
  border-radius: 50%;
  background: rgba(120, 90, 50, 0.22);
  transition: background 0.3s, transform 0.3s;
}
.fate-snap-dot.done { background: #9a6a3a; }
.fate-snap-dot.active { background: #7a5226; transform: scale(1.35); }
.fate-snap-head {
  font-size: 12px;
  letter-spacing: 0.2em;
  color: #97825f;
  margin-bottom: 14px;
}
.fate-snap-scene {
  font-size: 17px;
  line-height: 1.85;
  color: #4a3417;
  margin: 4px 0 16px;
  min-height: 3.4em;
}
.fate-snap-timer {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 38px;
  height: 38px;
  border-radius: 50%;
  margin-bottom: 16px;
  font-size: 18px;
  color: #6b4a22;
  background: rgba(160, 110, 50, 0.12);
  border: 1px solid rgba(160, 110, 50, 0.3);
}
.fate-snap-timer.urgent {
  color: #a83a28;
  background: rgba(180, 84, 58, 0.16);
  border-color: rgba(180, 84, 58, 0.5);
  animation: fate-snap-pulse 1s ease-in-out infinite;
}
@keyframes fate-snap-pulse { 0%,100% { opacity: 1; } 50% { opacity: 0.55; } }
.fate-snap-options { display: flex; flex-direction: column; gap: 12px; }
.fate-snap-options button {
  padding: 14px 16px;
  border: 1px solid rgba(140, 95, 45, 0.45);
  border-radius: 10px;
  background: rgba(255, 252, 245, 0.92);
  color: #5a3f1c;
  font-family: inherit;
  font-size: 16px;
  cursor: pointer;
  transition: background 0.2s, transform 0.1s;
}
.fate-snap-options button:hover { background: #f0dcb8; }
.fate-snap-options button:active { transform: scale(0.98); }
.fate-snap-hint { margin-top: 16px; font-size: 12px; color: #97825f; }
.fate-snap-reflection {
  font-size: 19px;
  line-height: 1.9;
  color: #6b4a22;
  padding: 28px 8px;
  min-height: 4.4em;
  display: flex;
  align-items: center;
  justify-content: center;
  animation: fate-snap-in 0.4s ease;
}
@keyframes fate-snap-in { from { opacity: 0; transform: translateY(8px); } to { opacity: 1; transform: translateY(0); } }
.fate-snap-signature {
  font-size: 22px;
  letter-spacing: 0.18em;
  color: #7a5226;
  margin: 10px 0 16px;
}
.fate-snap-verdict-text { font-size: 17px; line-height: 1.95; color: #5a4628; margin: 0; }
`;

// useSnapshotStyle 一次性把 SNAPSHOT_STYLE 注入 <head>（按 id 去重，多次挂载不重复插）。
function useSnapshotStyle(): void {
  useEffect(() => {
    const id = "fate-snapshot-style";
    if (document.getElementById(id)) return;
    const el = document.createElement("style");
    el.id = id;
    el.textContent = SNAPSHOT_STYLE;
    document.head.appendChild(el);
  }, []);
}

// PersonaSnapshot 是「即时人格快照」组件（GDD O2 压缩快进微选择）：
// 在降生后用 2-3 道情境微抉择让玩家快速点选、即时感知人格，收尾给一句「这就是她」的速写。
// 纯前端、确定性、零持久化；据 preview 已有的 persona 八轴拣题与折射。
function PersonaSnapshot(props: {
  name: string;
  traits: PersonaTraits;
  seed: string;
  onDone: () => void;
}): JSX.Element {
  const { name, traits, seed, onDone } = props;
  useSnapshotStyle();
  // 据该角色 persona 确定性地拣 3 道最具区分度的题（seed 保证「同一个她」每次一致）。
  const choices = useMemo(() => pickChoices(traits, seed, 3), [traits, seed]);

  const [step, setStep] = useState(0);
  const [picks, setPicks] = useState<MicroOption[]>([]);
  // reflection：刚选完这一题的即时折射文案；非空时短暂遮显，再进下一题。
  const [reflection, setReflection] = useState<string>("");
  const [remaining, setRemaining] = useState(SNAPSHOT_SECONDS);
  const [result, setResult] = useState<SnapshotResult | null>(null);

  const total = choices.length;
  const current = step < total ? choices[step] : null;

  // 推进到下一题或收尾。
  const advance = useCallback(
    (opt: MicroOption) => {
      const nextPicks = [...picks, opt];
      setPicks(nextPicks);
      if (step + 1 >= total) {
        // 末题：合成收尾速写。
        setResult(summarize(traits, name, nextPicks));
      }
      setReflection(opt.reflection);
    },
    [picks, step, total, traits, name],
  );

  // 玩家手动点选。
  const choose = useCallback(
    (opt: MicroOption) => {
      if (reflection || result) return; // 折射展示中或已结束，忽略重复点击。
      advance(opt);
    },
    [reflection, result, advance],
  );

  // 折射展示约 1.6 秒后翻到下一题（或停在收尾页）。
  useEffect(() => {
    if (!reflection) return;
    const t = window.setTimeout(() => {
      setReflection("");
      setStep((s) => s + 1);
      setRemaining(SNAPSHOT_SECONDS);
    }, 1600);
    return () => window.clearTimeout(t);
  }, [reflection]);

  // 每题倒计时：到 0 自动替玩家选「最契合她 persona 的那个」——「她会自己活」。
  useEffect(() => {
    if (!current || reflection || result) return;
    if (remaining <= 0) {
      // 超时：自动拣该角色更倾向的选项（与 summarize 同口径的契合度）。
      const [a, b] = current.options;
      advance(optionFit(b, traits) > optionFit(a, traits) ? b : a);
      return;
    }
    const t = window.setTimeout(() => setRemaining((r) => r - 1), 1000);
    return () => window.clearTimeout(t);
  }, [current, reflection, result, remaining, traits, advance]);

  // 收尾页。
  if (result) {
    return (
      <div className="fate-shell fate-onboarding">
        <div className="fate-preview fate-snap">
          <div className="fate-preview-title">这就是她</div>
          <div className="fate-snap-signature">{result.signature}</div>
          <p className="fate-snap-verdict-text">{result.verdict}</p>
          <button onClick={onDone}>进入她的人生 →</button>
        </div>
      </div>
    );
  }

  if (!current) {
    // 防御：无题可问（理论不会发生）——直接进局。
    return (
      <div className="fate-shell fate-onboarding">
        <div className="fate-preview">
          <button onClick={onDone}>进入她的人生 →</button>
        </div>
      </div>
    );
  }

  const urgent = remaining <= 2 && !reflection;
  return (
    <div className="fate-shell fate-onboarding">
      <div className="fate-preview fate-snap">
        <div className="fate-snap-progress">
          {choices.map((c, i) => (
            <span
              key={c.id}
              className={`fate-snap-dot${i < step ? " done" : ""}${i === step ? " active" : ""}`}
            />
          ))}
        </div>
        <div className="fate-snap-head">
          人生快进 · 第 {step + 1} / {total} 幕
        </div>

        {reflection ? (
          <div className="fate-snap-reflection">{reflection}</div>
        ) : (
          <>
            <p className="fate-snap-scene">{current.scene}</p>
            <div
              className={`fate-snap-timer${urgent ? " urgent" : ""}`}
              aria-label={`还剩 ${remaining} 秒`}
            >
              {remaining}
            </div>
            <div className="fate-snap-options">
              {current.options.map((opt) => (
                <button key={opt.id} onClick={() => choose(opt)}>
                  {opt.label}
                </button>
              ))}
            </div>
            <div className="fate-snap-hint">凭直觉，别多想——她会成为你此刻替她选的样子。</div>
          </>
        )}
      </div>
    </div>
  );
}
