package session

// 文件说明：威胁事件(ThreatEvent)容器与单人 elite 遭遇结算，把 engine/encounter 原语接入 session 层。
// 链路：撞见精英怪 → 反射护栏先保命 → combat_roll 确定性多回合消耗战 → 胜利走 encounter.AllocateLoot 分赃 /
// 失败走 encounter.DegradePenalty 后果分级闸 → 全程经 status.Mutator 留痕 → 产出祖魂语气的命运收件箱卡。
// 设计见 docs/PvE威胁系统.md。单人 elite 不依赖 World Bus，可在现有 session 内跑通。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"qunxiang/backend/internal/engine/decision"
	"qunxiang/backend/internal/engine/encounter"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/item"
	"qunxiang/backend/internal/unit"
)

// eliteMutationRetries 是 elite 遭遇写的乐观并发重试次数（PvE-3，镜像 real-3-0）。离线遭遇（region-runner 触发）可能与
// 战斗循环/部署期 HTTP 并发改同一单位；用 ApplyOptimistic 条件写 + 冲突重试（每次重读最新值再施 delta，**正确叠加**并发写而非
// 整块覆盖），保证战斗/HTTP 的写**永不被离线遭遇覆盖**。单线程（HTTP 手动触发、无并发写者）首次即成功，行为同 Apply。
const eliteMutationRetries = 8

// applyEliteMutation 经乐观并发把 elite 遭遇的一次字段变更落地，冲突即重读重试。重试耗尽仍冲突则返回 ErrConcurrentModification
// （上层据此让遭遇失败、计 encounter_errors），绝不退化成覆盖战斗写。
func (service *Service) applyEliteMutation(ctx context.Context, mutation status.Mutation) (status.Result, error) {
	var res status.Result
	var err error
	for attempt := 0; attempt <= eliteMutationRetries; attempt++ {
		res, err = service.mutator.ApplyOptimistic(ctx, mutation)
		if err == nil {
			return res, nil
		}
		if !errors.Is(err, status.ErrConcurrentModification) {
			return res, err
		}
	}
	return res, err
}

// ThreatTier 威胁档位（四档同一模型的参数化）。
type ThreatTier string

const (
	ThreatTierElite     ThreatTier = "elite"
	ThreatTierFieldBoss ThreatTier = "field_boss"
	ThreatTierDungeon   ThreatTier = "dungeon"
	ThreatTierWorldBoss ThreatTier = "world_boss"
)

// Threat 是一个威胁事件的轻量容器（单人 elite 先用内存态，不落新表）。
type Threat struct {
	ID       string
	Name     string
	Tier     ThreatTier
	RegionID string
	Power    int
	Attack   int
	Defense  int
	HPPool   int
	Severity float64
	Loot     []encounter.LootItem // 胜利掉落（elite 通常是少量货币/材料）
}

// EliteEncounterResult 单人 elite 遭遇结果。
type EliteEncounterResult struct {
	ThreatID     string
	Outcome      string // defeated / fled / down
	Rounds       int
	DamageDealt  int
	DamageTaken  int
	Contribution float64
	Awards       []encounter.Award
	PenaltyLayer int    // 失败时实际落地的后果层（经分级闸降级），0=未触发
	InboxCard    string // 祖魂语气的命运收件箱卡
}

const (
	eliteMaxRounds  = 30
	eliteFleeHP     = 25   // HP 低于此值反射撤退（HP 上限 100，≈0.25）
	eliteMissChance = 0.08 // 确定性掷骰命中判定
	// eliteHPMax 是角色 HP 上限（与 eliteFleeHP 的 0.25 口径同源），用于把当前 HP 归一化判濒死带（selfHPFraction）。
	eliteHPMax = 100
	// eliteFleeFraction 是归一化撤退线（= eliteFleeHP/eliteHPMax，0.25）：HP 跌破此线本回合开头即反射撤退（永不到达攻击分支）。
	eliteFleeFraction = float64(eliteFleeHP) / float64(eliteHPMax)
	// eliteNearDeathFraction 是「濒死反杀」的归一化濒死带上沿（= eliteFleeFraction*1.5，0.375），**位于撤退线之上**。
	// 修死代码（finding）：原濒死反杀判 selfHPFraction<eliteFleeFraction（撤退线之下），但反射护栏在每回合开头先于攻击分支
	// 触发——HP<eliteFleeHP 当场撤退 break，攻击分支恒在「HP≥撤退线」执行，故撤退线之下的判定永不到达=死代码、永不计分。
	// 改判「撤退线之上的濒死带」[eliteFleeFraction, eliteNearDeathFraction]：刚过撤退线、伤未透、仍咬牙打出有效输出的孤勇——
	// 这一带在攻击分支真实可达（角色撑过护栏、HP 略高于撤退线时攻击），让 Clutch 濒死反杀真正进 Score。
	eliteNearDeathFraction = eliteFleeFraction * 1.5
)

// TriggerEliteEncounter 为某角色生成一头与其战力相称的精英怪并跑通完整遭遇（供 API 触发）。
// 这是真实动作：会真的改动该角色的 HP/士气/钱包并落进命运收件箱。
func (service *Service) TriggerEliteEncounter(ctx context.Context, sessionID string, unitID string) (EliteEncounterResult, error) {
	if service == nil || service.units == nil {
		return EliteEncounterResult{}, fmt.Errorf("trigger elite encounter: missing dependencies")
	}
	actor, err := service.units.GetByID(ctx, unitID)
	if err != nil {
		return EliteEncounterResult{}, err
	}
	state := State{ID: sessionID}
	return service.ResolveEliteEncounter(ctx, &state, &actor, scaledElite(actor))
}

// scaledElite 生成一头与角色战力相称、通常可一战的精英怪。
func scaledElite(actor unit.Record) Threat {
	atk := actor.Status.Attack
	if atk < 4 {
		atk = 4
	}
	def := atk / 3
	if def < 1 {
		def = 1
	}
	hp := atk * 3
	if hp < 20 {
		hp = 20
	}
	eliteAtk := actor.Status.Defense
	if eliteAtk < 3 {
		eliteAtk = 3
	}
	return Threat{
		ID:       "elite_" + uuid.NewString(),
		Name:     "山魈",
		Tier:     ThreatTierElite,
		RegionID: "",
		Power:    atk * 4,
		Attack:   eliteAtk,
		Defense:  def,
		HPPool:   hp,
		Severity: 40,
		Loot:     []encounter.LootItem{{ID: "gold", Rarity: encounter.Common, Quantity: 15}},
	}
}

// candidateDefeatLayer 各档威胁失败时的候选后果层（再经分级闸按角色降级）。
func candidateDefeatLayer(tier ThreatTier) int {
	switch tier {
	case ThreatTierWorldBoss:
		return 3
	case ThreatTierFieldBoss, ThreatTierDungeon:
		return 2
	default: // elite
		return 1
	}
}

func eliteDamage(attack int, defense int) int {
	d := attack - defense
	if d < 1 {
		d = 1
	}
	return d
}

// ---- §2 参与意愿评估 join_intent ----
//
// 「单位自评是否迎战」：威胁浮现时，对每个在场/可达角色跑确定性可审计的意愿评估，供后续执行主循环 /
// regionrunner 决定要不要把她拉进遭遇——把「参战」从「endpoint 显式传 unitIDs」扶正为「她自己拿主意」。
// 三段（设计 §2）：① 反射护栏（零 LLM，怕死的物理上不去）；② 意愿评分（确定性，归因必过 ValidateAttribution，
// 无源→不参战 OOC）；③ 祖魂引导=偏置项（advice 进 join_intent 偏置而非覆盖，仍按 rejectProbability 概率采纳）。
// **已有显式传 unitIDs 的 endpoint 路径保留**（玩家手动触发不走意愿评估）；这里新增的是「自评」能力。

// JoinMode 是参与意愿评估的结局（与 reason-code / threat_participants.join_mode 对齐）。
type JoinMode string

const (
	JoinModeAutonomous JoinMode = "autonomous"      // 她自己拿定主意迎战（ReasonThreatJoinAuto）
	JoinModeAdvised    JoinMode = "advised"         // 采纳了祖魂叮嘱才迎战（ReasonThreatJoinAdvise）
	JoinModeReflexDecl JoinMode = "reflex_declined" // 反射护栏/意愿不足/无源退避（ReasonThreatJoinDecline）
)

// JoinIntent 是一次参与意愿评估的可审计结果（确定性、纯逻辑、零 LLM）。
type JoinIntent struct {
	UnitID    string
	Join      bool     // 是否自治参战
	Mode      JoinMode // 参战/退避方式
	Score     float64  // join_intent 评分（含 advice 偏置；留痕进 payload.join_intent_score）
	Threshold float64  // 当时的参战阈值（≥阈值才参战）
	ReasonZH  string   // 祖魂语气一句话（迎战/退避的因）
	Grounded  bool     // 归因是否过 ValidateAttribution（无源→不参战 OOC）
}

// join_intent 评分权重与阈值（设计 §2，确定性、可调，[待测试]）。反 P2W：全部来自人格/关系/目标/压力，绝不含 wallet/billing。
const (
	joinWeightAmbition = 0.40 // w_amb·野心（power/vengeance/mastery 驱动迎战）
	joinWeightRelation = 0.35 // w_rel·护短（在乎的人已在场）
	joinWeightGoal     = 0.30 // w_goal·目标契合（威胁卡了她的商路/红线）
	joinWeightFear     = 0.45 // −w_fear·惧战（prudence 高/courage 低）
	joinWeightRisk     = 0.20 // −w_risk·后果层（候选后果越重越却步）
	joinIntentGate     = 0.30 // ≥此值才自治参战
	joinAdviceBias     = 0.25 // 祖魂「去」的偏置幅度（advice 偏置非覆盖）
)

// JoinAdvice 是玩家（祖魂）对某次迎战的叮嘱（偏置项，绝不直接改 participant_ids）。
type JoinAdvice struct {
	Present bool    // 是否有叮嘱
	Urge    bool    // true=叮嘱去；false=叮嘱别去
	Reject  float64 // 角色抗命概率 [0,1]：惜命角色对「去」更可能抗命，护短角色对「去」更可能采纳
}

// EvaluateJoinIntent 对一个角色评估「是否自治迎战这头威胁」。
//
//	(a) 先过反射护栏（零 LLM）：HP/HPMax<0.25 或 离线宪章红线含「避战」 → Flee/绕开（JoinModeReflexDecl）。
//	(b) 意愿评分（确定性可审计）：join_intent = w_amb·野心 + w_rel·护短 + w_goal·目标契合 − w_fear·惧战 − w_risk·后果层，
//	    ≥阈值才自治参战；归因必过 ValidateAttribution（cause∈persona_trait/relation/goal/pressure），无源→不参战（OOC）。
//	(c) 祖魂引导=偏置非覆盖：advice 进 join_intent 作偏置项，仍按 rejectProbability 概率采纳。
//
// caresInPlace 表示「她在乎的人是否已在场」（护短项）；goalThreatened 表示「威胁是否卡了她的目标/红线」（目标项）。
// 全程确定性：advice 采纳的概率掷骰用 FNV(sessionID+turn+actor) 哈希，禁全局 rand。
func (service *Service) EvaluateJoinIntent(
	ctx context.Context, state *State, actor *unit.Record, threat Threat,
	caresInPlace bool, goalThreatened bool, advice JoinAdvice,
) JoinIntent {
	out := JoinIntent{UnitID: "", Mode: JoinModeReflexDecl, Threshold: joinIntentGate}
	if service == nil || state == nil || actor == nil {
		return out
	}
	out.UnitID = actor.ID

	// (a) 反射护栏：HP 危急 或 离线宪章红线含「避战」→ 物理上不去（怕死的不参战）。
	hpMax := 100
	if actor.Status.HP < int(float64(hpMax)*0.25) || charterForbidsEngage(state, actor.ID) {
		out.ReasonZH = fmt.Sprintf("权衡之下，她没有去硬碰那头%s", threat.Name)
		return out
	}

	// (b) 意愿评分（确定性）。野心取 power/vengeance/mastery 的最高位（迎战驱动）。
	amb := maxFloat(actor.Ambition.Power, maxFloat(actor.Ambition.Vengeance, actor.Ambition.Mastery))
	relTerm := 0.0
	if caresInPlace {
		relTerm = 1.0
	}
	goalTerm := 0.0
	if goalThreatened {
		goalTerm = 1.0
	}
	// 惧战：prudence 高 + courage 低（人格四轴归一已在 [0,1]）。
	fear := clamp01Float(actor.Personality.Prudence*0.6 + (1-actor.Personality.Courage)*0.4)
	risk := float64(candidateDefeatLayer(threat.Tier)) / 3.0 // 后果层越重越却步

	score := joinWeightAmbition*amb + joinWeightRelation*relTerm + joinWeightGoal*goalTerm -
		joinWeightFear*fear - joinWeightRisk*risk

	// (c) 祖魂引导偏置（非覆盖）：叮嘱「去」加偏置、叮嘱「别去」减偏置——但角色仍可按 rejectProbability 抗命。
	adviceAdopted := false
	if advice.Present {
		adopt := !service.adviceRejected(*state, actor.ID, advice.Reject)
		if adopt {
			adviceAdopted = true
			if advice.Urge {
				score += joinAdviceBias
			} else {
				score -= joinAdviceBias
			}
		}
	}
	out.Score = score

	// 归因必过 ValidateAttribution：构造 join 决断的归因（primary 取评分里最重的可解析前因），无源→不参战（OOC）。
	// 用与「PvE 后果前因」「LLM 决策前因」同一套真相源的快照（人格/记忆/红线/关系/压力），保证一把尺。
	snap := service.buildDefeatProvenanceSnapshot(ctx, *state, actor)
	if attr, ok := buildJoinAttribution(actor, amb, caresInPlace, goalThreatened, threat, snap); ok {
		out.Grounded = decision.ValidateAttribution(attr, snap).OK
	}

	if score >= joinIntentGate && out.Grounded {
		out.Join = true
		out.Mode = JoinModeAutonomous
		out.ReasonZH = fmt.Sprintf("她自己拿定主意，迎上了那头%s", threat.Name)
		if adviceAdopted && advice.Urge {
			out.Mode = JoinModeAdvised // 受嘱参战：偏置把她推过了阈值（仍是她自愿采纳）
			out.ReasonZH = fmt.Sprintf("听了你的话，她迎上了那头%s", threat.Name)
		}
		return out
	}

	// 未过阈值 / 无源 → 退避（贡献语义不适用——未参战）。
	out.ReasonZH = fmt.Sprintf("权衡之下，她没有去硬碰那头%s", threat.Name)
	return out
}

// charterForbidsEngage 判离线宪章是否含「避战/勿战」红线（反射护栏：怕死的物理上不去）。确定性、纯逻辑。
func charterForbidsEngage(state *State, unitID string) bool {
	for _, text := range charterRedlinesAsMap(state, unitID) {
		for _, kw := range []string{"避战", "勿战", "不战", "莫战", "别打", "退避", "勿斗"} {
			if strings.Contains(text, kw) {
				return true
			}
		}
	}
	return false
}

// adviceRejected 确定性判定角色是否抗命（不采纳祖魂叮嘱）。FNV(sessionID+turn+actor) 哈希取 [0,1)，
// < rejectProbability 即抗命。禁全局 rand，可复现。
func (service *Service) adviceRejected(state State, unitID string, rejectProbability float64) bool {
	if rejectProbability <= 0 {
		return false
	}
	if rejectProbability >= 1 {
		return true
	}
	roll := combatActionRoll(state, unit.Record{ID: unitID}, unit.Record{ID: "advice"}, "join_advice")
	return roll < rejectProbability
}

// buildJoinAttribution 为「迎战」决断构造可校验归因（primary 取评分里最重的可解析前因）。
// 顺序优先：护短关系 > 显著野心(persona_trait) > 现实压力——与 ValidateAttribution 的解析规则同源。
// snap 是该角色的真前因快照（buildDefeatProvenanceSnapshot 填的人格/关系/压力），归因 RefID 必指向其中真实记录，绝不凭空造锚。
// 返回 (归因, 是否构造成功)；无任何可锚前因时第二返回值为 false（调用方据此判 OOC=不参战）。
func buildJoinAttribution(actor *unit.Record, ambition float64, caresInPlace bool, goalThreatened bool, threat Threat, snap decision.Snapshot) (decision.Attribution, bool) {
	// 护短：在乎的人在场 → 关系类前因（RefID 取 snap.Relations 里最显著的真实目标，由 ValidateAttribution 校验显著性）。
	if caresInPlace {
		if target, ok := strongestRelationTarget(snap); ok {
			return decision.Attribution{
				Primary:     decision.CauseRef{Kind: decision.CauseRelation, RefID: target, Weight: 0.7},
				NarrativeZH: fmt.Sprintf("她迎上那头%s，是放不下在场的人", threat.Name),
			}, true
		}
	}
	// 显著野心：power/vengeance/mastery 任一足够高 → persona_trait 前因（RefID 取 snap.Traits 里真实显著的一维）。
	if ambition >= 0.5+traitSignificanceLocal {
		if trait, ok := dominantSignificantTrait(snap); ok {
			return decision.Attribution{
				Primary:     decision.CauseRef{Kind: decision.CausePersonaTrait, RefID: trait, Weight: 0.6},
				NarrativeZH: fmt.Sprintf("她迎上那头%s，正是她的性子使然", threat.Name),
			}, true
		}
	}
	// 目标受卡 → 现实压力前因（debt：威胁断了她的活路）。仅当战前确有 Debt 压力时才成立（由 snap.Pressure 校验显著）。
	if goalThreatened && snap.Pressure.Debt {
		return decision.Attribution{
			Primary:     decision.CauseRef{Kind: decision.CausePressure, RefID: "debt", Weight: 0.6},
			NarrativeZH: fmt.Sprintf("那头%s断了她的活路，她不得不应", threat.Name),
		}, true
	}
	return decision.Attribution{}, false
}

// traitSignificanceLocal 与 decision.traitSignificance 同源（|trait-0.5| ≥ 此值才显著），本包不可直接引未导出常量。
const traitSignificanceLocal = 0.25

// strongestRelationTarget 取快照里关系四轴绝对值最大、且过显著阈的真实目标 unitID（护短归因的 RefID）。
// 确定性：同强度按 unitID 升序兜底，不依赖 map 遍历顺序。无显著关系返回 ("", false)。
func strongestRelationTarget(snap decision.Snapshot) (string, bool) {
	best := ""
	bestMag := 0.0
	for id, r := range snap.Relations {
		mag := maxFloat(absFloat(r.Trust), maxFloat(absFloat(r.Affection), maxFloat(absFloat(r.Fear), absFloat(r.Rivalry))))
		if mag < defeatRelAxisMin {
			continue
		}
		if mag > bestMag || (mag == bestMag && (best == "" || id < best)) {
			best = id
			bestMag = mag
		}
	}
	return best, best != ""
}

// dominantSignificantTrait 取快照里 |trait-0.5| 最大且过显著阈的真实人格维名（persona_trait 归因 RefID）。
// 确定性：同显著度按名升序兜底。无显著人格返回 ("", false)。
func dominantSignificantTrait(snap decision.Snapshot) (string, bool) {
	best := ""
	bestMag := 0.0
	for name, v := range snap.Traits {
		mag := absFloat(v - 0.5)
		if mag < traitSignificanceLocal {
			continue
		}
		if mag > bestMag || (mag == bestMag && (best == "" || name < best)) {
			best = name
			bestMag = mag
		}
	}
	return best, best != ""
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func clamp01Float(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ---- §9 PvE 专属阶段/结局流程留痕（修审计假阴：阶段/结局/分赃/失败不再混用通用码）----
//
// 这些是「事件本身」（威胁浮现/讨平/覆没/同伴倒下）而非状态变更的留痕，经 events.EmitProcessEvent 旁路写入。
// HP 变更仍走既有 applyEliteMutation/Mutator（COMBAT_HIT/DOWN 通用码 OK）；这里补的是阶段/结局专属码。
// 一律 best-effort：吞错只 log、绝不阻断结算主链路。WorldID/Tick 随事件下沉（与 liveops 审计对齐）。

// recordThreatEmerged 在一次遭遇开打前留痕「威胁浮现」（ReasonThreatEmerged）。
func (service *Service) recordThreatEmerged(ctx context.Context, state *State, ownerID string, threat Threat) {
	if service == nil || service.db == nil || state == nil || ownerID == "" {
		return
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   state.ID,
		OwnerUnitID: ownerID,
		Code:        events.ReasonThreatEmerged,
		Category:    events.CategoryLifecycle,
		Payload:     map[string]any{"threat": threat.Name, "tier": string(threat.Tier), "severity": threat.Severity},
		WorldID:     state.WorldID,
		RegionID:    threat.RegionID,
		Tick:        state.TurnState.Turn,
	}); err != nil {
		slog.Warn("record threat emerged failed (best-effort)", "threat", threat.ID, "err", err)
	}
}

// recordThreatOutcome 留痕一次遭遇的整体结局：讨平=ReasonThreatDefeated；覆没=ReasonThreatWipe。
// ownerID 取一名结算代表（单人=该角色、组队=首位），participants 记进 payload 供审计复算（只记 unitID，不含付费维度）。
func (service *Service) recordThreatOutcome(ctx context.Context, state *State, ownerID string, threat Threat, victory bool, participants []string) {
	if service == nil || service.db == nil || state == nil || ownerID == "" {
		return
	}
	code := events.ReasonThreatWipe
	if victory {
		code = events.ReasonThreatDefeated
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   state.ID,
		OwnerUnitID: ownerID,
		Code:        code,
		Category:    events.CategoryLifecycle,
		Payload:     map[string]any{"threat": threat.Name, "tier": string(threat.Tier), "participants": participants},
		WorldID:     state.WorldID,
		RegionID:    threat.RegionID,
		Tick:        state.TurnState.Turn,
	}); err != nil {
		slog.Warn("record threat outcome failed (best-effort)", "threat", threat.ID, "victory", victory, "err", err)
	}
}

// recordRegionRavaged 在威胁未被挡下、且确有 region 归属时，写一条 ReasonRegionRavaged 世界事件（流程留痕，旁观者传播取材）。
// 仅当 threat.RegionID 非空（确有一片家乡遭殃）时才发——单人 elite 无 region 归属则不发（家园之难才有「她不在那座城，但她母亲在」）。
// best-effort：吞错只 log、绝不阻断败北结算。
func (service *Service) recordRegionRavaged(ctx context.Context, state *State, victimID string, threat Threat) {
	if service == nil || service.db == nil || state == nil || victimID == "" || threat.RegionID == "" {
		return
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   state.ID,
		OwnerUnitID: victimID,
		Code:        events.ReasonRegionRavaged,
		Category:    events.CategoryLifecycle,
		Payload:     map[string]any{"region": threat.RegionID, "threat": threat.Name, "tier": string(threat.Tier)},
		WorldID:     state.WorldID,
		RegionID:    threat.RegionID,
		Tick:        state.TurnState.Turn,
	}); err != nil {
		slog.Warn("record region ravaged failed (best-effort)", "region", threat.RegionID, "err", err)
	}
}

// propagateDefeatToRegion 在一次失败后把「家乡遭劫」按相关性扇出给「对这片家乡有牵挂的人」（§6：geo/relation/redline 锚被点亮）。
// 仅当 threat.RegionID 非空时触发：先写一条 ReasonRegionRavaged 世界事件，再复用 propagateRegionRavaged 一人一版扇出。
// regionName 用 region 的可读名（缺则回落 RegionID）。best-effort：吞错只 log、绝不阻断主结算。
func (service *Service) propagateDefeatToRegion(ctx context.Context, state *State, victim unit.Record, threat Threat, penaltyLayer int) {
	if service == nil || state == nil || threat.RegionID == "" || strings.TrimSpace(victim.ID) == "" {
		return
	}
	service.recordRegionRavaged(ctx, state, victim.ID, threat)
	regionName := threat.RegionID // region 可读名暂用 ID（无 region 名表时的稳定回退）；接 region 化后可替换为显示名。
	if _, err := service.propagateRegionRavaged(ctx, state, victim, regionName, penaltyLayer); err != nil {
		slog.Warn("propagate region ravaged failed (best-effort)", "region", threat.RegionID, "victim", victim.ID, "err", err)
	}
}

// recordAllyDown 在一名队员倒下时，对其余在场队友各落一条「同伴倒下」士气挫伤（ReasonThreatAllyDown，经 Mutator 改 morale）。
// 这是状态变更（受保护字段 morale）——经 status.Mutator 留痕，绝不直写。best-effort：单条失败只 log、不中断。
// 返回更新后的队友记录（就地写回 ms.rec）。
func (service *Service) recordAllyDown(ctx context.Context, state *State, members []*memberCombatState, downID string, threatName string) {
	if service == nil || service.mutator == nil || state == nil {
		return
	}
	for _, ms := range members {
		if ms == nil || ms.rec == nil || ms.rec.ID == downID {
			continue
		}
		if ms.status != "contributed" { // 已撤退/已倒下者不再叠加目睹挫伤
			continue
		}
		res, err := service.mutator.Apply(ctx, status.Mutation{
			UnitID:     ms.rec.ID,
			Turn:       state.TurnState.Turn,
			Field:      status.FieldMorale,
			Delta:      -0.12,
			ReasonCode: events.ReasonThreatAllyDown,
			ReasonText: fmt.Sprintf("并肩的人在与%s的搏斗中倒下，她心头一沉", threatName),
			Actors:     []string{ms.rec.ID},
			// §7.3 events 作用域双写：同伴倒下士气挫伤随会话世界三键留痕。
			Scope: mutationScopeFromState(state),
		})
		if err != nil {
			slog.Warn("record ally down morale hit failed (best-effort)", "unit", ms.rec.ID, "err", err)
			continue
		}
		*ms.rec = res.Record
	}
}

// ResolveEliteEncounter 跑通一次单人 elite 遭遇的完整确定性链路。
// 注意：威胁为内存态，结算用 combat_roll 确定性掷骰；角色 HP/士气/钱包变更全部经 status.Mutator 留痕。
func (service *Service) ResolveEliteEncounter(ctx context.Context, state *State, actor *unit.Record, threat Threat) (EliteEncounterResult, error) {
	result := EliteEncounterResult{ThreatID: threat.ID, Outcome: "down"}
	if service == nil || service.mutator == nil || state == nil || actor == nil {
		return result, fmt.Errorf("resolve elite encounter: missing dependencies")
	}

	// 威胁浮现留痕（ReasonThreatEmerged，best-effort）。
	service.recordThreatEmerged(ctx, state, actor.ID, threat)

	// 合成威胁的伪单位记录，供 combat_roll 取确定性随机。
	elite := unit.Record{ID: "threat:" + threat.ID}
	elite.Status.Attack = threat.Attack
	elite.Status.Defense = threat.Defense
	eliteHP := threat.HPPool
	turn := state.TurnState.Turn

	var contribution encounter.Contribution
	for round := 1; round <= eliteMaxRounds; round++ {
		result.Rounds = round

		// 反射护栏（L1）：HP 危急先保命，零 LLM。
		if actor.Status.HP < eliteFleeHP {
			result.Outcome = "fled"
			break
		}

		// 角色攻击威胁（确定性命中 + 伤害）。
		if combatActionRoll(*state, *actor, elite, "elite_atk:r"+strconv.Itoa(round)) >= eliteMissChance {
			dmg := eliteDamage(actor.Status.Attack, threat.Defense)
			eliteHPBefore := eliteHP
			eliteHP -= dmg
			result.DamageDealt += dmg
			contribution.Damage += float64(dmg)
			// ① 关键救场（Clutch）进 Score（设计 §5「关键救场」1.2 权重的数据来源）：
			//   - 终结一击：这一下把威胁从 >0 打到 ≤0（最靠近它心脏的那一击）；
			//   - 濒死反杀：自身归一化 HP 落在**撤退线之上的濒死带**（≤eliteNearDeathFraction，0.375）却仍打出有效输出且
			//     未当场倒下（不退反进的孤勇）。判在撤退线之上是因为反射护栏会在撤退线之下当场撤退、永不到此分支（见 const 注释）。
			if encounter.IsFinalBlow(eliteHPBefore, eliteHP) {
				contribution = encounter.MarkClutch(contribution, encounter.ClutchFinalBlow)
			}
			selfHPFraction := float64(actor.Status.HP) / float64(eliteHPMax)
			if selfHPFraction <= eliteNearDeathFraction && dmg > 0 && actor.Status.HP > 0 {
				contribution = encounter.MarkClutch(contribution, encounter.ClutchNearDeathReversal)
			}
		}
		if eliteHP <= 0 {
			result.Outcome = "defeated"
			break
		}

		// 威胁反扑（确定性命中 + 伤害，经 status.Mutator 落到角色 HP 并留痕）。
		if combatActionRoll(*state, elite, *actor, "elite_strike:r"+strconv.Itoa(round)) >= eliteMissChance {
			dmg := eliteDamage(threat.Attack, actor.Status.Defense)
			reasonCode := events.ReasonCombatHit
			if actor.Status.HP-dmg <= 0 {
				reasonCode = events.ReasonCombatDown
			}
			// 注意：威胁是合成伪单位、非 units 行，不能进 Actors（events.actor_unit_id 有 FK）；
			// 威胁身份记在 ReasonText/Location，事件 actor 落回角色自身。
			res, err := service.applyEliteMutation(ctx, status.Mutation{
				UnitID:     actor.ID,
				Turn:       turn,
				Field:      status.FieldHP,
				Delta:      -float64(dmg),
				ReasonCode: reasonCode,
				ReasonText: fmt.Sprintf("%s 在搏斗中伤到了她", threat.Name),
				Location:   threat.ID,
			})
			if err != nil {
				return result, fmt.Errorf("apply elite strike: %w", err)
			}
			*actor = res.Record
			result.DamageTaken += dmg
			contribution.Tank += float64(dmg)
		}
		if actor.Status.HP <= 0 {
			result.Outcome = "down"
			break
		}
	}

	result.Contribution = encounter.ContributionScore(contribution)

	if result.Outcome == "defeated" {
		if err := service.grantEliteLoot(ctx, state, actor, threat, result.Contribution, &result); err != nil {
			return result, err
		}
		// 讨平凶险留痕（ReasonThreatDefeated，best-effort）。
		service.recordThreatOutcome(ctx, state, actor.ID, threat, true, []string{actor.ID})
		// ③ Clutch→Legacy 闭环（§5）：这一仗里若用随身装备完成过关键救场（HadClutch），评估是否弹「要不要刻成传家物」待决策卡。
		service.surfaceLegacyUpgradeFromClutch(ctx, state, actor, threat, contribution)
		result.InboxCard = fmt.Sprintf("她砍翻了那头%s，带着一身伤回来了。%s", threat.Name, lootBlurb(result.Awards))
	} else {
		layer, err := service.applyDefeatPenalty(ctx, state, actor, threat)
		if err != nil {
			return result, err
		}
		result.PenaltyLayer = layer
		// ② 装备耐久（§6 层1 GEAR_DAMAGED）：败北/撤退后随身家伙折损（pinned 传家宝豁免、耐久 floor=1 不破坏，best-effort）。
		service.degradeParticipantGear(ctx, state, actor, threat, true, result.Rounds)
		verb := "没能拦住"
		if result.Outcome == "fled" {
			verb = "终究避开了"
		}
		result.InboxCard = fmt.Sprintf("她%s那头%s，伤得不轻，心气也低了一截——但她还在，命还攥在自己手里。", verb, threat.Name)
		// 失败后果向旁观者传播（一人一版，best-effort）：在乎她的人按相关性各收到一版「你挂念的人败了」。
		_, _ = service.propagateThreatFailure(ctx, state, *actor, threat.Name, result.PenaltyLayer)
		// 家乡遭劫扇出（§6 region_ravaged，仅 threat 有 region 归属时）：「她不在那座城，但她母亲在」。
		service.propagateDefeatToRegion(ctx, state, *actor, threat, result.PenaltyLayer)
	}

	appendLog(state, "threat_encounter", result.InboxCard, actor.ID, elite.ID)

	// 把遭遇结果按相关性路由进命运收件箱（她自己的事，相关性由重要度/情绪决定）。
	importance, emotion := eliteOutcomeWeight(result.Outcome)
	if _, err := service.SurfaceFateEvent(ctx, state.ID, actor, FateEvent{
		ActorID:       actor.ID,
		TargetID:      actor.ID,
		ReasonCode:    events.ReasonInboxHighlight,
		Importance:    importance,
		EmotionWeight: emotion,
		Summary:       result.InboxCard,
	}); err != nil {
		return result, err
	}
	return result, nil
}

// ---- 野外Boss / 组队遭遇 ----

const fieldBossMinMeaningful = 1.0 // 贡献低于此值的纯蹭场者不进排他件排名（反白嫖），仍可分 common。

// FieldBossMemberOutcome 某队员在野外Boss遭遇中的结算。
type FieldBossMemberOutcome struct {
	UnitID       string
	Outcome      string // contributed / fled / down
	DamageDealt  int
	DamageTaken  int
	Contribution float64
	Awards       []encounter.Award
	PenaltyLayer int    // 失败时经分级闸落地的后果层，0=未触发
	InboxCard    string // 祖魂语气命运卡
}

// FieldBossResult 野外Boss/组队遭遇的整体结算。
type FieldBossResult struct {
	ThreatID string
	Victory  bool
	Rounds   int
	Members  []FieldBossMemberOutcome
}

// memberCombatState 是单个队员在战斗循环内的可变态（内部用）。
type memberCombatState struct {
	rec     *unit.Record
	contrib encounter.Contribution
	dealt   int
	taken   int
	status  string // contributed / fled / down
}

// TriggerFieldBoss 为一支队伍生成一头与其总战力相称的野外Boss并跑通组队遭遇（供 API 触发）。真实动作。
func (service *Service) TriggerFieldBoss(ctx context.Context, sessionID string, unitIDs []string) (FieldBossResult, error) {
	if service == nil || service.units == nil {
		return FieldBossResult{}, fmt.Errorf("trigger field boss: missing dependencies")
	}
	if len(unitIDs) == 0 {
		return FieldBossResult{}, fmt.Errorf("trigger field boss: empty party")
	}
	party := make([]*unit.Record, 0, len(unitIDs))
	for _, id := range unitIDs {
		rec, err := service.units.GetByID(ctx, id)
		if err != nil {
			return FieldBossResult{}, fmt.Errorf("load party member %s: %w", id, err)
		}
		member := rec
		party = append(party, &member)
	}
	state := State{ID: sessionID}
	return service.ResolveFieldBoss(ctx, &state, party, scaledFieldBoss(party))
}

// scaledFieldBoss 生成一头与队伍总战力相称、协力可破的野外Boss。
func scaledFieldBoss(party []*unit.Record) Threat {
	totalAtk, maxDef := 0, 1
	for _, p := range party {
		if p == nil {
			continue
		}
		totalAtk += maxInt(1, p.Status.Attack)
		if p.Status.Defense > maxDef {
			maxDef = p.Status.Defense
		}
	}
	hp := totalAtk * 3 // 全员命中约 3 回合可破
	if hp < 60 {
		hp = 60
	}
	bossAtk := maxDef + 4 // 单击有威胁但不致命，逼出承伤与撤退抉择
	return Threat{
		ID:       "fieldboss_" + uuid.NewString(),
		Name:     "蛮荒巨蜥",
		Tier:     ThreatTierFieldBoss,
		Power:    totalAtk * 5,
		Attack:   bossAtk,
		Defense:  maxInt(2, totalAtk/(len(party)*4+1)),
		HPPool:   hp,
		Severity: 65,
		Loot: []encounter.LootItem{
			{ID: "gold", Rarity: encounter.Common, Quantity: 30 * len(party)},
			{ID: "boss_relic", Rarity: encounter.Epic, Quantity: 1}, // 唯一排他件，走 arbitration 仲裁归属
		},
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// pickLowestHPActive 确定性选出当前血量最低的活跃队员（平局按 UnitID 升序），作为 Boss 的攻击目标。
func pickLowestHPActive(members []*memberCombatState) *memberCombatState {
	var target *memberCombatState
	for _, ms := range members {
		if ms.status != "contributed" {
			continue
		}
		if target == nil || ms.rec.Status.HP < target.rec.Status.HP ||
			(ms.rec.Status.HP == target.rec.Status.HP && ms.rec.ID < target.rec.ID) {
			target = ms
		}
	}
	return target
}

// fieldBossMemberWeight 把队员结局映射为命运重要度与情绪强度（驱动收件箱三档路由）。
func fieldBossMemberWeight(victory bool, memberOutcome string) (int, float64) {
	switch {
	case memberOutcome == "down":
		return 8, -0.7 // 被打倒，强相关
	case victory:
		return 5, 0.4 // 协力得胜
	case memberOutcome == "fled":
		return 6, -0.4
	default:
		return 6, -0.5 // 战败仍在场
	}
}

// ResolveFieldBoss 跑通一次组队野外Boss遭遇的完整确定性链路：
// 多回合 combat_roll 消耗战（队员轮流攻击、Boss 专打残血者、危急者反射撤退）→ 胜利按贡献分赃(含 epic 仲裁)
// /失败各自经分级闸降级 → 全程经 status.Mutator 留痕 → 每人一张祖魂语气命运卡入收件箱。
func (service *Service) ResolveFieldBoss(ctx context.Context, state *State, party []*unit.Record, threat Threat) (FieldBossResult, error) {
	result := FieldBossResult{ThreatID: threat.ID}
	if service == nil || service.mutator == nil || state == nil {
		return result, fmt.Errorf("resolve field boss: missing dependencies")
	}

	members := make([]*memberCombatState, 0, len(party))
	for _, p := range party {
		if p != nil {
			members = append(members, &memberCombatState{rec: p, status: "contributed"})
		}
	}
	if len(members) == 0 {
		return result, fmt.Errorf("resolve field boss: empty party")
	}

	// 威胁浮现留痕（ReasonThreatEmerged，best-effort）：owner 取首位队员作结算代表。
	service.recordThreatEmerged(ctx, state, members[0].rec.ID, threat)

	boss := unit.Record{ID: "threat:" + threat.ID}
	boss.Status.Attack = threat.Attack
	boss.Status.Defense = threat.Defense
	bossHP := threat.HPPool
	turn := state.TurnState.Turn

	for round := 1; round <= eliteMaxRounds; round++ {
		result.Rounds = round

		anyActive := false
		for _, ms := range members {
			if ms.status != "contributed" {
				continue
			}
			if ms.rec.Status.HP < eliteFleeHP { // 反射护栏：危急先撤，零 LLM
				ms.status = "fled"
				continue
			}
			anyActive = true
			if combatActionRoll(*state, *ms.rec, boss, fmt.Sprintf("fb_atk:%s:r%d", ms.rec.ID, round)) >= eliteMissChance {
				dmg := eliteDamage(ms.rec.Status.Attack, threat.Defense)
				bossHP -= dmg
				ms.dealt += dmg
				ms.contrib.Damage += float64(dmg)
			}
		}
		if bossHP <= 0 {
			result.Victory = true
			break
		}
		if !anyActive {
			break // 全员撤退/倒下
		}

		// Boss 反扑：专打当前残血的活跃队员（承伤计入其贡献）。
		if target := pickLowestHPActive(members); target != nil {
			if combatActionRoll(*state, boss, *target.rec, fmt.Sprintf("fb_strike:r%d", round)) >= eliteMissChance {
				dmg := eliteDamage(threat.Attack, target.rec.Status.Defense)
				reasonCode := events.ReasonCombatHit
				if target.rec.Status.HP-dmg <= 0 {
					reasonCode = events.ReasonCombatDown
				}
				res, err := service.mutator.Apply(ctx, status.Mutation{
					UnitID:     target.rec.ID,
					Turn:       turn,
					Field:      status.FieldHP,
					Delta:      -float64(dmg),
					ReasonCode: reasonCode,
					ReasonText: fmt.Sprintf("%s 一击砸在她身上", threat.Name),
					Location:   threat.ID,
				})
				if err != nil {
					return result, fmt.Errorf("apply field boss strike: %w", err)
				}
				*target.rec = res.Record
				target.taken += dmg
				target.contrib.Tank += float64(dmg)
				if target.rec.Status.HP <= 0 {
					target.status = "down"
					// 同伴倒下：在场队友各受一记士气挫伤（ReasonThreatAllyDown，经 Mutator，best-effort）。
					service.recordAllyDown(ctx, state, members, target.rec.ID, threat.Name)
				}
			}
		}
	}

	if err := service.settleFieldBoss(ctx, state, threat, members, &result); err != nil {
		return result, err
	}
	return result, nil
}

// settleFieldBoss 在战斗循环后完成分赃/惩罚、留痕与命运收件箱投递。
func (service *Service) settleFieldBoss(ctx context.Context, state *State, threat Threat, members []*memberCombatState, result *FieldBossResult) error {
	turn := state.TurnState.Turn
	participants := make([]encounter.Participant, 0, len(members))
	for _, ms := range members {
		participants = append(participants, encounter.Participant{UnitID: ms.rec.ID, Score: encounter.ContributionScore(ms.contrib)})
	}

	awardsByUnit := map[string][]encounter.Award{}
	goldByUnit := map[string]int{}
	if result.Victory {
		key := fmt.Sprintf("%s|fieldboss|%s|%d", state.ID, threat.ID, turn)
		allAwards := encounter.AllocateLoot(key, threat.Loot, participants, fieldBossMinMeaningful)
		for _, a := range allAwards {
			awardsByUnit[a.UnitID] = append(awardsByUnit[a.UnitID], a)
			if a.ItemID == "gold" {
				goldByUnit[a.UnitID] += a.Quantity
			}
		}
		// H2：排他件(boss_relic 等)若有 ≥2 争夺者，给胜/败方各留可审计零和争夺事件（仅 WorldID 非空时；best-effort）。
		scoreByUnit := make(map[string]float64, len(participants))
		for _, p := range participants {
			scoreByUnit[p.UnitID] = p.Score
		}
		service.recordExclusiveContestOutcomes(ctx, state.ID, state.WorldID, turn, allAwards, scoreByUnit)
		// 排他件经 arbitration 仲裁归属后，对每名胜者写 ReasonEconomyLootArbitrated（带 Scope.WorldID/Tick，与 liveops 审计对齐）。
		service.recordArbitratedLoot(ctx, state, threat, turn, allAwards, scoreByUnit)
	}

	for _, ms := range members {
		outcome := FieldBossMemberOutcome{
			UnitID:       ms.rec.ID,
			Outcome:      ms.status,
			DamageDealt:  ms.dealt,
			DamageTaken:  ms.taken,
			Contribution: encounter.ContributionScore(ms.contrib),
			Awards:       awardsByUnit[ms.rec.ID],
		}

		if result.Victory {
			if gold := goldByUnit[ms.rec.ID]; gold > 0 {
				res, err := service.mutator.Apply(ctx, status.Mutation{
					UnitID:     ms.rec.ID,
					Turn:       turn,
					Field:      status.FieldWallet,
					Delta:      float64(gold),
					ReasonCode: events.ReasonEconomyLoot,
					ReasonText: fmt.Sprintf("讨平%s后的分成", threat.Name),
					Actors:     []string{ms.rec.ID},
				})
				if err != nil {
					return fmt.Errorf("grant field boss loot: %w", err)
				}
				*ms.rec = res.Record
			}
			outcome.InboxCard = fieldBossVictoryCard(threat, ms.status, outcome.Awards)
		} else {
			layer, err := service.applyDefeatPenalty(ctx, state, ms.rec, threat)
			if err != nil {
				return err
			}
			outcome.PenaltyLayer = layer
			// ② 装备耐久（§6 层1 GEAR_DAMAGED）：败北队员的随身家伙折损（pinned 豁免、耐久 floor=1，best-effort）。
			service.degradeParticipantGear(ctx, state, ms.rec, threat, true, result.Rounds)
			outcome.InboxCard = fmt.Sprintf("她和同伴没能拿下那头%s，伤痕累累退了下来——但人都还在，命还攥在自己手里。", threat.Name)
			// 每个战败队员各向其牵挂者扇出一版失败后果（best-effort）。
			_, _ = service.propagateThreatFailure(ctx, state, *ms.rec, threat.Name, outcome.PenaltyLayer)
			// 家乡遭劫扇出（§6 region_ravaged，仅 threat 有 region 归属时）。
			service.propagateDefeatToRegion(ctx, state, *ms.rec, threat, outcome.PenaltyLayer)
		}

		appendLog(state, "field_boss_encounter", outcome.InboxCard, ms.rec.ID, threat.ID)
		importance, emotion := fieldBossMemberWeight(result.Victory, ms.status)
		if _, err := service.SurfaceFateEvent(ctx, state.ID, ms.rec, FateEvent{
			ActorID:       ms.rec.ID,
			TargetID:      ms.rec.ID,
			ReasonCode:    events.ReasonInboxHighlight,
			Importance:    importance,
			EmotionWeight: emotion,
			Summary:       outcome.InboxCard,
		}); err != nil {
			return err
		}
		result.Members = append(result.Members, outcome)
	}

	// 整体结局留痕（ReasonThreatDefeated / ReasonThreatWipe，best-effort）：owner 取首位结算代表，participants 记全员。
	if len(members) > 0 {
		ids := make([]string, 0, len(members))
		for _, ms := range members {
			ids = append(ids, ms.rec.ID)
		}
		service.recordThreatOutcome(ctx, state, members[0].rec.ID, threat, result.Victory, ids)
	}
	return nil
}

// recordArbitratedLoot 对每件经 arbitration 仲裁决出归属的排他件（AwardWon）写一条 ReasonEconomyLootArbitrated。
// 必带 Scope.WorldID/Tick（否则被 liveops 审计的 world_id/tick 过滤掉）；worldID 为空（未接多世界）则不发（审计无从检索）。
// 反 P2W：payload 只记 item/score（贡献），绝不含 wallet/billing/付费档。best-effort：吞错只 log，绝不阻断分赃。
func (service *Service) recordArbitratedLoot(ctx context.Context, state *State, threat Threat, tick int, awards []encounter.Award, scoreByUnit map[string]float64) {
	if service == nil || service.db == nil || state == nil || state.WorldID == "" {
		return // 未接入多世界 → 审计无从检索，不发（零状态变化）
	}
	for _, a := range awards {
		if a.Reason != encounter.AwardWon {
			continue // 只对仲裁胜者（排他件归属）留痕；可分件(AwardShare)/补偿(AwardConsolation)不属仲裁归属
		}
		if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
			SessionID:   state.ID,
			OwnerUnitID: a.UnitID,
			Code:        events.ReasonEconomyLootArbitrated,
			Category:    events.CategoryEconomy,
			Payload:     map[string]any{"item": a.ItemID, "score": scoreByUnit[a.UnitID], "threat": threat.Name},
			WorldID:     state.WorldID,
			RegionID:    threat.RegionID,
			Tick:        tick,
		}); err != nil {
			slog.Warn("record arbitrated loot failed (best-effort)", "unit", a.UnitID, "item", a.ItemID, "err", err)
		}
	}
}

// fieldBossVictoryCard 渲染胜利方某队员的祖魂语气命运卡（含其分得的战利品）。
func fieldBossVictoryCard(threat Threat, memberOutcome string, awards []encounter.Award) string {
	gotRelic := false
	for _, a := range awards {
		if a.ItemID == "boss_relic" && a.Reason == encounter.AwardWon {
			gotRelic = true
		}
	}
	prefix := fmt.Sprintf("她和同伴合力讨平了那头%s。", threat.Name)
	if memberOutcome == "down" {
		prefix = fmt.Sprintf("她在讨伐%s时倒下了，是同伴把这一仗打完、也把她拖了回来。", threat.Name)
	}
	if gotRelic {
		return prefix + "那件众人觊觎的遗物，最后落在了她手里。" + lootBlurb(awards)
	}
	return prefix + lootBlurb(awards)
}

// eliteOutcomeWeight 把遭遇结局映射为命运重要度与情绪强度（驱动收件箱三档路由）。
func eliteOutcomeWeight(outcome string) (int, float64) {
	switch outcome {
	case "down":
		return 8, -0.7 // 濒死，强相关，应升级待决策
	case "fled":
		return 6, -0.4
	default: // defeated
		return 5, 0.4
	}
}

// recordExclusiveContestOutcomes 在排他件经 arbitration 决出归属后，给胜者/败者各留一条可审计的零和争夺事件
// （H2 producer 侧补缺：排他仲裁胜负不落库 → 审计 queryOutcomes 假阴）。约定（与 liveops 审计闭合）：
//   - 胜者发 events.ReasonCrossContestWin、每个败者发 events.ReasonCrossContestLose；
//   - 必须显式设 Scope.WorldID / Scope.Tick——否则被审计的 world_id/tick 过滤掉（必须项）。
//   - **仅当一件排他件有 ≥2 个合格争夺者**时才发（AllocateLoot 会给排他件的胜者发 AwardWon、其余争夺者发
//     AwardConsolation；单参与者只有 AwardWon、无 consolation → 无争夺、不发）。
//   - 反 P2W：payload 只记 actor+score（参与人数），**绝不含 wallet/billing/付费档**。
//   - worldID 为空（未接入多世界）→ 不发（审计按 world 检索，无世界归属即无意义；与既有 contest.go 同口径）。
//   - best-effort：吞错只 log，绝不阻断主结算。
//
// scoreByUnit 提供每个争夺者的 arbitration Score（贡献分），写进 payload 供审计/复算（只记贡献、非付费）。
func (service *Service) recordExclusiveContestOutcomes(
	ctx context.Context, sessionID string, worldID string, tick int,
	awards []encounter.Award, scoreByUnit map[string]float64,
) {
	if service == nil || service.db == nil || worldID == "" {
		return // 未接入多世界 → 审计无从检索，不发（零状态变化）
	}
	// 按排他件聚合：胜者(AwardWon) + 败者(AwardConsolation)。Common 件(AwardShare)不是排他争夺，跳过。
	type contestGroup struct {
		winner string
		losers []string
	}
	groups := map[string]*contestGroup{}
	order := make([]string, 0)
	for _, a := range awards {
		switch a.Reason {
		case encounter.AwardWon:
			g := groups[a.ItemID]
			if g == nil {
				g = &contestGroup{}
				groups[a.ItemID] = g
				order = append(order, a.ItemID)
			}
			g.winner = a.UnitID
		case encounter.AwardConsolation:
			g := groups[a.ItemID]
			if g == nil {
				g = &contestGroup{}
				groups[a.ItemID] = g
				order = append(order, a.ItemID)
			}
			g.losers = append(g.losers, a.UnitID)
		}
	}

	for _, itemID := range order {
		g := groups[itemID]
		// 单参与者（只有 winner、无 loser）= 无争夺 → 不发。
		if g.winner == "" || len(g.losers) == 0 {
			continue
		}
		// 胜者留痕。
		if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
			SessionID:   sessionID,
			OwnerUnitID: g.winner,
			Code:        events.ReasonCrossContestWin,
			Category:    events.CategoryLifecycle,
			Payload: map[string]any{
				"item": itemID, "score": scoreByUnit[g.winner], "contenders": len(g.losers) + 1,
			},
			WorldID: worldID,
			Tick:    tick,
		}); err != nil {
			slog.Warn("record cross contest win failed (best-effort)", "unit", g.winner, "item", itemID, "err", err)
		}
		// 每个败者各留一条。
		for _, loser := range g.losers {
			if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
				SessionID:   sessionID,
				OwnerUnitID: loser,
				Code:        events.ReasonCrossContestLose,
				Category:    events.CategoryLifecycle,
				Payload: map[string]any{
					"item": itemID, "score": scoreByUnit[loser], "contenders": len(g.losers) + 1,
				},
				WorldID: worldID,
				Tick:    tick,
			}); err != nil {
				slog.Warn("record cross contest lose failed (best-effort)", "unit", loser, "item", itemID, "err", err)
			}
		}
	}
}

// grantEliteLoot 按贡献分配掉落（单人=独享），货币类经 Mutator 落 Wallet 并留痕；排他件记入结果（入库为后续）。
func (service *Service) grantEliteLoot(ctx context.Context, state *State, actor *unit.Record, threat Threat, score float64, result *EliteEncounterResult) error {
	if len(threat.Loot) == 0 {
		return nil
	}
	key := fmt.Sprintf("%s|elite|%s|%d", state.ID, threat.ID, state.TurnState.Turn)
	awards := encounter.AllocateLoot(key, threat.Loot, []encounter.Participant{{UnitID: actor.ID, Score: score}}, 0)
	result.Awards = awards

	// H2：单人 elite 仅一名参与者、无排他争夺（AllocateLoot 不产 consolation）→ recordExclusiveContestOutcomes 自然不发。
	// 仍调用以保持「凡排他归属皆走同一留痕口径」的一致性（无争夺即零状态变化）。
	service.recordExclusiveContestOutcomes(ctx, state.ID, state.WorldID, state.TurnState.Turn,
		awards, map[string]float64{actor.ID: score})
	// 排他件归属（即便单人独得）经 ReasonEconomyLootArbitrated 留痕（带 Scope，worldID 空则自然不发）。
	service.recordArbitratedLoot(ctx, state, threat, state.TurnState.Turn, awards, map[string]float64{actor.ID: score})

	gold := 0
	for _, a := range awards {
		if a.UnitID != actor.ID {
			continue
		}
		if a.ItemID == "gold" {
			gold += a.Quantity
		}
	}
	if gold > 0 {
		res, err := service.applyEliteMutation(ctx, status.Mutation{
			UnitID:     actor.ID,
			Turn:       state.TurnState.Turn,
			Field:      status.FieldWallet,
			Delta:      float64(gold),
			ReasonCode: events.ReasonEconomyLoot,
			ReasonText: fmt.Sprintf("从%s身上得来的进项", threat.Name),
			Actors:     []string{actor.ID},
		})
		if err != nil {
			return fmt.Errorf("grant elite loot: %w", err)
		}
		*actor = res.Record
	}
	return nil
}

// applyDefeatPenalty 失败惩罚经后果分级闸降级（elite 候选层=1，恒落「可恢复」：士气重挫），经 Mutator 留痕。
//
// §4.3 provenance 强制接 PvE（与 fate.go:ResolveFateDecision 的不可逆 provenance 守门同构）：层2/层3 的「高代价/不可逆」
// 后果落地前，要求该角色携带**可解析前因链**（人格/记忆/红线/关系/压力之一）；无源则强制降到层1——绝不无源致残/致死。
// 层3 的「不可逆」额外仍受既有牵挂×在世天数硬锁（PenaltyCap）约束，层3 落地前还须再过 consent 降级路径（见 defeatLayerAfterConsent）。
func (service *Service) applyDefeatPenalty(ctx context.Context, state *State, actor *unit.Record, threat Threat) (int, error) {
	candidate := candidateDefeatLayer(threat.Tier)
	// 后果分级闸用**真实牵挂**：深牵挂+陪伴久的角色才可能挨更重的后果；萍水相逢的被硬锁在「可恢复」。
	care := service.ComputeAttachment(ctx, actor.ID, actor.Status.Loyalty, state.TurnState.Turn)
	layer := encounter.DegradePenalty(candidate, care, state.TurnState.Turn)

	// §4.3 provenance 闸：候选层≥2（高代价/不可逆）才校验前因——构造前因快照，无可解析前因即强制降到层1。
	provenanceZH := ""
	if candidate >= 2 {
		snap := service.buildDefeatProvenanceSnapshot(ctx, *state, actor)
		if snapshotHasResolvableCause(snap) {
			provenanceZH = deriveDefeatProvenance(snap, threat) // 可追溯凭证（写进命运卡 payload）
		} else if layer >= 2 {
			layer = 1 // 无源 → 绝不无源致残/致死，硬降到「可恢复」
		}
	}
	// 层3「不可逆」落地前必过 consent 降级路径（§6：ConsentTierFor(3)=RequiresConsent）：
	// 不直接落地不可逆后果，而是升级一张「要不要让她退？」的 RequiresConsent 待决策卡，在玩家/角色同意前挂 pending；
	// 当下只先落地可恢复的层2 代价（士气+忠诚），不可逆的残废由 consent 同意 / 超时兜底（degradeLayer3OnTimeout）决定。
	// **绝不在玩家离线时把角色阵亡**——超时只降级为残废（COMBAT_MAIMED 改 hp），永不 FELL_IN_DEFEAT。
	if layer >= 3 {
		layer = service.defeatLayerAfterConsent(ctx, state, actor, threat, provenanceZH)
	}

	// 失败惩罚的士气/忠诚挫伤改用 PvE 专属败北分级码（修审计假阴：通用 EMOTION_TRAUMA 与 PvE 败北混淆）。
	// 字段级 clamp 与影响域以各码 StatDomains 为准（D1=morale；D2=morale+loyalty；D3 流程广播另走，此处士气仍按 D2 落）。
	moraleCode := defeatReasonCodeForLayer(layer)
	res, err := service.applyEliteMutation(ctx, status.Mutation{
		UnitID:     actor.ID,
		Turn:       state.TurnState.Turn,
		Field:      status.FieldMorale,
		Delta:      -defeatMoraleHitForLayer(layer), // 代价随分级层放大（绝不触碰 lives，D0-D3 保护）
		ReasonCode: moraleCode,
		ReasonText: fmt.Sprintf("没能赢下那头%s，心气受了挫", threat.Name),
		Actors:     []string{actor.ID},
	})
	if err != nil {
		return layer, fmt.Errorf("apply defeat penalty: %w", err)
	}
	*actor = res.Record

	// 层2/层3：忠诚也受挫（D2 影响域含 loyalty）——一败再败，她对你的信靠也松动了。绝不触碰 lives。
	if layer >= 2 {
		if res2, err := service.applyEliteMutation(ctx, status.Mutation{
			UnitID:     actor.ID,
			Turn:       state.TurnState.Turn,
			Field:      status.FieldLoyalty,
			Delta:      -0.10,
			ReasonCode: events.ReasonDefeatScarred,
			ReasonText: fmt.Sprintf("败给那头%s的代价，压在了她心上", threat.Name),
			Actors:     []string{actor.ID},
		}); err == nil {
			*actor = res2.Record
		} else {
			slog.Warn("apply defeat loyalty hit failed (best-effort)", "unit", actor.ID, "err", err)
		}
	}

	// provenance 凭证随命运卡可追溯落地（§4.3）：把该败北的「为什么会挨到层≥2」作为归因句路由进收件箱命运卡的尾注。
	// best-effort：失败只记 log、绝不影响惩罚结算（SurfaceFateEvent 在 ResolveEliteEncounter/settleFieldBoss 后另调）。
	if provenanceZH != "" {
		service.recordDefeatProvenance(ctx, state.ID, actor, threat, layer, provenanceZH)
	}
	return layer, nil
}

// degradeParticipantGear 在一次失败/撤退/苦战后对一名参战角色的装备栏 + 背包逐件做确定性耐久衰减（§6 层1 GEAR_DAMAGED）。
// 每件 ItemStack 的耐久经 item.DegradeDurability（无限耐久/pinned 传家宝/非正扣减一律不动；衰减硬锁在 floor=1 不归零=不破坏）；
// 实际发生衰减（r.Changed）才写回 stack.Durability + units.Save + events.EmitProcessEvent 落 ReasonGearDamaged。
// 耐久**非受保护字段**——不走 status.Mutator，留痕走流程事件旁路（与 item.DegradeDurability 注释一致）。
// best-effort：任一步失败只 log、绝不阻断败北结算主链路；确定性（扣减量只由 lost/rounds 派生，非随机、非钱包）。
// 返回实际折损的件数（供 log/可测）。
func (service *Service) degradeParticipantGear(ctx context.Context, state *State, actor *unit.Record, threat Threat, lost bool, rounds int) int {
	if service == nil || service.units == nil || state == nil || actor == nil {
		return 0
	}
	loss := item.DefeatDurabilityLoss(lost, rounds)
	if loss <= 0 {
		return 0 // 零回合参战（未实际交手）→ 无磨损。
	}

	changed := 0
	// 装备栏（按 slot 键升序，map 遍历无序 → 排序保确定性）。
	slots := make([]string, 0, len(actor.Inventory.Equipment))
	for slot := range actor.Inventory.Equipment {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	for _, slot := range slots {
		stack := actor.Inventory.Equipment[slot]
		r := item.DegradeDurability(stack.Durability, loss, stack.Pinned)
		if !r.Changed {
			continue
		}
		stack.Durability = r.Durability
		actor.Inventory.Equipment[slot] = stack
		changed++
	}
	// 背包（按下标升序）。
	for i := range actor.Inventory.Backpack {
		stack := actor.Inventory.Backpack[i]
		r := item.DegradeDurability(stack.Durability, loss, stack.Pinned)
		if !r.Changed {
			continue
		}
		stack.Durability = r.Durability
		actor.Inventory.Backpack[i] = stack
		changed++
	}
	if changed == 0 {
		return 0
	}

	if err := service.units.Save(ctx, *actor); err != nil {
		slog.Warn("save gear durability degrade failed (best-effort)", "unit", actor.ID, "err", err)
		return 0
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   state.ID,
		OwnerUnitID: actor.ID,
		Code:        events.ReasonGearDamaged,
		Category:    events.CategoryEconomy,
		Payload:     map[string]any{"threat": threat.Name, "tier": string(threat.Tier), "items_damaged": changed, "lost": lost},
		WorldID:     state.WorldID,
		RegionID:    threat.RegionID,
		Tick:        state.TurnState.Turn,
	}); err != nil {
		slog.Warn("record gear damaged failed (best-effort)", "unit", actor.ID, "err", err)
	}
	return changed
}

// surfaceLegacyUpgradeFromClutch 在一场胜利里若用随身装备完成过关键救场（encounter.HadClutch），评估并按需弹出
// 「要不要把它刻成传家物」的待决策卡（设计 §5 Clutch→Legacy 闭环的入口）。
//   - 选一件「陪她出生入死」的代表装备（优先武器槽，再退首个非空装备/背包遗物候选）；已是永久锚则跳过（幂等）；
//   - 跑 encounter.QualifiesForLegacyUpgrade（要求在世持有者 + 非传家物 + Clutch≥1 + 羁绊≥MinLegacyBondTurns）；
//   - 过则经 SurfaceFateEvent 弹一张 ReasonLegacyBequeathed 待决策卡（绑定 LegacyItemID），玩家 confirm→ ResolveFateDecision
//     调 upgradeItemToLegacy 落标记。羁绊回合数用该角色真实在世天数代理（无 per-item 取得回合表，保守地以「活了多久」为上界）。
//
// best-effort：吞错只 log、绝不阻断胜利结算。无 Clutch / 无合格装备 / 未达标即静默 no-op。
func (service *Service) surfaceLegacyUpgradeFromClutch(ctx context.Context, state *State, actor *unit.Record, threat Threat, contribution encounter.Contribution) {
	if service == nil || state == nil || actor == nil {
		return
	}
	if !encounter.HadClutch(contribution) {
		return // 这一仗没有关键救场 → 不触发升级评估
	}
	itemID, alreadyAnchor, ok := pickClutchLegacyItem(actor)
	if !ok || alreadyAnchor {
		return // 无随身装备 / 已是永久锚（幂等 no-op）
	}
	bondTurns := state.TurnState.Turn - actor.Social.BornTurn // 真实在世天数代理羁绊时长（保守上界）
	if bondTurns < 0 {
		bondTurns = 0
	}
	clutchCount := int(contribution.Clutch + 0.5) // Clutch 折算分≈救场次数（四舍五入；HadClutch 已保证 >0）
	if clutchCount < 1 {
		clutchCount = 1
	}
	q := encounter.LegacyUpgradeQuery{
		Trigger:       encounter.TriggerClutch,
		AlreadyLegacy: false, // pickClutchLegacyItem 已排除永久锚；IsLegacy 但未满三标记者仍可走升级补齐
		HasOwner:      true,
		BondTurns:     bondTurns,
		ClutchCount:   clutchCount,
	}
	if !encounter.QualifiesForLegacyUpgrade(q) {
		return // 羁绊太浅/不够格 → 不弹卡（防一捡到手就因偶然 clutch 弹卡）
	}
	itemName := displayItemName(itemID)
	card := fmt.Sprintf(
		"她带着这把陪她出生入死的%s，在与那头%s的死斗里救了场。要不要把它刻成传家之物——从此它认她、也只认她的血脉？",
		itemName, threat.Name,
	)
	if _, err := service.SurfaceFateEvent(ctx, state.ID, actor, FateEvent{
		ActorID:       actor.ID,
		TargetID:      actor.ID,
		ReasonCode:    events.ReasonLegacyBequeathed,
		Importance:    9,   // 高重要度：确保自身事件路由进待决策（fateScore≥0.55），否则升级无从确认
		EmotionWeight: 0.6, // 偏正向强情绪（一桩值得郑重对待的羁绊时刻）
		Summary:       card,
		AttributionZH: fmt.Sprintf("这把%s陪她闯过了这一关", itemName),
		LegacyItemID:  itemID, // 绑定待升级装备 ID，confirm 时反查升级
	}); err != nil {
		slog.Warn("surface legacy upgrade card failed (best-effort)", "unit", actor.ID, "item", itemID, "err", err)
	}
}

// pickClutchLegacyItem 选一件「陪她出生入死」的代表装备作为传家物升级候选：优先武器槽，再退其余装备槽（按 slot 升序），
// 最后退背包首个具名/遗物物品。返回 (itemID, 是否已是永久锚, 是否找到)。空背包/无装备 → ("", false, false)。
// 确定性：map 遍历前按 slot 键排序。已是永久锚（三标记齐备）的装备直接报 alreadyAnchor=true，调用方据此跳过（幂等）。
func pickClutchLegacyItem(actor *unit.Record) (string, bool, bool) {
	if actor == nil {
		return "", false, false
	}
	isAnchor := func(s unit.ItemStack) bool {
		return item.IsPermanentAnchor(item.LegacyFlags{IsLegacy: s.IsLegacy, SoulBound: s.SoulBound, Pinned: s.Pinned})
	}
	// 武器槽优先。
	if w, ok := actor.Inventory.Equipment[itemSlotWeapon]; ok && w.ItemID != "" {
		return w.ItemID, isAnchor(w), true
	}
	// 其余装备槽（按 slot 键升序，确定性）。
	slots := make([]string, 0, len(actor.Inventory.Equipment))
	for slot := range actor.Inventory.Equipment {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	for _, slot := range slots {
		s := actor.Inventory.Equipment[slot]
		if s.ItemID == "" {
			continue
		}
		return s.ItemID, isAnchor(s), true
	}
	// 背包首个具名/遗物物品（陪伴感优先于消耗品）。
	for _, s := range actor.Inventory.Backpack {
		if s.ItemID == "" {
			continue
		}
		if s.IsLegacy || s.SoulBound || strings.TrimSpace(s.CustomName) != "" {
			return s.ItemID, isAnchor(s), true
		}
	}
	return "", false, false
}

// itemSlotWeapon 是武器装备槽键（与 item.SlotWeapon 同值；本包不直引 item.Slot 以免耦合，仅取其字符串值）。
const itemSlotWeapon = "weapon"

// buildDefeatProvenanceSnapshot 复用决策归因的生产链路（人格/压力/记忆/红线/关系）为败北后果构造前因快照。
// 与 prepareAttribution 同源（buildDecisionAttributionContext），保证「PvE 后果的前因」与「LLM 决策的前因」同一套真相源。
func (service *Service) buildDefeatProvenanceSnapshot(ctx context.Context, state State, actor *unit.Record) decision.Snapshot {
	snap, _ := service.buildDecisionAttributionContext(ctx, state, actor)
	return snap
}

// deriveDefeatProvenance 在前因快照非空时派生一句可追溯的败北归因（≤40 字，纯函数、无 LLM）。
// 每一节都取自真实数据（命中的前因类别 + 威胁名），绝不凭空编戏剧性——对齐 §5「意外但合理」的代码强制。
func deriveDefeatProvenance(snap decision.Snapshot, threat Threat) string {
	cause := dominantProvenanceCauseZH(snap)
	if cause == "" {
		return ""
	}
	return fmt.Sprintf("她栽在那头%s手里，%s", threat.Name, cause)
}

// recordDefeatProvenance 把败北的 provenance 凭证作为一条命运事件路由进该角色收件箱（高光/待决策含归因尾注）。
// best-effort：吞错只记 log，绝不阻断惩罚结算（载具是既有 SurfaceFateEvent，重要度按惩罚层升高）。
func (service *Service) recordDefeatProvenance(ctx context.Context, sessionID string, actor *unit.Record, threat Threat, layer int, provenanceZH string) {
	importance := 4 + layer // 层越高越牵动（层1=5、层2=6、层3=7）
	emotion := -0.3 - 0.2*float64(layer)
	if _, err := service.SurfaceFateEvent(ctx, sessionID, actor, FateEvent{
		ActorID:       actor.ID,
		TargetID:      actor.ID,
		ReasonCode:    events.ReasonEmotionTrauma,
		Importance:    importance,
		EmotionWeight: emotion,
		Summary:       fmt.Sprintf("她败给了那头%s", threat.Name),
		AttributionZH: provenanceZH, // 可追溯前因句，进命运卡 payload.attribution_zh
	}); err != nil {
		slog.Warn("record defeat provenance failed (best-effort)", "unit", actor.ID, "err", err)
	}
}

// defeatReasonCodeForLayer 把后果分级层映射为 PvE 专属败北留痕码（修审计假阴：败北不再混用通用 EMOTION_TRAUMA）。
//   - 层1=PVE_DEFEAT_D1（士气受挫）；层2=PVE_DEFEAT_D2（士气+忠诚双挫）；层3=PVE_DEFEAT_D2（士气仍按 D2 落，
//     层3 的「不可逆残废」另由 consent 路径经 COMBAT_MAIMED 落地，不在这条士气留痕里）。
func defeatReasonCodeForLayer(layer int) events.ReasonCode {
	if layer >= 2 {
		return events.ReasonDefeatScarred
	}
	return events.ReasonDefeatSetback
}

// layer3ConsentTTLTurns 是层3 consent 待决策的「超时回合数」窗口（确定性，不用墙钟）：占位标记里记下 deadline turn，
// 调用方（执行循环/regionrunner 的回合边界）以当前 turn ≥ deadline 判超时 → degradeLayer3OnTimeout 降级为残废。
const layer3ConsentTTLTurns = 6

// defeatLayerAfterConsent 是层3「不可逆」落地前的真 consent 降级路径（§6：ConsentTierFor(3)=RequiresConsent + 超时兜底）。
// 不直接落地不可逆后果：① 经 SurfaceFateEvent 升级一张 RequiresConsent 待决策卡（祖魂语气「要不要让她退？不回应=她会活下来
// 但永远瘸了」）；② 落一条可查询的 pending 标记（events，含 deadline_turn / degrade=maimed）供超时兜底；③ **当下只返回层2**
// （先落地可恢复的士气+忠诚代价），不可逆的残废留给 consent 同意 / 超时兜底，绝不在此处无同意施加不可逆，更绝不阵亡。
// best-effort：surfacing/标记失败只 log、绝不阻断惩罚结算（仍保守降到层2）。
func (service *Service) defeatLayerAfterConsent(ctx context.Context, state *State, actor *unit.Record, threat Threat, provenanceZH string) int {
	if service == nil || state == nil || actor == nil {
		return 2
	}
	consentCard := fmt.Sprintf(
		"你寄宿了她这许多日子……她在与那头%s的搏斗里受了致命的伤，却还在往人群里挡。她可能回不来了。——你听见了吗？要不要让她退？（不回应：她会活下来，但永远瘸了。）",
		threat.Name,
	)
	// ① RequiresConsent 待决策卡：用最高重要度/最强负情绪强制路由进待决策收件箱（fateScore 取≈1）。
	if _, err := service.SurfaceFateEvent(ctx, state.ID, actor, FateEvent{
		ActorID:       actor.ID,
		TargetID:      actor.ID,
		ReasonCode:    events.ReasonCombatDown, // 不可逆类（fateReasonIsIrreversibleClass）→ 强制走待决策守门
		Importance:    10,
		EmotionWeight: -0.9,
		Summary:       consentCard,
		AttributionZH: provenanceZH,
	}); err != nil {
		slog.Warn("surface layer3 consent card failed (best-effort)", "unit", actor.ID, "err", err)
	}
	// ② pending 标记（可查询）：记 consent_tier=requires_consent + deadline_turn + degrade=maimed，供超时兜底检索。
	service.recordLayer3ConsentPending(ctx, state, actor, threat)
	// ③ 当下只落地可恢复的层2（不可逆残废待 consent 同意 / 超时降级），绝不无同意施加不可逆、绝不阵亡。
	return 2
}

// recordLayer3ConsentPending 落一条可查询的层3 consent pending 标记（流程事件，不改状态）。
// payload 记 consent_tier(=RequiresConsent)、deadline_turn（当前 turn + TTL，确定性，超时判据）、degrade=maimed（绝不 fell）。
// best-effort：吞错只 log。
func (service *Service) recordLayer3ConsentPending(ctx context.Context, state *State, actor *unit.Record, threat Threat) {
	if service == nil || service.db == nil || state == nil || actor == nil {
		return
	}
	tier := relevance.ConsentTierFor(3) // = RequiresConsent
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   state.ID,
		OwnerUnitID: actor.ID,
		Code:        events.ReasonDefeatCrippled, // 层3 流程广播（StatDomains 空，纯留痕标注分级）
		Category:    events.CategoryLifecycle,
		Payload: map[string]any{
			"consent_tier":  string(tier),
			"deadline_turn": state.TurnState.Turn + layer3ConsentTTLTurns,
			"degrade":       "maimed", // 超时绝不阵亡，只降级为残废
			"threat":        threat.Name,
		},
		WorldID: state.WorldID,
		Tick:    state.TurnState.Turn,
	}); err != nil {
		slog.Warn("record layer3 consent pending failed (best-effort)", "unit", actor.ID, "err", err)
	}
}

// degradeLayer3OnTimeout 是层3 consent 的「超时兜底」：玩家/角色在 deadline 前未应答 → **自动降级为残废**
// （COMBAT_MAIMED 永久折损 hp，floored 在 >0，绝不 FELL_IN_DEFEAT 阵亡），并补一张祖魂语气回响卡。
// 供执行循环 / regionrunner 的回合边界调用（以当前 turn ≥ deadline_turn 判超时）。
// **异步致死防护核心**：无论玩家是否离线，超时只让她瘸，绝不让她死。返回是否真的落了残废。
// best-effort：吞错只 log、绝不阻断回合推进。
func (service *Service) degradeLayer3OnTimeout(ctx context.Context, state *State, actor *unit.Record, threatName string) (bool, error) {
	if service == nil || service.mutator == nil || state == nil || actor == nil {
		return false, fmt.Errorf("degrade layer3 on timeout: missing dependencies")
	}
	// 永久折损：扣一截 hp 但 floor 在 1 以上（COMBAT_MAIMED 改 hp，绝不触碰 lives_remaining）。
	const maimLoss = 25
	target := actor.Status.HP - maimLoss
	if target < 1 {
		target = 1 // 残废而非阵亡：HP 永不被这条降到 0/致死。
	}
	delta := float64(target - actor.Status.HP)
	res, err := service.applyEliteMutation(ctx, status.Mutation{
		UnitID:     actor.ID,
		Turn:       state.TurnState.Turn,
		Field:      status.FieldHP,
		Delta:      delta,
		ReasonCode: events.ReasonCombatMaimed,
		ReasonText: fmt.Sprintf("没人替她应那一声，她从与%s的搏斗里活了下来，却落下了一辈子的残", threatName),
		Actors:     []string{actor.ID},
	})
	if err != nil {
		return false, fmt.Errorf("apply maim on consent timeout: %w", err)
	}
	*actor = res.Record
	// 回响卡（best-effort）：把「她活下来了，只是永远瘸了」路由进收件箱。
	if _, err := service.SurfaceFateEvent(ctx, state.ID, actor, FateEvent{
		ActorID:       actor.ID,
		TargetID:      actor.ID,
		ReasonCode:    events.ReasonCombatMaimed,
		Importance:    9,
		EmotionWeight: -0.6,
		Summary:       fmt.Sprintf("你没回应那一声。于是她从那头%s手里活了下来——只是从此瘸了一条腿，再不能像从前那样跑了。", threatName),
	}); err != nil {
		slog.Warn("surface maim echo card failed (best-effort)", "unit", actor.ID, "err", err)
	}
	return true, nil
}

// degradeLayer3ConsentTimeoutsAtBoundary 是 Execution→Deployment 回合边界钩子（§6 层3 consent 超时降级）：
// 扫本会话所有「层3 consent pending 标记」（recordLayer3ConsentPending 落的 ReasonDefeatCrippled，payload.consent_tier=requires_consent），
// 对当前 turn ≥ deadline_turn 且尚未应答/尚未超时降级过的，自动调 degradeLayer3OnTimeout——COMBAT_MAIMED 永久折损 hp
// （floored≥1，**绝不触 lives_remaining、绝不 FELL_IN_DEFEAT**）+ 回响卡。
//
// 幂等：每条 pending 由其 event id 唯一标识；超时降级后写一条 ReasonDecisionResolved 解决标记（payload.layer3_pending_id=<id>），
// 本扫描先载入已解决集合并跳过——保证同一 pending 绝不被每个边界重复 maim。返回实际超时降级的角色数。
// 全程 best-effort：任一条出错只 log、跳过，绝不阻断阶段切换。**异步致死防护核心**：无论玩家是否在线，超时只让她瘸、绝不让她死。
func (service *Service) degradeLayer3ConsentTimeoutsAtBoundary(ctx context.Context, state *State) (int, error) {
	if service == nil || service.db == nil || service.units == nil || state == nil {
		return 0, nil
	}
	resolved, err := service.resolvedLayer3PendingIDs(ctx, state.ID)
	if err != nil {
		return 0, err // 查不到已解决集合则不冒进降级（保守，绝不重复 maim）
	}
	rows, err := service.db.QueryContext(ctx,
		`SELECT id, actor_unit_id, payload_json FROM events WHERE session_id = ? AND reason_code = ?`,
		state.ID, string(events.ReasonDefeatCrippled))
	if err != nil {
		return 0, fmt.Errorf("query layer3 consent pending: %w", err)
	}
	type pendingRow struct {
		id, actorID, threat string
		deadline            int
	}
	pendings := make([]pendingRow, 0)
	for rows.Next() {
		var id string
		var actorID sql.NullString
		var payloadJSON string
		if err := rows.Scan(&id, &actorID, &payloadJSON); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan layer3 consent pending: %w", err)
		}
		if resolved[id] {
			continue // 已超时降级过（或被显式 resolve）→ 幂等跳过，绝不重复 maim
		}
		var payload struct {
			ConsentTier  string `json:"consent_tier"`
			DeadlineTurn int    `json:"deadline_turn"`
			Degrade      string `json:"degrade"`
			Threat       string `json:"threat"`
		}
		_ = json.Unmarshal([]byte(payloadJSON), &payload)
		// 只认真的「requires_consent + maimed」层3 标记，且已到/过 deadline（确定性回合判据，不用墙钟）。
		if payload.ConsentTier != string(relevance.ConsentTierFor(3)) || payload.Degrade != "maimed" {
			continue
		}
		if state.TurnState.Turn < payload.DeadlineTurn {
			continue // 尚未超时：还在玩家可应答的窗口内
		}
		pendings = append(pendings, pendingRow{id: id, actorID: actorID.String, threat: payload.Threat, deadline: payload.DeadlineTurn})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate layer3 consent pending: %w", err)
	}

	degraded := 0
	for _, p := range pendings {
		if strings.TrimSpace(p.actorID) == "" {
			continue
		}
		actor, err := service.units.GetByID(ctx, p.actorID)
		if err != nil {
			slog.Warn("load layer3 timeout actor failed (best-effort)", "unit", p.actorID, "err", err)
			continue
		}
		// 已阵亡的不再 maim（残废对死者无意义；且绝不触碰 lives）。
		if actor.Status.LifeState == unit.LifeStateDead {
			service.markLayer3PendingResolved(ctx, state, p.id, p.actorID) // 标记已了结，免每边界重扫
			continue
		}
		ok, err := service.degradeLayer3OnTimeout(ctx, state, &actor, p.threat)
		if err != nil {
			slog.Warn("degrade layer3 on timeout failed (best-effort)", "unit", p.actorID, "err", err)
			continue
		}
		// 落幂等解决标记（无论是否真改了 hp：标记本身代表「这条 pending 的超时已处理」，免重复扫）。
		service.markLayer3PendingResolved(ctx, state, p.id, p.actorID)
		if ok {
			degraded++
		}
	}
	return degraded, nil
}

// resolvedLayer3PendingIDs 载入本会话已被超时降级/显式解决的层3 pending event id 集合（幂等闸的「已处理」集）。
// 来源：ReasonDecisionResolved 流程事件里 payload.layer3_pending_id 非空者。查询失败返回错误（调用方据此保守地不降级）。
func (service *Service) resolvedLayer3PendingIDs(ctx context.Context, sessionID string) (map[string]bool, error) {
	rows, err := service.db.QueryContext(ctx,
		`SELECT payload_json FROM events WHERE session_id = ? AND reason_code = ?`,
		sessionID, string(events.ReasonDecisionResolved))
	if err != nil {
		return nil, fmt.Errorf("query resolved layer3 pendings: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var payloadJSON string
		if err := rows.Scan(&payloadJSON); err != nil {
			return nil, fmt.Errorf("scan resolved layer3 pending: %w", err)
		}
		var payload struct {
			Layer3PendingID string `json:"layer3_pending_id"`
		}
		_ = json.Unmarshal([]byte(payloadJSON), &payload)
		if id := strings.TrimSpace(payload.Layer3PendingID); id != "" {
			out[id] = true
		}
	}
	return out, rows.Err()
}

// markLayer3PendingResolved 写一条 ReasonDecisionResolved 流程事件，把某条层3 pending（pendingID）标记为已处理（幂等闸）。
// best-effort：吞错只 log（写失败的极端情形下，下个边界会重扫并重试——degradeLayer3OnTimeout 把 hp floor 在 1，重复也不致死，安全）。
func (service *Service) markLayer3PendingResolved(ctx context.Context, state *State, pendingID, actorID string) {
	if service == nil || service.db == nil || state == nil {
		return
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   state.ID,
		OwnerUnitID: actorID,
		Code:        events.ReasonDecisionResolved,
		Category:    events.CategoryLifecycle,
		Payload:     map[string]any{"layer3_pending_id": pendingID, "resolve_type": "consent_timeout_maimed"},
		WorldID:     state.WorldID,
		Tick:        state.TurnState.Turn,
	}); err != nil {
		slog.Warn("mark layer3 pending resolved failed (best-effort)", "unit", actorID, "pending", pendingID, "err", err)
	}
}

// ---- §4.3 PvE 后果 provenance 闸：前因可解析判定与归因句派生 ----

// provenance 显著度阈值（与 engine/decision 的 ValidateAttribution 同源，保证「PvE 后果前因」与「LLM 决策前因」一把尺）。
const (
	defeatTraitSignificance = 0.25 // |trait-0.5| ≥ 此值才算显著前因（同 decision.traitSignificance）
	defeatMemImportanceMin  = 6    // 记忆重要度 ≥ 此值才算可锚前因（同 decision.memImportanceMin）
	defeatRelAxisMin        = 0.3  // 关系轴归一绝对值 ≥ 此值才算可锚前因（同 decision.relAxisMin）
)

// snapshotHasResolvableCause 判断一个前因快照里**是否存在至少一条可解析前因**（人格显著/可锚记忆/任一红线/
// 显著关系/任一现实压力/任一玩家动作）。这是「无源戏剧性后果判 OOC」的代码强制（§5）——纯函数、确定性、可测。
func snapshotHasResolvableCause(snap decision.Snapshot) bool {
	for _, v := range snap.Traits {
		if absFloat(v-0.5) >= defeatTraitSignificance {
			return true
		}
	}
	for _, m := range snap.Memories {
		if m.Importance >= defeatMemImportanceMin {
			return true
		}
	}
	if len(snap.Redlines) > 0 {
		return true
	}
	for _, r := range snap.Relations {
		if absFloat(r.Trust) >= defeatRelAxisMin || absFloat(r.Affection) >= defeatRelAxisMin ||
			absFloat(r.Fear) >= defeatRelAxisMin || absFloat(r.Rivalry) >= defeatRelAxisMin {
			return true
		}
	}
	p := snap.Pressure
	// 只认**战前既有**的现实压力（Hunger/Fatigue/Debt）。Injury/Threat 会被本次战斗自产——
	// applyDefeatPenalty 用战后残血 actor 构快照，down/fled 败北时残血必致 Injury=true、撞见威胁必致 Threat=true，
	// 若认这两位则 provenance 闸对最常见的 down/fled 被「战斗自身造成的伤势」自满足而恒判有前因（橡皮图章），
	// 「无源→降层1」分支永不触发。故排除这两个可能自产的压力位，杜绝无源致残/致死。
	if p.Hunger || p.Fatigue || p.Debt {
		return true
	}
	return len(snap.PlayerActionEventIDs) > 0
}

// dominantProvenanceCauseZH 把前因快照里最重的一类可解析前因翻成一句「凭什么会败到这一步」的引子（≤约 20 字）。
// 顺序优先：红线 > 现实压力 > 关系 > 显著人格 > 可锚记忆（越「硬」的前因越优先解释败北）。无前因返回空串。确定性、纯函数。
func dominantProvenanceCauseZH(snap decision.Snapshot) string {
	if len(snap.Redlines) > 0 {
		return "这一仗本就踩着她不肯退的红线"
	}
	p := snap.Pressure
	// 与 snapshotHasResolvableCause 一把尺：Injury/Threat 可能由本次战斗自产，不作可解析前因（避免橡皮图章），
	// 仅认战前既有的 Hunger/Fatigue/Debt。
	switch {
	case p.Hunger:
		return "饿着肚子上阵，力气先垮了"
	case p.Fatigue:
		return "连日奔劳，她早已力竭"
	case p.Debt:
		return "压在身上的事太多，她分了神"
	}
	for _, r := range snap.Relations {
		if absFloat(r.Trust) >= defeatRelAxisMin || absFloat(r.Affection) >= defeatRelAxisMin ||
			absFloat(r.Fear) >= defeatRelAxisMin || absFloat(r.Rivalry) >= defeatRelAxisMin {
			return "她心里挂着人，不敢拼到尽头"
		}
	}
	for _, v := range snap.Traits {
		if absFloat(v-0.5) >= defeatTraitSignificance {
			return "这正是她的性子使然"
		}
	}
	for _, m := range snap.Memories {
		if m.Importance >= defeatMemImportanceMin {
			return "她想起了从前那桩事，手一软"
		}
	}
	return ""
}

func lootBlurb(awards []encounter.Award) string {
	gold := 0
	for _, a := range awards {
		if a.ItemID == "gold" {
			gold += a.Quantity
		}
	}
	if gold > 0 {
		return fmt.Sprintf("腰间多了 %d 枚铜钱。", gold)
	}
	return ""
}
