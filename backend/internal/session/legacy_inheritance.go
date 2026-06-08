package session

// 文件说明：战利品传承（SoulBound / Legacy）——角色死亡时把其背包/装备中的传家遗物（IsLegacy）与灵魂绑定
// epic 掉落（SoulBound）转移给「在乎死者的人」里关系最亲的继承人，并记一条编年史/记忆「继承了 X 的遗物」。
// 设计见 docs/PvE威胁系统.md 战利品传承小节。
//
// 三条不变量：
//  1. best-effort：传承是叙事/玩法派生物，任何失败都吞错、绝不中断战斗主结算（调用方在死亡路径以 _,_= 忽略返回）。
//  2. 确定性：继承人选择不用全局 rand——先按对死者的「净亲密度」（affection 优先、trust 次之）降序，
//     同分用 FNV(session|deceased|mourner) 派生的稳定权重打破平手，全程可复现。
//  3. 不碰受保护状态字段：仅改 ItemStack 所在的 Inventory（JSON blob），经 units.Save 落库；
//     绝不触碰 unit.Status 的 HP/Hunger/Morale/Loyalty/LivesRemaining/Mood（无需 status.Mutator）。

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"

	"qunxiang/backend/internal/unit"
)

// inheritLegacyItems 在 deceased 死亡时把其可传承物品转移给确定性选出的继承人。
// 返回成功转移的物品件数与错误（best-effort：调用方通常忽略，仅留作可测）。
//
// 可传承判定：背包与装备中 IsLegacy==true（传家遗物）或 SoulBound==true（灵魂绑定 epic 掉落）的物品。
// 继承人口径：在乎死者的人（relations.target=死者）里对死者净亲密度最高、且本会话存活的单位。
// 无合格继承人时不实发——遗物随死者归名人堂档案（已由 persistHallOfFame / WorldizeDeath 覆盖叙事），本函数 no-op。
func (service *Service) inheritLegacyItems(ctx context.Context, state *State, deceased unit.Record) (int, error) {
	if service == nil || state == nil || service.units == nil {
		return 0, nil
	}

	// 1) 摘出死者背包/装备里的可传承物品（保持原 ItemStack，含 SoulBound/IsLegacy/Durability 等标记一并传走）。
	legacyItems := collectInheritableItems(deceased)
	if len(legacyItems) == 0 {
		return 0, nil
	}

	// 2) 确定性选继承人：在乎死者的人里净亲密度最高的本会话存活单位。
	heirID := service.chooseLegacyHeir(ctx, state, deceased.ID)
	if heirID == "" {
		// 无合格继承人——遗物归档（名人堂已覆盖叙事），不实发。
		return 0, nil
	}

	heir, err := service.units.GetByID(ctx, heirID)
	if err != nil {
		return 0, fmt.Errorf("load legacy heir %s: %w", heirID, err)
	}
	// 继承人须存活（chooseLegacyHeir 已过滤，这里二次防御：DB 读出后再校验，避免竞态）。
	if heir.Status.LifeState == unit.LifeStateDead {
		return 0, nil
	}

	// 3) 把遗物追加进继承人背包（标记 IsLegacy，确保后续仍可继续传承；保留 SoulBound 不可交易性）。
	// 设计声明：传家遗物**刻意豁免** BackpackCapacity 上限——遗物是叙事/传承资产、数量天然稀少，宁可超额保全也不丢弃
	// （区别于普通战利品走 unit.AddBackpackItem 的容量校验）。遗物不进装备栏、不影响 RecalculateDerivedStats。
	transferred := 0
	for _, stack := range legacyItems {
		stack.IsLegacy = true // 已成传家遗物：继承后仍是遗物，可继续向下传承
		heir.Inventory.Backpack = append(heir.Inventory.Backpack, stack)
		transferred++
	}
	if transferred == 0 {
		return 0, nil
	}

	if err := service.units.Save(ctx, heir); err != nil {
		return 0, fmt.Errorf("save legacy heir %s: %w", heirID, err)
	}

	// 4) 编年史 + 记忆留痕「继承了 X 的遗物」（best-effort：失败不回滚已落库的转移）。
	itemNames := make([]string, 0, len(legacyItems))
	for _, stack := range legacyItems {
		name := strings.TrimSpace(stack.CustomName)
		if name == "" {
			name = displayItemName(stack.ItemID)
		}
		itemNames = append(itemNames, name)
	}
	deceasedName := deceased.DisplayName()
	chronicleText := fmt.Sprintf("我继承了 %s 留下的遗物：%s。", deceasedName, strings.Join(itemNames, "、"))
	_ = service.recordChronicleEntry(ctx, state.ID, heirID, state.TurnState.Turn, "legacy_inherit", chronicleText)
	_ = service.storeMemoryAndSyncHighlights(ctx, &heir, state.TurnState.Turn, chronicleText, "legacy_inherit", 2)

	appendLog(
		state,
		"legacy_inherit",
		fmt.Sprintf("%s 继承了 %s 的遗物（%d 件）。", heir.DisplayName(), deceasedName, transferred),
		heirID,
		deceased.ID,
	)
	return transferred, nil
}

// collectInheritableItems 摘出 record 背包与装备里可传承的 ItemStack（IsLegacy 或 SoulBound）。
// 返回的切片是值拷贝，调用方可安全改其标记后追加到继承人背包，不影响死者原档（死者随后归档/退场）。
func collectInheritableItems(record unit.Record) []unit.ItemStack {
	out := make([]unit.ItemStack, 0, 4)
	for _, stack := range record.Inventory.Backpack {
		if stack.ItemID == "" {
			continue // 跳过空槽幽灵物，避免把空 ItemID 计入传承（与 loot_inheritor 防御一致）
		}
		if stack.IsLegacy || stack.SoulBound {
			out = append(out, stack)
		}
	}
	// 装备栏按 slot 键稳定排序后摘取（map 遍历无序，排序以保确定性）。
	slots := make([]string, 0, len(record.Inventory.Equipment))
	for slot := range record.Inventory.Equipment {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	for _, slot := range slots {
		stack := record.Inventory.Equipment[slot]
		if stack.ItemID == "" {
			continue
		}
		if stack.IsLegacy || stack.SoulBound {
			out = append(out, stack)
		}
	}
	return out
}

// chooseLegacyHeir 在「在乎死者的人」里确定性选出继承人：对死者净亲密度最高、且本会话存活的单位。
// 净亲密度 = affection（主权重）+ 0.5*trust（次权重）；同分用 FNV(session|deceased|mourner) 稳定权重打破平手。
// 无合格继承人返回空串。复用 blood_feud 的 loadCarersOf（relations.target=死者→在乎她的人的关系四轴）。
func (service *Service) chooseLegacyHeir(ctx context.Context, state *State, deceasedID string) string {
	deceasedID = strings.TrimSpace(deceasedID)
	if service == nil || state == nil || deceasedID == "" {
		return ""
	}
	carers := service.loadCarersOf(ctx, deceasedID, legacyHeirCandidateLimit)
	if len(carers) == 0 {
		return ""
	}

	// 本会话存活单位集合（玩家/敌方/野人皆可继承——传承按「在乎」而非阵营）。
	alive := service.aliveUnitIDSet(ctx, state)

	type candidate struct {
		id     string
		score  float64
		tieKey uint64
	}
	candidates := make([]candidate, 0, len(carers))
	for _, c := range carers {
		id := strings.TrimSpace(c.MournerID)
		if id == "" || id == deceasedID {
			continue
		}
		if alive != nil {
			if _, ok := alive[id]; !ok {
				continue
			}
		}
		// 净亲密度：affection 主、trust 半权次（纯敌视者 affection<0 自然排到末尾、被正分候选挤掉）。
		net := c.Affection + 0.5*c.Trust
		candidates = append(candidates, candidate{id: id, score: net, tieKey: legacyHeirTieKey(state.ID, deceasedID, id)})
	}
	if len(candidates) == 0 {
		return ""
	}
	// 净亲密度降序；同分用确定性 tieKey 升序打破平手；再同则按 id 字典序（绝对确定）。
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		if candidates[i].tieKey != candidates[j].tieKey {
			return candidates[i].tieKey < candidates[j].tieKey
		}
		return candidates[i].id < candidates[j].id
	})
	// 至少要有「正向在乎」才继承：净亲密度 <=0 的人不配做遗物继承人（纯敌视/陌路）。
	if candidates[0].score <= 0 {
		return ""
	}
	return candidates[0].id
}

// legacyHeirCandidateLimit 取「在乎死者的人」候选上限（够覆盖最亲密的一圈，避免拉爆）。
const legacyHeirCandidateLimit = 16

// legacyHeirTieKey 由 (session|deceased|heir) 派生稳定的平手权重（无全局 rand，可复现）。
func legacyHeirTieKey(sessionID, deceasedID, heirID string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("legacy_heir:" + sessionID + ":" + deceasedID + ":" + heirID))
	return h.Sum64()
}

// aliveUnitIDSet 返回本会话当前存活单位 id 集合（玩家/敌方/野人）。
// best-effort：逐个 GetByID 读 life_state，读失败的单位视为不存活（保守跳过，不阻断）。
func (service *Service) aliveUnitIDSet(ctx context.Context, state *State) map[string]struct{} {
	if service == nil || state == nil || service.units == nil {
		return nil
	}
	ids := make([]string, 0, len(state.PlayerUnitIDs)+len(state.EnemyUnitIDs)+len(state.WildUnitIDs))
	ids = append(ids, state.PlayerUnitIDs...)
	ids = append(ids, state.EnemyUnitIDs...)
	ids = append(ids, state.WildUnitIDs...)
	alive := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		record, err := service.units.GetByID(ctx, id)
		if err != nil {
			continue
		}
		if record.Status.LifeState == unit.LifeStateDead {
			continue
		}
		alive[id] = struct{}{}
	}
	return alive
}
