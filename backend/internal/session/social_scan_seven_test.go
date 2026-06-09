package session

// 文件说明：七种交互自治触发的补全测试（trade/marriage/war，设计 docs/事件耦合与跨玩家关联.md §2.3）。
// 覆盖：① classifySocialInteraction 对 trade/marriage/war 三类新分类的判定 + 既有 4 类不被改动（边界回归）；
// ② trade 的 arbitration 定价成败差量只改本侧 + 确定性可复现；③ 高后果（联姻/复仇/开战）走 consent_gate 挂 pending；
// ④ frameAutonomousWar 只改本侧 FactionRelations 且同势力安全 no-op。全部对真实 SQLite，确定性、无网络。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// seedRelationUpsert 直接把一对单位的四轴关系行写/覆盖到目标值（upsert，便于在同一测试内重置同一对、把门精确卡到阈值上）。
// 与 blood_feud_test 的 seedRelation（纯 INSERT）区分：本测试需对同一对重复重置，故用 ON CONFLICT 覆盖。
func seedRelationUpsert(t *testing.T, service *Service, src, tgt string, trust, fear, affection, rivalry float64) {
	t.Helper()
	if _, err := service.db.Exec(
		`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry, notes_json, updated_at)
		 VALUES (?,?,?,?,?,?, '{}', '2026-01-01 00:00:00')
		 ON CONFLICT(source_unit_id, target_unit_id) DO UPDATE SET
		   trust=excluded.trust, fear=excluded.fear, affection=excluded.affection, rivalry=excluded.rivalry`,
		src, tgt, trust, fear, affection, rivalry); err != nil {
		t.Fatalf("seed relation %s->%s: %v", src, tgt, err)
	}
}

// TestClassifySocialInteraction_NewThreeTypes 验证 trade/marriage/war 三类新增自治分类按四轴正确判定。
func TestClassifySocialInteraction_NewThreeTypes(t *testing.T) {
	cases := []struct {
		name                            string
		trust, fear, affection, rivalry float64
		want                            SevenInteraction
		wantOK                          bool
	}{
		// 开战：竞争与戒备都极高（rivalry≥8 且 fear≥7）→ war，且优先于复仇。
		{"war", 0, 7, 0, 8, InteractionWar, true},
		{"war_even_higher", -2, 9, -3, 9, InteractionWar, true},
		// 联姻：极高好感 + 高信任（affection≥8 且 trust≥6.5）→ marriage，且优先于结盟。
		{"marriage", 7, 0, 8, 0, InteractionMarriage, true},
		// 交易：浅互利（trust≥3 且 affection≥1.5 且 rivalry<3 且 fear<3）→ trade，排在结识后兜浅对。
		{"trade", 3.5, 1, 2, 0.5, InteractionTrade, true},
		{"trade_floor", 3, 0, 1.5, 0, InteractionTrade, true},
		// 高竞争压制交易：rivalry 达反目门 → 不做生意，落反目而非交易。
		{"trade_blocked_by_rivalry", 3.5, 1, 2, 6, InteractionFallout, true},
		// 未跨任何阈值：信任太低 → 不触发。
		{"none", 1, 0, 0.5, 0, "", false},
	}
	for _, c := range cases {
		got, ok := classifySocialInteraction(relationPromptRow{Trust: c.trust, Fear: c.fear, Affection: c.affection, Rivalry: c.rivalry})
		if ok != c.wantOK || got != c.want {
			t.Fatalf("%s: 期望 (%q,%v)，得 (%q,%v)", c.name, c.want, c.wantOK, got, ok)
		}
	}
}

// TestClassifySocialInteraction_ExistingFourUnchanged 边界回归：既有 4 类（结识/结盟/反目/复仇）判定不被新增类改动。
func TestClassifySocialInteraction_ExistingFourUnchanged(t *testing.T) {
	cases := []struct {
		name                            string
		trust, fear, affection, rivalry float64
		want                            SevenInteraction
	}{
		// 复仇：rivalry≥6 且 fear≥6，但未到开战门（fear<7 或 rivalry<8）→ 仍是复仇，不被 war 抢走。
		{"vengeance_below_war", 0, 6, 0, 6, InteractionVengeance},
		{"vengeance_high_riv_low_fear", 0, 6, 0, 8, InteractionVengeance}, // rivalry8 但 fear6<7 → 非 war
		// 反目：rivalry≥6 或 fear≥5（未到复仇）→ 仍是反目。
		{"fallout_rivalry", 0, 0, 0, 6, InteractionFallout},
		{"fallout_fear", 0, 5, 0, 0, InteractionFallout},
		// 结盟：trust≥6.5 且 affection≥5，但 affection<8 未到联姻门 → 仍是结盟，不被 marriage 抢走。
		{"alliance_below_marriage", 7, 0, 5, 0, InteractionAlliance},
		{"alliance_aff7", 7, 0, 7, 0, InteractionAlliance}, // affection7<8 → 非联姻
		// 结识：trust≥4 且 affection≥2.5（高于交易门）→ 仍是结识，不被 trade 抢走。
		{"acquaint_above_trade", 4, 0, 2.5, 0, InteractionAcquaint},
	}
	for _, c := range cases {
		got, ok := classifySocialInteraction(relationPromptRow{Trust: c.trust, Fear: c.fear, Affection: c.affection, Rivalry: c.rivalry})
		if !ok || got != c.want {
			t.Fatalf("%s: 期望 %q，得 %q(ok=%v)", c.name, c.want, got, ok)
		}
	}
}

// TestAutonomousTradePricing_SideOnlyDeterministic 验证交易 arbitration 定价：
// 只改本侧 source→target、确定性（同输入同结果）、成功 trust 增 / 违约 trust 降·rivalry 增。
func TestAutonomousTradePricing_SideOnlyDeterministic(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	actor := unit.BootstrapRecord(1, "s1", "player", "甲")
	actor.ID = "a"
	target := unit.BootstrapRecord(2, "s1", "player", "乙")
	target.ID = "b"
	for _, r := range []unit.Record{actor, target} {
		if err := repo.Save(ctx, r); err != nil {
			t.Fatalf("save %s: %v", r.ID, err)
		}
	}
	state := &State{ID: "s1"}

	// 确定性：tradeHonored 对同一 (session,turn,pair,row) 必返相同结果。
	row := relationPromptRow{Trust: 3.5, Affection: 2, Rivalry: 0.5}
	first := tradeHonored(state, "a", "b", row)
	for i := 0; i < 8; i++ {
		if tradeHonored(state, "a", "b", row) != first {
			t.Fatalf("tradeHonored 非确定性")
		}
	}

	// 高信任、零竞争 → 履约 Score 远高于违约 → 必判成功（履约）。
	if !tradeHonored(state, "a", "b", relationPromptRow{Trust: 10}) {
		t.Fatalf("高信任零竞争应判履约成功")
	}
	// 零信任、高竞争 → 违约 Score 远高于履约 → 必判违约。
	if tradeHonored(state, "a", "b", relationPromptRow{Trust: 0, Rivalry: 10}) {
		t.Fatalf("零信任高竞争应判违约")
	}

	// 履约路径：只改本侧 a→b 关系（trust 增），不触碰 b→a。
	seedRelationUpsert(t, service, "a", "b", 0, 0, 0, 0)
	service.settleAutonomousTradePricing(ctx, state, &actor, &target, relationPromptRow{Trust: 10})
	if relAxis(t, service, "trust", "a", "b") <= 0 {
		t.Fatalf("履约应增 a→b 信任")
	}
	if relAxis(t, service, "trust", "b", "a") != 0 {
		t.Fatalf("只改本侧：b→a 不应被触碰")
	}

	// 违约路径：trust 降、rivalry 增（用零信任高竞争确保判违约）。
	seedRelationUpsert(t, service, "a", "b", 0, 0, 0, 0)
	service.settleAutonomousTradePricing(ctx, state, &actor, &target, relationPromptRow{Trust: 0, Rivalry: 10})
	if relAxis(t, service, "trust", "a", "b") >= 0 {
		t.Fatalf("违约应降 a→b 信任")
	}
	if relAxis(t, service, "rivalry", "a", "b") <= 0 {
		t.Fatalf("违约应增 a→b 竞争")
	}
}

// TestAutonomousHighConsequenceGoesThroughConsent 验证联姻/开战自治触发经 RecordSevenInteraction 走 consent_gate 挂 pending、本侧暂不变。
func TestAutonomousHighConsequenceGoesThroughConsent(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	if _, err := world.Create(ctx, db, world.World{ID: "w1", Name: "世界"}); err != nil {
		t.Fatalf("create world: %v", err)
	}
	for _, id := range []string{"a", "b"} {
		rec := unit.BootstrapRecord(1, "w1", "player", "x")
		rec.ID = id
		if err := repo.Save(ctx, rec); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}

	for _, interaction := range []SevenInteraction{InteractionMarriage, InteractionWar} {
		affBefore := relAxis(t, service, "affection", "a", "b")
		res, err := service.RecordSevenInteraction(ctx, "w1", "a", "b", interaction, socialImportanceFor(interaction))
		if err != nil {
			t.Fatalf("%s: %v", interaction, err)
		}
		if res.Applied || res.Tier != "requires_consent" || res.ConsentRequestID == "" {
			t.Fatalf("%s 应走 requires_consent 挂 pending、暂不应用，得 %+v", interaction, res)
		}
		if relAxis(t, service, "affection", "a", "b") != affBefore {
			t.Fatalf("%s 待同意期间本侧关系不应变", interaction)
		}
	}
}

// TestFrameAutonomousWar_SideOnly 验证开战落势力级：异势力 → 本侧 FactionRelations 置 war；同势力 → 安全 no-op。
func TestFrameAutonomousWar_SideOnly(t *testing.T) {
	_, _, service := newThreatTestService(t)
	state := &State{ID: "s1", PlayerFactionID: "red", EnemyFactionID: "blue"}

	// 异势力两人 → 把本侧势力关系置 war。
	redUnit := unit.BootstrapRecord(1, "s1", "red", "甲")
	redUnit.ID = "u-red"
	blueUnit := unit.BootstrapRecord(2, "s1", "blue", "乙")
	blueUnit.ID = "u-blue"
	if !service.frameAutonomousWar(state, &redUnit, &blueUnit) {
		t.Fatalf("异势力开战应改本侧势力关系")
	}
	if factionRelationBetween(*state, "red", "blue") != FactionRelationWar {
		t.Fatalf("本侧 red<->blue 应为 war")
	}

	// 同势力两人 → canonicalFactionPair 拒，安全 no-op（不 panic、返回 false）。
	sameA := unit.BootstrapRecord(3, "s1", "red", "丙")
	sameA.ID = "u-r2"
	sameB := unit.BootstrapRecord(4, "s1", "red", "丁")
	sameB.ID = "u-r3"
	if service.frameAutonomousWar(state, &sameA, &sameB) {
		t.Fatalf("同势力两人不应产生势力级开战（应安全 no-op）")
	}

	// nil 守卫安全。
	if service.frameAutonomousWar(nil, &redUnit, &blueUnit) {
		t.Fatalf("state 为 nil 应安全返回 false")
	}
}

// TestCrossReasonForInteraction 验证三类新交互各映射到其专属跨玩家 reason-code。
func TestCrossReasonForInteraction(t *testing.T) {
	cases := map[SevenInteraction]string{
		InteractionTrade:    "CROSS_TRADE",
		InteractionMarriage: "CROSS_MARRIAGE",
		InteractionWar:      "CROSS_WAR_DRAW",
	}
	for interaction, want := range cases {
		if got := string(crossReasonForInteraction(interaction)); got != want {
			t.Fatalf("%s 应映射 %s，得 %s", interaction, want, got)
		}
	}
}
