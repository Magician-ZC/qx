package session

// 文件说明：阵营冲突战斗源（F3）与阵营概览（ListFactionsDetail）的集成/单元测试（对真实 SQLite）。
// 覆盖：
//   - hostileFactionFor：order↔chaos 互敌、freedom/未知无敌对。
//   - factionPvEEnabled / scanFactionConflicts：flag 默认关不触发（零行为）。
//   - scanFactionConflicts（flag 开）：确定性触发后对手入 EnemyUnitIDs、出 FACTION_CONFLICT 留痕、命运卡可接管。
//   - 确定性：同输入同结果（触发与否、对手数一致）。
//   - best-effort：缺依赖（nil units）不 panic、不触发。
//   - ListFactionsDetail：返三阵营 + 人口按 profile.faction 计数。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/faction"
	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/unit"
)

func TestHostileFactionFor(t *testing.T) {
	if got := hostileFactionFor(faction.IDOrder); got != faction.IDChaos {
		t.Errorf("order 的敌对应为 chaos，得 %q", got)
	}
	if got := hostileFactionFor(faction.IDChaos); got != faction.IDOrder {
		t.Errorf("chaos 的敌对应为 order，得 %q", got)
	}
	if got := hostileFactionFor(faction.IDFreedom); got != "" {
		t.Errorf("freedom 游离应无天然敌对，得 %q", got)
	}
	if got := hostileFactionFor(""); got != "" {
		t.Errorf("无阵营应无敌对，得 %q", got)
	}
	// 中文别名也应归一识别。
	if got := hostileFactionFor("秩序"); got != faction.IDChaos {
		t.Errorf("中文别名 秩序 的敌对应为 chaos，得 %q", got)
	}
}

// seedConflictActor 落一名秩序阵营、玩家可控、battle-ready 的角色，返回其记录。
func seedConflictActor(t *testing.T, service *Service, state *State, name string) unit.Record {
	t.Helper()
	actor := unit.BootstrapRecord(7, state.ID, state.PlayerFactionID, name)
	actor.Faction = faction.IDOrder
	actor.MoralAlignment = faction.BaselineFor(faction.IDOrder)
	actor.Status.HP = 100
	actor.Status.Attack = 12
	actor.Status.Defense = 4
	if err := service.units.Save(context.Background(), actor); err != nil {
		t.Fatalf("save actor: %v", err)
	}
	state.PlayerUnitIDs = append(state.PlayerUnitIDs, actor.ID)
	return actor
}

func TestScanFactionConflicts_FlagOffNoOp(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()

	// flag 默认关：不设 override、不设 env → 零行为。
	resetFactionPvEFlag(t)

	state := &State{ID: "s_conf_off", PlayerFactionID: "player", EnemyFactionID: "enemy"}
	state.TurnState.Turn = factionConflictEveryNTurns // 命中扫描周期
	actor := seedConflictActor(t, service, state, "秩序游骑")

	service.scanFactionConflicts(ctx, state, []unit.Record{actor})

	if len(state.EnemyUnitIDs) != 0 {
		t.Fatalf("flag 关时不应触发冲突（EnemyUnitIDs 应空），得 %d 个", len(state.EnemyUnitIDs))
	}
	if n := eventCount(t, service.db); n != 0 {
		t.Fatalf("flag 关时不应留痕，events 得 %d 条", n)
	}
}

func TestScanFactionConflicts_FlagOnAppendsOpponentAndCard(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	enableFactionPvEFlag(t)

	state := &State{ID: "s_conf_on", PlayerFactionID: "player", EnemyFactionID: "enemy"}
	actor := seedConflictActor(t, service, state, "执法者")

	// 在命中扫描周期里，找到一个能让该角色掷骰过阈的回合（确定性 FNV，必有低回合命中）。
	turn, hit := findTriggeringTurn(state.ID, actor.ID)
	if !hit {
		t.Skip("确定性掷骰在搜索区间内未命中触发阈（极少见）；跳过")
	}
	state.TurnState.Turn = turn

	before := len(state.EnemyUnitIDs)
	service.scanFactionConflicts(ctx, state, []unit.Record{actor})

	if len(state.EnemyUnitIDs) != before+1 {
		t.Fatalf("触发后应 append 一名对手进 EnemyUnitIDs，得 %d（before %d）", len(state.EnemyUnitIDs), before)
	}
	opponentID := state.EnemyUnitIDs[len(state.EnemyUnitIDs)-1]
	// 对手已落库且属敌对阵营（chaos）。
	opp, err := repo.GetByID(ctx, opponentID)
	if err != nil {
		t.Fatalf("对手应已落库: %v", err)
	}
	if opp.Faction != faction.IDChaos {
		t.Fatalf("对手应属敌对阵营 chaos，得 %q", opp.Faction)
	}
	if opp.FactionID == state.PlayerFactionID {
		t.Fatalf("对手队伍 FactionID 不应等于玩家阵营（否则不被视为敌方）")
	}
	// 应有 FACTION_CONFLICT 留痕（流程事件 + 命运卡相关事件均落 events 表，至少 1 条）。
	if n := eventCount(t, service.db); n == 0 {
		t.Fatalf("触发后应有 events 留痕，得 0 条")
	}

	// 确定性：同输入再扫一遍（新 state，同 sessionID/turn/actor）应同样触发同样对手数。
	state2 := &State{ID: state.ID, PlayerFactionID: "player", EnemyFactionID: "enemy"}
	state2.TurnState.Turn = turn
	state2.PlayerUnitIDs = []string{actor.ID}
	service.scanFactionConflicts(ctx, state2, []unit.Record{actor})
	if len(state2.EnemyUnitIDs) != 1 {
		t.Fatalf("确定性：同输入应同样触发 1 名对手，得 %d", len(state2.EnemyUnitIDs))
	}
}

func TestScanFactionConflicts_FreedomActorNeverTriggers(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	enableFactionPvEFlag(t)

	state := &State{ID: "s_conf_free", PlayerFactionID: "player", EnemyFactionID: "enemy"}
	actor := unit.BootstrapRecord(9, state.ID, "player", "浪客")
	actor.Faction = faction.IDFreedom // 游离阵营：不主动挑事
	actor.Status.HP = 100
	if err := service.units.Save(ctx, actor); err != nil {
		t.Fatalf("save: %v", err)
	}
	state.PlayerUnitIDs = []string{actor.ID}

	// 扫遍多个命中周期：freedom 角色恒不触发（hostileFactionFor 返空）。
	for k := 1; k <= 20; k++ {
		state.TurnState.Turn = k * factionConflictEveryNTurns
		service.scanFactionConflicts(ctx, state, []unit.Record{actor})
	}
	if len(state.EnemyUnitIDs) != 0 {
		t.Fatalf("freedom 游离角色不应触发任何冲突，得 %d 个对手", len(state.EnemyUnitIDs))
	}
}

func TestScanFactionConflicts_NilDepsNoPanic(t *testing.T) {
	enableFactionPvEFlag(t)
	// nil units 的服务：best-effort 早返回、不 panic、不触发。
	service := &Service{}
	state := &State{ID: "s_nil", PlayerFactionID: "player"}
	state.TurnState.Turn = factionConflictEveryNTurns
	service.scanFactionConflicts(context.Background(), state, []unit.Record{{ID: "u1", Faction: faction.IDOrder}})
	if len(state.EnemyUnitIDs) != 0 {
		t.Fatalf("nil 依赖时应零行为，得 %d 个对手", len(state.EnemyUnitIDs))
	}
}

func TestListFactionsDetail(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	// 落几名带阵营的单位：2 order、1 chaos、1 无阵营（不计）。
	saveFactioned := func(id, fid string) {
		rec := unit.BootstrapRecord(1, "s_fac", "player", id)
		rec.ID = id
		rec.Faction = fid
		if err := repo.Save(ctx, rec); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	saveFactioned("u_o1", faction.IDOrder)
	saveFactioned("u_o2", faction.IDOrder)
	saveFactioned("u_c1", faction.IDChaos)
	saveFactioned("u_none", "") // 无阵营：不计入任何阵营人口

	details, err := service.ListFactionsDetail(ctx)
	if err != nil {
		t.Fatalf("ListFactionsDetail 失败: %v", err)
	}
	if len(details) != 3 {
		t.Fatalf("应返回 3 阵营，得 %d", len(details))
	}
	byID := map[string]FactionDetail{}
	for _, d := range details {
		byID[d.ID] = d
	}
	for _, want := range []string{faction.IDFreedom, faction.IDOrder, faction.IDChaos} {
		if _, ok := byID[want]; !ok {
			t.Fatalf("应含阵营 %s", want)
		}
	}
	if byID[faction.IDOrder].Population != 2 {
		t.Errorf("order 人口应为 2，得 %d", byID[faction.IDOrder].Population)
	}
	if byID[faction.IDChaos].Population != 1 {
		t.Errorf("chaos 人口应为 1，得 %d", byID[faction.IDChaos].Population)
	}
	if byID[faction.IDFreedom].Population != 0 {
		t.Errorf("freedom 人口应为 0，得 %d", byID[faction.IDFreedom].Population)
	}
	// 道德基准 + 据点 + 信条非空（来自 faction.All() 静态定义）。
	od := byID[faction.IDOrder]
	if od.NameZH != "秩序" {
		t.Errorf("order 中文名应为 秩序，得 %q", od.NameZH)
	}
	if od.Baseline.Order != 70 {
		t.Errorf("order 道德基准 Order 维应为 70，得 %v", od.Baseline.Order)
	}
	if len(od.SpawnPoints) == 0 {
		t.Errorf("order 应有出生据点")
	}
	if od.MoralCreed == "" {
		t.Errorf("order 应有道德信条")
	}
}

// ---- 测试辅助 ----

// resetFactionPvEFlag 清除 QUNXIANG_FACTION_PVE 的运行时 override 与环境（默认关态）。
func resetFactionPvEFlag(t *testing.T) {
	t.Helper()
	t.Setenv(factionPvEFlagEnv, "")
	_, _ = featureflags.ClearOverride(factionPvEFlagEnv)
	t.Cleanup(func() { _, _ = featureflags.ClearOverride(factionPvEFlagEnv) })
}

// enableFactionPvEFlag 经运行时 override 打开 QUNXIANG_FACTION_PVE（测试结束自动清除）。
func enableFactionPvEFlag(t *testing.T) {
	t.Helper()
	if err := featureflags.SetOverride(factionPvEFlagEnv, "true"); err != nil {
		t.Fatalf("打开 QUNXIANG_FACTION_PVE 失败: %v", err)
	}
	t.Cleanup(func() { _, _ = featureflags.ClearOverride(factionPvEFlagEnv) })
}

// findTriggeringTurn 在命中扫描周期的若干回合里，找一个让该 actor 确定性掷骰过触发阈的回合。
// 返回 (回合号, 是否找到)。用于在确定性框架下稳定构造「会触发」的输入。
func findTriggeringTurn(sessionID, actorID string) (int, bool) {
	for k := 1; k <= 200; k++ {
		turn := k * factionConflictEveryNTurns
		if factionConflictRoll(sessionID, turn, actorID) < factionConflictProbability {
			return turn, true
		}
	}
	return 0, false
}
