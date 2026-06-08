package session

// 文件说明：把一局会话接入某个世界，并在实战交互发生时自动写进世界总线（设计 耦合 §1 / 大世界 §8）。
// 现阶段当前对局是单人战棋（敌方是 NPC），自动写总线只在 state.WorldID 非空（接入多世界）时生效——
// 这是为「世界里有别的玩家角色」预备的管线：一旦多世界落地，击杀/救援/馈赠即自动成为跨玩家事实源。

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"

	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
	"qunxiang/backend/internal/worldbus"
)

// defaultWorldID 是共享主世界的稳定常量 ID（get-or-create 的固定锚）。
// 取固定字面量而非 uuid：唯一性约束 + 固定 ID 让「先 Get 再 Create」天然幂等，
// 并发下两个建局同时撞 Create 会触发主键冲突，由 EnsureDefaultWorld 兜底再 Get 一次。
const defaultWorldID = "world_default"

// worldBindingMode 读 QUNXIANG_WORLD_BINDING，归一为三档绑定策略（大小写不敏感、去空白）：
//   - shared（默认，未设/非法/缺省皆归此）：所有 session 接入同一个共享主世界 world_default，
//     是「跨玩家关联」（设计问题 B）的前置——不同 session 的角色须共享世界才能相互浮现/撮合/血仇传播。
//   - per_session：每局接入各自专属世界 world_sess_<sessionID>，跨玩家整层在域内但天然不与他局相交（隔离演练用）。
//   - off：不接入任何世界（WorldID 留空），等价旧行为——worldbus/cross-event/七交互/world-boss/auto_match 全早返回。
//
// 默认 shared 安全：relevance 阈值（0.35）+ 锚机制保证无跨会话关系时不会乱浮现，
// 共享世界只是把跨玩家管线「通电」，而非强制每局都互相干预。
func worldBindingMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_WORLD_BINDING"))) {
	case "per_session":
		return "per_session"
	case "off":
		return "off"
	default:
		return "shared"
	}
}

// EnsureDefaultWorld get-or-create 共享主世界 world_default 并返回其 ID（确定性、幂等）。
// 先 world.Get(defaultWorldID)：命中即返回；ErrNotFound 才 world.Create 固定 ID 的主世界。
// 并发兜底：两个建局同时 Create 会有一方撞唯一键报错，此时再 Get 一次拿到已存在的世界——
// 因此无论谁赢得创建竞争，所有调用方最终都收敛到同一个 world_default。
func (service *Service) EnsureDefaultWorld(ctx context.Context) (string, error) {
	if service == nil || service.db == nil {
		return "", errors.New("ensure default world: nil service or db")
	}
	if _, err := world.Get(ctx, service.db, defaultWorldID); err == nil {
		return defaultWorldID, nil
	} else if !errors.Is(err, world.ErrNotFound) {
		return "", err
	}
	if _, err := world.Create(ctx, service.db, world.World{
		ID:            defaultWorldID,
		Name:          "主世界",
		Status:        world.StatusActive,
		MaxPopulation: 100000,
	}); err != nil {
		// 并发竞争：另一建局先 Create 成功导致主键冲突 → 再 Get 一次兜底；仍失败才真报错。
		if _, getErr := world.Get(ctx, service.db, defaultWorldID); getErr == nil {
			return defaultWorldID, nil
		}
		return "", err
	}
	return defaultWorldID, nil
}

// bindSessionWorld 按 worldBindingMode 把一局接入世界，best-effort：吞错只记日志、绝不阻断建局。
// 调用点须在玩家单位已落库之后（AssignSessionToWorld 要接入这些单位）、且在 ambient/村庄锚落库之前
// （使 state.WorldID 在那时已置位，跨玩家锚才会带正确世界域）。
//   - off：直接 return nil，WorldID 留空 = 旧行为。
//   - shared：worldID = EnsureDefaultWorld（共享主世界）。
//   - per_session：worldID = world_sess_<sessionID>，幂等 Create 一局专属世界。
func (service *Service) bindSessionWorld(ctx context.Context, state *State) error {
	if service == nil || service.db == nil || state == nil {
		return nil
	}
	var worldID string
	switch worldBindingMode() {
	case "off":
		return nil
	case "per_session":
		worldID = "world_sess_" + state.ID
		if _, err := world.Create(ctx, service.db, world.World{
			ID:            worldID,
			Name:          "对局世界 " + state.ID,
			Status:        world.StatusActive,
			MaxPopulation: 100000,
		}); err != nil {
			// 幂等：重连/重复建局撞已存在的同 ID 世界即视为已就绪，仍继续接入。
			if _, getErr := world.Get(ctx, service.db, worldID); getErr != nil {
				log.Printf("bind session world (per_session) best-effort failed (session=%s): %v", state.ID, err)
				return nil
			}
		}
	default: // shared
		id, err := service.EnsureDefaultWorld(ctx)
		if err != nil {
			log.Printf("bind session world (shared) best-effort failed (session=%s): %v", state.ID, err)
			return nil
		}
		worldID = id
	}
	if err := service.AssignSessionToWorld(ctx, state, worldID); err != nil {
		log.Printf("assign session to world best-effort failed (session=%s world=%s): %v", state.ID, worldID, err)
		return nil
	}
	return nil
}

// AssignSessionToWorld 把本局接入某世界：设 state.WorldID，并把本局所有单位接入该世界（幂等）。
func (service *Service) AssignSessionToWorld(ctx context.Context, state *State, worldID string) error {
	if service == nil || service.db == nil || state == nil {
		return nil
	}
	state.WorldID = worldID
	if worldID == "" {
		return nil
	}
	dialect := dbdialect.For(service.db)
	for _, id := range append(append([]string{}, state.PlayerUnitIDs...), state.EnemyUnitIDs...) {
		if id != "" {
			_ = world.Join(ctx, service.db, worldID, id, "inhabitant", dialect)
		}
	}
	return nil
}

// recordWorldizedKill 在接入世界后，把一次击杀作为 CROSS_ATTACK 写进世界总线（best-effort，gate 在 WorldID）。
func (service *Service) recordWorldizedKill(ctx context.Context, state *State, attacker *unit.Record, victim *unit.Record) {
	if service == nil || state == nil || state.WorldID == "" || attacker == nil || victim == nil {
		return
	}
	_, _ = service.RecordCrossInteraction(ctx, state.WorldID, attacker.ID, victim.ID,
		worldbus.KindAttack, 8, map[string]any{"fatal": true})
}
