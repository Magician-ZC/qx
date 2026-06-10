package session

// 文件说明：任务目录 · 生成护栏（设计 docs/分区大世界设计方案-2026-06-10.md §5「目录只提供任务骨架/类型/区域/等级约束作为生成护栏」）。
// 这里定义**确定性、不含具体文案**的任务骨架（questSkeleton）：按区域 kind/等级给出 Type + Objective 类型 + Required + Reward 倾向。
// 具体标题/叙事/动机由 quest_gen.go 的 LLM 据角色画像动态填充（LLM 只填叙事不改机制——机制全锁在这里）。
//
// 骨架投放规则（确定性，按 zone 属性）：
//   - capital（阵营主城区）：① slay 骨架——讨平本区 boss，奖励解锁同阵营 wild 区传送（魔兽式「打完主城 boss 才能去野外」）；
//                            ② collect 骨架——采集 N 个本区材料（城镇日常委托）。
//   - wild（阵营野外区）：    ① collect 骨架——采集更多材料（野外资源丰）；
//                            ② explore 骨架——到达某相邻区（探索委托）。
//   - starter（中立新手区）：  ① collect 骨架——采集少量材料（引导教学，低压力）；
//                            ② explore 骨架——到达三阵营某主城（引导玩家踏出新手区）。
//
// 不含随机：骨架完全由 zone 字段派生；同一区恒产出同一组骨架（确定性，前端可缓存）。

import "qunxiang/backend/internal/world"

// questSkeleton 是任务骨架（生成护栏）：锁定机制（类型/目标/数值/奖励倾向），不含任何中文文案。
// quest_gen.go 据它 + 角色画像生成 Quest（填 Title/NarrativeZH），id 由 FNV(sessionID+zoneId+Key) 派生。
type questSkeleton struct {
	Key        string      // 骨架稳定键（同区唯一，进 questId 哈希；如 "slay_boss"/"collect_0"/"explore_0"）
	Type       QuestType   // slay/collect/explore
	Objectives []Objective // 目标骨架（Current 恒 0；Target/Required 已锁定）
	Reward     QuestReward // 奖励倾向（数值确定性派生自区域等级；UnlockZone 锁定解锁目标）
}

// questGatherItemID 是收集类任务的目标材料 id（用既有可采集材料，确保 advanceQuestObjectives 能被采集结算命中、
// turn-in 物品奖励也用既有物品）。"ration"（口粮）是地块采集/狩猎的通用产物，全区可得，作收集目标稳妥。
const questGatherItemID = "ration"

// questSkeletonsForZone 返回某区可发布的任务骨架（确定性，按 zone 属性）。
// 无分区世界字段（zone 零值）时返回空——调用方据此不发布任务。
func questSkeletonsForZone(zone world.Zone) []questSkeleton {
	switch zone.Kind {
	case world.ZoneCapital:
		return capitalSkeletons(zone)
	case world.ZoneWild:
		return wildSkeletons(zone)
	case world.ZoneStarter:
		return starterSkeletons(zone)
	default:
		return nil
	}
}

// capitalSkeletons：主城区两桩——讨平本区 boss（解锁同阵营 wild 区传送）+ 采集材料。
func capitalSkeletons(zone world.Zone) []questSkeleton {
	out := make([]questSkeleton, 0, 2)
	// ① slay：讨平本区 boss → 解锁同阵营 wild 区（capital→wild 的 portal 解锁，魔兽式分级推进）。
	//    仅本区确有 boss（capital 恒有）时投放；UnlockZone 取同阵营 wild 区 id（无则不解锁，仍可接打 boss 拿钱）。
	if zone.BossCoord != "" && zone.BossLevel > 0 {
		out = append(out, questSkeleton{
			Key:        "slay_boss",
			Type:       QuestTypeSlay,
			Objectives: []Objective{{Kind: ObjectiveDefeatBoss, Target: zone.ID, Required: 1}},
			Reward: QuestReward{
				Wallet:     questWalletReward(zone.LevelMax),
				Exp:        questExpReward(zone.LevelMax),
				UnlockZone: sameFactionWildZoneID(zone),
			},
		})
	}
	// ② collect：城镇日常委托，采集若干口粮。
	out = append(out, collectSkeleton(zone, "collect_0", questCollectRequired(zone.LevelMax)))
	return out
}

// wildSkeletons：野外区两桩——采集（资源丰，量更大）+ 探索（到达某相邻区）。
func wildSkeletons(zone world.Zone) []questSkeleton {
	out := make([]questSkeleton, 0, 2)
	out = append(out, collectSkeleton(zone, "collect_0", questCollectRequired(zone.LevelMax)+2))
	// explore：到达本区某条传送门/边界通往的相邻区（取首个 portal 目标，确定性）。无传送门则不投放探索骨架。
	if dest := firstPortalDestination(zone); dest != "" {
		out = append(out, questSkeleton{
			Key:        "explore_0",
			Type:       QuestTypeExplore,
			Objectives: []Objective{{Kind: ObjectiveReachZone, Target: dest, Required: 1}},
			Reward: QuestReward{
				Wallet: questWalletReward(zone.LevelMin),
				Exp:    questExpReward(zone.LevelMin),
			},
		})
	}
	return out
}

// starterSkeletons：新手区两桩——少量采集（教学）+ 探索（到达三阵营某主城，引导踏出新手区）。
func starterSkeletons(zone world.Zone) []questSkeleton {
	out := make([]questSkeleton, 0, 2)
	out = append(out, collectSkeleton(zone, "collect_0", 2)) // 新手低压力：仅采 2 个
	if dest := firstPortalDestination(zone); dest != "" {
		out = append(out, questSkeleton{
			Key:        "explore_0",
			Type:       QuestTypeExplore,
			Objectives: []Objective{{Kind: ObjectiveReachZone, Target: dest, Required: 1}},
			Reward: QuestReward{
				Wallet: questWalletReward(zone.LevelMin),
				Exp:    questExpReward(zone.LevelMin),
			},
		})
	}
	return out
}

// collectSkeleton 构造一个采集骨架（采 required 个 questGatherItemID，奖励据区域等级）。
func collectSkeleton(zone world.Zone, key string, required int) questSkeleton {
	if required < 1 {
		required = 1
	}
	return questSkeleton{
		Key:        key,
		Type:       QuestTypeCollect,
		Objectives: []Objective{{Kind: ObjectiveCollectItem, Target: questGatherItemID, Required: required}},
		Reward: QuestReward{
			Wallet:     questWalletReward(zone.LevelMin),
			ItemGrants: nil,
			Exp:        questExpReward(zone.LevelMin),
		},
	}
}

// sameFactionWildZoneID 据主城区推导同阵营野外区 id（zone_<faction>_capital → zone_<faction>_wild）。
// 用约定的 id 命名（worldgen.go 的 defaultWorldSpecs）：仅当区 id 形如 zone_X_capital 时返回 zone_X_wild，否则空。
func sameFactionWildZoneID(zone world.Zone) string {
	const capitalSuffix = "_capital"
	if zone.Kind != world.ZoneCapital {
		return ""
	}
	id := zone.ID
	if len(id) <= len(capitalSuffix) || id[len(id)-len(capitalSuffix):] != capitalSuffix {
		return ""
	}
	return id[:len(id)-len(capitalSuffix)] + "_wild"
}

// firstPortalDestination 取某区首条传送门/边界通往的相邻区 id（确定性：Portals 切片首元素）。无传送门返回空。
func firstPortalDestination(zone world.Zone) string {
	for _, p := range zone.Portals {
		if p.ToZoneID != "" {
			return p.ToZoneID
		}
	}
	return ""
}

// questWalletReward 据区域等级派生金币奖励（确定性，线性：base 20 + level·3）。
func questWalletReward(level int) int {
	if level < 1 {
		level = 1
	}
	return 20 + level*3
}

// questExpReward 据区域等级派生经验奖励（确定性，线性：base 10 + level·5）。
func questExpReward(level int) int {
	if level < 1 {
		level = 1
	}
	return 10 + level*5
}

// questCollectRequired 据区域等级派生采集需求量（确定性：3 + level/5，clamp [3,8]）。
func questCollectRequired(level int) int {
	n := 3 + level/5
	if n < 3 {
		n = 3
	}
	if n > 8 {
		n = 8
	}
	return n
}
