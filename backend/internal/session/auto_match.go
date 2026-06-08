package session

// 文件说明：撮合自动扫描（设计 docs/事件耦合与跨玩家关联.md §2.2）。把「撮合」从只有 ops HTTP 手动触发、候选要手填，
// 扶正为部署边界的低频自动扫描：在同一世界/会话内挑符合条件的角色作候选，用**确定性启发式**算 MatchScore 四因子
// （地理临近/钩子契合/关系交集/密度调节，各 [0,1]），交给现有 MatchIntoSocialObject 打分→过门→arbitration 择 slots 人。
// 纪律：flag-gated（QUNXIANG_AUTO_MATCH 默认关 → 零行为变化）、低频（turn 取模确定性触发）、best-effort（吞错，绝不中断推进）。
// 四因子全部确定性（sessionID+turn+unit 的 FNV / 关系四轴 / 锚密度派生），无全局 rand，保证可复现。

import (
	"context"
	"hash/fnv"
	"math"
	"os"
	"strconv"
	"strings"

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
)

// autoMatchEnabled 读 QUNXIANG_AUTO_MATCH（true/1/yes/on 视为开），默认关 → scanAndMatch 整方法 no-op、零行为变化、零 DB 写。
func autoMatchEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_AUTO_MATCH"))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// scanAndMatch 在部署边界低频自动撮合：挑本局玩家阵营的存活角色作候选、确定性算四因子、调 MatchIntoSocialObject 绑社会客体。
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
	// 低频触发：每 autoMatchEveryNTurns 个部署回合扫一次（确定性，turn 取模）。
	if autoMatchEveryNTurns <= 0 || state.TurnState.Turn%autoMatchEveryNTurns != 0 {
		return
	}

	candidates := service.buildMatchCandidates(ctx, state, units)
	if len(candidates) < 2 {
		return // 不足两人，撮不成社会客体
	}
	if len(candidates) > autoMatchMaxCandidates {
		candidates = candidates[:autoMatchMaxCandidates]
	}

	// label 带 turn，使不同周期产出不同确定性社会客体 id（同周期重撮合幂等更新同一客体）。
	label := autoMatchLabel + "·" + strconv.Itoa(state.TurnState.Turn/autoMatchEveryNTurns)
	_, _, _ = service.MatchIntoSocialObject(ctx, worldID, autoMatchKind, label, candidates, autoMatchSlots)
}

// buildMatchCandidates 把本局玩家阵营的存活角色构造成带四因子的撮合候选。
// 四因子全部确定性：地理临近=活跃单位群质心的 hex 距离衰减；钩子契合=pair-stable 的 FNV 哈希；
// 关系交集=该角色对其余候选的现有四轴关系强度；密度调节=锚密度反向（锚越少越易被撮合，反垄断）。
func (service *Service) buildMatchCandidates(ctx context.Context, state *State, units []unit.Record) []MatchCandidate {
	// 先筛出本局玩家阵营、存活、非战斗的角色作为候选池。
	pool := make([]unit.Record, 0, len(units))
	for i := range units {
		u := units[i]
		if state.PlayerFactionID != "" && u.FactionID != state.PlayerFactionID {
			continue
		}
		if u.Status.LifeState == unit.LifeStateDead || u.Status.LivesRemaining <= 0 {
			continue
		}
		pool = append(pool, u)
	}
	if len(pool) < 2 {
		return nil
	}

	// 地理临近：以候选群质心为参照，离质心越近越「地理临近」（同一片活动区域的人更易同行）。
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
			GeoNear:           geoNearFromCentroid(u, centroidQ, centroidR),
			HookFit:           hookFitFor(state.ID, state.TurnState.Turn, u.ID),
			RelationIntersect: service.relationIntersectFor(ctx, u.ID, pool),
			DensityAdj:        densityAdjFor(service.AnchorDensity(ctx, u.ID)),
		})
	}
	return candidates
}

// geoNearFromCentroid 把单位到候选群质心的 hex 距离映射成 [0,1] 临近度（距离 0→1，随距离指数衰减）。
func geoNearFromCentroid(u unit.Record, centroidQ, centroidR float64) float64 {
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
