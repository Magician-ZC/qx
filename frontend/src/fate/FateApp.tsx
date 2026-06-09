/* 文件说明：角色命运开盒的独立入口（与旧战棋客户端分离，Root.tsx 默认路由到此）。
   流程（账号绑定主世界版）：登录/注册（账号鉴权）→ 拉取「我的主世界角色」→
     已有 → 直接进四槽主界面（resume 该账号在主世界的同一角色，多设备一致）；
     未有 → 捏人三步 + 离线宪章（onboarding，宪法 §5.1/GDD §4）→ 即时人格快照（O2 最高 ROI）→ 四槽主界面。
   权威态：账号 Bearer 令牌（api.ts 经 localStorage 持久化、自动随请求发送）。localStorage 另缓存
   当前角色 (sessionId/unitId/name) 仅为减少请求的「乐观缓存」，与令牌取到的权威角色不一致时以后者为准。*/

import { useCallback, useEffect, useMemo, useState } from "react";
import {
  createMyCharacter,
  getAccountToken,
  getMyCharacter,
  getUnitStatus,
  loginAccount,
  logoutAccount,
  recordPlayerIntervention,
  registerAccount,
  trackFunnel,
} from "../session/api";
import { FateView } from "./FateView";
import {
  fromPersonalityBlock,
  optionFit,
  pickChoices,
  summarize,
  type MicroOption,
  type PersonaTraits,
  type SnapshotResult,
} from "./personaSnapshot";
import "./fate.css";

// crossFileNeeds（本波 W-B 三人分工，本文件只编辑 FateApp.tsx）：
//   - api.ts（B1 拥有）已提供并被本文件调用：getMyCharacter() → MyCharacter{ has_character, session_id?,
//     unit_id?, name? }；createMyCharacter(MyCharacterInput{name?,origin?,desire?,wound?,redline?}) → MyCharacter。
//     账号鉴权三函数当前签名为 registerAccount({username,password})/loginAccount({username,password})（内部
//     setAccountToken 持久化 Bearer，本文件不取返回值）与 logoutAccount(token)（传 getAccountToken()）——
//     本文件已按现签名调用；若 B1 后续统一为 (username,password)→{token}/logoutAccount()，本文件三处调用需同步。
//   - FateView（B3 拥有）当前 props 为 { sessionId, unitId }，已够用。若日后要在四槽内「接管关键战」，
//     需 FateView 新增 onEnterBattle?(sessionId) 回调（点击后由本壳 window.location.hash = `#battle/${sessionId}`
//     切到 App 战棋接管视图）——属 B3 改 FateView + B1 改 Root.tsx 的 #battle 路由，本文件届时只透传回调。
//   - 即时人格快照仍为纯体验层、零持久化；若要把玩家微选择落库/反哺 persona，需 api.ts 新增回执函数。

type Phase = "auth" | "gate" | "onboarding" | "preview" | "snapshot" | "play";

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

export function FateApp() {
  // 初始相位：已有账号令牌 → 进 gate 拉取我的角色；无令牌 → 先登录。
  const [phase, setPhase] = useState<Phase>(() => (getAccountToken() ? "gate" : "auth"));
  // saved 仅作乐观缓存兜底；play 渲染前会被 gate 用权威角色覆盖。
  const [saved, setSaved] = useState<Saved | null>(() => loadSaved());
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  // 捏人四要素 + 出身。
  const [name, setName] = useState("");
  const [origin, setOrigin] = useState(ORIGINS[0]);
  const [desire, setDesire] = useState("");
  const [wound, setWound] = useState("");
  const [redline, setRedline] = useState("");
  const [preview, setPreview] = useState<{ name: string; bio: string; traits: PersonaTraits } | null>(
    null,
  );

  // 登录/注册表单态。
  const [authMode, setAuthMode] = useState<"login" | "register">("login");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");

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
      // 拉取失败（如令牌过期）→ 回登录，让玩家重新鉴权。
      setPhase("auth");
    } finally {
      setBusy(false);
    }
  }, [saved?.name]);

  // 进入 gate 相位即拉取我的角色（仅在该相位触发一次）。
  useEffect(() => {
    if (phase === "gate") void loadCharacter();
  }, [phase, loadCharacter]);

  // 登录/注册：成功后 api.ts 已把 Bearer 令牌写入 localStorage，转 gate 拉角色。
  const submitAuth = useCallback(async () => {
    const u = username.trim();
    const p = password;
    if (!u || !p) {
      setError("请填写账号与口令。");
      return;
    }
    setBusy(true);
    setError("");
    try {
      // 登录/注册成功后 api.ts 内部已 setAccountToken（Bearer 入 localStorage），故此处无需用返回值。
      if (authMode === "register") {
        await registerAccount({ username: u, password: p });
      } else {
        await loginAccount({ username: u, password: p });
      }
      setPassword("");
      setPhase("gate");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }, [authMode, username, password]);

  // create：账号绑定的幂等降生（非匿名 bootstrap）。保留捏人四要素 + 离线宪章 + 即时人格快照体验，
  // 只把「创建」那步换成账号绑定端点 createMyCharacter。
  const create = useCallback(async () => {
    const trimmed = name.trim() || "无名";
    setBusy(true);
    setError("");
    try {
      // 账号绑定幂等降生：后端据令牌把角色挂到该账号，重复调用返回同一角色（多设备/重试安全）。
      const mine = await createMyCharacter({
        name: trimmed,
        origin,
        desire: desire.trim(),
        wound: wound.trim(),
        redline: redline.trim(),
      });
      const sessionId = mine.session_id;
      const unitId = mine.unit_id;
      if (!sessionId || !unitId) throw new Error("未能创建角色");

      // 离线宪章 + 欲望/伤痕作为「家训/托梦」落成可被回响引用的玩家动作。
      // （createMyCharacter 已带四要素入库；这里再以托梦形式落一条人话版家训，供日后回响引用。）
      const charter = [
        desire.trim() && `她想要的：${desire.trim()}`,
        wound.trim() && `她的伤痕：${wound.trim()}`,
        redline.trim() && `你立下的家训：她绝不能${redline.trim()}`,
        `出身：${origin}`,
      ]
        .filter(Boolean)
        .join("；");
      if (charter) {
        // best-effort：托梦失败不挡降生（角色已建）。
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
  }, [name, origin, desire, wound, redline]);

  // 登出：清账号令牌（api.ts 负责）+ 本地角色缓存，回登录页。
  const signOut = useCallback(async () => {
    try {
      // logoutAccount 当前签名收 token；传当前 Bearer，内部无论成败都会清本地令牌。
      await logoutAccount(getAccountToken());
    } catch {
      /* 后端登出失败也无妨，本地令牌仍会被清 */
    }
    persistSaved(null);
    setSaved(null);
    setPreview(null);
    setUsername("");
    setPassword("");
    setPhase("auth");
  }, []);

  // ── play：四槽主界面（账号的主世界角色） ──
  if (phase === "play" && saved) {
    return (
      <div className="fate-shell">
        <FateView sessionId={saved.sessionId} unitId={saved.unitId} />
        <button className="fate-restart" onClick={() => void signOut()}>
          换个账号登入
        </button>
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

  // ── auth：登录 / 注册（账号鉴权，令牌存 localStorage 作 Bearer） ──
  if (phase === "auth") {
    return (
      <div className="fate-shell fate-onboarding">
        <div className="fate-create">
          <h1>群像 · 命运</h1>
          <p className="fate-create-lead">
            登入你的家门。你牵挂的那个人，在世上某处，等你回来看她。
          </p>

          <label>
            账号
            <input
              value={username}
              placeholder="你的名号"
              autoComplete="username"
              onChange={(e) => setUsername(e.target.value)}
            />
          </label>
          <label>
            口令
            <input
              type="password"
              value={password}
              placeholder="你的口令"
              autoComplete={authMode === "register" ? "new-password" : "current-password"}
              onChange={(e) => setPassword(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") void submitAuth();
              }}
            />
          </label>

          {error && <div className="fate-error">{error}</div>}
          <button className="fate-create-btn" disabled={busy} onClick={() => void submitAuth()}>
            {busy
              ? authMode === "register"
                ? "正在为你立户…"
                : "正在唤你回门…"
              : authMode === "register"
                ? "立一道家门"
                : "登入家门"}
          </button>
          <button
            className="fate-restart"
            disabled={busy}
            onClick={() => {
              setError("");
              setAuthMode((m) => (m === "login" ? "register" : "login"));
            }}
          >
            {authMode === "login" ? "还没有家门？立一个 →" : "已有家门？登入 →"}
          </button>
        </div>
      </div>
    );
  }

  // ── onboarding：捏人（账号已登入、尚未降生角色） ──
  return (
    <div className="fate-shell fate-onboarding">
      <div className="fate-create">
        <h1>群像 · 命运开盒</h1>
        <p className="fate-create-lead">捏一个人，把她丢进世界。她会自己活——你只能托梦、疾呼，却不能替她做主。</p>

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
