package session

// 文件说明：会话接入世界 + 实战击杀自动写总线测试（对真实 SQLite）。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
	"qunxiang/backend/internal/worldbus"
)

func TestAssignWorldAndKillWritesToBus(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	wid, err := world.Create(ctx, service.db, world.World{Name: "战场所在世界"})
	if err != nil {
		t.Fatalf("建世界失败: %v", err)
	}
	attacker := unit.BootstrapRecord(1, "s1", "player", "甲")
	victim := unit.BootstrapRecord(2, "s1", "enemy", "乙")
	_ = repo.Save(ctx, attacker)
	_ = repo.Save(ctx, victim)

	state := State{ID: "s1", PlayerUnitIDs: []string{attacker.ID}, EnemyUnitIDs: []string{victim.ID}}

	countCross := func(kind worldbus.EventKind) int {
		return countCrossKind(t, service, wid, kind)
	}

	// 未接入世界（WorldID 空）→ 击杀不写总线。
	service.recordWorldizedKill(ctx, &state, &attacker, &victim)
	if countCross(worldbus.KindAttack) != 0 {
		t.Fatalf("未接入世界时击杀不应写总线")
	}

	// 接入世界 → 设 WorldID + 单位入世界。
	if err := service.AssignSessionToWorld(ctx, &state, wid); err != nil {
		t.Fatalf("接入世界失败: %v", err)
	}
	if state.WorldID != wid {
		t.Fatalf("应设 WorldID")
	}
	if members, _ := world.Members(ctx, service.db, wid, 0); len(members) != 2 {
		t.Fatalf("两单位都应入世界，得到 %d", len(members))
	}

	// 接入后击杀 → 写一条 CROSS_ATTACK。
	service.recordWorldizedKill(ctx, &state, &attacker, &victim)
	if countCross(worldbus.KindAttack) != 1 {
		t.Fatalf("接入世界后击杀应写 1 条 CROSS_ATTACK，得到 %d", countCross(worldbus.KindAttack))
	}
}
