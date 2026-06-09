/* 文件说明：GM 后台「内容运营」面板。
   一个面板内三个折叠分区，对应三套内容运营数据，均带 list + 新增表单 + 删除：
   ① 母题库（season_content_themes）：GET/POST/DELETE /api/admin/content-themes。
      decisive_event_ids / title_ids / landmark_names 三个字符串数组字段用逗号分隔输入框编辑
      （输入 "a,b,c" → split 成数组；展示时 join 回逗号串）。
   ② 翻译模板（translation_templates）：GET/POST/DELETE /api/admin/translation-templates。
      按 reason_code+anchor_kind upsert，写后即时生效；force_pending 用 toggle，priority 数值。
   ③ SKU 目录（billing_skus）：GET /api/billing/skus（公开）+ POST /api/admin/skus。
      billing 关闭时 GET 失败 / POST 返 503——分区降级提示「billing 未开启」并禁用新增。

   风格 mirror FlagsPanel：adm-card 卡片 / adm-toast 反馈 / backendReady 降级。
   后端这三套路由均已接线（crossFileNeeds 已落地），未接线/失败时各分区独立降级，互不影响。 */

import { useCallback, useEffect, useState } from "react";
import {
  deleteContentTheme,
  deleteTranslationTemplate,
  errText,
  listContentThemes,
  listSKUs,
  listTranslationTemplates,
  upsertContentTheme,
  upsertSKU,
  upsertTranslationTemplate,
  type ContentTheme,
  type SKU,
  type TranslationTemplate,
} from "./adminApi";

// parseCsv 把逗号分隔输入归一成去空白、去空项的字符串数组（"a, b ,," → ["a","b"]）。
function parseCsv(text: string): string[] {
  return text
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s !== "");
}

// ============ ① 母题库分区 ============

function ThemesSection(): JSX.Element {
  const [themes, setThemes] = useState<ContentTheme[]>([]);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState("");
  const [ok, setOk] = useState("");
  // backendReady=false 表示母题端点不可用（拉取失败）：新增表单禁用。
  const [backendReady, setBackendReady] = useState(true);
  const [busy, setBusy] = useState(false);

  // 新增表单（数组字段用逗号分隔文本暂存，提交时 split）。
  const [seasonId, setSeasonId] = useState("");
  const [decisiveEvents, setDecisiveEvents] = useState("");
  const [titleIds, setTitleIds] = useState("");
  const [landmarks, setLandmarks] = useState("");

  const load = useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      const data = await listContentThemes();
      setThemes(data);
      setBackendReady(true);
    } catch (e) {
      setBackendReady(false);
      setErr(`母题库端点不可用：${errText(e)}`);
      setThemes([]);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const submit = useCallback(async () => {
    if (!seasonId.trim()) {
      setErr("赛季 ID（season_id）必填。");
      setOk("");
      return;
    }
    setBusy(true);
    setErr("");
    setOk("");
    try {
      const id = await upsertContentTheme({
        id: "",
        season_id: seasonId.trim(),
        decisive_event_ids: parseCsv(decisiveEvents),
        title_ids: parseCsv(titleIds),
        landmark_names: parseCsv(landmarks),
        created_at: "",
      });
      setOk(`已保存母题：id=${id.slice(0, 8)}…`);
      setSeasonId("");
      setDecisiveEvents("");
      setTitleIds("");
      setLandmarks("");
      await load();
    } catch (e) {
      setErr(`保存母题失败：${errText(e)}`);
    } finally {
      setBusy(false);
    }
  }, [decisiveEvents, landmarks, load, seasonId, titleIds]);

  const remove = useCallback(
    async (id: string) => {
      setBusy(true);
      setErr("");
      setOk("");
      try {
        await deleteContentTheme(id);
        setOk(`已删除母题 ${id.slice(0, 8)}…`);
        await load();
      } catch (e) {
        setErr(`删除母题失败：${errText(e)}`);
      } finally {
        setBusy(false);
      }
    },
    [load],
  );

  return (
    <div className="adm-card">
      <div className="adm-card-title">
        <span>母题库（season_content_themes）</span>
        <button type="button" className="adm-btn" onClick={() => void load()} disabled={loading}>
          {loading ? "加载中…" : "刷新"}
        </button>
      </div>
      <p className="adm-card-sub">
        每个赛季的内容母题：本季关键事件 / 头衔 / 地标名集合。三个数组字段用英文逗号分隔多项（如 a,b,c）。
      </p>

      {err ? <div className="adm-toast adm-toast-err">{err}</div> : null}
      {ok ? <div className="adm-toast adm-toast-ok">{ok}</div> : null}

      {/* 新增表单 */}
      <label className="adm-label">赛季 ID（season_id）</label>
      <input
        className="adm-input"
        value={seasonId}
        onChange={(e) => setSeasonId(e.target.value)}
        placeholder="season_id"
        disabled={!backendReady}
      />
      <label className="adm-label">关键事件 ID（decisive_event_ids，逗号分隔）</label>
      <input
        className="adm-input"
        value={decisiveEvents}
        onChange={(e) => setDecisiveEvents(e.target.value)}
        placeholder="evt_a, evt_b"
        disabled={!backendReady}
      />
      <label className="adm-label">头衔 ID（title_ids，逗号分隔）</label>
      <input
        className="adm-input"
        value={titleIds}
        onChange={(e) => setTitleIds(e.target.value)}
        placeholder="title_a, title_b"
        disabled={!backendReady}
      />
      <label className="adm-label">地标名（landmark_names，逗号分隔）</label>
      <input
        className="adm-input"
        value={landmarks}
        onChange={(e) => setLandmarks(e.target.value)}
        placeholder="落霞渡, 听涛崖"
        disabled={!backendReady}
      />
      <div style={{ marginTop: 12 }}>
        <button
          type="button"
          className="adm-btn adm-btn-primary"
          onClick={() => void submit()}
          disabled={!backendReady || busy}
        >
          {busy ? "保存中…" : "新增母题"}
        </button>
      </div>

      {/* 列表 */}
      {themes.length === 0 ? (
        <div className="adm-empty">暂无母题。</div>
      ) : (
        <table className="adm-table" style={{ marginTop: 12 }}>
          <thead>
            <tr>
              <th>母题 / 赛季</th>
              <th>关键事件</th>
              <th>头衔</th>
              <th>地标</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {themes.map((t) => (
              <tr key={t.id}>
                <td>
                  <div style={{ fontFamily: "ui-monospace, monospace", fontSize: 11, color: "#f0ead8" }}>
                    {t.id.slice(0, 8)}…
                  </div>
                  <div style={{ color: "#9aa0ad", fontSize: 11 }}>季 {t.season_id.slice(0, 8)}…</div>
                </td>
                <td style={{ fontSize: 11 }}>{t.decisive_event_ids.join(", ") || "—"}</td>
                <td style={{ fontSize: 11 }}>{t.title_ids.join(", ") || "—"}</td>
                <td style={{ fontSize: 11 }}>{t.landmark_names.join(", ") || "—"}</td>
                <td>
                  <button
                    type="button"
                    className="adm-btn adm-btn-danger"
                    style={{ padding: "5px 9px", fontSize: 11 }}
                    onClick={() => void remove(t.id)}
                    disabled={busy}
                  >
                    删除
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

// ============ ② 翻译模板分区 ============

function TemplatesSection(): JSX.Element {
  const [templates, setTemplates] = useState<TranslationTemplate[]>([]);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState("");
  const [ok, setOk] = useState("");
  const [backendReady, setBackendReady] = useState(true);
  const [busy, setBusy] = useState(false);

  // 新增表单。
  const [reasonCode, setReasonCode] = useState("");
  const [anchorKind, setAnchorKind] = useState("");
  const [narrative, setNarrative] = useState("");
  const [forcePending, setForcePending] = useState(false);
  const [priority, setPriority] = useState(0);

  const load = useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      const data = await listTranslationTemplates();
      setTemplates(data);
      setBackendReady(true);
    } catch (e) {
      setBackendReady(false);
      setErr(`翻译模板端点不可用：${errText(e)}`);
      setTemplates([]);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const submit = useCallback(async () => {
    if (!reasonCode.trim() || !anchorKind.trim()) {
      setErr("reason_code 与 anchor_kind 均必填（联合主键）。");
      setOk("");
      return;
    }
    setBusy(true);
    setErr("");
    setOk("");
    try {
      await upsertTranslationTemplate({
        id: "",
        reason_code: reasonCode.trim(),
        anchor_kind: anchorKind.trim(),
        narrative_template: narrative,
        force_pending: forcePending,
        priority,
        updated_at: "",
      });
      setOk(`已保存模板：${reasonCode.trim()} / ${anchorKind.trim()}（写后即时生效）。`);
      setReasonCode("");
      setAnchorKind("");
      setNarrative("");
      setForcePending(false);
      setPriority(0);
      await load();
    } catch (e) {
      setErr(`保存模板失败：${errText(e)}`);
    } finally {
      setBusy(false);
    }
  }, [anchorKind, forcePending, load, narrative, priority, reasonCode]);

  const remove = useCallback(
    async (rc: string, ak: string) => {
      setBusy(true);
      setErr("");
      setOk("");
      try {
        await deleteTranslationTemplate(rc, ak);
        setOk(`已删除模板 ${rc} / ${ak}。`);
        await load();
      } catch (e) {
        setErr(`删除模板失败：${errText(e)}`);
      } finally {
        setBusy(false);
      }
    },
    [load],
  );

  return (
    <div className="adm-card">
      <div className="adm-card-title">
        <span>翻译模板（translation_templates）</span>
        <button type="button" className="adm-btn" onClick={() => void load()} disabled={loading}>
          {loading ? "加载中…" : "刷新"}
        </button>
      </div>
      <p className="adm-card-sub">
        把世界事件按 reason_code + 关系锚类型（anchor_kind）翻译成叙事文案。按联合主键 upsert，写后即时生效。
        force_pending 强制走待决策（高光卡 / 收件箱）而非自治。
      </p>

      {err ? <div className="adm-toast adm-toast-err">{err}</div> : null}
      {ok ? <div className="adm-toast adm-toast-ok">{ok}</div> : null}

      {/* 新增表单 */}
      <div className="adm-row">
        <div style={{ flex: "1 1 160px" }}>
          <label className="adm-label">reason_code</label>
          <input
            className="adm-input"
            value={reasonCode}
            onChange={(e) => setReasonCode(e.target.value)}
            placeholder="如 combat_damage"
            disabled={!backendReady}
          />
        </div>
        <div style={{ flex: "1 1 160px" }}>
          <label className="adm-label">anchor_kind</label>
          <input
            className="adm-input"
            value={anchorKind}
            onChange={(e) => setAnchorKind(e.target.value)}
            placeholder="如 relation / self"
            disabled={!backendReady}
          />
        </div>
        <div style={{ flex: "0 0 120px" }}>
          <label className="adm-label">priority</label>
          <input
            className="adm-input"
            type="number"
            value={priority}
            onChange={(e) => setPriority(Number(e.target.value))}
            disabled={!backendReady}
          />
        </div>
      </div>
      <label className="adm-label">叙事模板（narrative_template）</label>
      <textarea
        className="adm-textarea"
        value={narrative}
        onChange={(e) => setNarrative(e.target.value)}
        placeholder="如 {actor} 在你最需要的时候，替你挡下了那一刀。"
        disabled={!backendReady}
      />
      <div className="adm-row" style={{ marginTop: 10 }}>
        <label className="adm-label" style={{ margin: 0 }}>
          force_pending（强制待决策）
        </label>
        <label className="adm-toggle" title="强制走待决策而非自治">
          <input
            type="checkbox"
            checked={forcePending}
            onChange={(e) => setForcePending(e.target.checked)}
            disabled={!backendReady}
          />
          <span className="adm-toggle-track" />
        </label>
      </div>
      <div style={{ marginTop: 12 }}>
        <button
          type="button"
          className="adm-btn adm-btn-primary"
          onClick={() => void submit()}
          disabled={!backendReady || busy}
        >
          {busy ? "保存中…" : "新增 / 更新模板"}
        </button>
      </div>

      {/* 列表 */}
      {templates.length === 0 ? (
        <div className="adm-empty">暂无翻译模板。</div>
      ) : (
        <table className="adm-table" style={{ marginTop: 12 }}>
          <thead>
            <tr>
              <th>reason_code / anchor_kind</th>
              <th>叙事模板</th>
              <th>待决策</th>
              <th className="adm-num">优先级</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {templates.map((t) => (
              <tr key={`${t.reason_code}|${t.anchor_kind}`}>
                <td style={{ fontFamily: "ui-monospace, monospace", fontSize: 11 }}>
                  {t.reason_code}
                  <span style={{ color: "#9aa0ad" }}> / {t.anchor_kind}</span>
                </td>
                <td style={{ fontSize: 11 }}>{t.narrative_template || "—"}</td>
                <td>
                  <span className={`adm-badge ${t.force_pending ? "adm-badge-on" : "adm-badge-off"}`}>
                    {t.force_pending ? "ON" : "OFF"}
                  </span>
                </td>
                <td className="adm-num">{t.priority}</td>
                <td>
                  <button
                    type="button"
                    className="adm-btn adm-btn-danger"
                    style={{ padding: "5px 9px", fontSize: 11 }}
                    onClick={() => void remove(t.reason_code, t.anchor_kind)}
                    disabled={busy}
                  >
                    删除
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

// ============ ③ SKU 目录分区 ============

function SkuSection(): JSX.Element {
  const [skus, setSkus] = useState<SKU[]>([]);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState("");
  const [ok, setOk] = useState("");
  // billingReady=false 表示 GET /api/billing/skus 失败（多为 billing 未开启 503/404）：禁用新增。
  const [billingReady, setBillingReady] = useState(true);
  const [busy, setBusy] = useState(false);

  // 新增表单。
  const [kind, setKind] = useState("");
  const [name, setName] = useState("");
  const [priceCents, setPriceCents] = useState(0);
  const [period, setPeriod] = useState("");
  const [active, setActive] = useState(true);

  const load = useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      const data = await listSKUs();
      setSkus(data);
      setBillingReady(true);
    } catch (e) {
      setBillingReady(false);
      setErr(`SKU 目录不可用：${errText(e)}（billing 未开启或端点未接线，新增已禁用）`);
      setSkus([]);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const submit = useCallback(async () => {
    if (!name.trim() || !kind.trim()) {
      setErr("名称（name）与类型（kind）均必填。");
      setOk("");
      return;
    }
    setBusy(true);
    setErr("");
    setOk("");
    try {
      const id = await upsertSKU({
        id: "",
        kind: kind.trim(),
        name: name.trim(),
        price_cents: priceCents,
        period: period.trim(),
        active,
        created_at: "",
      });
      setOk(`已保存 SKU：id=${id.slice(0, 8)}…`);
      setKind("");
      setName("");
      setPriceCents(0);
      setPeriod("");
      setActive(true);
      await load();
    } catch (e) {
      // billing 关时 POST 返 503——给明确提示并标 billing 未就绪。
      setBillingReady(false);
      setErr(`保存 SKU 失败：${errText(e)}（billing 关闭时返 503）`);
    } finally {
      setBusy(false);
    }
  }, [active, kind, load, name, period, priceCents]);

  return (
    <div className="adm-card">
      <div className="adm-card-title">
        <span>SKU 目录（billing_skus）</span>
        <button type="button" className="adm-btn" onClick={() => void load()} disabled={loading}>
          {loading ? "加载中…" : "刷新"}
        </button>
      </div>
      <p className="adm-card-sub">
        计费 SKU 目录（订阅 / 一次性内购档）。列表读公开端点 /api/billing/skus，新增写 /api/admin/skus。
        billing 未开启时本分区禁用新增并提示。
      </p>

      {!billingReady ? <div className="adm-toast adm-toast-err">billing 未开启：SKU 新增已禁用（GET 失败或 POST 返 503）。</div> : null}
      {err ? <div className="adm-toast adm-toast-err">{err}</div> : null}
      {ok ? <div className="adm-toast adm-toast-ok">{ok}</div> : null}

      {/* 新增表单 */}
      <div className="adm-row">
        <div style={{ flex: "1 1 140px" }}>
          <label className="adm-label">类型（kind）</label>
          <input
            className="adm-input"
            value={kind}
            onChange={(e) => setKind(e.target.value)}
            placeholder="如 subscription / one_time"
            disabled={!billingReady}
          />
        </div>
        <div style={{ flex: "1 1 160px" }}>
          <label className="adm-label">名称（name）</label>
          <input
            className="adm-input"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="如 月卡"
            disabled={!billingReady}
          />
        </div>
      </div>
      <div className="adm-row">
        <div style={{ flex: "0 0 140px" }}>
          <label className="adm-label">定价（分，price_cents）</label>
          <input
            className="adm-input"
            type="number"
            min={0}
            value={priceCents}
            onChange={(e) => setPriceCents(Number(e.target.value))}
            disabled={!billingReady}
          />
        </div>
        <div style={{ flex: "0 0 140px" }}>
          <label className="adm-label">周期（period）</label>
          <input
            className="adm-input"
            value={period}
            onChange={(e) => setPeriod(e.target.value)}
            placeholder="如 month / once"
            disabled={!billingReady}
          />
        </div>
        <div style={{ display: "flex", alignItems: "center", gap: 8, flex: "0 0 auto" }}>
          <label className="adm-label" style={{ margin: 0 }}>
            上架（active）
          </label>
          <label className="adm-toggle" title="是否上架">
            <input
              type="checkbox"
              checked={active}
              onChange={(e) => setActive(e.target.checked)}
              disabled={!billingReady}
            />
            <span className="adm-toggle-track" />
          </label>
        </div>
      </div>
      <div style={{ marginTop: 12 }}>
        <button
          type="button"
          className="adm-btn adm-btn-primary"
          onClick={() => void submit()}
          disabled={!billingReady || busy}
        >
          {busy ? "保存中…" : "新增 SKU"}
        </button>
      </div>

      {/* 列表 */}
      {skus.length === 0 ? (
        <div className="adm-empty">暂无 SKU。</div>
      ) : (
        <table className="adm-table" style={{ marginTop: 12 }}>
          <thead>
            <tr>
              <th>名称 / 类型</th>
              <th className="adm-num">定价（分）</th>
              <th>周期</th>
              <th>上架</th>
            </tr>
          </thead>
          <tbody>
            {skus.map((s) => (
              <tr key={s.id}>
                <td>
                  <div style={{ color: "#f0ead8" }}>{s.name || "(未命名)"}</div>
                  <div style={{ color: "#9aa0ad", fontSize: 11 }}>{s.kind}</div>
                </td>
                <td className="adm-num">{s.price_cents}</td>
                <td>{s.period || "—"}</td>
                <td>
                  <span className={`adm-badge ${s.active ? "adm-badge-on" : "adm-badge-off"}`}>
                    {s.active ? "上架" : "下架"}
                  </span>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

// ContentPanel 是「内容运营」页签根：母题库 / 翻译模板 / SKU 目录三分区纵向排列，各自独立加载与降级。
export function ContentPanel(): JSX.Element {
  return (
    <>
      <ThemesSection />
      <TemplatesSection />
      <SkuSection />
    </>
  );
}

export default ContentPanel;
