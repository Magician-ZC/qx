package session

// 文件说明：道德漂移结算（阵营开放世界 F2，设计「道德漂移 + 自治偏置 + 概率切换阵营」之①）。
//
// 角色在自治生活中，其数值道德轴（unit.Record.MoralAlignment，三维 {Freedom,Order,Chaos}，各 [0,100]）
// 据**本回合该角色的道德效价信号**确定性小步累积漂移——体现「经历会沉淀成道德取向」而非一夜剧变：
//   - 抗命/自行其是 / 独立追目标 → +Freedom；
//   - 服从指令/尽责 / 结盟守诺/救援 → +Order；
//   - 战斗杀伤 / 背叛毁约（CROSS_BETRAYAL）→ +Chaos。
// 每信号一小步（±moralDriftStep 内，2~5 量级），同回合多信号叠加；各维写回前 clamp[0,100]。
//
// 三条硬约束（与 personality_drift.go 同纪律，均有单测守护）：
//   1. 步长封顶：每信号·单维变化 ∈ (0, moralDriftStepCap]（5），同回合各信号在同维上叠加但每维总量再夹 moralDriftPerTurnCap（10）。
//   2. 确定性：每信号的步长仅由 sessionID+turn+actor+维名+reasonCode 的 FNV-64a 哈希派生（禁用全局 math/rand），
//      同输入必同输出、可复放（与 personality_drift / combat_roll 同纪律）。方向由信号语义（轴 + 符号）钦定，不随机。
//   3. 留痕不直改受保护字段：MoralAlignment 是非保护字段（不在 HP/Hunger/Morale/Loyalty/LivesRemaining/Mood 之列，
//      statuslint 不拦），但每次漂移必经标准事件留痕（reason=MORAL_DRIFT，经 EmitProcessEvent 旁路，不走 status.Mutator）。
//
// 默认开（无 flag）：漂移只改非保护道德轴 + 留痕，不切阵营（切换另由 faction_switch.go 的 flag 门控），安全。
// best-effort：缺依赖 / 单位读不到 / 本回合无信号 → 优雅返回（漂移不影响主循环；调用方以 _ 忽略错误即可）。

import (
	"context"
	"fmt"
	"math"
	"strings"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/faction"
	"qunxiang/backend/internal/unit"
)

const (
	// moralDriftStepCap 单个道德信号、单维度允许的最大步长（绝对值）。落在「每信号小步 ±2~5」的上界。
	moralDriftStepCap = 5.0
	// moralDriftStepFloor 单个道德信号、单维度的最小步长（绝对值）——保证一个信号至少推动 2 点，不至于哈希派生出近 0 的无效漂移。
	moralDriftStepFloor = 2.0
	// moralDriftPerTurnCap 单回合·单维度允许的累计漂移上限（绝对值）——同回合多信号在同维叠加后再夹此上限，防一回合暴涨。
	moralDriftPerTurnCap = 10.0
)

// moralAxis 标识道德三轴之一（喂哈希 + 写回 + 留痕），与 faction.MoralAlignment 三分量一一对应。
type moralAxis string

const (
	moralAxisFreedom moralAxis = "freedom"
	moralAxisOrder   moralAxis = "order"
	moralAxisChaos   moralAxis = "chaos"
)

// moralSignal 是一条「本回合该角色的道德效价信号」：朝哪个轴推、推的相对强度（[0,1]，缩放步长）。
// 方向（轴）由信号语义钦定、不随机；强度只缩放步长幅度（步长仍被哈希派生 + 双上限约束）。
type moralSignal struct {
	Axis     moralAxis // 推向哪个道德轴
	Strength float64   // 相对强度 [0,1]——1=满步长，越小步长越小（重大背叛比寻常杀伤更猛）
	Reason   string    // 触发该信号的 reason code（喂哈希 + 留痕，便于复盘「为什么变」）
}

// moralSignalsForReasonCode 把一个 reason code 归类成它携带的道德效价信号（可能零条/多条）。
//
// 纯函数、确定性：仅依赖 reasonCode 字符串查表，无随机、无外部状态。设计映射（信号分类表）：
//   - 抗命/自行其是（defiance/OOC 拒绝）→ +Freedom（她在自行其是、不受束缚）。
//   - 服从尽责 / 受嘱参战 → +Order（她守规矩、尽其分）。
//   - 战斗杀伤（击倒/残伤敌手）→ +Chaos（暴力破局）。
//   - 结盟守诺 / 救援同伴 → +Order（守诺护众）。
//   - 背叛毁约（CROSS_BETRAYAL / RELATION_BETRAYAL 中她是施害方）→ +Chaos（破而后立、视秩序为枷锁）。
//   - 独立追目标（目标重估 / 野心流转）→ +Freedom（各凭本心、独立逐求）。
//
// 注意：本表只认「该角色作为 actor 主动发出/承受」的语义；调用方按 actor_unit_id 过滤本回合事件后喂入，
// 故「遭背叛/被救」这类**承受**信号不在此放大道德轴（道德是她自己的选择沉淀，不由别人对她做了什么定义）。
func moralSignalsForReasonCode(reasonCode events.ReasonCode) []moralSignal {
	rc := string(reasonCode)
	switch reasonCode {
	// —— +Freedom：自行其是 / 独立追目标 ——
	case events.ReasonOOCRejected:
		// 动机被拦回退（她本想自行其是，被拦下）——仍记一小步自由倾向（她有过自主冲动）。
		return []moralSignal{{Axis: moralAxisFreedom, Strength: 0.4, Reason: rc}}
	case events.ReasonGoalReassess, events.ReasonAmbitionShift:
		// 独立重估目标 / 野心流转——她在按自己的图景逐求，偏自由。
		return []moralSignal{{Axis: moralAxisFreedom, Strength: 0.5, Reason: rc}}
	case events.ReasonThreatJoinAuto:
		// 自己拿主意迎战（毅然，非受嘱）——自主 + 暴力将临，偏自由（主动）。
		return []moralSignal{{Axis: moralAxisFreedom, Strength: 0.5, Reason: rc}}

	// —— +Order：服从尽责 / 守诺护众 / 受嘱 ——
	case events.ReasonThreatJoinAdvise:
		// 听你的话迎战（受嘱、尽责）——偏秩序。
		return []moralSignal{{Axis: moralAxisOrder, Strength: 0.5, Reason: rc}}
	case events.ReasonCharterActivated:
		// 依你立的章程自行其是（受你授权、尽责履约）——偏秩序。
		return []moralSignal{{Axis: moralAxisOrder, Strength: 0.3, Reason: rc}}

	// —— +Chaos：战斗杀伤 / 背叛毁约 ——
	case events.ReasonCombatDown, events.ReasonCombatMaimed:
		// 把对手打至倒地/残废（重度杀伤）——破局暴力，偏混乱（强）。
		return []moralSignal{{Axis: moralAxisChaos, Strength: 0.8, Reason: rc}}
	case events.ReasonCombatHit:
		// 寻常战斗伤害——偏混乱（弱）。
		return []moralSignal{{Axis: moralAxisChaos, Strength: 0.4, Reason: rc}}
	case events.ReasonCrossBetrayal:
		// 黑吃黑/背刺（她反咬了本该共担的人）——破而后立、视秩序为枷锁，偏混乱（最强）。
		return []moralSignal{{Axis: moralAxisChaos, Strength: 1.0, Reason: rc}}
	case events.ReasonVengeanceFulfilled:
		// 夙愿得偿（了结恩怨，多含暴力清算）——偏混乱。
		return []moralSignal{{Axis: moralAxisChaos, Strength: 0.6, Reason: rc}}
	default:
		return nil
	}
}

// moralDriftStep 计算某信号在某轴上的确定性步长（绝对值），夹进 [moralDriftStepFloor, moralDriftStepCap] × strength。
//
// 纯函数、确定性：仅依赖入参经 FNV-64a 派生。步长 = floor + (cap-floor)·roll，再乘信号强度 strength。
// 返回值 ∈ [moralDriftStepFloor·strength, moralDriftStepCap·strength]（恒非负，方向由调用方按轴累加）。
func moralDriftStep(sessionID string, turn int, actorID string, sig moralSignal) float64 {
	salt := fmt.Sprintf("%s|%d|%s|%s|%s|moraldrift", sessionID, turn, actorID, sig.Axis, sig.Reason)
	roll := driftRoll(salt) // [0,1)，复用 personality_drift.go 的同款 FNV-64a 派生
	strength := clamp01(sig.Strength)
	step := (moralDriftStepFloor + (moralDriftStepCap-moralDriftStepFloor)*roll) * strength
	return step
}

// accumulateMoralDrift 把一组本回合信号确定性累加成三轴的净增量（已夹单回合单维上限），纯函数、可单测。
//
// 语义：逐信号算步长、按轴累加；每轴累加完成后再夹 [−moralDriftPerTurnCap, +moralDriftPerTurnCap]（防一回合暴涨）。
// 本游戏的道德信号方向恒为「加某轴」（无减向信号），故各轴净增量恒 ≥0；clamp 仅封顶。
func accumulateMoralDrift(sessionID string, turn int, actorID string, signals []moralSignal) faction.MoralAlignment {
	var delta faction.MoralAlignment
	for _, sig := range signals {
		step := moralDriftStep(sessionID, turn, actorID, sig)
		switch sig.Axis {
		case moralAxisFreedom:
			delta.Freedom += step
		case moralAxisOrder:
			delta.Order += step
		case moralAxisChaos:
			delta.Chaos += step
		}
	}
	delta.Freedom = clampAbs(delta.Freedom, moralDriftPerTurnCap)
	delta.Order = clampAbs(delta.Order, moralDriftPerTurnCap)
	delta.Chaos = clampAbs(delta.Chaos, moralDriftPerTurnCap)
	return delta
}

// clampAbs 把 v 的绝对值夹到 [-cap, +cap]（保符号）。
func clampAbs(v, cap float64) float64 {
	if v > cap {
		return cap
	}
	if v < -cap {
		return -cap
	}
	return v
}

// moralAxisChangeEntry 是一次道德漂移里单轴的明细（留痕 payload）。
type moralAxisChangeEntry struct {
	Axis   string  `json:"axis"`
	Before float64 `json:"before"`
	After  float64 `json:"after"`
	Delta  float64 `json:"delta"`
}

// settleMoralDrift 对一个单位施加一次道德漂移：读**执行回合**该角色作为 actor 的事件 → 分类成道德信号 →
// 确定性累加成三轴增量 → 写回 MoralAlignment（各维 clamp[0,100]）→ 更新阵营背离连击计数 → 经 MORAL_DRIFT 留痕。
//
// 在 Execution→Deployment 回合边界由 settleAutonomyAtDeploymentBoundary 逐单位调用（与人格漂移同址）。
// **tick 契约（F4 H1 修）**：边界结算恒在 TurnState.Advance 之后跑（state.TurnState.Turn 已 = N+1=部署回合），
// 但执行期事件（战斗杀伤/抗命等道德信号源）落 tick = N（执行回合）。故本方法接收显式 executedTurn（= Advance 前的回合 N），
// **事件查询 + 哈希派生 + 留痕 Tick 一律用 executedTurn**——保证道德信号源、确定性步长与审计 Tick 三者同处执行回合、互相自洽，
// 不再因「写事件 tick=N、查询 turn=N+1」差一而让战斗杀伤主信号永不命中（修前 settle 几乎恒查不到信号、漂移名存实亡）。
//
// best-effort：缺依赖 / 单位读不到 / 本回合无道德信号 → 优雅返回（不报错、不写库、不影响主循环）。
//
// 返回本次三轴明细（空切片=无漂移）与错误（仅写库失败时非 nil；调用方通常以 _ 忽略）。
func (service *Service) settleMoralDrift(ctx context.Context, sessionID string, record *unit.Record, executedTurn int) ([]moralAxisChangeEntry, error) {
	if service == nil || service.db == nil || record == nil || record.ID == "" {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// 首回合（部署 turn=1）执行回合 N=0 无历史事件，moralSignalsThisTurn 查 tick=0 返回空、安全无漂；
	// 后续回合 executedTurn = 边界 turn-1 = 真正的执行回合，命中执行期落下的道德信号。
	turn := executedTurn

	signals := service.moralSignalsThisTurn(ctx, record.ID, turn)
	delta := accumulateMoralDrift(sessionID, turn, record.ID, signals)
	if delta.IsZero() {
		// 无任何道德信号/漂移 → 仍要更新背离连击（漂移不动也得据当前轴判背离），但不留痕。
		service.refreshMoralDriftStreak(record)
		return nil, nil
	}

	before := record.MoralAlignment
	after := faction.MoralAlignment{
		Freedom: before.Freedom + delta.Freedom,
		Order:   before.Order + delta.Order,
		Chaos:   before.Chaos + delta.Chaos,
	}.Clamped()

	changes := make([]moralAxisChangeEntry, 0, 3)
	appendAxis := func(axis string, b, a float64) {
		if d := a - b; d != 0 {
			changes = append(changes, moralAxisChangeEntry{Axis: axis, Before: roundTo2(b), After: roundTo2(a), Delta: roundTo2(d)})
		}
	}
	appendAxis(string(moralAxisFreedom), before.Freedom, after.Freedom)
	appendAxis(string(moralAxisOrder), before.Order, after.Order)
	appendAxis(string(moralAxisChaos), before.Chaos, after.Chaos)
	if len(changes) == 0 {
		service.refreshMoralDriftStreak(record)
		return nil, nil // 全被 clamp 顶到边界、无实际变化
	}

	// 写回非保护字段（直接读写，不走 Mutator）。
	record.MoralAlignment = after
	// 漂移后据新道德轴刷新「主导阵营背离当前阵营」的连击计数（阵营切换隐藏条件②的持久化锚）。
	service.refreshMoralDriftStreak(record)

	if err := service.units.Save(ctx, *record); err != nil {
		return nil, fmt.Errorf("save morally drifted unit: %w", err)
	}

	// 标准事件留痕（MORAL_DRIFT，流程事件旁路；MoralAlignment 非受保护字段，故不走 status.Mutator）。
	def, _ := events.Lookup(events.ReasonMoralDrift)
	payload := map[string]any{
		"changes":      changes,
		"dominant":     faction.DominantFaction(after),
		"faction":      record.Faction,
		"drift_streak": record.MoralDriftStreak,
		"summary":      def.DefaultReasonText,
		"signal_count": len(signals),
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   sessionID,
		OwnerUnitID: record.ID,
		Code:        events.ReasonMoralDrift,
		Category:    events.CategoryLifecycle,
		Payload:     payload,
		Tick:        turn,
	}); err != nil {
		// 漂移已落库；留痕失败只吞错（不回滚——审计缺一条胜过状态被回退）。
		return changes, nil
	}

	// 实时推送（best-effort）：让前端「道德流转」提示带出本次变化。
	service.pushRealtime(sessionID, "moral_drift", map[string]any{
		"unit_id":  record.ID,
		"changes":  changes,
		"dominant": faction.DominantFaction(after),
	})

	return changes, nil
}

// refreshMoralDriftStreak 据当前道德轴的主导阵营是否背离当前 Faction，更新背离连击计数（阵营切换隐藏条件②的锚）。
//
// 语义：主导阵营存在且 != 当前 Faction（背离）→ MoralDriftStreak +1；否则（一致/无主导/无当前阵营）→ 归 0。
// 纯增量、确定性（只读 record，无随机）。直接读写非保护字段 MoralDriftStreak。
func (service *Service) refreshMoralDriftStreak(record *unit.Record) {
	if record == nil {
		return
	}
	current := faction.Normalize(record.Faction)
	dominant := faction.DominantFaction(record.MoralAlignment)
	if current != "" && dominant != "" && dominant != current {
		record.MoralDriftStreak++
		return
	}
	record.MoralDriftStreak = 0
}

// moralSignalsThisTurn 查本回合（tick==turn）该单位作为 actor 的事件，分类成道德信号集合。
//
// 复用项目「按 actor_unit_id + tick 查 events 表」的口径（fate.go 同款），不依赖 SQL 日期函数、跨方言安全。
// best-effort：查询/扫描失败按「无信号」返回空（保守：宁可不漂，绝不误漂）。
func (service *Service) moralSignalsThisTurn(ctx context.Context, unitID string, turn int) []moralSignal {
	signals := make([]moralSignal, 0, 4)
	if service == nil || service.db == nil || unitID == "" {
		return signals
	}
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT reason_code FROM events WHERE actor_unit_id = ? AND tick = ?`,
		unitID, turn,
	)
	if err != nil {
		return signals
	}
	defer rows.Close()
	for rows.Next() {
		var reasonCode string
		if scanErr := rows.Scan(&reasonCode); scanErr != nil {
			continue
		}
		code := events.ReasonCode(strings.TrimSpace(reasonCode))
		signals = append(signals, moralSignalsForReasonCode(code)...)
	}
	return signals
}

// roundTo2 把浮点保留两位小数（与道德轴留痕精度一致）。
func roundTo2(v float64) float64 {
	return math.Round(v*100) / 100
}
