package session

// 文件说明：elite 濒死反杀 Clutch 计分回归测试（修死代码 finding LOW）。
// 原 ClutchNearDeathReversal 判定基于「撤退线之下」（selfHPFraction<eliteFleeFraction），但反射护栏在每回合开头先于攻击分支
// 触发（HP<eliteFleeHP 当场撤退 break），攻击分支恒在「HP≥撤退线」执行 → 撤退线之下的判定永不到达=死代码、永不计分。
// 修后改判「撤退线之上的濒死带」(selfHPFraction<=eliteNearDeathFraction，0.375)：该带在攻击分支真实可达，Clutch 真正进 Score。
// 这里用「同一弱 elite、不同起始 HP」对照验证：濒死带内的胜利贡献 > 安全区的胜利贡献（差额即 NearDeathReversal 的 Clutch 加成）。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/engine/encounter"
	"qunxiang/backend/internal/unit"
)

// weakOneHPElite 是一头血量极低的弱 elite：角色一击（命中即）终结，便于把变量收敛到「起始 HP 落在濒死带与否」。
func weakOneHPElite(id string) Threat {
	return Threat{
		ID: id, Name: "残兽", Tier: ThreatTierElite, RegionID: "",
		Power: 4, Attack: 1, Defense: 0, HPPool: 1, Severity: 40,
		Loot: []encounter.LootItem{{ID: "gold", Rarity: encounter.Common, Quantity: 1}},
	}
}

// resolveWithStartHP 在干净库里用给定起始 HP 跑一次 elite 遭遇，返回结果（角色攻高、防高，确保终结一击且自身不被反杀）。
func resolveWithStartHP(t *testing.T, startHP int, eliteID string) EliteEncounterResult {
	t.Helper()
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	actor := unit.BootstrapRecord(7, "s_clutch", "player", "孤勇")
	// 确定性首击命中（消除历史 flaky）：BootstrapRecord 的 actor.ID 是随机 UUID，而 combatActionRoll 按
	// (sessionID+attackerID+targetID+salt+turn) 哈希定掷骰——随机 ID 会让「玩家首击是否命中」每跑变一次：偶发
	// 首击 miss → elite 反扑 → 后续回合 selfHPFraction 漂移、濒死带判定抖动。固定 actor.ID 为 clutch_hit_1_<eliteID>
	// （已离线核验：该前缀对全部四个 eliteID 的 round-1 atk roll 均 ≥ eliteMissChance=0.08，首击必中、elite 当场毙、无反扑），
	// 使掷骰完全可复现、贡献分恒定。
	actor.ID = "clutch_hit_1_" + eliteID
	actor.Status.HP = startHP
	actor.Status.Attack = 50  // 远超 elite 1 血：命中即终结
	actor.Status.Defense = 50 // 防高：万一 elite 反扑也不致命
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("保存角色失败: %v", err)
	}
	state := State{ID: "s_clutch"}
	res, err := service.ResolveEliteEncounter(ctx, &state, &actor, weakOneHPElite(eliteID))
	if err != nil {
		t.Fatalf("遭遇失败: %v", err)
	}
	if res.Outcome != "defeated" {
		t.Fatalf("强角色应秒杀 1 血 elite，得到 outcome=%q rounds=%d dealt=%d", res.Outcome, res.Rounds, res.DamageDealt)
	}
	return res
}

// TestEliteNearDeathReversalScoresInBand 濒死带（撤退线之上、≤0.375）内胜利应记 NearDeathReversal Clutch，
// 安全区（>0.375）不记。两者唯一变量是起始 HP；贡献差额恰为 Clutch 折算（NearDeathReversal=1.2 × 权重 1.2）。
func TestEliteNearDeathReversalScoresInBand(t *testing.T) {
	// HP=30：fraction=0.30，落在 (0.25, 0.375] 濒死带 → 计 NearDeathReversal Clutch（撤退线之上、护栏不触发）。
	inBand := resolveWithStartHP(t, 30, "elite_band")
	// HP=90：fraction=0.90，安全区（>0.375）→ 不计 NearDeathReversal Clutch。
	safe := resolveWithStartHP(t, 90, "elite_safe")

	// 终结一击两边都有（FinalBlow），差额只来自濒死带内多计的 NearDeathReversal。
	if inBand.Contribution <= safe.Contribution {
		t.Fatalf("濒死带内胜利贡献应高于安全区（多计 NearDeathReversal Clutch），得到 band=%v safe=%v",
			inBand.Contribution, safe.Contribution)
	}

	// 差额必须恰等于 NearDeathReversal 的 Clutch 折算（ClutchValues.NearDeathReversal × ContributionWeights.Clutch）。
	wantDelta := encounter.ClutchValues.NearDeathReversal * encounter.ContributionWeights.Clutch
	gotDelta := inBand.Contribution - safe.Contribution
	if !floatNear(gotDelta, wantDelta) {
		t.Fatalf("濒死带的贡献加成应恰为 NearDeathReversal 折算 %v，得到差额 %v", wantDelta, gotDelta)
	}
}

// TestEliteNearDeathReversalBoundary 边界：撤退线本身（HP=25，fraction=0.25）在濒死带内（护栏只挡 HP<25，恰=25 不撤退）→ 计 Clutch；
// HP=38（fraction=0.38>0.375）刚出带 → 不计。证明判定锚在「撤退线之上的濒死带上沿 0.375」而非旧的撤退线之下。
func TestEliteNearDeathReversalBoundary(t *testing.T) {
	atFleeLine := resolveWithStartHP(t, 25, "elite_atflee") // fraction=0.25，护栏不触发（HP<25 才撤），在带内
	justAbove := resolveWithStartHP(t, 38, "elite_above")   // fraction=0.38，出带

	if atFleeLine.Contribution <= justAbove.Contribution {
		t.Fatalf("撤退线 HP=25（在濒死带内）应计 Clutch、HP=38（出带）不计，得到 atFlee=%v above=%v",
			atFleeLine.Contribution, justAbove.Contribution)
	}
}
