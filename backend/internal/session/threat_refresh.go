package session

// 文件说明：威胁刷新调度（设计 PvE威胁系统.md §1）。在部署边界确定性地决定野外威胁是否出没。
// 设计取舍：**surface-only**——只投一张「野外有精英出没」的高光卡，不自动开打（尊重玩家/角色能动性），
// 实际遭遇由 HTTP 触发或后续决策接入（避免在边界自动改动战斗态、惊扰正在跑的对局）。全程 best-effort、确定性。

import (
	"context"
	"fmt"
	"hash/fnv"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/unit"
)

const threatAppearanceRate = 0.12 // 每个合格单位每部署阶段撞见野外威胁的概率（确定性掷骰）

var wildThreatNames = []string{"山魈", "独眼熊", "赤鳞蜥", "断尾狼", "夜枭", "石背犀"}

// refreshThreats 在部署边界刷新野外威胁：至多为一名合格单位（非战斗、健康、有命）投一张威胁出没高光卡。
func (service *Service) refreshThreats(ctx context.Context, state *State, units []unit.Record) {
	if service == nil || state == nil {
		return
	}
	for i := range units {
		u := units[i]
		if state.PlayerFactionID != "" && u.FactionID != state.PlayerFactionID {
			continue
		}
		if u.Status.InCombat || u.Status.HP < 40 || u.Status.LivesRemaining <= 0 {
			continue
		}
		if threatRoll(state.ID, state.TurnState.Turn, u.ID) >= threatAppearanceRate {
			continue
		}
		name := wildThreatNames[threatHash(state.ID, state.TurnState.Turn, u.ID, "name")%uint64(len(wildThreatNames))]
		_, _ = service.SurfaceFateEvent(ctx, state.ID, &u, FateEvent{
			ActorID: u.ID, TargetID: u.ID, ReasonCode: events.ReasonInboxHighlight,
			Importance: 5, EmotionWeight: -0.2,
			Summary: fmt.Sprintf("山野间有一头%s在游荡，离她不远。", name),
		})
		break // 至多刷一个，避免一边界刷一堆
	}
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
