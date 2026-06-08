/* 文件说明：角色命运开盒的独立入口（与旧战棋客户端分离，main.tsx 按 #fate 路由到此）。
   流程：捏人三步 + 离线宪章（onboarding，宪法 §5.1/GDD §4）→ 即时人格快照（O2 最高 ROI）→ 四槽主界面。
   会话与角色 ID 存 localStorage，刷新自动续上她的人生。*/

import { useCallback, useEffect, useState } from "react";
import { bootstrapCharacter, recordPlayerIntervention } from "../session/api";
import { FateView } from "./FateView";
import "./fate.css";

type Phase = "onboarding" | "preview" | "play";

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

const ORIGINS = ["边境猎户", "铁匠之女", "落魄书生", "行脚商人", "庙祝巫医", "流亡贵族", "采药孤女"];

export function FateApp() {
  const [phase, setPhase] = useState<Phase>("onboarding");
  const [saved, setSaved] = useState<Saved | null>(() => loadSaved());
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const [name, setName] = useState("");
  const [origin, setOrigin] = useState(ORIGINS[0]);
  const [desire, setDesire] = useState("");
  const [wound, setWound] = useState("");
  const [redline, setRedline] = useState("");
  const [preview, setPreview] = useState<{ name: string; bio: string } | null>(null);

  useEffect(() => {
    if (saved) setPhase("play");
  }, [saved]);

  const create = useCallback(async () => {
    const trimmed = name.trim() || "无名";
    setBusy(true);
    setError("");
    try {
      const sessionId = (window.crypto?.randomUUID?.() ?? `fate_${Date.now()}`);
      // withVillage=true：onboarding 触发 SeedVillage，兑现「她身边已有二十个有名有姓的人」。
      const unit = await bootstrapCharacter(trimmed, sessionId, "player", true);
      const unitId = unit ? String((unit as Record<string, unknown>).id ?? "") : "";
      if (!unitId) throw new Error("未能创建角色");

      // 离线宪章 + 欲望/伤痕作为「家训/托梦」落成可被回响引用的玩家动作。
      const charter = [
        desire.trim() && `她想要的：${desire.trim()}`,
        wound.trim() && `她的伤痕：${wound.trim()}`,
        redline.trim() && `你立下的家训：她绝不能${redline.trim()}`,
        `出身：${origin}`,
      ]
        .filter(Boolean)
        .join("；");
      if (charter) {
        await recordPlayerIntervention(sessionId, unitId, charter);
      }

      const identity = (unit as Record<string, unknown>).identity as Record<string, unknown> | undefined;
      setPreview({
        name: trimmed,
        bio:
          String(identity?.biography ?? "") ||
          `${origin}出身的${trimmed}。${desire.trim() ? "她心里一直惦记着：" + desire.trim() + "。" : ""}`,
      });
      const next: Saved = { sessionId, unitId, name: trimmed };
      window.localStorage.setItem(STORE_KEY, JSON.stringify(next));
      setSaved(next);
      setPhase("preview");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }, [name, origin, desire, wound, redline]);

  const restart = useCallback(() => {
    window.localStorage.removeItem(STORE_KEY);
    setSaved(null);
    setPreview(null);
    setPhase("onboarding");
  }, []);

  if (phase === "play" && saved) {
    return (
      <div className="fate-shell">
        <FateView sessionId={saved.sessionId} unitId={saved.unitId} />
        <button className="fate-restart" onClick={restart}>
          另起一段人生
        </button>
      </div>
    );
  }

  if (phase === "preview" && saved && preview) {
    return (
      <div className="fate-shell fate-onboarding">
        <div className="fate-preview">
          <div className="fate-preview-title">她来到了世上</div>
          <div className="fate-preview-name">{preview.name}</div>
          <p className="fate-preview-bio">{preview.bio}</p>
          <p className="fate-preview-hint">她身边，已有二十个有名有姓、有恩有怨的人。从此，她的命运不再由你操控，只由你牵挂。</p>
          <button onClick={() => setPhase("play")}>进入她的人生 →</button>
        </div>
      </div>
    );
  }

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
      </div>
    </div>
  );
}
