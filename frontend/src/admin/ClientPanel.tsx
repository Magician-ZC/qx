/* 文件说明：GM 后台「客户管理」面板（账户运营 + 合规处置）。
   左栏：客户列表——搜索框 q（模糊匹配 username/display_name 或 id 精确）+ 列
   username/display_name/封禁徽标/created_at，点行选中。读 GET /api/admin/clients?q=&limit=
   （operator 级 opsWriter；未配 ops 鉴权时 503 → backendReady=false 禁用并提示「需配 ops 鉴权(operator+)」）。
   右栏：选中客户详情——账号信息 + 角色摘要表（hero_name/world/turn/life_state）+ 充值权益表（sku/status）+
   实名/防沉迷合规态。读 GET /api/admin/clients/:id。
   底部三个**带二次确认**的危险操作（admin 级）：
   - 封禁/解封（切 banned）→ POST /api/admin/clients/:id/ban {banned}；
   - 按账户擦除数据（不可逆，强确认文案）→ POST /api/admin/clients/:id/erase；
   - 退款撤权益（可选填 sku_id）→ POST /api/admin/clients/:id/refund {sku_id?}（billing 关时 503）。
   每个操作各自 try/catch + 独立 toast + 操作后刷新；二次确认走 window.confirm，避免误点。 */

import { useCallback, useEffect, useState } from "react";
import {
  eraseClientData,
  errText,
  getClientDetail,
  listClients,
  refundClient,
  setClientBanned,
  type AdminClient,
  type ClientDetail,
} from "./adminApi";

export function ClientPanel(): JSX.Element {
  const [clients, setClients] = useState<AdminClient[]>([]);
  const [query, setQuery] = useState("");
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState("");
  const [ok, setOk] = useState("");
  // backendReady=false 表示读列表 503/失败（未配 ops 鉴权或后端不可用）：搜索/操作禁用并提示。
  const [backendReady, setBackendReady] = useState(true);

  // 选中客户与其详情。
  const [selectedId, setSelectedId] = useState<string>("");
  const [detail, setDetail] = useState<ClientDetail | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  // busy 记录正在执行中的危险操作种类（ban/erase/refund），避免重复点击。
  const [busy, setBusy] = useState<string>("");
  // refundSku 是退款时可选填的 SKU（留空=撤全部权益）。
  const [refundSku, setRefundSku] = useState("");

  // loadList 拉客户列表（按当前搜索词）。503/失败 → backendReady=false。
  const loadList = useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      const data = await listClients(query, 100);
      setClients(data);
      setBackendReady(true);
    } catch (e) {
      setBackendReady(false);
      setErr(`客户列表端点不可用：${errText(e)}（读端为 operator 级，需配 ops 鉴权(operator+)；后端未配鉴权时可能 503）`);
      setClients([]);
    } finally {
      setLoading(false);
    }
  }, [query]);

  useEffect(() => {
    void loadList();
    // 仅首挂载拉一次；后续靠搜索按钮/回车显式触发，避免每键一请求。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // loadDetail 拉某客户详情并设为选中。
  const loadDetail = useCallback(async (id: string) => {
    setSelectedId(id);
    setDetailLoading(true);
    setErr("");
    setRefundSku("");
    try {
      const data = await getClientDetail(id);
      setDetail(data);
    } catch (e) {
      setErr(`加载客户详情失败：${errText(e)}`);
      setDetail(null);
    } finally {
      setDetailLoading(false);
    }
  }, []);

  // toggleBan 封禁/解封当前选中客户（二次确认）。
  const toggleBan = useCallback(async () => {
    if (!backendReady || !detail) return;
    const target = !detail.account.banned;
    const verb = target ? "封禁" : "解封";
    if (!window.confirm(`确认${verb}账户「${detail.account.username || detail.account.id}」？`)) return;
    setBusy("ban");
    setErr("");
    setOk("");
    try {
      const banned = await setClientBanned(detail.account.id, target);
      setOk(`已${verb}账户 ${detail.account.username || detail.account.id}（banned=${banned}）。`);
      await loadDetail(detail.account.id);
      await loadList();
    } catch (e) {
      setErr(`${verb}失败：${errText(e)}`);
    } finally {
      setBusy("");
    }
  }, [backendReady, detail, loadDetail, loadList]);

  // eraseData 按账户不可逆擦除数据（双重二次确认，强调不可逆）。
  const eraseData = useCallback(async () => {
    if (!backendReady || !detail) return;
    const name = detail.account.username || detail.account.id;
    if (
      !window.confirm(
        `【不可逆】确认按账户擦除「${name}」的全部会话数据？此操作无法撤销，所有角色与进度将永久删除。`,
      )
    ) {
      return;
    }
    if (!window.confirm(`再次确认：永久擦除「${name}」的数据，且不可恢复。继续？`)) return;
    setBusy("erase");
    setErr("");
    setOk("");
    try {
      const erased = await eraseClientData(detail.account.id);
      setOk(`已按账户擦除 ${name} 的数据（共 ${erased} 个会话，不可逆）。`);
      await loadDetail(detail.account.id);
      await loadList();
    } catch (e) {
      setErr(`擦除失败：${errText(e)}`);
    } finally {
      setBusy("");
    }
  }, [backendReady, detail, loadDetail, loadList]);

  // doRefund 撤权益/退款（二次确认；refundSku 留空=撤全部）。billing 关时后端 503。
  const doRefund = useCallback(async () => {
    if (!backendReady || !detail) return;
    const name = detail.account.username || detail.account.id;
    const sku = refundSku.trim();
    const scope = sku === "" ? "全部权益" : `SKU「${sku}」`;
    if (!window.confirm(`确认撤销账户「${name}」的${scope}（退款）？`)) return;
    setBusy("refund");
    setErr("");
    setOk("");
    try {
      const revoked = await refundClient(detail.account.id, sku === "" ? undefined : sku);
      setOk(`已撤销 ${name} 的${scope}（共撤 ${revoked} 项权益）。`);
      await loadDetail(detail.account.id);
    } catch (e) {
      setErr(`退款/撤权益失败：${errText(e)}（billing 关闭时后端返 503）`);
    } finally {
      setBusy("");
    }
  }, [backendReady, detail, refundSku, loadDetail]);

  return (
    <div className="adm-card">
      <div className="adm-card-title">
        <span>客户管理</span>
        <button type="button" className="adm-btn" onClick={() => void loadList()} disabled={loading || !backendReady}>
          {loading ? "加载中…" : "刷新"}
        </button>
      </div>
      <p className="adm-card-sub">
        检索并处置客户账户：查看角色 / 充值权益 / 实名防沉迷态，封禁解封、按账户不可逆擦除、退款撤权益。
        读列表为 operator 级，封禁/擦除/退款为 admin 级——危险操作均需二次确认。
      </p>

      {err ? <div className="adm-toast adm-toast-err">{err}</div> : null}
      {ok ? <div className="adm-toast adm-toast-ok">{ok}</div> : null}

      {/* 搜索框 */}
      <div className="adm-row" style={{ marginTop: 8 }}>
        <input
          className="adm-input"
          type="text"
          style={{ flex: 1, minWidth: 200 }}
          value={query}
          disabled={!backendReady}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") void loadList();
          }}
          placeholder="搜索 username / display_name（模糊）或 id（精确）"
          aria-label="客户搜索"
        />
        <button
          type="button"
          className="adm-btn adm-btn-primary"
          onClick={() => void loadList()}
          disabled={loading || !backendReady}
        >
          搜索
        </button>
      </div>

      {/* 两栏：左列表 + 右详情 */}
      <div style={{ display: "flex", gap: 16, marginTop: 12, alignItems: "flex-start", flexWrap: "wrap" }}>
        {/* 左：客户列表 */}
        <div style={{ flex: "1 1 320px", minWidth: 280 }}>
          {clients.length === 0 && !loading ? (
            <div className="adm-empty">{backendReady ? "暂无匹配客户。" : "客户列表不可用（需配 ops 鉴权）。"}</div>
          ) : (
            <table className="adm-table">
              <thead>
                <tr>
                  <th>username</th>
                  <th>display_name</th>
                  <th>状态</th>
                  <th>created_at</th>
                </tr>
              </thead>
              <tbody>
                {clients.map((c) => (
                  <tr
                    key={c.id}
                    onClick={() => void loadDetail(c.id)}
                    style={{
                      cursor: "pointer",
                      background: c.id === selectedId ? "rgba(217, 188, 115, 0.12)" : undefined,
                    }}
                  >
                    <td>{c.username || "(无)"}</td>
                    <td>{c.display_name || "(无)"}</td>
                    <td>
                      <span className={`adm-badge ${c.banned ? "adm-badge-off" : "adm-badge-on"}`}>
                        {c.banned ? "已封禁" : "正常"}
                      </span>
                    </td>
                    <td>{c.created_at || "(未知)"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>

        {/* 右：选中客户详情 */}
        <div style={{ flex: "1 1 360px", minWidth: 320 }}>
          {!selectedId ? (
            <div className="adm-empty">从左侧列表选择一个客户查看详情。</div>
          ) : detailLoading ? (
            <div className="adm-empty">加载详情中…</div>
          ) : !detail ? (
            <div className="adm-empty">无法加载该客户详情。</div>
          ) : (
            <div>
              {/* 账号信息 */}
              <div className="adm-card-sub" style={{ fontWeight: 600, marginBottom: 6 }}>
                账号信息
              </div>
              <div className="adm-flag-row">
                <div className="adm-flag-main">
                  <div className="adm-flag-name">{detail.account.username || detail.account.id}</div>
                  <div className="adm-flag-desc">
                    id {detail.account.id} · 展示名 {detail.account.display_name || "(无)"} · 创建于{" "}
                    {detail.account.created_at || "(未知)"}
                  </div>
                </div>
                <div className="adm-flag-state">
                  <span className={`adm-badge ${detail.account.banned ? "adm-badge-off" : "adm-badge-on"}`}>
                    {detail.account.banned ? "已封禁" : "正常"}
                  </span>
                </div>
              </div>

              {/* 角色摘要 */}
              <div className="adm-card-sub" style={{ fontWeight: 600, marginTop: 14, marginBottom: 6 }}>
                角色摘要（{detail.characters.length}）
              </div>
              {detail.characters.length === 0 ? (
                <div className="adm-empty">该账户暂无角色。</div>
              ) : (
                <table className="adm-table">
                  <thead>
                    <tr>
                      <th>hero_name</th>
                      <th>world</th>
                      <th className="adm-num">turn</th>
                      <th>life_state</th>
                    </tr>
                  </thead>
                  <tbody>
                    {detail.characters.map((ch) => (
                      <tr key={ch.session_id}>
                        <td>{ch.hero_name || "(无名)"}</td>
                        <td>{ch.world_id || "(无)"}</td>
                        <td className="adm-num">{ch.turn}</td>
                        <td>{ch.life_state || "(未知)"}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}

              {/* 充值权益 */}
              <div className="adm-card-sub" style={{ fontWeight: 600, marginTop: 14, marginBottom: 6 }}>
                充值权益（{detail.entitlements.length}）
              </div>
              {detail.entitlements.length === 0 ? (
                <div className="adm-empty">该账户暂无权益。</div>
              ) : (
                <table className="adm-table">
                  <thead>
                    <tr>
                      <th>sku_id</th>
                      <th>status</th>
                      <th>granted_at</th>
                      <th>expires_at</th>
                    </tr>
                  </thead>
                  <tbody>
                    {detail.entitlements.map((en, i) => (
                      <tr key={`${en.sku_id}-${i}`}>
                        <td>{en.sku_id || "(无)"}</td>
                        <td>
                          <span className="adm-badge adm-badge-on">{en.status || "(未知)"}</span>
                        </td>
                        <td>{en.granted_at || "(无)"}</td>
                        <td>{en.expires_at || "(无)"}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}

              {/* 实名 / 防沉迷 */}
              <div className="adm-card-sub" style={{ fontWeight: 600, marginTop: 14, marginBottom: 6 }}>
                实名 / 防沉迷
              </div>
              {!detail.compliance ? (
                <div className="adm-empty">无合规数据。</div>
              ) : (
                <div className="adm-flag-row">
                  <div className="adm-flag-main">
                    <div className="adm-flag-desc">
                      出生日期 {detail.compliance.birth_date || "(无)"} · 当日时段 {detail.compliance.day_bucket || "(无)"} ·
                      当日已玩 {detail.compliance.daily_play_seconds}s
                    </div>
                  </div>
                  <div className="adm-flag-state">
                    <span
                      className={`adm-badge ${detail.compliance.realname_verified ? "adm-badge-on" : "adm-badge-off"}`}
                    >
                      {detail.compliance.realname_verified ? "已实名" : "未实名"}
                    </span>
                    <span className={`adm-badge ${detail.compliance.minor_mode ? "adm-badge-override" : "adm-badge-env"}`}>
                      {detail.compliance.minor_mode ? "未成年模式" : "成年"}
                    </span>
                  </div>
                </div>
              )}

              {/* 危险操作区 */}
              <div className="adm-card-sub" style={{ fontWeight: 600, marginTop: 18, marginBottom: 6, color: "#f0b0a6" }}>
                危险操作（均需二次确认）
              </div>
              <div className="adm-row" style={{ alignItems: "flex-end", gap: 10 }}>
                <button
                  type="button"
                  className="adm-btn adm-btn-danger"
                  disabled={!backendReady || busy !== ""}
                  onClick={() => void toggleBan()}
                  title="封禁 / 解封该账户"
                >
                  {busy === "ban" ? "处理中…" : detail.account.banned ? "解封账户" : "封禁账户"}
                </button>
                <button
                  type="button"
                  className="adm-btn adm-btn-danger"
                  disabled={!backendReady || busy !== ""}
                  onClick={() => void eraseData()}
                  title="按账户不可逆擦除全部会话数据"
                >
                  {busy === "erase" ? "擦除中…" : "按账户擦除数据（不可逆）"}
                </button>
              </div>
              <div className="adm-row" style={{ alignItems: "flex-end", gap: 8, marginTop: 10 }}>
                <input
                  className="adm-input"
                  type="text"
                  style={{ width: 200, padding: "5px 8px", fontSize: 12 }}
                  value={refundSku}
                  disabled={!backendReady || busy !== ""}
                  onChange={(e) => setRefundSku(e.target.value)}
                  placeholder="sku_id（留空=撤全部权益）"
                  aria-label="退款 SKU"
                />
                <button
                  type="button"
                  className="adm-btn adm-btn-danger"
                  disabled={!backendReady || busy !== ""}
                  onClick={() => void doRefund()}
                  title="撤销权益 / 退款（billing 关闭时 503）"
                >
                  {busy === "refund" ? "处理中…" : "退款 / 撤权益"}
                </button>
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

export default ClientPanel;
