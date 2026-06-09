package session

// 文件说明：威胁刷新真调度（设计 PvE威胁系统.md §1「刷新=region威胁升级+锚加权」）。在部署边界确定性地决定野外威胁
// 是否出没、落在谁身边。设计取舍：**surface-only**——只投一张「野外有精英出没」的高光卡，不自动开打（尊重玩家/角色
// 能动性），实际遭遇由 HTTP 触发或后续决策接入（避免在边界自动改动战斗态、惊扰正在跑的对局）。开打另受 QUNXIANG_AUTO_PVE
// 管。全程 best-effort、确定性（FNV）。
//
// **本切片把默认路径从「固定 0.12 凭空投卡」升级为设计的 threat_spawn_score 真调度**：
//
//	threat_spawn_score = 0.5·threat_level/100 + 0.3·anchor_density + 0.2·freshness
//
//	① threat_level：region 威胁度累积（读 region 注册表的真实 threat_level；region 未登记=默认单人局常态时，
//	   退化为 **session 内确定性近似**——按本局已沉淀的威胁/战斗类事件数估算，越多越危险，对回合单调不减）；
//	② anchor_density：玩家锚密度（复用 AnchorDensityByRef「多少角色以她为锚」反向密度，威胁更易落在她在乎的地方）；
//	③ freshness：反扎堆破圈项——同一目标刚出过威胁则短期内压低再出概率，但**每日保留破圈下限**（≥1 个零锚来源），
//	   世界仍处处有危险、不全扎堆活跃区。
//
// 跨阈值（score 高到该出没）时用 **arbitration.Resolve**（确定性首位、胜率∝Score、与频率/入队顺序无关、付费不进）
// 在合格单位里选刷新点（非纯随机），让威胁确定性地落在「分最高」的目标身旁、可复算可仲裁。
//
// 反 P2W：score 三项全来自世界事件聚合 / 锚关系 / 回合节奏，**严禁含 wallet/billing**。不破坏既有默认开行为的安全性
// （仍是 surface-only 不主动开打）。新留痕复用已登记 reason-code（ReasonInboxHighlight 投卡、ReasonAnchorWeightedEvent
// 高锚命中留痕）。

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"

	"qunxiang/backend/internal/engine/arbitration"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/region"
	"qunxiang/backend/internal/runtimeconfig"
	"qunxiang/backend/internal/unit"
)

const (
	// threat_spawn_score 三项权重（设计 §1：0.5·threat_level/100 + 0.3·anchor_density + 0.2·freshness）已迁可配层
	// （threat.weight_level / threat.weight_anchor / threat.weight_freshness，catalog 默认 0.5/0.3/0.2），
	// 在 threatSpawnScore 边界读一次传入；出没概率上下限亦迁可配层（threat.spawn_floor / threat.spawn_cap，默认 0.05/0.55）、
	// freshness 窗口迁可配层（threat.freshness_window_turns，默认 6），运行时可热调。

	threatLevelScoreMax = 100.0 // threat_level 归一上限（喂 score 的 threat_level/100 项，超量不再加成）

	// session 内 threat_level 近似的饱和缩放：region 未登记时，把「本局已沉淀的威胁/战斗类事件数」按此除数缩成
	// [0,threatLevelScoreMax] 的近似威胁度。事件越多→近似威胁度越高（威胁扎堆），对回合单调不减。
	threatLevelApproxPerEvent = 4.0
)

// threatProvenanceCodes 是「算 session 内近似 threat_level」时计入的威胁/战斗类事件 reason-code 集合。
// 这些事件越多代表本局世界越凶险（威胁扎堆，§11.3）；纯事件驱动、付费无关、对回合单调不减（events 只增不删）。
// **刻意不含 ReasonInboxHighlight**（投卡本身用的码）——否则「投卡→威胁度升→更易投卡」会自我强化失控；
// 这里只数**真实威胁结算/家乡蒙难**这类世界凶险信号，刷威胁卡本身不抬高威胁度。
var threatProvenanceCodes = []string{
	string(events.ReasonThreatEmerged),
	string(events.ReasonThreatDefeated),
	string(events.ReasonThreatWipe),
	string(events.ReasonRegionRavaged),
	string(events.ReasonThreatAllyDown),
}

// 注：原 autoPvEFireRate（凭空概率门「撞见威胁多大比例直接开打」）已随 join_intent 去 stub 删除——是否真打改由
// EvaluateJoinIntent 的意愿门（反射护栏怕死不上 → 意愿评分 → 无源不参战）决定，不再用概率门，参与决断回归角色能动性。

var wildThreatNames = []string{"山魈", "独眼熊", "赤鳞蜥", "断尾狼", "夜枭", "石背犀"}

// autoPvEEnabled 读 QUNXIANG_AUTO_PVE（true/1/yes/on 视为开），默认关 → refreshThreats 保持 surface-only 行为完全不变。
func autoPvEEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(featureflags.EnvOrOverride("QUNXIANG_AUTO_PVE"))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// refreshThreats 在部署边界刷新野外威胁：按设计的 threat_spawn_score 真调度，为**至多一名**合格单位（非战斗、健康、有命）
// 投一张威胁出没高光卡。流程：①算本局 region 威胁度（真 threat_level 优先，未登记则 session 内确定性近似）；②为每个合格
// 单位算 threat_spawn_score（威胁度 + 她的锚密度 + freshness 反扎堆）；③把 score≥本回合确定性阈值的单位作为候选，用
// arbitration.Resolve 确定性选首位作为刷新点（非纯随机，威胁落在「分最高」目标身旁、可复算）。默认 surface-only。
// QUNXIANG_AUTO_PVE 开时把命中单位的「投卡」升级为真实开打（仍要求健康、非异步执行中）。全程 best-effort、确定性、付费不进。
func (service *Service) refreshThreats(ctx context.Context, state *State, units []unit.Record) {
	if service == nil || state == nil {
		return
	}
	autoPvE := autoPvEEnabled()
	regionLevel := service.regionThreatLevel(ctx, state)

	// 边界读一次可配权重/概率上下限（避免热循环里每单位 RLock），传入纯函数 threatSpawnScore/spawnProbFromScore。
	wLevel := runtimeconfig.GetFloat("threat.weight_level")
	wAnchor := runtimeconfig.GetFloat("threat.weight_anchor")
	wFreshness := runtimeconfig.GetFloat("threat.weight_freshness")
	spawnFloor := runtimeconfig.GetFloat("threat.spawn_floor")
	spawnCap := runtimeconfig.GetFloat("threat.spawn_cap")

	// 一遍扫描合格单位：算各自 score 与「本回合是否过阈」，过阈者进 arbitration 候选（Score=score，付费不进）。
	type candidate struct {
		rec     unit.Record
		score   float64
		density float64
	}
	contestants := make([]arbitration.Contestant, 0, len(units))
	byID := make(map[string]candidate, len(units))
	for i := range units {
		u := units[i]
		if state.PlayerFactionID != "" && u.FactionID != state.PlayerFactionID {
			continue
		}
		if u.Status.InCombat || u.Status.HP < 40 || u.Status.LivesRemaining <= 0 {
			continue
		}
		density := service.AnchorDensityByRef(ctx, u.ID, "")
		fresh := service.threatFreshness(ctx, state, u.ID)
		score := threatSpawnScore(regionLevel, density, fresh, wLevel, wAnchor, wFreshness)
		spawnProb := spawnProbFromScore(score, spawnFloor, spawnCap)
		// 本回合该单位的确定性出没掷骰 [0,1)：过阈（draw<spawnProb）才进 arbitration 候选。
		if threatRoll(state.ID, state.TurnState.Turn, u.ID) >= spawnProb {
			continue
		}
		byID[u.ID] = candidate{rec: u, score: score, density: density}
		// arbitration Score 必 >0 才有意义参与抽样；score 至少有破圈下限对应的正分，这里直接用 score（已 >0）。
		contestants = append(contestants, arbitration.Contestant{UnitID: u.ID, Score: score})
	}
	if len(contestants) == 0 {
		return // 本边界无单位过阈，不刷
	}

	// 跨阈值选址：arbitration.Resolve 确定性取首位（胜率∝Score=threat_spawn_score、与入队顺序/频率无关、付费不进）。
	// Key 含 sessionID+turn，保证同局同回合可复算可仲裁；不同回合换 Key → 选址随世界推进确定性变化。
	outcome := arbitration.Resolve(arbitration.Contest{
		Key:         fmt.Sprintf("threat-spawn:%s:%d", state.ID, state.TurnState.Turn),
		Resource:    "wild_threat_site",
		Contestants: contestants,
	})
	winner, ok := byID[outcome.WinnerID]
	if !ok {
		return
	}
	u := winner.rec

	// 参与意愿评估（去 stub）：把「是否真打」从凭空概率门换成她自己拿主意的意愿门——构造与其战力相称的精英怪，
	// 跑 EvaluateJoinIntent（反射护栏怕死不上 → 意愿评分含归因 → 无源不参战 OOC）。caresInPlace 复用护短近似
	// （winner.density>0.5：被够多人在乎=她在乎的人多半也在场）；goalThreatened 暂无可得信号（false）。
	actor := u
	elite := scaledElite(actor)
	caresInPlace := winner.density > 0.5
	goalThreatened := false
	intent := service.EvaluateJoinIntent(ctx, state, &actor, elite, caresInPlace, goalThreatened, JoinAdvice{})

	// 自动开打升级：flag 开 + 非异步执行中（让位聚焦战斗）+ 她的意愿门判定参战（Auto/Advised）→ 真实跑通 elite 遭遇，
	// 否则仍只投卡。无论开打与否，都按意愿评估结果记一条 JOIN 留痕（3 个 JOIN 码真写入）。
	if autoPvE && !IsExecutionRunning(state.ID) && intent.Join {
		service.recordThreatJoin(ctx, state, u.ID, intent.Mode, elite, intent.Score)
		if _, err := service.ResolveEliteEncounter(ctx, state, &actor, elite); err == nil {
			service.recordThreatHit(ctx, state, u.ID) // 记本目标最近命中（freshness 反扎堆）
			return                                    // 自动开打已落地（含其自身的命运收件箱卡），不再额外投出没卡
		}
		// 遭遇失败（如并发写冲突）：吞错，退回 surface-only 投卡，绝不中断推进。
	} else {
		// surface-only 默认路径（autoPvE 关 / 异步执行中 / 她权衡后退避）：仍按意愿评估结果记 JOIN 留痕，
		// 让退避（Decline）/「想上但没真打」（Auto/Advised）三态都在审计里留痕，再走出没卡。
		service.recordThreatJoin(ctx, state, u.ID, intent.Mode, elite, intent.Score)
	}

	name := wildThreatNames[threatHash(state.ID, state.TurnState.Turn, u.ID, "name")%uint64(len(wildThreatNames))]
	_, _ = service.SurfaceFateEvent(ctx, state.ID, &u, FateEvent{
		ActorID: u.ID, TargetID: u.ID, ReasonCode: events.ReasonInboxHighlight,
		Importance: 5, EmotionWeight: -0.2,
		Summary: fmt.Sprintf("山野间有一头%s在游荡，离她不远。", name),
	})
	// 锚加权留痕（设计 §1.5「祸福偏要落在她最在意的人和地方」）：命中的若是高锚目标，写 ReasonAnchorWeightedEvent。
	if isHighThreatAnchorDensity(winner.density) {
		service.emitThreatAnchorWeighted(ctx, state, u.ID, winner.density)
	}
	// 记本目标最近出威胁回合（freshness 反扎堆，避免同一目标一窝蜂连刷）。
	service.recordThreatHit(ctx, state, u.ID)
}

// threatSpawnScore 实现设计 §1 的 threat_spawn_score = w_level·threat_level/100 + w_anchor·anchor_density + w_freshness·freshness ∈ [0,1]。
// 三项权重（默认 0.5/0.3/0.2）由调用方从 runtimeconfig 读出后传入（边界读一次，避免热循环每单位 RLock）；三项入参各夹 [0,1]
// （regionLevel 按 threatLevelScoreMax 归一，anchorDensity/freshness 已是 [0,1]）。纯函数、确定性、付费无关。
func threatSpawnScore(regionLevel int64, anchorDensity float64, freshness float64, wLevel, wAnchor, wFreshness float64) float64 {
	lvl := float64(regionLevel) / threatLevelScoreMax
	lvl = clamp01(lvl)
	anchorDensity = clamp01(anchorDensity)
	freshness = clamp01(freshness)
	return wLevel*lvl + wAnchor*anchorDensity + wFreshness*freshness
}

// spawnProbFromScore 把 [0,1] 的 threat_spawn_score 线性映射成本边界出没概率 [floor, cap]（默认 0.05/0.55，由调用方
// 从 runtimeconfig 读出后传入，边界读一次）。**破圈下限恒保留**：score=0（零威胁+零锚+刚出过）也返回 floor（世界处处
// 有危险，不全扎堆活跃区）。
func spawnProbFromScore(score float64, floor, cap float64) float64 {
	score = clamp01(score)
	return floor + (cap-floor)*score
}

// isHighThreatAnchorDensity 判定锚密度是否「高」（被够多角色在乎，事件值得留 ReasonAnchorWeightedEvent 痕）。
// 与 regionrunner.isHighAnchorDensity 同口径（density>0.5）。纯函数、确定性。
func isHighThreatAnchorDensity(density float64) bool {
	return density > 0.5
}

// regionThreatLevel 算本局 region 威胁度（喂 threat_spawn_score 的 threat_level 项）：
//
//	① 优先读 region 注册表的真实 threat_level 累积值（单人局默认 region_id==sessionID，命中威胁经 region-runner
//	   BumpThreatLevel 持续累积）；
//	② region 未登记（单人局默认常态）→ 退化为 **session 内确定性近似**：按本局已沉淀的威胁/战斗类事件数估算，
//	   事件越多→威胁度越高（威胁扎堆 §11.3），对回合单调不减（events 只增不删）。
//
// best-effort：db 缺失/读失败均回退到近似，绝不报错。确定性、付费无关。
func (service *Service) regionThreatLevel(ctx context.Context, state *State) int64 {
	if service == nil || service.db == nil || state == nil {
		return 0
	}
	// region_id 约定：单人局默认 == sessionID（与 ambient_scheduling.go 的回退口径一致）。
	regionID := state.ID
	reg, err := region.New(service.db).GetRegion(ctx, regionID)
	if err == nil {
		lvl := reg.ThreatLevel
		if lvl < 0 {
			lvl = 0
		}
		if lvl > int64(threatLevelScoreMax) {
			lvl = int64(threatLevelScoreMax)
		}
		return lvl
	}
	// 回退：session 内近似（region 未登记是单人局默认常态，Debug 都不必——这是预期路径）。
	return service.approxThreatLevel(ctx, state.ID)
}

// approxThreatLevel 用「本局已沉淀的威胁/战斗类事件数」近似 region 威胁度 ∈ [0,threatLevelScoreMax]。
// 事件越多→近似威胁度越高（威胁扎堆）；events 表只增不删 → 对回合单调不减。db 查询失败返回 0（保守不夸大）。
// 纯事件驱动、付费无关、确定性（同一 events 表状态查得同值）。
func (service *Service) approxThreatLevel(ctx context.Context, sessionID string) int64 {
	if service == nil || service.db == nil || sessionID == "" {
		return 0
	}
	placeholders := make([]string, len(threatProvenanceCodes))
	args := make([]any, 0, len(threatProvenanceCodes)+1)
	args = append(args, sessionID)
	for i, c := range threatProvenanceCodes {
		placeholders[i] = "?"
		args = append(args, c)
	}
	query := fmt.Sprintf(
		`SELECT COUNT(*) FROM events WHERE session_id = ? AND reason_code IN (%s)`,
		strings.Join(placeholders, ","))
	var n int64
	if err := service.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0
	}
	approx := int64(float64(n) * threatLevelApproxPerEvent)
	if approx > int64(threatLevelScoreMax) {
		approx = int64(threatLevelScoreMax)
	}
	return approx
}

// threatFreshness 算某目标的 freshness 反扎堆项 ∈ [0,1]：距上次为她出威胁越近越小（更压低 score），窗口外/从未出过 → 1。
// 「上次出威胁回合」从 events 表读该单位最近一条威胁出没留痕（ReasonThreatEmerged，由 recordThreatHit 带显式 tick 写入）的 tick；
// 读不到视为从未出过（=1）。**刻意不读 ReasonInboxHighlight**——SurfaceFateEvent 写卡时不带 tick（恒 0），无法用作回合参照；
// 故由 recordThreatHit 专门写一条带 tick 的 ReasonThreatEmerged 作为 freshness 锚点（surface/开打两路径都写）。
// 破圈语义由 spawnProbFromScore 的 floor 兜底（哪怕 freshness=0，仍保留 threat.spawn_floor 概率，每日≥1 个来源能出）。
// 确定性（只看回合差，读持久化 tick）；付费无关。
func (service *Service) threatFreshness(ctx context.Context, state *State, unitID string) float64 {
	if service == nil || service.db == nil || state == nil || unitID == "" {
		return 1
	}
	var lastTick int64
	err := service.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(tick), -1) FROM events WHERE session_id = ? AND actor_unit_id = ? AND reason_code = ?`,
		state.ID, unitID, string(events.ReasonThreatEmerged)).Scan(&lastTick)
	if err != nil || lastTick < 0 {
		return 1 // 读失败/从未出过 → 完全新鲜，不压低
	}
	window := int64(runtimeconfig.GetInt("threat.freshness_window_turns"))
	return freshnessFromTurns(lastTick, int64(state.TurnState.Turn), window)
}

// freshnessFromTurns 由「上次出威胁回合 lastTurn」与「当前回合 curTurn」算 freshness ∈ [0,1]：
//
//	Δturn=0（同回合刚出）→ 0（最强压制）；Δturn≥窗口 → 1（完全恢复）；窗口内线性回升。
//
// window 是反扎堆窗口（默认 6，由调用方从 runtimeconfig 读出后传入）；window≤0 视为无窗口（恒完全恢复=1）。
// 时钟回拨/乱序保护：Δ<0 视为刚出（0）。纯函数、确定性。
func freshnessFromTurns(lastTurn, curTurn, window int64) float64 {
	if window <= 0 {
		return 1
	}
	elapsed := curTurn - lastTurn
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed >= window {
		return 1
	}
	return float64(elapsed) / float64(window)
}

// recordThreatHit 写一条带显式 tick 的 ReasonThreatEmerged 作为某目标的 **freshness 锚点**（threatFreshness 直接读它的 tick）。
// 两条路径都调用：surface-only 投卡后、auto-PvE 开打后——因为 SurfaceFateEvent 的 ReasonInboxHighlight 卡不带 tick（恒 0），
// 无法用作回合参照，故由本函数统一记录「本回合为该目标出过威胁」。best-effort：写失败只吞错、绝不阻断推进。
func (service *Service) recordThreatHit(ctx context.Context, state *State, unitID string) {
	if service == nil || service.db == nil || state == nil || unitID == "" {
		return
	}
	_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   state.ID,
		OwnerUnitID: unitID,
		Code:        events.ReasonThreatEmerged,
		Category:    events.CategoryLifecycle,
		RegionID:    state.ID,
		Tick:        state.TurnState.Turn,
		Payload:     map[string]any{"unit_id": unitID, "context": "wild_threat_spawn"},
	})
}

// recordThreatJoin 按参与意愿评估的结局写一条 JOIN 流程事件留痕（让 reason_codes.go:124-126 的三个 JOIN 码真写入）：
// JoinModeAutonomous→ReasonThreatJoinAuto（她自己拿主意迎战）、JoinModeAdvised→ReasonThreatJoinAdvise（采纳祖魂叮嘱）、
// 其余（JoinModeReflexDecl/未知）→ReasonThreatJoinDecline（反射护栏/意愿不足/无源退避）。这是「事件本身」而非状态变更，
// 经 events.EmitProcessEvent 旁路写入（Category=CategoryLifecycle，OwnerUnitID=该角色，Tick=当前回合）。
// payload 只记意愿评分/威胁名（反 P2W，绝不含 wallet/billing）。best-effort：写失败只吞错、绝不阻断刷新结算。
func (service *Service) recordThreatJoin(ctx context.Context, state *State, unitID string, mode JoinMode, threat Threat, score float64) {
	if service == nil || service.db == nil || state == nil || unitID == "" {
		return
	}
	code := events.ReasonThreatJoinDecline
	switch mode {
	case JoinModeAutonomous:
		code = events.ReasonThreatJoinAuto
	case JoinModeAdvised:
		code = events.ReasonThreatJoinAdvise
	}
	_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   state.ID,
		OwnerUnitID: unitID,
		Code:        code,
		Category:    events.CategoryLifecycle,
		WorldID:     state.WorldID,
		RegionID:    state.ID,
		Tick:        state.TurnState.Turn,
		Payload: map[string]any{
			"unit_id":           unitID,
			"join_mode":         string(mode),
			"join_intent_score": score,
			"threat":            threat.Name,
			"tier":              string(threat.Tier),
		},
	})
}

// emitThreatAnchorWeighted 在「威胁确实落到高锚目标」时写一条 ReasonAnchorWeightedEvent 流程事件留痕
// （设计 §1.5「祸福偏要落在她最在意的人和地方」）。best-effort：写失败只吞错、绝不影响刷新结算。
func (service *Service) emitThreatAnchorWeighted(ctx context.Context, state *State, unitID string, density float64) {
	if service == nil || service.db == nil || state == nil || unitID == "" {
		return
	}
	_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   state.ID,
		OwnerUnitID: unitID,
		Code:        events.ReasonAnchorWeightedEvent,
		Category:    events.CategoryFate,
		RegionID:    state.ID,
		Tick:        state.TurnState.Turn,
		Payload: map[string]any{
			"unit_id":        unitID,
			"anchor_density": density,
			"context":        "wild_threat_spawn",
		},
	})
}

func threatHash(sessionID string, turn int, unitID string, salt string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(fmt.Sprintf("threat:%s:%d:%s:%s", sessionID, turn, unitID, salt)))
	return h.Sum64()
}

// threatRoll 确定性掷骰 [0,1)（项目约定 sessionID+turn+actor 的 FNV）。
func threatRoll(sessionID string, turn int, unitID string) float64 {
	return float64(threatHash(sessionID, turn, unitID, "roll")%10000) / 10000.0
}
