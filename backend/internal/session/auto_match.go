package session

// 文件说明：撮合自动扫描（设计 docs/事件耦合与跨玩家关联.md §2.2）。把「撮合」从只有 ops HTTP 手动触发、候选要手填，
// 扶正为部署边界的低频自动扫描：在同一世界/会话内挑符合条件的角色作候选，用**确定性启发式**算 MatchScore 四因子
// （地理临近/钩子契合/关系交集/密度调节，各 [0,1]），交给现有 MatchIntoSocialObject 打分→过门→arbitration 择 slots 人。
//
// 本轮升级（事件耦合 §2.2 落地，对齐 spine 已补的 social_objects.region_id/severity/expires_at 三列）：
//   ① 落 region_id（候选群主导 region，地理就近择人）/severity（候选规模+关系密度派生，定 consent 档）/expires_at（过期回收窗口）；
//      MatchScore 的地理近项改由 region_id 同区度驱动（同主导 region 得满分、异区随质心距衰减）。
//   ② 每角色每日新绑定**跨玩家社会客体 ≤ autoMatchDailyBindCap**（确定性日窗计数 SOCIAL_OBJECT_BIND，仿 fate.go 日配额），
//      超额者本周期不再入候选——防大 R 垄断社交、反洪泛。
//   ③ NPC 兜底：撮到的真人不足 autoMatchBackfillFloor 时，由后台确定性 NPC 占位补齐（玩家分不出对方是 NPC 还是另一个玩家），
//      不强求玩家相遇（设计 §2.2「撮不到由后台 NPC 社会客体兜底」）。
//   ④ 过期回收：expires_at 到点的 active 客体在边界扫描时 status→expired + 留痕 ANCHOR_DECAYED（牵挂渐淡），best-effort。
//
// 纪律：flag-gated（QUNXIANG_AUTO_MATCH 默认关 → 零行为变化、零 DB 写）、低频（turn 取模确定性触发）、best-effort（吞错，绝不中断推进）。
// 四因子 + region 选取 + severity + 日窗计数 + NPC 占位 id 全部确定性（sessionID+turn+unit 的 FNV / 关系四轴 / 锚密度派生），
// 无全局 rand，保证可复现。region_id/severity/expires_at 三列经原生 SQL 在 Create 之后回填（socialobject.Create 只写基础列，
// 三列归本撮合层按情境算 → 不改 socialobject 包）。

import (
	"context"
	"database/sql"
	"hash/fnv"
	"math"
	"strconv"
	"strings"
	"time"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/runtimeconfig"
	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
)

const (
	// autoMatchEveryNTurns 撮合扫描的部署回合周期：每 N 个部署回合扫一次（低频，避免每边界都撮合扰动）。
	autoMatchEveryNTurns = 4
	// autoMatchSlots 单次撮合绑入社会客体的名额上限（一支小队/一个结盟的规模）。
	autoMatchSlots = 4
	// autoMatchMaxCandidates 单次扫描最多取的候选数（控算量；按确定性强度截断）。
	autoMatchMaxCandidates = 12
	// autoMatchKind / autoMatchLabel 自动撮合产出的社会客体类型与标签（party=临时同行小队）。
	autoMatchKind  = "party"
	autoMatchLabel = "野外同行"

	// autoMatchDailyBindCap 单角色每自然日新绑定跨玩家社会客体上限（设计 §2.2「每角色每日新绑定 ≤2」，反大 R 垄断社交）。
	// 用确定性日窗计数本角色当天的 SOCIAL_OBJECT_BIND 留痕实现，超额者本周期不再入候选。
	autoMatchDailyBindCap = 2

	// autoMatchBackfillFloor 撮合后真人成员的最低规模：真人不足此数时由 NPC 占位补齐到 autoMatchSlots（兜底，不强求玩家相遇）。
	// 取 2 → 至少凑成「两人成局」的最小社会客体；真人 ≥2 则不补 NPC（让真人优先）。
	autoMatchBackfillFloor = 2

	// autoMatchTTLDays 社会客体存活窗口（天）：超此未续期则被过期回收（status→expired）。野外同行是临时性客体，给较短窗口。
	autoMatchTTLDays = 3

	// autoMatchExpirySweepLimit 单次边界过期回收扫描的客体上限（控算量，按到期早的优先）。
	autoMatchExpirySweepLimit = 32

	// autoMatchTimeLayout 与 socialobject.nowTimestamp 同格式（定宽 UTC，字典序==时间序，双驱动一致），用于 expires_at 比对/写入。
	autoMatchTimeLayout = "2006-01-02 15:04:05"
)

// autoMatchEnabled 读 QUNXIANG_AUTO_MATCH（true/1/yes/on 视为开），默认关 → scanAndMatch 整方法 no-op、零行为变化、零 DB 写。
func autoMatchEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(featureflags.EnvOrOverride("QUNXIANG_AUTO_MATCH"))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// scanAndMatch 在部署边界低频自动撮合：先回收过期客体，再挑本局玩家阵营的存活角色作候选、确定性算四因子、
// 调 MatchIntoSocialObject 绑社会客体并回填 region_id/severity/expires_at。
// 守卫：flag 关 / WorldID 空（撮合需世界域，MatchIntoSocialObject 要求非空 worldID）/ 候选不足两人 / 未到周期 → no-op。
// 全程 best-effort：任何错误只吞掉，绝不影响阶段推进。
func (service *Service) scanAndMatch(ctx context.Context, state *State, units []unit.Record) {
	if service == nil || service.db == nil || state == nil {
		return
	}
	if !autoMatchEnabled() {
		return // 默认关：零行为变化
	}
	worldID := state.WorldID
	if worldID == "" {
		return // 未接入多世界：社会客体撮合无世界域可绑，跳过
	}
	// 低频触发：每 N 个部署回合扫一次（确定性，turn 取模）。读一次存局部，同函数内 epoch 计算复用同值。
	everyNTurns := runtimeconfig.GetInt("social.auto_match_every_n_turns")
	if everyNTurns <= 0 || state.TurnState.Turn%everyNTurns != 0 {
		return
	}

	// ④ 过期回收：每个撮合边界先把到期 active 客体翻成 expired 并留痕（best-effort，不阻断后续撮合）。
	service.reclaimExpiredSocialObjects(ctx, worldID)

	candidates := service.buildMatchCandidates(ctx, state, units)
	if len(candidates) < 2 {
		return // 不足两人，撮不成社会客体
	}
	if len(candidates) > autoMatchMaxCandidates {
		candidates = candidates[:autoMatchMaxCandidates]
	}

	// 主导 region（候选群的 region 众数）：作社会客体的地理锚，并供 region 同区度的地理近项参照。
	dominantRegion := service.dominantRegionOf(ctx, candidates)

	// label 带 turn，使不同周期产出不同确定性社会客体 id（同周期重撮合幂等更新同一客体）。
	// epoch 同时作 NPC backfill 的确定性 seed：撮合节奏（social.auto_match_every_n_turns）经 GM 改后即时生效、
	// 不回溯历史 epoch——旧客体仍按其生成时的 epoch 留痕，新周期按新节奏划窗，无需迁移既有社会客体。
	epoch := state.TurnState.Turn / everyNTurns
	label := autoMatchLabel + "·" + strconv.Itoa(epoch)
	objID, chosen, err := service.MatchIntoSocialObject(ctx, worldID, autoMatchKind, label, candidates, autoMatchSlots)
	if err != nil || objID == "" {
		return // 撮合失败/无人达标：best-effort，不阻断推进
	}

	// ② 日配额已在 buildMatchCandidates 滤掉超额者；这里把当周期实际绑成的真人记数，由 SOCIAL_OBJECT_BIND 留痕自然计入。
	//    （MatchIntoSocialObject 内部已对每个 chosen 落一条 SOCIAL_OBJECT_BIND，日窗计数据此累计。）

	// ③ NPC 兜底：真人不足 floor 则补 NPC 占位到 slots（确定性 NPC id，玩家分不出），不强求玩家相遇。
	chosen = service.backfillWithNPC(ctx, objID, worldID, epoch, chosen, autoMatchSlots)

	// ① 回填 region_id/severity/expires_at（severity 由候选规模+关系密度派生 → 定 consent 档；expires_at=now+TTL 供过期回收）。
	severity := autoMatchSeverity(len(chosen), candidates)
	service.stampSocialObjectColumns(ctx, objID, dominantRegion, severity, autoMatchExpiresAt())
}

// buildMatchCandidates 把本局玩家阵营、且当日未超日配额的存活角色构造成带四因子的撮合候选。
// 四因子全部确定性：地理临近=region 同区度（同主导 region 满分，否则按质心 hex 距衰减）；钩子契合=pair-stable 的 FNV 哈希；
// 关系交集=该角色对其余候选的现有四轴关系强度；密度调节=锚密度反向（锚越少越易被撮合，反垄断）。
func (service *Service) buildMatchCandidates(ctx context.Context, state *State, units []unit.Record) []MatchCandidate {
	// 先筛出本局玩家阵营、存活、非战斗、且当日新绑定未达上限的角色作为候选池。
	pool := make([]unit.Record, 0, len(units))
	for i := range units {
		u := units[i]
		if state.PlayerFactionID != "" && u.FactionID != state.PlayerFactionID {
			continue
		}
		if u.Status.LifeState == unit.LifeStateDead || u.Status.LivesRemaining <= 0 {
			continue
		}
		// ② 每日 ≤ autoMatchDailyBindCap 冷却：当日已绑满者本周期不再入候选（确定性日窗计数）。
		if service.dailyBindExhausted(ctx, u.ID) {
			continue
		}
		pool = append(pool, u)
	}
	if len(pool) < 2 {
		return nil
	}

	// region 同区度需要先知道候选群的主导 region；同时算质心作异区时的连续衰减回退。
	regionByUnit := service.regionsOf(ctx, pool)
	dominantRegion := modalRegion(regionByUnit)

	// 地理临近回退：以候选群质心为参照，离质心越近越「地理临近」（缺 region 时的连续近似）。
	var sumQ, sumR float64
	for i := range pool {
		sumQ += float64(pool[i].Status.PositionQ)
		sumR += float64(pool[i].Status.PositionR)
	}
	centroidQ := sumQ / float64(len(pool))
	centroidR := sumR / float64(len(pool))

	candidates := make([]MatchCandidate, 0, len(pool))
	for i := range pool {
		u := pool[i]
		candidates = append(candidates, MatchCandidate{
			UnitID:            u.ID,
			GeoNear:           geoNearByRegion(u, regionByUnit[u.ID], dominantRegion, centroidQ, centroidR),
			HookFit:           hookFitFor(state.ID, state.TurnState.Turn, u.ID),
			RelationIntersect: service.relationIntersectFor(ctx, u.ID, pool),
			DensityAdj:        densityAdjFor(service.AnchorDensity(ctx, u.ID)),
		})
	}
	return candidates
}

// geoNearByRegion 地理近项：候选与候选群主导 region 同区 → 满分 1.0（同一片地方的人最易同行）；
// 异区或 region 缺失 → 退回到与质心的 hex 距离指数衰减（连续近似，与升级前行为兼容）。
func geoNearByRegion(u unit.Record, regionID, dominantRegion string, centroidQ, centroidR float64) float64 {
	if dominantRegion != "" && regionID != "" && regionID == dominantRegion {
		return 1.0
	}
	dq := float64(u.Status.PositionQ) - centroidQ
	dr := float64(u.Status.PositionR) - centroidR
	// 轴向坐标系的 hex 距离（连续近似）：(|dq|+|dr|+|dq+dr|)/2。
	dist := (math.Abs(dq) + math.Abs(dr) + math.Abs(dq+dr)) / 2
	return math.Exp(-dist / 6.0) // 距离 6 格处≈0.37，邻近高、远处低
}

// hookFitFor 钩子契合：用 sessionID+turn+unit 的 FNV 派生一个稳定的 [0,1]「叙事钩子」契合度（无全局 rand，可复现）。
func hookFitFor(sessionID string, turn int, unitID string) float64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("match_hook:" + sessionID + ":" + strconv.Itoa(turn) + ":" + unitID))
	return float64(h.Sum64()%10000) / 10000.0
}

// relationIntersectFor 关系交集：该角色对候选池其余成员的现有四轴关系平均强度（归一 [0,1]）。
// 关系越多越强 → 越可能被撮进同一社会客体（已有羁绊的人更易同行）。四轴绝对值取均，clamp 后归一。
func (service *Service) relationIntersectFor(ctx context.Context, unitID string, pool []unit.Record) float64 {
	relations := service.loadOutgoingRelationMap(ctx, unitID)
	if len(relations) == 0 {
		return 0
	}
	var sum float64
	var n int
	for i := range pool {
		other := pool[i].ID
		if other == unitID {
			continue
		}
		row, ok := relations[other]
		if !ok {
			continue
		}
		// 四轴各 clamp 到 [-10,10]，取绝对值之和 / 40 归一到 [0,1]（满轴=10×4=40）。
		strength := (math.Abs(row.Trust) + math.Abs(row.Fear) + math.Abs(row.Affection) + math.Abs(row.Rivalry)) / 40.0
		if strength > 1 {
			strength = 1
		}
		sum += strength
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// densityAdjFor 密度调节：锚密度的反向 [0,1]——「在乎的事」越少（社交越空）越容易被撮合，抑制大 R/重度玩家垄断社交。
func densityAdjFor(anchorDensity float64) float64 {
	v := 1 - anchorDensity
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ===== ② 每日 ≤ autoMatchDailyBindCap 冷却（确定性日窗计数，仿 fate.go 的 pendingBudgetExhausted）=====

// dailyBindExhausted 判断某角色当天（UTC 自然日）新绑定的跨玩家社会客体是否已达 autoMatchDailyBindCap。
// 计数本角色当日的 SOCIAL_OBJECT_BIND 流程事件（MatchIntoSocialObject 每绑一名成员落一条，OwnerUnitID=被绑者）。
// occurred_at 以 RFC3339Nano（UTC）写入，用 [dayStart, nextDay) 字符串区间过滤——双驱动安全、不依赖 SQL 日期函数、确定性。
// best-effort：查错保守返回 true（满额 → 本周期不再给她新绑定，宁缺毋滥不轰炸；与入向探针硬上限同向）。
func (service *Service) dailyBindExhausted(ctx context.Context, unitID string) bool {
	if service == nil || service.db == nil || unitID == "" {
		return true
	}
	now := time.Now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	lo := dayStart.Format(time.RFC3339Nano)
	hi := dayStart.Add(24 * time.Hour).Format(time.RFC3339Nano)
	var count int
	if err := service.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM events
		 WHERE actor_unit_id = ? AND reason_code = ? AND occurred_at >= ? AND occurred_at < ?`,
		unitID, string(events.ReasonSocialObjectBind), lo, hi,
	).Scan(&count); err != nil {
		return true
	}
	return count >= runtimeconfig.GetInt("social.auto_match_daily_bind_cap")
}

// ===== ① region 就近 + severity 定档 + expires_at 回填（原生 SQL，socialobject.Create 不写这三列）=====

// regionsOf 批量读候选的 region_id（units 表去规范化列，不在 Record blob 内）。空 region 不入 map。best-effort：查错跳过。
func (service *Service) regionsOf(ctx context.Context, pool []unit.Record) map[string]string {
	out := make(map[string]string, len(pool))
	if service == nil || service.db == nil {
		return out
	}
	for i := range pool {
		out[pool[i].ID] = service.regionOf(ctx, pool[i].ID)
	}
	return out
}

// regionOf 读单个单位的 region_id（去规范化列）。缺/查错 → 空串。
func (service *Service) regionOf(ctx context.Context, unitID string) string {
	if service == nil || service.db == nil || unitID == "" {
		return ""
	}
	var region sql.NullString
	if err := service.db.QueryRowContext(ctx, `SELECT region_id FROM units WHERE id = ?`, unitID).Scan(&region); err != nil {
		return ""
	}
	return strings.TrimSpace(region.String)
}

// dominantRegionOf 取候选群的主导 region（众数）：撮合候选已是 MatchCandidate（只有 UnitID），故按 UnitID 逐个读 region 再取众数。
func (service *Service) dominantRegionOf(ctx context.Context, candidates []MatchCandidate) string {
	byUnit := make(map[string]string, len(candidates))
	for _, c := range candidates {
		byUnit[c.UnitID] = service.regionOf(ctx, c.UnitID)
	}
	return modalRegion(byUnit)
}

// modalRegion 返回 region→unit 映射中出现最多的非空 region（众数）；并列时按字典序最小（确定性）。全空则空串。
func modalRegion(regionByUnit map[string]string) string {
	counts := make(map[string]int)
	for _, r := range regionByUnit {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		counts[r]++
	}
	best := ""
	bestN := 0
	for r, n := range counts {
		if n > bestN || (n == bestN && (best == "" || r < best)) {
			best, bestN = r, n
		}
	}
	return best
}

// autoMatchSeverity 由「实际成员规模 + 候选群平均关系密度」派生社会客体严重度（[0,3]，对齐后果分级闸/ConsentTierFor 三档）。
// 规模越大、羁绊越深 → 越「重」（更高 consent 档）。纯确定性，无 rand：成员 ≥4 或高关系密度 → 3（不可逆类）；中等 → 2；其余 1。
func autoMatchSeverity(memberCount int, candidates []MatchCandidate) int {
	var relSum float64
	for _, c := range candidates {
		relSum += c.RelationIntersect
	}
	avgRel := 0.0
	if len(candidates) > 0 {
		avgRel = relSum / float64(len(candidates))
	}
	switch {
	case memberCount >= 4 || avgRel >= 0.6:
		return 3 // 大规模/深羁绊：层3（不可逆类，REQUIRES_CONSENT）
	case memberCount >= 3 || avgRel >= 0.3:
		return 2 // 中等：层2（高代价，CONTESTED）
	default:
		return 1 // 小规模浅羁绊：层1（可恢复，UNILATERAL）
	}
}

// ConsentTierForSocialObject 把社会客体 severity 映射为知情权档（复用 relevance.ConsentTierFor 的后果层语义）。
// 供下游（consent_gate / 跨玩家交互）按 severity 决定撮合是否需对方角色自治同意；本撮合层只负责把 severity 落库定档。
func ConsentTierForSocialObject(severity int) relevance.ConsentTier {
	return relevance.ConsentTierFor(severity)
}

// autoMatchExpiresAt 计算过期时间戳（now + TTL，定宽 UTC，与 socialobject 时间列同格式，字典序==时间序便于区间比对）。
func autoMatchExpiresAt() string {
	return time.Now().UTC().Add(autoMatchTTLDays * 24 * time.Hour).Format(autoMatchTimeLayout)
}

// stampSocialObjectColumns 把 region_id/severity/expires_at 三列回填到已建社会客体（socialobject.Create 只写基础列，
// 这三列归撮合层按情境算 → 不改 socialobject 包）。best-effort：查错只吞掉，绝不阻断撮合。双驱动通用（纯 UPDATE，无方言差异）。
func (service *Service) stampSocialObjectColumns(ctx context.Context, objectID, regionID string, severity int, expiresAt string) {
	if service == nil || service.db == nil || objectID == "" {
		return
	}
	// region_id/expires_at 空串先归一为「真空串」再交 nullableStr（包内既有 helper，空→NULL，与可空列语义一致）。
	_, _ = service.db.ExecContext(
		ctx,
		`UPDATE social_objects SET region_id = ?, severity = ?, expires_at = ? WHERE id = ?`,
		nullableStr(strings.TrimSpace(regionID)), severity, nullableStr(strings.TrimSpace(expiresAt)), objectID,
	)
}

// ===== ③ NPC 兜底（撮不齐真人时由后台 NPC 占位补齐，玩家分不出）=====

// backfillWithNPC 当真人成员不足 autoMatchBackfillFloor 时，用确定性 NPC 占位 id 把成员补齐到 slots（设计 §2.2「撮不到由 NPC 兜底」）。
// NPC id 由 (objectID, epoch, 序号) 派生 → 确定性、可复现、玩家分不出对方是 NPC 还是另一个玩家。
// 真人 ≥ floor 则不补（让真人优先，NPC 只在「凑不齐一局」时兜底，不强求玩家相遇）。best-effort：绑定失败只吞错。
// 返回补齐后的完整成员 id 列表（真人在前、NPC 在后）。
func (service *Service) backfillWithNPC(ctx context.Context, objectID, worldID string, epoch int, realMembers []string, slots int) []string {
	if service == nil || service.db == nil || objectID == "" {
		return realMembers
	}
	if len(realMembers) >= autoMatchBackfillFloor || len(realMembers) >= slots {
		return realMembers // 真人够数：不补 NPC
	}
	out := make([]string, len(realMembers))
	copy(out, realMembers)
	idx := 0
	for len(out) < slots && len(out) < autoMatchBackfillFloor {
		npcID := npcBackfillID(objectID, epoch, idx)
		idx++
		// NPC 占位成员：score 取保守 NPC 兜底分（低于真人撮合分，叙事上「凑数的过路人」），best-effort 绑定 + 留痕。
		if !service.bindBackfillMember(ctx, objectID, worldID, npcID) {
			continue
		}
		out = append(out, npcID)
	}
	return out
}

// bindBackfillMember 把一名 NPC 占位成员绑进社会客体并留痕（best-effort）。直接走原生 INSERT（socialobject 包的 AddMember
// 也可，但 NPC id 非真实 units、为避免与真人成员留痕口径混淆，这里独立落 member 行 + SOCIAL_OBJECT_BIND 留痕但 OwnerUnitID
// 用 NPC id——NPC 不进 events 的 units FK 校验路径，故 RelatedUnitID 留空回落 owner 仅作叙事锚，不触发 FK）。
// 注意：NPC 的 SOCIAL_OBJECT_BIND **不计入真人日配额**（dailyBindExhausted 只按真实 actor_unit_id 计数，NPC id 不是任何玩家角色）。
func (service *Service) bindBackfillMember(ctx context.Context, objectID, worldID, npcID string) bool {
	if service == nil || service.db == nil || objectID == "" || npcID == "" {
		return false
	}
	// 函数内读一次存局部：下方 INSERT/UPDATE 与留痕 payload 共用同一撮合分，避免多次 RLock。
	npcScore := runtimeconfig.GetFloat("social.auto_match_npc_score")
	now := time.Now().UTC().Format(autoMatchTimeLayout)
	// 幂等绑定：按驱动选 ON DUPLICATE（MySQL）/ON CONFLICT（SQLite），与 socialobject.AddMember 同口径，避免方言语法错。
	if dbdialect.IsMySQL(service.db) {
		if _, err := service.db.ExecContext(
			ctx,
			`INSERT INTO social_object_members (object_id, unit_id, score, joined_at) VALUES (?,?,?,?)
			 ON DUPLICATE KEY UPDATE score=VALUES(score)`,
			objectID, npcID, npcScore, now,
		); err != nil {
			return false
		}
	} else if _, err := service.db.ExecContext(
		ctx,
		`INSERT INTO social_object_members (object_id, unit_id, score, joined_at) VALUES (?,?,?,?)
		 ON CONFLICT(object_id, unit_id) DO UPDATE SET score=excluded.score`,
		objectID, npcID, npcScore, now,
	); err != nil {
		return false
	}
	// 留痕：NPC 兜底入局（与真人 SOCIAL_OBJECT_BIND 同 code，玩家分不出；payload 标 backfill=true 供审计区分）。
	_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID: worldID, OwnerUnitID: npcID,
		Code: events.ReasonSocialObjectBind, Category: events.CategoryFate,
		Payload: map[string]any{"object_id": objectID, "kind": autoMatchKind, "backfill": true, "score": npcScore},
		WorldID: worldID,
	})
	return true
}

// NPC 占位成员的撮合分（保守低分，不抢真人名次）现由 runtimeconfig "social.auto_match_npc_score" 提供
// （默认 0.30 在 catalog 注册），读取站点见上方 bindBackfillMember；此处原 const 已迁出、无残留引用。

// npcBackfillID 由 (objectID, epoch, 序号) 派生确定性 NPC 占位 id（可复现、玩家分不出）。前缀 npc_so_ 便于审计识别。
func npcBackfillID(objectID string, epoch, idx int) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte("npc_backfill:" + objectID + ":" + strconv.Itoa(epoch) + ":" + strconv.Itoa(idx)))
	return "npc_so_" + strconv.FormatUint(h.Sum64(), 16)
}

// ===== ④ 过期回收（expires_at 到点 → status=expired + 留痕 ANCHOR_DECAYED）=====

// reclaimExpiredSocialObjects 把某世界内 expires_at 已过且仍 active 的社会客体翻成 expired，并对其成员落 ANCHOR_DECAYED 留痕。
// 时间比对用定宽 UTC 串（字典序==时间序，与 expires_at 写入格式一致）——双驱动安全、不依赖 SQL 日期函数、确定性。
// best-effort：任何一步失败只吞错/跳过，绝不阻断撮合或推进。
func (service *Service) reclaimExpiredSocialObjects(ctx context.Context, worldID string) {
	if service == nil || service.db == nil || worldID == "" {
		return
	}
	nowStr := time.Now().UTC().Format(autoMatchTimeLayout)
	// 先查到期客体（限世界域 + active + expires_at 非空且 < now），按到期早优先、上限截断。
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT id FROM social_objects
		 WHERE world_id = ? AND status = 'active' AND expires_at IS NOT NULL AND expires_at != '' AND expires_at < ?
		 ORDER BY expires_at ASC, id ASC LIMIT ?`,
		worldID, nowStr, autoMatchExpirySweepLimit,
	)
	if err != nil {
		return
	}
	expired := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			break
		}
		expired = append(expired, id)
	}
	rows.Close()

	for _, objID := range expired {
		// 翻 status → expired（幂等：只翻 active 的，并发下重复翻无副作用）。
		if _, err := service.db.ExecContext(
			ctx,
			`UPDATE social_objects SET status = 'expired' WHERE id = ? AND status = 'active'`,
			objID,
		); err != nil {
			continue
		}
		// 对每名成员落「牵挂渐淡」留痕（ANCHOR_DECAYED）：这段同行/羁绊随时间淡去。best-effort。
		members, mErr := service.listSocialObjectMembers(ctx, objID)
		if mErr != nil {
			continue
		}
		for _, m := range members {
			if strings.HasPrefix(m, "npc_so_") {
				continue // NPC 占位成员不进任何玩家收件箱/留痕（它不是任何玩家的角色）
			}
			_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
				SessionID: worldID, OwnerUnitID: m,
				Code: events.ReasonAnchorDecayed, Category: events.CategoryFate,
				Payload: map[string]any{"object_id": objID, "kind": autoMatchKind, "reason": "social_object_expired"},
				WorldID: worldID,
			})
		}
	}
}

// listSocialObjectMembers 读某社会客体的成员 unit_id 列表（过期回收留痕用）。best-effort：查错返回已收集到的。
func (service *Service) listSocialObjectMembers(ctx context.Context, objectID string) ([]string, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT unit_id FROM social_object_members WHERE object_id = ? ORDER BY unit_id ASC`, objectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return out, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
