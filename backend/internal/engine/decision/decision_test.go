package decision

// 文件说明：决策层路由的单元测试，验证「安全反射优先 / 关键节点升级 LLM / 日常零 LLM / 确定性」。

import "testing"

func TestRoute_SafetyReflexPreemptsKeyNode(t *testing.T) {
	r := DefaultRouter()
	// HP 危急 + 同时是关键节点(首次遭遇)：应先保命走反射，而不是上 LLM。
	d := r.Route(Situation{UnitID: "u1", HP: 10, HPMax: 100, Hunger: 80, FirstContact: true})
	if d.NeedsLLM {
		t.Fatalf("HP 危急时应走安全反射，不应升级 LLM")
	}
	if d.Intent.Action != ActionFlee || d.Intent.Tier != TierReflex {
		t.Fatalf("期望 reflex flee，得到 %+v", d.Intent)
	}
}

func TestRoute_HungerReflex(t *testing.T) {
	r := DefaultRouter()
	d := r.Route(Situation{UnitID: "u1", HP: 100, HPMax: 100, Hunger: 10, HasRation: true})
	if d.NeedsLLM || d.Intent.Action != ActionEat {
		t.Fatalf("饥饿且有口粮应反射进食，得到 %+v needsLLM=%v", d.Intent, d.NeedsLLM)
	}
	// 没口粮则不应反射进食（落到日常反射）。
	d2 := r.Route(Situation{UnitID: "u1", HP: 100, HPMax: 100, Hunger: 10, HasRation: false})
	if d2.Intent.Action == ActionEat {
		t.Fatalf("没有口粮不应进食")
	}
}

func TestRoute_EscalatesOnKeyNodes(t *testing.T) {
	r := DefaultRouter()
	base := Situation{UnitID: "u1", HP: 100, HPMax: 100, Hunger: 90}
	cases := map[string]Situation{
		"first_contact":  withFlag(base, func(s *Situation) { s.FirstContact = true }),
		"new_order":      withFlag(base, func(s *Situation) { s.HasNewOrder = true }),
		"social_offer":   withFlag(base, func(s *Situation) { s.SocialOffer = true }),
		"strategic_fork": withFlag(base, func(s *Situation) { s.StrategicFork = true }),
		"watched_combat": withFlag(base, func(s *Situation) { s.EnemyInSight = true; s.PlayerWatching = true }),
	}
	for name, s := range cases {
		d := r.Route(s)
		if !d.NeedsLLM || d.Intent.Tier != TierDeliberate {
			t.Fatalf("%s 应升级决断层，得到 needsLLM=%v tier=%s", name, d.NeedsLLM, d.Intent.Tier)
		}
	}
}

func TestRoute_RoutineIsReflexNoLLM(t *testing.T) {
	r := DefaultRouter()
	d := r.Route(Situation{UnitID: "u1", HP: 100, HPMax: 100, Hunger: 90, CurrentGoal: ActionGather})
	if d.NeedsLLM {
		t.Fatalf("日常 tick 不应上 LLM")
	}
	if d.Intent.Action != ActionGather || d.Intent.Tier != TierReflex {
		t.Fatalf("日常应沿用既有目标走反射，得到 %+v", d.Intent)
	}
}

func TestRoute_EnemyInSightAloneDoesNotEscalate(t *testing.T) {
	r := DefaultRouter()
	d := r.Route(Situation{UnitID: "u1", HP: 100, HPMax: 100, Hunger: 90, EnemyInSight: true, PlayerWatching: false})
	if d.NeedsLLM {
		t.Fatalf("无玩家在场的视野内敌人属常规，不应上 LLM")
	}
}

// 结构性性质：纯日常 tick 永远零 LLM —— 这是 "<2% 上 LLM 是常态" 的工程保证。
func TestRoute_RoutineNeverEscalates(t *testing.T) {
	r := DefaultRouter()
	escalations := 0
	const n = 2000
	for i := 0; i < n; i++ {
		s := Situation{UnitID: "u", WorldID: "w", RegionID: "rg", Tick: i, HP: 100, HPMax: 100, Hunger: 90, CurrentGoal: ActionContinue}
		if r.Route(s).NeedsLLM {
			escalations++
		}
	}
	if escalations != 0 {
		t.Fatalf("纯日常 tick 应零升级，实际 %d/%d", escalations, n)
	}
}

func TestSeedDeterministicAndRollRange(t *testing.T) {
	s := Situation{UnitID: "u1", WorldID: "w", RegionID: "rg", Tick: 7}
	if Seed(s, "dir") != Seed(s, "dir") {
		t.Fatalf("相同输入的 Seed 应一致")
	}
	if Seed(s, "dir") == Seed(s, "other") {
		t.Fatalf("不同 salt 的 Seed 期望不同")
	}
	s2 := s
	s2.Tick = 8
	if Seed(s, "dir") == Seed(s2, "dir") {
		t.Fatalf("不同 tick 的 Seed 期望不同")
	}
	for i := 0; i < 100; i++ {
		sx := Situation{UnitID: "u", Tick: i}
		v := Roll(sx, "x")
		if v < 0 || v >= 1 {
			t.Fatalf("Roll 应落在 [0,1)，得到 %f", v)
		}
	}
}

func TestZeroRouterUsable(t *testing.T) {
	var r Router // 零值
	d := r.Route(Situation{UnitID: "u", HP: 5, HPMax: 100, Hunger: 90})
	if d.Intent.Action != ActionFlee {
		t.Fatalf("零值 Router 应采用默认阈值仍能撤退，得到 %+v", d.Intent)
	}
}

func withFlag(base Situation, mut func(*Situation)) Situation {
	s := base
	mut(&s)
	return s
}
