package session

// 文件说明：维护势力关系状态机（标准化、去重、补缺、省则更新）并驱动外交事件写入。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/engine/turns"
)

// normalizeFactionRelationState 把势力关系状态字符串标准化为受支持枚举。
func normalizeFactionRelationState(state FactionRelationState) FactionRelationState {
	switch FactionRelationState(strings.ToLower(strings.TrimSpace(string(state)))) {
	case FactionRelationWar:
		return FactionRelationWar
	case FactionRelationAllied:
		return FactionRelationAllied
	case FactionRelationNeutral:
		return FactionRelationNeutral
	default:
		return ""
	}
}

// canonicalFactionPair 生成稳定的双势力键顺序，避免 A-B 与 B-A 被当成两条关系。
func canonicalFactionPair(leftFactionID string, rightFactionID string) (string, string, bool) {
	left := strings.TrimSpace(leftFactionID)
	right := strings.TrimSpace(rightFactionID)
	if left == "" || right == "" || left == right {
		return "", "", false
	}
	if left > right {
		left, right = right, left
	}
	return left, right, true
}

// factionRelationKey 以统一格式拼接势力对，作为去重和索引键。
func factionRelationKey(leftFactionID string, rightFactionID string) string {
	return leftFactionID + "::" + rightFactionID
}

// ensureFactionRelations 清洗并修复 state 中的势力关系表。
// 它会移除非法/重复记录，并在缺失时补上玩家阵营与敌对阵营的默认战争关系。
func ensureFactionRelations(state *State) {
	if state == nil {
		return
	}
	if state.FactionRelations == nil {
		state.FactionRelations = []FactionRelation{}
	}

	normalized := make([]FactionRelation, 0, len(state.FactionRelations))
	seen := map[string]struct{}{}
	for _, relation := range state.FactionRelations {
		left, right, ok := canonicalFactionPair(relation.LeftFactionID, relation.RightFactionID)
		if !ok {
			continue
		}
		nextState := normalizeFactionRelationState(relation.State)
		if nextState == "" {
			continue
		}
		key := factionRelationKey(left, right)
		if _, duplicated := seen[key]; duplicated {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, FactionRelation{
			LeftFactionID:  left,
			RightFactionID: right,
			State:          nextState,
			UpdatedAt:      relation.UpdatedAt,
			Reason:         strings.TrimSpace(relation.Reason),
		})
	}
	state.FactionRelations = normalized

	left, right, ok := canonicalFactionPair(state.PlayerFactionID, state.EnemyFactionID)
	if !ok {
		return
	}
	key := factionRelationKey(left, right)
	if _, exists := seen[key]; exists {
		return
	}
	state.FactionRelations = append(state.FactionRelations, FactionRelation{
		LeftFactionID:  left,
		RightFactionID: right,
		State:          FactionRelationWar,
		UpdatedAt:      time.Now().UTC(),
		Reason:         "default_relation",
	})
}

// setFactionRelation 写入或更新一对势力的关系状态，并返回是否发生实际变更。
func setFactionRelation(
	state *State,
	leftFactionID string,
	rightFactionID string,
	relationState FactionRelationState,
	reason string,
	occurredAt time.Time,
) bool {
	if state == nil {
		return false
	}
	ensureFactionRelations(state)
	left, right, ok := canonicalFactionPair(leftFactionID, rightFactionID)
	if !ok {
		return false
	}
	relationState = normalizeFactionRelationState(relationState)
	if relationState == "" {
		return false
	}
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	reason = strings.TrimSpace(reason)

	for index := range state.FactionRelations {
		relation := state.FactionRelations[index]
		if relation.LeftFactionID != left || relation.RightFactionID != right {
			continue
		}
		changed := relation.State != relationState || relation.Reason != reason
		state.FactionRelations[index].State = relationState
		state.FactionRelations[index].UpdatedAt = occurredAt
		state.FactionRelations[index].Reason = reason
		return changed
	}

	state.FactionRelations = append(state.FactionRelations, FactionRelation{
		LeftFactionID:  left,
		RightFactionID: right,
		State:          relationState,
		UpdatedAt:      occurredAt,
		Reason:         reason,
	})
	return true
}

// factionRelationBetween 查询两势力当前关系，查不到时回退到默认中立/默认战争规则。
func factionRelationBetween(state State, leftFactionID string, rightFactionID string) FactionRelationState {
	left := strings.TrimSpace(leftFactionID)
	right := strings.TrimSpace(rightFactionID)
	if left == "" || right == "" {
		return FactionRelationNeutral
	}
	if left == right {
		return FactionRelationAllied
	}
	keyLeft, keyRight, ok := canonicalFactionPair(left, right)
	if !ok {
		return FactionRelationNeutral
	}
	for _, relation := range state.FactionRelations {
		candidateLeft, candidateRight, candidateOK := canonicalFactionPair(relation.LeftFactionID, relation.RightFactionID)
		if !candidateOK {
			continue
		}
		if candidateLeft == keyLeft && candidateRight == keyRight {
			if normalized := normalizeFactionRelationState(relation.State); normalized != "" {
				return normalized
			}
		}
	}
	defaultLeft, defaultRight, defaultOK := canonicalFactionPair(state.PlayerFactionID, state.EnemyFactionID)
	if defaultOK && defaultLeft == keyLeft && defaultRight == keyRight {
		return FactionRelationWar
	}
	return FactionRelationNeutral
}

// opposedUnitIDs 根据当前势力关系返回“可视为敌对方”的单位列表。
// 当双方不是战争状态时返回空，避免非战状态误选敌人目标。
func opposedUnitIDs(state State, factionID string) []string {
	switch factionID {
	case state.PlayerFactionID:
		if factionRelationBetween(state, state.PlayerFactionID, state.EnemyFactionID) != FactionRelationWar {
			return []string{}
		}
		return state.EnemyUnitIDs
	case state.EnemyFactionID:
		if factionRelationBetween(state, state.EnemyFactionID, state.PlayerFactionID) != FactionRelationWar {
			return []string{}
		}
		return state.PlayerUnitIDs
	case FactionWildling:
		ids := append([]string{}, state.PlayerUnitIDs...)
		ids = append(ids, state.EnemyUnitIDs...)
		return ids
	default:
		return append([]string{}, state.WildUnitIDs...)
	}
}

// SetFactionRelation 对外暴露势力关系修改入口。
// 仅允许在战略/部署阶段调整当前两大阵营关系，并把变更写入日志与原始事件流。
func (service *Service) SetFactionRelation(
	ctx context.Context,
	sessionID string,
	leftFactionID string,
	rightFactionID string,
	nextState FactionRelationState,
	reason string,
) (Snapshot, error) {
	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	if state.Outcome != OutcomeOngoing {
		return Snapshot{}, fmt.Errorf("session is already finished")
	}
	if state.TurnState.Phase != turns.PhaseDeployment {
		return Snapshot{}, fmt.Errorf("faction relation can only be updated during deployment phase")
	}

	normalizedState := normalizeFactionRelationState(nextState)
	if normalizedState == "" {
		return Snapshot{}, fmt.Errorf("relation state must be one of: war/neutral/allied")
	}
	leftFactionID = strings.TrimSpace(leftFactionID)
	rightFactionID = strings.TrimSpace(rightFactionID)
	if leftFactionID == "" {
		leftFactionID = state.PlayerFactionID
	}
	if rightFactionID == "" {
		rightFactionID = state.EnemyFactionID
	}
	left, right, ok := canonicalFactionPair(leftFactionID, rightFactionID)
	if !ok {
		return Snapshot{}, fmt.Errorf("two different faction ids are required")
	}
	if !((left == state.PlayerFactionID && right == state.EnemyFactionID) ||
		(left == state.EnemyFactionID && right == state.PlayerFactionID)) {
		return Snapshot{}, fmt.Errorf("only current two factions are supported in prototype")
	}

	updatedAt := time.Now().UTC()
	if !setFactionRelation(&state, left, right, normalizedState, reason, updatedAt) {
		return buildSnapshot(state, units), nil
	}
	appendLog(
		&state,
		"faction_relation",
		fmt.Sprintf("势力关系更新：%s <-> %s = %s。%s", left, right, normalizedState, strings.TrimSpace(reason)),
		"",
		"",
	)
	appendRawEvent(&state, rawEventSpec{
		source:  "diplomacy",
		kind:    "faction_relation",
		summary: fmt.Sprintf("%s<->%s=%s", left, right, normalizedState),
		payload: map[string]any{
			"id":               uuid.NewString(),
			"left_faction_id":  left,
			"right_faction_id": right,
			"state":            normalizedState,
			"reason":           strings.TrimSpace(reason),
			"updated_at":       updatedAt,
		},
	})

	if err := service.sessions.Save(ctx, &state); err != nil {
		return Snapshot{}, err
	}
	return buildSnapshot(state, units), nil
}
