package session

// 文件说明：把本局单位 seed 进大世界 region-runner 的离线调度（M7.3-real-4b，沙盘 §8.2）。建局/组队落库后，
// 为玩家单位登记作用域(region=sessionID)、生命态列(active)并入初始唤醒，让 region-runner 在战斗之外也能唤醒它们
// 自主生活（觅食/休息/社交/反思）。全程受 ambientSchedulingEnabled 开关把守（main 按 QUNXIANG_REGION_RUNNER_ENABLED
// 灰度注入），默认关 → 全程 no-op、对默认建局链路零成本，也避免运行器开启时撞见关闭期沉积的历史脏唤醒。

import (
	"context"

	"qunxiang/backend/internal/agentqueue"
	"qunxiang/backend/internal/engine/scheduler"
	"qunxiang/backend/internal/unit"
)

// SetAmbientSchedulingEnabled 由 main/router 按 region-runner 是否启用注入（默认关）。
func (service *Service) SetAmbientSchedulingEnabled(enabled bool) {
	if service == nil {
		return
	}
	service.ambientSchedulingEnabled = enabled
}

// seedAmbientForNewUnit 为「建局之后中途新生/归化」的单位补登记离线调度（婚育子嗣、野民归化等 live 造人点）。仅当单位
// 归属玩家阵营（与建局只 seed 玩家 roster 的口径一致）才 seed——敌方/野民单位不进离线模拟。best-effort、幂等、开关关时 no-op。
// 把「哪些单位进离线调度」的策略集中于此一处：未来新增造人点照此 idiom 调一行即可，避免再漏 seed
// （M7.3-real-4b 评审发现婚育/归化中途造人漏 seed → 中途出生的玩家单位永久缺席离线生活，即此类）。
func (service *Service) seedAmbientForNewUnit(ctx context.Context, state *State, rec unit.Record) {
	if service == nil || state == nil || rec.FactionID != state.PlayerFactionID {
		return
	}
	_ = service.seedAmbientForUnits(ctx, state.ID, "", []string{rec.ID})
}

// seedAmbientForUnits 为一组单位登记离线调度：作用域(world=worldID, region=sessionID)、生命态列=active、入初始唤醒
// (立即到点 WakeAtTick=0、起始 COLD；首次 processOne 会按真实空闲度重新分层)。幂等——唤醒队列按 unit_id upsert、
// 作用域/生命态列幂等覆盖，故重复 seed 安全。**best-effort**：任一步失败只跳过该单位、不中断建局（离线自治是辅助能力，
// 绝不因它拖垮造人）；调用方 `_ =` 其返回，返回首个错误仅供测试断言。开关关 / 依赖缺失时整体 no-op。
func (service *Service) seedAmbientForUnits(ctx context.Context, sessionID string, worldID string, unitIDs []string) error {
	if service == nil || !service.ambientSchedulingEnabled || service.units == nil || service.db == nil {
		return nil
	}
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, unitID := range unitIDs {
		if unitID == "" {
			continue
		}
		// region=sessionID：单人局每会话自成一个 region（region 分片是 real-5+ 的事）。world 可空（单人局无世界）。
		if err := service.units.SetUnitScope(ctx, unitID, worldID, sessionID); err != nil {
			note(err)
			continue
		}
		if err := service.units.SetLifeState(ctx, unitID, unit.LifeStateActive); err != nil {
			note(err)
			continue
		}
		if err := agentqueue.EnqueueWake(ctx, service.db, agentqueue.WakeEntry{
			UnitID: unitID, SessionID: sessionID, WorldID: worldID, RegionID: sessionID,
			WakeAtTick: 0, Tier: string(scheduler.TierCold),
		}); err != nil {
			note(err)
		}
	}
	return firstErr
}
