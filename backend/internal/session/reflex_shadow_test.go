package session

// 反射层影子遥测测试：验证「本可省下 LLM」的判定与计数（不依赖 DB/LLM）。

import (
	"testing"

	"qunxiang/backend/internal/unit"
)

func TestReflexShadowCriticalCouldSkip(t *testing.T) {
	t0, s0 := ReflexStats()
	actor := &unit.Record{ID: "u-crit"}
	actor.Status.HP = 10 // < 25% of 100 → 反射撤退，即便有敌在视野/玩家在场也零成本保命
	recordReflexShadow(State{}, actor, []string{"enemy"})
	t1, s1 := ReflexStats()
	if t1 != t0+1 {
		t.Fatalf("总数应 +1：%d -> %d", t0, t1)
	}
	if s1 != s0+1 {
		t.Fatalf("濒死应判可省 LLM（反射撤退）：%d -> %d", s0, s1)
	}
}

func TestReflexShadowWatchedCombatNotSkippable(t *testing.T) {
	t0, s0 := ReflexStats()
	actor := &unit.Record{ID: "u-fight"}
	actor.Status.HP = 90
	actor.Status.InCombat = true
	recordReflexShadow(State{}, actor, []string{"enemy"}) // 敌在视野 + 玩家在场 = 高光节点，应上 LLM
	t1, s1 := ReflexStats()
	if t1 != t0+1 {
		t.Fatalf("总数应 +1：%d -> %d", t0, t1)
	}
	if s1 != s0 {
		t.Fatalf("玩家目睹的交战应交给 LLM、不可省：%d -> %d", s0, s1)
	}
}

func TestReflexShadowQuietTickCouldSkip(t *testing.T) {
	t0, s0 := ReflexStats()
	actor := &unit.Record{ID: "u-quiet"}
	actor.Status.HP = 80
	actor.Status.Hunger = 80
	recordReflexShadow(State{}, actor, nil) // 无敌可打、状态平稳 → 日常反射，零成本
	t1, s1 := ReflexStats()
	if t1 != t0+1 || s1 != s0+1 {
		t.Fatalf("安静 tick 应判可省 LLM：total %d->%d skip %d->%d", t0, t1, s0, s1)
	}
}
