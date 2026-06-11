package session

// 文件说明：生命周期三分类衰老死亡的集成测试（分区大世界阶段4 §6）——
// mortal 高龄老死 + 主角绝对不死（双保险）+ functional 永不死 + flag 默认关零行为 + 死亡入世界编年史。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/engine/turns"
	"qunxiang/backend/internal/unit"
)

func TestSettleAging_MortalDiesOfOldAge(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	t.Setenv("QUNXIANG_AGING", "true") // 开衰老

	// mortal 老者：年龄推到硬上限（naturalDeathProbability(hardCap)==1，必死，确定性）。
	old := unit.BootstrapRecord(7, "s_aging", "freedom", "白发老者")
	old.Identity.Age = agingAgeHardCap // ≥ 硬上限 → 必死
	old.Identity.LifecycleClass = unit.LifecycleMortal
	if err := repo.Save(ctx, old); err != nil {
		t.Fatalf("save old: %v", err)
	}

	state := &State{
		ID:            "s_aging",
		PlayerUnitIDs: []string{}, // 老者不是主角
		TurnState:     turns.State{Turn: 5},
	}
	units := []unit.Record{old}
	service.settleAgingBestEffort(ctx, state, units)

	reloaded, err := repo.GetByID(ctx, old.ID)
	if err != nil {
		t.Fatalf("reload old: %v", err)
	}
	if reloaded.Status.LifeState != unit.LifeStateDead {
		t.Fatalf("到硬上限的 mortal 应自然老死，得 LifeState=%q", reloaded.Status.LifeState)
	}
	if reloaded.Status.LivesRemaining != 0 {
		t.Fatalf("老死应 LivesRemaining=0，得 %d", reloaded.Status.LivesRemaining)
	}
}

func TestSettleAging_ProtagonistNeverDies(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	t.Setenv("QUNXIANG_AGING", "true")

	// 主角：年龄推到硬上限——但因 protagonist 双保险，必须 Age 冻结、绝不死。
	hero := unit.BootstrapRecord(3, "s_hero", "freedom", "恒定主角")
	hero.Identity.Age = agingAgeHardCap + 50
	hero.Identity.LifecycleClass = unit.LifecycleProtagonist
	ageBefore := hero.Identity.Age
	if err := repo.Save(ctx, hero); err != nil {
		t.Fatalf("save hero: %v", err)
	}

	state := &State{
		ID:            "s_hero",
		PlayerUnitIDs: []string{hero.ID}, // 双保险②
		TurnState:     turns.State{Turn: 9},
	}
	service.settleAgingBestEffort(ctx, state, []unit.Record{hero})

	reloaded, _ := repo.GetByID(ctx, hero.ID)
	if reloaded.Status.LifeState == unit.LifeStateDead {
		t.Fatal("主角绝对不死（protagonist 双保险），却被老死了")
	}
	if reloaded.Identity.Age != ageBefore {
		t.Fatalf("主角 Age 应完全冻结=%d，得 %d", ageBefore, reloaded.Identity.Age)
	}
}

func TestSettleAging_ProtagonistByPlayerIDsEvenIfBlankClass(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	t.Setenv("QUNXIANG_AGING", "true")

	// 旧档主角：空 class（未标记），但在 PlayerUnitIDs——双保险②必须兜住、绝不死。
	hero := unit.BootstrapRecord(11, "s_old", "freedom", "旧档主角")
	hero.Identity.Age = agingAgeHardCap + 20
	hero.Identity.LifecycleClass = "" // 空 class（旧档）
	if err := repo.Save(ctx, hero); err != nil {
		t.Fatalf("save hero: %v", err)
	}
	state := &State{ID: "s_old", PlayerUnitIDs: []string{hero.ID}, TurnState: turns.State{Turn: 1}}
	service.settleAgingBestEffort(ctx, state, []unit.Record{hero})

	reloaded, _ := repo.GetByID(ctx, hero.ID)
	if reloaded.Status.LifeState == unit.LifeStateDead {
		t.Fatal("旧档空 class 主角应被 PlayerUnitIDs 双保险兜住，却被老死了")
	}
}

func TestSettleAging_FunctionalNeverDies(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	t.Setenv("QUNXIANG_AGING", "true")

	// functional NPC（商人/铁匠等）：年龄到硬上限也永不死（世界服务恒定）。
	npc := unit.BootstrapRecord(5, "s_fn", "freedom", "城镇铁匠")
	npc.Identity.Age = agingAgeHardCap + 30
	npc.Identity.LifecycleClass = unit.LifecycleFunctional
	ageBefore := npc.Identity.Age
	if err := repo.Save(ctx, npc); err != nil {
		t.Fatalf("save npc: %v", err)
	}
	state := &State{ID: "s_fn", PlayerUnitIDs: []string{}, TurnState: turns.State{Turn: 2}}
	service.settleAgingBestEffort(ctx, state, []unit.Record{npc})

	reloaded, _ := repo.GetByID(ctx, npc.ID)
	if reloaded.Status.LifeState == unit.LifeStateDead {
		t.Fatal("functional NPC 应永不死（世界服务恒定），却死了")
	}
	if reloaded.Identity.Age != ageBefore {
		t.Fatalf("functional Age 应冻结=%d，得 %d", ageBefore, reloaded.Identity.Age)
	}
}

func TestSettleAging_FlagOffNoOp(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	t.Setenv("QUNXIANG_AGING", "false") // 显式关闭（默认已开），测关闭路径

	old := unit.BootstrapRecord(7, "s_off", "freedom", "本不该死的老者")
	old.Identity.Age = agingAgeHardCap + 100
	old.Identity.LifecycleClass = unit.LifecycleMortal
	ageBefore := old.Identity.Age
	if err := repo.Save(ctx, old); err != nil {
		t.Fatalf("save: %v", err)
	}
	state := &State{ID: "s_off", PlayerUnitIDs: []string{}, TurnState: turns.State{Turn: 3}}
	service.settleAgingBestEffort(ctx, state, []unit.Record{old})

	reloaded, _ := repo.GetByID(ctx, old.ID)
	if reloaded.Status.LifeState == unit.LifeStateDead {
		t.Fatal("flag 默认关时不应有任何死亡（零行为）")
	}
	if reloaded.Identity.Age != ageBefore {
		t.Fatalf("flag 关时 Age 不应增长（零行为），期望 %d 得 %d", ageBefore, reloaded.Identity.Age)
	}
}

// TestSettleAging_DeathRemovesFromWorldRosters 验「修复 issue 2」：mortal 散人/村民老死后，其 id 必须从
// state.WildUnitIDs / state.AmbientUnitIDs 摘除（数据正源），否则快照会留「幽灵」token。同时验存活者 id 不被误删。
func TestSettleAging_DeathRemovesFromWorldRosters(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	t.Setenv("QUNXIANG_AGING", "true")

	dyingWild := unit.BootstrapRecord(11, "s_roster", "freedom", "野外老散人")
	dyingWild.Identity.Age = agingAgeHardCap // 必死
	dyingWild.Identity.LifecycleClass = unit.LifecycleMortal
	dyingAmbient := unit.BootstrapRecord(12, "s_roster", "freedom", "据点老村民")
	dyingAmbient.Identity.Age = agingAgeHardCap // 必死
	dyingAmbient.Identity.LifecycleClass = unit.LifecycleMortal
	survivor := unit.BootstrapRecord(13, "s_roster", "freedom", "年轻村民")
	survivor.Identity.Age = 25 // 必活
	survivor.Identity.LifecycleClass = unit.LifecycleMortal
	for _, r := range []unit.Record{dyingWild, dyingAmbient, survivor} {
		if err := repo.Save(ctx, r); err != nil {
			t.Fatalf("save %s: %v", r.ID, err)
		}
	}

	state := &State{
		ID:             "s_roster",
		PlayerUnitIDs:  []string{},
		WildUnitIDs:    []string{dyingWild.ID},
		AmbientUnitIDs: []string{dyingAmbient.ID, survivor.ID},
		TurnState:      turns.State{Turn: 7},
	}
	service.settleAgingBestEffort(ctx, state, []unit.Record{dyingWild, dyingAmbient, survivor})

	if len(state.WildUnitIDs) != 0 {
		t.Fatalf("老死的野外散人 id 应从 WildUnitIDs 摘除，残留 %v", state.WildUnitIDs)
	}
	if len(state.AmbientUnitIDs) != 1 || state.AmbientUnitIDs[0] != survivor.ID {
		t.Fatalf("老死村民应摘除、存活者应保留，得 AmbientUnitIDs=%v", state.AmbientUnitIDs)
	}
}

func TestSettleAging_YoungMortalAgesButLives(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	t.Setenv("QUNXIANG_AGING", "true")

	young := unit.BootstrapRecord(7, "s_young", "freedom", "青年")
	young.Identity.Age = 25 // 远低于 minAge，必活、只增龄
	young.Identity.LifecycleClass = unit.LifecycleMortal
	if err := repo.Save(ctx, young); err != nil {
		t.Fatalf("save: %v", err)
	}
	state := &State{ID: "s_young", PlayerUnitIDs: []string{}, TurnState: turns.State{Turn: 4}}
	service.settleAgingBestEffort(ctx, state, []unit.Record{young})

	reloaded, _ := repo.GetByID(ctx, young.ID)
	if reloaded.Status.LifeState == unit.LifeStateDead {
		t.Fatal("青年 mortal 不应老死")
	}
	if reloaded.Identity.Age != 26 {
		t.Fatalf("青年 mortal 应 Age++=26，得 %d", reloaded.Identity.Age)
	}
}

func TestNaturalDeathProbability_Monotonic(t *testing.T) {
	// 低于 min 恒 0；硬上限恒 1；之间单调不减且夹在 [0,cap]。
	if naturalDeathProbability(agingNaturalDeathMinAge-1) != 0 {
		t.Fatal("低于 minAge 应为 0")
	}
	if naturalDeathProbability(agingAgeHardCap) != 1.0 {
		t.Fatal("硬上限应为 1")
	}
	prev := -1.0
	for age := agingNaturalDeathMinAge; age < agingAgeHardCap; age++ {
		p := naturalDeathProbability(age)
		if p < 0 || p > agingDeathProbCap {
			t.Fatalf("age=%d 概率应在 [0,cap]，得 %.3f", age, p)
		}
		if p < prev {
			t.Fatalf("概率应随龄单调不减，age=%d p=%.3f < prev=%.3f", age, p, prev)
		}
		prev = p
	}
}
