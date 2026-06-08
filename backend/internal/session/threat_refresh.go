package session

// 文件说明：威胁刷新调度（设计 PvE威胁系统.md §1）。在部署边界确定性地决定野外威胁是否出没。
// 设计取舍：**surface-only**——只投一张「野外有精英出没」的高光卡，不自动开打（尊重玩家/角色能动性），
// 实际遭遇由 HTTP 触发或后续决策接入（避免在边界自动改动战斗态、惊扰正在跑的对局）。全程 best-effort、确定性。

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"strings"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/unit"
)

const threatAppearanceRate = 0.12 // 每个合格单位每部署阶段撞见野外威胁的概率（确定性掷骰）

// autoPvEFireRate 在 QUNXIANG_AUTO_PVE 开时，撞见威胁的单位里有多大比例直接开打（其余仍只投高光卡）。
// 取较低值：自动开打是真实改 HP/士气/钱包的动作，应克制——大多数遭遇仍尊重角色能动性走「先投卡、后决策」。
const autoPvEFireRate = 0.5

var wildThreatNames = []string{"山魈", "独眼熊", "赤鳞蜥", "断尾狼", "夜枭", "石背犀"}

// autoPvEEnabled 读 QUNXIANG_AUTO_PVE（true/1/yes/on 视为开），默认关 → refreshThreats 保持 surface-only 行为完全不变。
func autoPvEEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_AUTO_PVE"))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// refreshThreats 在部署边界刷新野外威胁：至多为一名合格单位（非战斗、健康、有命）投一张威胁出没高光卡。
// 默认 surface-only（只投卡、不开打，尊重角色能动性）。QUNXIANG_AUTO_PVE 开时，按确定性概率把「投卡」升级为
// 直接调 ResolveEliteEncounter 真实开打（仍要求单位健康、非异步执行中——让位正在聚焦的战斗）。全程 best-effort。
func (service *Service) refreshThreats(ctx context.Context, state *State, units []unit.Record) {
	if service == nil || state == nil {
		return
	}
	autoPvE := autoPvEEnabled()
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
		// 自动开打升级：flag 开 + 非异步执行中（让位聚焦战斗）+ 确定性掷骰命中 → 真实跑通 elite 遭遇，否则仍只投卡。
		if autoPvE && !IsExecutionRunning(state.ID) &&
			threatRoll(state.ID, state.TurnState.Turn, u.ID+"|pve") < autoPvEFireRate {
			actor := u
			if _, err := service.ResolveEliteEncounter(ctx, state, &actor, scaledElite(actor)); err == nil {
				break // 自动开打已落地（含其自身的命运收件箱卡），不再额外投出没卡
			}
			// 遭遇失败（如并发写冲突）：吞错，退回 surface-only 投卡，绝不中断推进。
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
