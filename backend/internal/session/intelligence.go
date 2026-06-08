package session

// 文件说明：处理跨阵营情报线事件（卧底招募、泄密、暴露、周期回传）并维护情报资产生命周期。

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// maybeResolveIntelligenceEvent 在跨阵营接触后尝试触发情报事件。
// 优先尝试“招募卧底”，未命中则退化为“情报泄露”。
func (service *Service) maybeResolveIntelligenceEvent(
	ctx context.Context,
	state *State,
	left *unit.Record,
	right *unit.Record,
	scene string,
) {
	if service == nil || state == nil || left == nil || right == nil {
		return
	}
	if left.ID == "" || right.ID == "" || left.ID == right.ID {
		return
	}
	if left.FactionID == right.FactionID {
		return
	}
	if !isBattleReady(*left) || !isBattleReady(*right) {
		return
	}
	if !isPlayerEnemyFactionPair(*state, left.FactionID, right.FactionID) {
		return
	}

	scene = strings.TrimSpace(scene)
	if scene == "" {
		scene = "cross_faction_contact"
	}

	if service.tryRecruitUndercover(ctx, state, left, right, scene) {
		return
	}
	service.tryIntelLeak(ctx, state, left, right, scene)
}

// tryRecruitUndercover 评估并结算一次“卧底招募”事件。
// 触发后会写入资产、日志、情报报告、关系变化和双边记忆。
func (service *Service) tryRecruitUndercover(
	ctx context.Context,
	state *State,
	left *unit.Record,
	right *unit.Record,
	scene string,
) bool {
	candidate, handler, vulnerability := selectUndercoverPair(*left, *right)
	if candidate == nil || handler == nil || vulnerability < 0.38 {
		return false
	}
	if hasActiveIntelAsset(*state, candidate.ID, handler.FactionID) {
		return false
	}

	affinity := 0.0
	if relation, ok := service.loadOutgoingRelationMap(ctx, candidate.ID)[handler.ID]; ok {
		affinity = relationAffinityFromScores(relation.Trust, relation.Affection, relation.Rivalry, relation.Fear)
	}
	chance := clampFloat(
		0.07+
			vulnerability*0.88+
			handler.Personality.Sociability*0.12+
			float64(handler.Skills.Social.Charm)/220.0+
			clampFloat(affinity, -0.2, 0.5)*0.22,
		0,
		1,
	)
	roll := intelligenceRoll(*state, candidate.ID, handler.ID, "undercover_recruit:"+scene)
	if roll > chance {
		return false
	}

	byID := map[string]*unit.Record{candidate.ID: candidate, handler.ID: handler}
	proposal := fmt.Sprintf("是否由 %s 自愿成为 %s 阵营的卧底/内线，并向 %s 回传情报。", candidate.DisplayName(), handler.FactionID, handler.DisplayName())
	consent, result, interaction, ok := service.requestPairConsent(ctx, *state, byID, candidate, handler, proposal, "卧底招募、背叛原阵营、秘密情报合作或改变忠诚对象", "undercover_consent")
	service.appendLLMInteractionWithSpend(ctx, state, interaction)
	if !ok || !consent.LeftAgree || !consent.RightAgree {
		service.recordRomanceConsentDialogue(ctx, state, candidate, handler, consent, result)
		appendLog(state, "intel_undercover_hold", romanceConsentHoldMessage(consent, candidate, handler), candidate.ID, handler.ID)
		return false
	}
	service.recordRomanceConsentDialogue(ctx, state, candidate, handler, consent, result)

	now := time.Now().UTC()
	asset := IntelAsset{
		ID:               uuid.NewString(),
		UnitID:           candidate.ID,
		HomeFactionID:    candidate.FactionID,
		HandlerFactionID: handler.FactionID,
		Mode:             "undercover",
		Motivation:       undercoverMotivation(*candidate, *handler),
		Risk:             clampFloat(0.18+(1-effectiveLoyalty(*candidate))*0.56, 0, 1),
		SinceTurn:        state.TurnState.Turn,
		UpdatedAt:        now,
	}
	upsertIntelAsset(state, asset)

	summary := strings.TrimSpace(consent.Summary)
	if summary == "" {
		summary = fmt.Sprintf(
			"%s 与 %s 私下达成了卧底接触：%s 开始向 %s 回传情报。",
			handler.DisplayName(),
			candidate.DisplayName(),
			candidate.DisplayName(),
			handler.DisplayName(),
		)
	}
	appendLog(state, "intel_undercover", summary, candidate.ID, handler.ID)
	appendIntelReport(state, IntelReport{
		ID:              uuid.NewString(),
		Turn:            state.TurnState.Turn,
		Phase:           state.TurnState.Phase,
		Kind:            "undercover_recruit",
		UnitID:          candidate.ID,
		SourceFactionID: candidate.FactionID,
		TargetFactionID: handler.FactionID,
		Summary:         summary,
		CreatedAt:       now,
	})

	service.applyMutualRelationShiftBestEffort(
		ctx,
		state,
		candidate,
		handler,
		relationDelta{Trust: 0.52, Fear: -0.06, Affection: 0.12, Rivalry: -0.08},
		relationDelta{Trust: 0.22, Fear: -0.03, Affection: 0.06, Rivalry: -0.02},
		"私下卧底接触",
	)
	service.rememberUnitWithSourceBestEffort(ctx, candidate, state.TurnState.Turn, "我私下搭上了另一边。", "intel_undercover", 2)
	service.rememberUnitWithSourceBestEffort(ctx, handler, state.TurnState.Turn, "我拿到了一条可用内线。", "intel_undercover", 1)

	return true
}

// tryIntelLeak 评估并结算一次“临时情报泄露”事件。
func (service *Service) tryIntelLeak(
	ctx context.Context,
	state *State,
	left *unit.Record,
	right *unit.Record,
	scene string,
) bool {
	leaker, listener, vulnerability := selectUndercoverPair(*left, *right)
	if leaker == nil || listener == nil || vulnerability < 0.24 {
		return false
	}

	affinity := 0.0
	if relation, ok := service.loadOutgoingRelationMap(ctx, leaker.ID)[listener.ID]; ok {
		affinity = relationAffinityFromScores(relation.Trust, relation.Affection, relation.Rivalry, relation.Fear)
	}
	chance := clampFloat(
		0.11+
			vulnerability*0.74+
			listener.Personality.Sociability*0.15+
			float64(listener.Skills.Social.Negotiation)/240.0+
			clampFloat(affinity, -0.3, 0.6)*0.18,
		0,
		1,
	)
	roll := intelligenceRoll(*state, leaker.ID, listener.ID, "intel_leak:"+scene)
	if roll > chance {
		return false
	}

	summary := fmt.Sprintf(
		"%s 向 %s 私下泄露了本方近况与动向。",
		leaker.DisplayName(),
		listener.DisplayName(),
	)
	appendLog(state, "intel_leak", summary, leaker.ID, listener.ID)
	appendIntelReport(state, IntelReport{
		ID:              uuid.NewString(),
		Turn:            state.TurnState.Turn,
		Phase:           state.TurnState.Phase,
		Kind:            "intel_leak",
		UnitID:          leaker.ID,
		SourceFactionID: leaker.FactionID,
		TargetFactionID: listener.FactionID,
		Summary:         summary,
		CreatedAt:       time.Now().UTC(),
	})

	service.applyMutualRelationShiftBestEffort(
		ctx,
		state,
		leaker,
		listener,
		relationDelta{Trust: 0.32, Fear: -0.04, Affection: 0.08, Rivalry: -0.03},
		relationDelta{Trust: 0.28, Fear: -0.02, Affection: 0.05, Rivalry: -0.02},
		"私下交换情报",
	)
	service.rememberUnitWithSourceBestEffort(ctx, leaker, state.TurnState.Turn, "我把内部风声递了出去。", "intel_leak", 1)
	service.rememberUnitWithSourceBestEffort(ctx, listener, state.TurnState.Turn, "我拿到了一手内情。", "intel_leak", 1)
	return true
}

// processIntelAssets 逐回合推进已有情报资产生命周期。
// 包括暴露判定、定期回传报告、风险更新与兜底跨阵营互动补偿。
func (service *Service) processIntelAssets(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
) {
	if service == nil || state == nil {
		return
	}
	if len(state.IntelAssets) == 0 {
		service.ensureCrossFactionInteractionFloor(ctx, state, byID)
		return
	}
	now := time.Now().UTC()
	for index := range state.IntelAssets {
		asset := state.IntelAssets[index]
		if asset.Exposed || strings.TrimSpace(asset.UnitID) == "" {
			continue
		}
		if asset.LastReportTurn == state.TurnState.Turn {
			continue
		}

		spy := byID[asset.UnitID]
		if spy == nil || !isBattleReady(*spy) {
			continue
		}
		if asset.HomeFactionID == "" {
			asset.HomeFactionID = spy.FactionID
		}
		if asset.HandlerFactionID == "" || asset.HandlerFactionID == spy.FactionID {
			continue
		}

		loyalty := effectiveLoyalty(*spy)
		stability := clampFloat(spy.Personality.Stability, 0, 1)
		morale := clampFloat(spy.Status.Morale, 0, 1)
		exposureChance := clampFloat(
			(0.36-loyalty)*0.92+
				(0.42-stability)*0.65+
				(0.40-morale)*0.58,
			0,
			0.68,
		)
		exposureRoll := intelligenceRoll(*state, asset.UnitID, asset.HandlerFactionID, fmt.Sprintf("intel_exposed:%d", state.TurnState.Turn))
		if exposureRoll < exposureChance {
			asset.Exposed = true
			asset.UpdatedAt = now
			state.IntelAssets[index] = asset
			summary := fmt.Sprintf("%s 的卧底身份暴露，双方立刻提高了警戒。", spy.DisplayName())
			appendLog(state, "intel_exposed", summary, spy.ID, "")
			appendIntelReport(state, IntelReport{
				ID:              uuid.NewString(),
				Turn:            state.TurnState.Turn,
				Phase:           state.TurnState.Phase,
				Kind:            "undercover_exposed",
				UnitID:          spy.ID,
				SourceFactionID: asset.HomeFactionID,
				TargetFactionID: asset.HandlerFactionID,
				Summary:         summary,
				CreatedAt:       now,
			})
			service.rememberUnitWithSourceBestEffort(ctx, spy, state.TurnState.Turn, "我被识破了，得立刻自保。", "intel_exposed", 2)
			continue
		}

		summary := buildUndercoverReportSummary(*state, byID, spy, asset)
		asset.LastReportTurn = state.TurnState.Turn
		asset.Risk = clampFloat(exposureChance+0.16, 0, 1)
		asset.UpdatedAt = now
		state.IntelAssets[index] = asset

		appendLog(state, "intel_report", summary, spy.ID, "")
		appendIntelReport(state, IntelReport{
			ID:              uuid.NewString(),
			Turn:            state.TurnState.Turn,
			Phase:           state.TurnState.Phase,
			Kind:            "undercover_report",
			UnitID:          spy.ID,
			SourceFactionID: asset.HomeFactionID,
			TargetFactionID: asset.HandlerFactionID,
			Summary:         summary,
			CreatedAt:       now,
		})
		service.rememberUnitWithSourceBestEffort(ctx, spy, state.TurnState.Turn, "我把侦得的动向传出去了。", "intel_report", 1)
	}
	service.ensureCrossFactionInteractionFloor(ctx, state, byID)
}

// selectUndercoverPair 在两名单位中选出“更脆弱者”为潜在被渗透对象。
func selectUndercoverPair(left unit.Record, right unit.Record) (*unit.Record, *unit.Record, float64) {
	leftScore := intelVulnerability(left)
	rightScore := intelVulnerability(right)
	if leftScore >= rightScore {
		return &left, &right, leftScore
	}
	return &right, &left, rightScore
}

// hasActiveIntelAsset 判断某单位是否已有未暴露的情报资产，避免重复建档。
func hasActiveIntelAsset(state State, unitID string, handlerFactionID string) bool {
	for _, asset := range state.IntelAssets {
		if asset.Exposed {
			continue
		}
		if asset.UnitID != unitID {
			continue
		}
		if strings.TrimSpace(handlerFactionID) != "" && asset.HandlerFactionID != handlerFactionID {
			continue
		}
		return true
	}
	return false
}

// upsertIntelAsset 按 unitID 写入或更新情报资产，并维护容量上限。
func upsertIntelAsset(state *State, asset IntelAsset) {
	if state == nil || strings.TrimSpace(asset.UnitID) == "" {
		return
	}
	if asset.ID == "" {
		asset.ID = uuid.NewString()
	}
	if strings.TrimSpace(asset.Mode) == "" {
		asset.Mode = "undercover"
	}
	if asset.SinceTurn <= 0 {
		asset.SinceTurn = state.TurnState.Turn
	}
	if asset.UpdatedAt.IsZero() {
		asset.UpdatedAt = time.Now().UTC()
	}
	for index := range state.IntelAssets {
		current := state.IntelAssets[index]
		if current.UnitID != asset.UnitID {
			continue
		}
		if current.SinceTurn > 0 && (asset.SinceTurn == 0 || current.SinceTurn < asset.SinceTurn) {
			asset.SinceTurn = current.SinceTurn
		}
		if asset.LastReportTurn == 0 {
			asset.LastReportTurn = current.LastReportTurn
		}
		state.IntelAssets[index] = asset
		return
	}
	state.IntelAssets = append(state.IntelAssets, asset)
	if len(state.IntelAssets) > maxIntelAssets {
		state.IntelAssets = state.IntelAssets[len(state.IntelAssets)-maxIntelAssets:]
	}
}

// appendIntelReport 追加情报报告并同步跨阵营交互统计。
func appendIntelReport(state *State, report IntelReport) {
	if state == nil {
		return
	}
	if report.ID == "" {
		report.ID = uuid.NewString()
	}
	if report.Turn <= 0 {
		report.Turn = state.TurnState.Turn
	}
	if report.Phase == "" {
		report.Phase = state.TurnState.Phase
	}
	if report.CreatedAt.IsZero() {
		report.CreatedAt = time.Now().UTC()
	}
	state.IntelReports = append(state.IntelReports, report)
	incrementCrossFactionInteractionByFaction(
		state,
		report.Kind,
		report.SourceFactionID,
		report.TargetFactionID,
		report.UnitID,
		"",
	)
	if len(state.IntelReports) > maxIntelReports {
		state.IntelReports = state.IntelReports[len(state.IntelReports)-maxIntelReports:]
	}
}

// ensureCrossFactionInteractionFloor 在本回合跨阵营互动不足时补齐最低情报交互量。
// 该兜底机制用于防止全局互动指标长期为零，影响叙事密度与 AI 记忆素材。
func (service *Service) ensureCrossFactionInteractionFloor(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
) {
	if service == nil || state == nil || byID == nil {
		return
	}
	if state.Metrics.CrossFactionInteractions >= 2 {
		return
	}

	type leakCandidate struct {
		source *unit.Record
		target *unit.Record
		score  float64
	}
	candidates := make([]leakCandidate, 0, 6)
	for _, source := range byID {
		if source == nil || !isBattleReady(*source) {
			continue
		}
		if source.FactionID != state.PlayerFactionID && source.FactionID != state.EnemyFactionID {
			continue
		}
		target := nearestBattleReady(opposedUnitIDs(*state, source.FactionID), byID, source)
		if target == nil || !isBattleReady(*target) {
			continue
		}
		distance := unit.HexDistance(source.Status.PositionQ, source.Status.PositionR, target.Status.PositionQ, target.Status.PositionR)
		if distance > 3 {
			continue
		}
		vulnerability := intelVulnerability(*source)
		score := vulnerability + (3.0-float64(distance))*0.28 + source.Personality.Ambition*0.18 - effectiveLoyalty(*source)*0.24
		candidates = append(candidates, leakCandidate{
			source: source,
			target: target,
			score:  score,
		})
	}
	if len(candidates) == 0 {
		return
	}

	sort.Slice(candidates, func(i int, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].source.ID < candidates[j].source.ID
		}
		return candidates[i].score > candidates[j].score
	})

	needed := 2 - state.Metrics.CrossFactionInteractions
	if needed <= 0 {
		return
	}
	usedSource := map[string]struct{}{}
	created := 0
	for _, candidate := range candidates {
		if created >= needed {
			break
		}
		if candidate.source == nil || candidate.target == nil {
			continue
		}
		if _, duplicated := usedSource[candidate.source.ID]; duplicated {
			continue
		}
		summary := fmt.Sprintf(
			"%s 在 %d,%d 附近捕捉到 %s 的动向，并通过暗线放出了风声。",
			candidate.source.DisplayName(),
			candidate.source.Status.PositionQ,
			candidate.source.Status.PositionR,
			candidate.target.DisplayName(),
		)
		appendLog(state, "intel_leak", summary, candidate.source.ID, candidate.target.ID)
		appendIntelReport(state, IntelReport{
			ID:              uuid.NewString(),
			Turn:            state.TurnState.Turn,
			Phase:           state.TurnState.Phase,
			Kind:            "ambient_leak_floor",
			UnitID:          candidate.source.ID,
			SourceFactionID: candidate.source.FactionID,
			TargetFactionID: candidate.target.FactionID,
			Summary:         summary,
			CreatedAt:       time.Now().UTC(),
		})
		service.rememberUnitWithSourceBestEffort(ctx, candidate.source, state.TurnState.Turn, "我放出了敌方动向的风声。", "intel_leak", 1)
		usedSource[candidate.source.ID] = struct{}{}
		created++
	}
}

// buildUndercoverReportSummary 生成卧底回传摘要，聚合地形、邻近友军、威胁距离和跟踪目标。
func buildUndercoverReportSummary(
	state State,
	byID map[string]*unit.Record,
	spy *unit.Record,
	asset IntelAsset,
) string {
	if spy == nil {
		return "卧底回传了模糊态势。"
	}

	coord := world.Coord{Q: spy.Status.PositionQ, R: spy.Status.PositionR}
	terrain := terrainDisplayName(terrainAt(state.Map, coord))
	homeNearby := countAdjacentFactionUnits(state, byID, spy, asset.HomeFactionID)

	threat := nearestBattleReady(opposedUnitIDs(state, asset.HomeFactionID), byID, spy)
	threatLabel := "无"
	if threat != nil {
		threatLabel = fmt.Sprintf("%s(距%d)", threat.DisplayName(), unit.HexDistance(spy.Status.PositionQ, spy.Status.PositionR, threat.Status.PositionQ, threat.Status.PositionR))
	}

	homeLeader := nearestFactionUnit(state, byID, spy, asset.HomeFactionID, spy.ID)
	homeLeaderLabel := "暂无"
	if homeLeader != nil {
		homeLeaderLabel = homeLeader.DisplayName()
	}

	return fmt.Sprintf(
		"卧底回传：%s 位于 %d,%d（%s），附近同阵营单位=%d，最近对手=%s，重点跟踪对象=%s。",
		spy.DisplayName(),
		spy.Status.PositionQ,
		spy.Status.PositionR,
		terrain,
		homeNearby,
		threatLabel,
		homeLeaderLabel,
	)
}

// nearestFactionUnit 查找指定阵营中距离 actor 最近的可战斗单位。
func nearestFactionUnit(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	factionID string,
	excludedUnitID string,
) *unit.Record {
	if actor == nil {
		return nil
	}
	targetIDs := factionUnitIDs(state, factionID)
	var chosen *unit.Record
	bestDistance := 1 << 30
	for _, unitID := range targetIDs {
		if unitID == excludedUnitID {
			continue
		}
		record := byID[unitID]
		if record == nil || !isBattleReady(*record) {
			continue
		}
		distance := unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, record.Status.PositionQ, record.Status.PositionR)
		if distance < bestDistance {
			bestDistance = distance
			chosen = record
		}
	}
	return chosen
}

// countAdjacentFactionUnits 统计 actor 周围一格内、同指定阵营的可战斗单位数量。
func countAdjacentFactionUnits(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	factionID string,
) int {
	if actor == nil {
		return 0
	}
	count := 0
	for _, unitID := range factionUnitIDs(state, factionID) {
		target := byID[unitID]
		if target == nil || target.ID == actor.ID || !isBattleReady(*target) {
			continue
		}
		if unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, target.Status.PositionQ, target.Status.PositionR) <= 1 {
			count++
		}
	}
	return count
}

// factionUnitIDs 返回状态中指定阵营对应的单位 ID 列表。
func factionUnitIDs(state State, factionID string) []string {
	switch factionID {
	case state.PlayerFactionID:
		return state.PlayerUnitIDs
	case state.EnemyFactionID:
		return state.EnemyUnitIDs
	default:
		return []string{}
	}
}

// undercoverMotivation 根据忠诚/野心生成卧底动机文本，供报告与叙事使用。
func undercoverMotivation(candidate unit.Record, handler unit.Record) string {
	loyalty := effectiveLoyalty(candidate)
	switch {
	case loyalty < 0.2:
		return fmt.Sprintf("对本方已失望，愿意与 %s 私下合作。", handler.DisplayName())
	case candidate.Personality.Ambition > 0.7:
		return "希望为自己争取更有利的位置。"
	default:
		return "短期利益驱动，先保持双向试探。"
	}
}

// intelVulnerability 计算单位被策反/泄密的脆弱度分数。
// 分数综合忠诚、士气、稳定性、野心与低血量压力。
func intelVulnerability(record unit.Record) float64 {
	loyalty := effectiveLoyalty(record)
	morale := clampFloat(record.Status.Morale, 0, 1)
	stability := clampFloat(record.Personality.Stability, 0, 1)
	ambition := clampFloat(record.Personality.Ambition, 0, 1)
	hpPressure := 0.0
	if record.Status.HP <= 35 {
		hpPressure = 0.08
	}
	return clampFloat(
		(1-loyalty)*0.62+
			(1-morale)*0.2+
			(1-stability)*0.1+
			ambition*0.08+
			hpPressure,
		0,
		1,
	)
}

// effectiveLoyalty 融合状态忠诚与人格忠诚，得到用于情报判定的有效忠诚值。
func effectiveLoyalty(record unit.Record) float64 {
	statusLoyalty := clampFloat(record.Status.Loyalty, 0, 1)
	personalityLoyalty := clampFloat(record.Personality.Loyalty, 0, 1)
	if statusLoyalty == 0 && personalityLoyalty > 0 {
		return personalityLoyalty
	}
	if personalityLoyalty == 0 && statusLoyalty > 0 {
		return statusLoyalty
	}
	return clampFloat(statusLoyalty*0.55+personalityLoyalty*0.45, 0, 1)
}

// intelligenceRoll 生成可复现的情报事件随机值，输入包含会话、回合、阶段和事件标签。
func intelligenceRoll(state State, leftID string, rightID string, label string) float64 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(state.ID))
	_, _ = hasher.Write([]byte(fmt.Sprintf("%d", state.TurnState.Turn)))
	_, _ = hasher.Write([]byte(string(state.TurnState.Phase)))
	_, _ = hasher.Write([]byte(leftID))
	_, _ = hasher.Write([]byte(rightID))
	_, _ = hasher.Write([]byte(label))
	return float64(hasher.Sum32()%10000) / 10000
}

// rememberUnitWithSourceBestEffort 以“尽力而为”方式写入单位记忆，失败不阻断主流程。
func (service *Service) rememberUnitWithSourceBestEffort(
	ctx context.Context,
	record *unit.Record,
	turn int,
	summary string,
	source string,
	importanceBoost int,
) {
	if err := service.rememberUnitWithSource(ctx, record, turn, summary, source, importanceBoost); err != nil {
		_ = err
	}
}
