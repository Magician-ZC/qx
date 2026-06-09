/* 文件说明：GM 后台「世界事件注入」面板。
   表单 → POST /api/ops/worlds/:worldId/events（已落地，opsTokenGuard）：往某活世界投一条权威跨事件
   （天灾/外敌/丰年…），与玩家事件同总线、同时钟，append-only、全量留审计。
   kind/importance 必填，actor/target/region/叙事文案可选（叙事文案并入 payload.narrative）。 */

import { useCallback, useState } from "react";
import { errText, injectWorldEvent } from "./adminApi";

export function GmEventPanel(): JSX.Element {
  const [worldId, setWorldId] = useState("");
  const [kind, setKind] = useState("");
  const [importance, setImportance] = useState(5);
  const [actorId, setActorId] = useState("");
  const [targetId, setTargetId] = useState("");
  const [regionId, setRegionId] = useState("");
  const [narrative, setNarrative] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [ok, setOk] = useState("");

  const submit = useCallback(async () => {
    if (!worldId.trim() || !kind.trim()) {
      setErr("世界 ID 与事件类型（kind）均必填。");
      setOk("");
      return;
    }
    setBusy(true);
    setErr("");
    setOk("");
    try {
      const res = await injectWorldEvent(worldId.trim(), {
        kind: kind.trim(),
        importance,
        actorId: actorId.trim() || undefined,
        targetId: targetId.trim() || undefined,
        regionId: regionId.trim() || undefined,
        payload: narrative.trim() ? { narrative: narrative.trim() } : undefined,
      });
      setOk(
        `已注入：tick=${res.world_tick}，cross_event=${res.cross_event_id.slice(0, 8)}…，审计=${res.audit_id.slice(0, 8)}…`,
      );
    } catch (e) {
      setErr(`注入失败：${errText(e)}`);
    } finally {
      setBusy(false);
    }
  }, [actorId, importance, kind, narrative, regionId, targetId, worldId]);

  return (
    <div className="adm-card">
      <div className="adm-card-title">GM 世界事件注入</div>
      <p className="adm-card-sub">
        往某活世界投一条权威跨事件（天灾 / 外敌压境 / 集市丰年…），与玩家事件同总线、同时钟，append-only、全量留审计。
      </p>

      <label className="adm-label">世界 ID</label>
      <input className="adm-input" value={worldId} onChange={(e) => setWorldId(e.target.value)} placeholder="world_id" />

      <label className="adm-label">事件类型（kind）</label>
      <input
        className="adm-input"
        value={kind}
        onChange={(e) => setKind(e.target.value)}
        placeholder="如 天灾 / 外敌压境 / 集市丰年"
      />

      <label className="adm-label">重要度（0–10）</label>
      <input
        className="adm-input"
        type="number"
        min={0}
        max={10}
        value={importance}
        onChange={(e) => setImportance(Number(e.target.value))}
      />

      <div className="adm-row">
        <div style={{ flex: "1 1 150px" }}>
          <label className="adm-label">Actor ID（可选）</label>
          <input className="adm-input" value={actorId} onChange={(e) => setActorId(e.target.value)} placeholder="actor_id" />
        </div>
        <div style={{ flex: "1 1 150px" }}>
          <label className="adm-label">Target ID（可选）</label>
          <input
            className="adm-input"
            value={targetId}
            onChange={(e) => setTargetId(e.target.value)}
            placeholder="target_id"
          />
        </div>
        <div style={{ flex: "1 1 150px" }}>
          <label className="adm-label">Region ID（可选）</label>
          <input
            className="adm-input"
            value={regionId}
            onChange={(e) => setRegionId(e.target.value)}
            placeholder="region_id"
          />
        </div>
      </div>

      <label className="adm-label">叙事文案（可选，进 payload.narrative）</label>
      <textarea
        className="adm-textarea"
        value={narrative}
        onChange={(e) => setNarrative(e.target.value)}
        placeholder="如 山洪暴发，沿河村落告急"
      />

      <div style={{ marginTop: 12 }}>
        <button type="button" className="adm-btn adm-btn-primary" onClick={() => void submit()} disabled={busy}>
          {busy ? "注入中…" : "注入世界事件"}
        </button>
      </div>
      {err ? <div className="adm-toast adm-toast-err">{err}</div> : null}
      {ok ? <div className="adm-toast adm-toast-ok">{ok}</div> : null}
    </div>
  );
}

export default GmEventPanel;
