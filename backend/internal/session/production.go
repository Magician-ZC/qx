package session

// 文件说明：管理生产与建造系统，负责候选生成、资源结算、建筑进度推进与经济行为落地。

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/item"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// itemGrant 结构体用于承载该模块的核心数据。
type itemGrant struct {
	ItemID   string
	Quantity int
}

type equipmentNamePayload struct {
	Name string `json:"name"`
}

var equipmentNameSchema = []byte(`{
  "type":"object",
  "properties":{"name":{"type":"string","minLength":2,"maxLength":16}},
  "required":["name"],
  "additionalProperties":false
}`)

// buildEconomyCandidates 基于地形、威胁距离与方针，生成单位可自主执行的生产/建造候选。
func buildEconomyCandidates(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
) []decisionCandidate {
	if actor == nil || actor.Status.LifeState != unit.LifeStateActive {
		return nil
	}

	coord := world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}
	terrain := terrainAt(state.Map, coord)
	threatDistance := nearestThreatDistance(state, byID, actor)
	directive := strings.ToLower(strings.TrimSpace(directiveContextForActor(state, actor.ID, actor.FactionID)))
	if directive == "" {
		directive = strings.ToLower(strings.TrimSpace(directiveForFaction(state, actor.FactionID)))
	}
	buildingDirective := directivePrefersBuild(directive)

	allowGather := threatDistance >= 3 || (threatDistance >= 2 && actor.Status.Hunger <= 20)
	allowEconomicBuild := threatDistance >= 3 || (buildingDirective && threatDistance >= 2 && actor.Status.HP >= 50)
	allowDefensiveBuild := threatDistance >= 2 || directivePrefersFortify(directive)
	currentStructure := structureAt(state.Structures, coord)

	candidates := make([]decisionCandidate, 0, 6)

	if currentStructure != nil &&
		currentStructure.FactionID == actor.FactionID &&
		currentStructure.Type == StructureTypeFarmland &&
		structureReady(*currentStructure) &&
		currentStructure.HarvestReadyTurn > 0 &&
		state.TurnState.Turn >= currentStructure.HarvestReadyTurn &&
		allowGather {
		candidates = append(candidates, decisionCandidate{
			ID:          fmt.Sprintf("gather:%s:%s", ProductionActivityFarm, currentStructure.ID),
			Action:      DecisionActionGather,
			Activity:    ProductionActivityFarm,
			StructureID: currentStructure.ID,
			Summary:     gatherCandidateSummary(state, *actor, ProductionActivityFarm, currentStructure.ID),
		})
	}

	if allowGather {
		if terrainSupportsActivity(terrain, ProductionActivityHunt) && hasUsableWeapon(*actor) {
			candidates = append(candidates, decisionCandidate{
				ID:       fmt.Sprintf("gather:%s", ProductionActivityHunt),
				Action:   DecisionActionGather,
				Activity: ProductionActivityHunt,
				Summary:  gatherCandidateSummary(state, *actor, ProductionActivityHunt, ""),
			})
		}
		if terrainSupportsActivity(terrain, ProductionActivityForage) {
			candidates = append(candidates, decisionCandidate{
				ID:       fmt.Sprintf("gather:%s", ProductionActivityForage),
				Action:   DecisionActionGather,
				Activity: ProductionActivityForage,
				Summary:  gatherCandidateSummary(state, *actor, ProductionActivityForage, ""),
			})
		}
		if terrainSupportsActivity(terrain, ProductionActivityMine) {
			candidates = append(candidates, decisionCandidate{
				ID:       fmt.Sprintf("gather:%s", ProductionActivityMine),
				Action:   DecisionActionGather,
				Activity: ProductionActivityMine,
				Summary:  gatherCandidateSummary(state, *actor, ProductionActivityMine, ""),
			})
		}
		if terrainSupportsActivity(terrain, ProductionActivityFish) {
			candidates = append(candidates, decisionCandidate{
				ID:       fmt.Sprintf("gather:%s", ProductionActivityFish),
				Action:   DecisionActionGather,
				Activity: ProductionActivityFish,
				Summary:  gatherCandidateSummary(state, *actor, ProductionActivityFish, ""),
			})
		}
	}

	if currentStructure == nil && allowEconomicBuild {
		if terrainSupportsStructure(terrain, StructureTypeFarmland) {
			candidates = append(candidates, decisionCandidate{
				ID:            fmt.Sprintf("build:%s", StructureTypeFarmland),
				Action:        DecisionActionBuild,
				StructureType: StructureTypeFarmland,
				Summary:       buildCandidateSummary(StructureTypeFarmland),
			})
		}
		if canBuildStructure(*actor, StructureTypeForge) && terrainSupportsStructure(terrain, StructureTypeForge) {
			candidates = append(candidates, decisionCandidate{
				ID:            fmt.Sprintf("build:%s", StructureTypeForge),
				Action:        DecisionActionBuild,
				StructureType: StructureTypeForge,
				Summary:       buildCandidateSummary(StructureTypeForge),
			})
		}
	}

	if currentStructure != nil &&
		currentStructure.FactionID == actor.FactionID &&
		!structureReady(*currentStructure) {
		candidates = append(candidates, decisionCandidate{
			ID:            fmt.Sprintf("build:%s:%s", currentStructure.Type, currentStructure.ID),
			Action:        DecisionActionBuild,
			StructureID:   currentStructure.ID,
			StructureType: currentStructure.Type,
			Summary:       fmt.Sprintf("继续修建%s，当前进度 %d/%d。", structureDisplayName(currentStructure.Type), currentStructure.BuildProgress, currentStructure.BuildRequired),
		})
		candidates = append(candidates, buildDemolishCandidate(*currentStructure))
		return candidates
	}

	if currentStructure != nil && currentStructure.FactionID == actor.FactionID {
		candidates = append(candidates, buildDemolishCandidate(*currentStructure))
		return candidates
	}

	if currentStructure == nil && allowDefensiveBuild {
		if canBuildStructure(*actor, StructureTypeTrap) && terrainSupportsStructure(terrain, StructureTypeTrap) {
			candidates = append(candidates, decisionCandidate{
				ID:            fmt.Sprintf("build:%s", StructureTypeTrap),
				Action:        DecisionActionBuild,
				StructureType: StructureTypeTrap,
				Summary:       buildCandidateSummary(StructureTypeTrap),
			})
		}
		if canBuildStructure(*actor, StructureTypeWatchtower) && terrainSupportsStructure(terrain, StructureTypeWatchtower) {
			candidates = append(candidates, decisionCandidate{
				ID:            fmt.Sprintf("build:%s", StructureTypeWatchtower),
				Action:        DecisionActionBuild,
				StructureType: StructureTypeWatchtower,
				Summary:       buildCandidateSummary(StructureTypeWatchtower),
			})
		}
		if canBuildStructure(*actor, StructureTypeTurret) && terrainSupportsStructure(terrain, StructureTypeTurret) {
			candidates = append(candidates, decisionCandidate{
				ID:            fmt.Sprintf("build:%s", StructureTypeTurret),
				Action:        DecisionActionBuild,
				StructureType: StructureTypeTurret,
				Summary:       buildCandidateSummary(StructureTypeTurret),
			})
		}
	}

	sort.SliceStable(candidates, func(i int, j int) bool {
		if buildingDirective && candidates[i].Action != candidates[j].Action {
			if candidates[i].Action == DecisionActionBuild {
				return true
			}
			if candidates[j].Action == DecisionActionBuild {
				return false
			}
		}
		return candidates[i].ID < candidates[j].ID
	})
	return candidates
}

// buildEquipmentCandidates 生成装备相关候选：穿戴背包装备、在铁匠铺锻造新装备、强化已有装备。
func buildEquipmentCandidates(state State, actor *unit.Record) []decisionCandidate {
	if actor == nil || !isBattleReady(*actor) {
		return nil
	}
	candidates := make([]decisionCandidate, 0, 6)
	for _, stack := range actor.Inventory.Backpack {
		definition, ok := item.Lookup(stack.ItemID)
		if !ok || definition.Slot == "" {
			continue
		}
		candidates = append(candidates, decisionCandidate{
			ID:     fmt.Sprintf("equip:%s", stack.ItemID),
			Action: DecisionActionEquip,
			ItemID: stack.ItemID,
			Summary: fmt.Sprintf(
				"装备背包中的%s到%s栏；效果：%s。",
				displayStackName(stack),
				equipmentSlotDisplayName(definition.Slot),
				formatItemStackEffect(stack),
			),
		})
	}

	if !actorAtReadyFriendlyForge(state, actor) {
		return candidates
	}
	for _, forgeItemID := range forgeableEquipmentIDs(*actor) {
		candidates = append(candidates, decisionCandidate{
			ID:     fmt.Sprintf("forge:%s", forgeItemID),
			Action: DecisionActionForge,
			ItemID: forgeItemID,
			Summary: fmt.Sprintf(
				"在铁匠铺锻造%s（消耗：%s；成品效果：%s），成品会由 LLM 结合锻造者命名并自动装备。",
				displayItemName(forgeItemID),
				formatItemRewards(forgeCosts(forgeItemID)),
				formatItemEffectByID(forgeItemID),
			),
		})
	}
	for _, stack := range upgradeableEquipment(*actor) {
		costs := upgradeCosts(stack)
		if !hasBackpackCosts(*actor, costs) {
			continue
		}
		candidates = append(candidates, decisionCandidate{
			ID:     fmt.Sprintf("upgrade:%s", stack.ItemID),
			Action: DecisionActionUpgrade,
			ItemID: stack.ItemID,
			Summary: fmt.Sprintf(
				"在铁匠铺强化%s到 +%d（消耗：%s；强化后效果：%s）。",
				displayStackName(stack),
				stack.Level+1,
				formatItemRewards(costs),
				formatItemStackEffect(unit.ItemStack{ItemID: stack.ItemID, Quantity: stack.Quantity, CustomName: stack.CustomName, Level: stack.Level + 1}),
			),
		})
	}
	return candidates
}

func actorAtReadyFriendlyForge(state State, actor *unit.Record) bool {
	if actor == nil {
		return false
	}
	structure := structureAt(state.Structures, world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR})
	return structure != nil && structure.FactionID == actor.FactionID && structure.Type == StructureTypeForge && structureReady(*structure)
}

func preferredForgeItemID(record unit.Record) string {
	text := strings.ToLower(strings.Join(append([]string{record.Identity.Biography}, record.Skills.Specialties...), " "))
	switch {
	case record.Skills.Weapons.Bow > max(record.Skills.Weapons.Sword, record.Skills.Weapons.Blunt) || containsAny(text, "猎", "弓", "射手", "斥候", "ranger", "archer"):
		return "bow"
	case record.Skills.Weapons.Blunt > max(record.Skills.Weapons.Sword, record.Skills.Weapons.Bow) || containsAny(text, "锤", "矿", "smith", "miner"):
		return "warhammer"
	case record.Skills.Weapons.Shield >= 3 || record.Personality.Prudence >= 0.7 || containsAny(text, "守", "护卫", "warden", "guardian"):
		return "kite_shield"
	case record.Skills.Survival.Scouting >= 3 || record.Skills.Survival.Stealth >= 3 || containsAny(text, "侦", "潜", "游侠", "scout", "spy"):
		return "leather_boots"
	default:
		return "long_sword"
	}
}

func forgeableEquipmentIDs(record unit.Record) []string {
	preferred := preferredForgeItemID(record)
	ids := make([]string, 0, 4)
	seen := map[string]struct{}{}
	appendIfForgeable := func(itemID string) {
		if _, ok := seen[itemID]; ok || !canForgeEquipment(record, itemID) {
			return
		}
		ids = append(ids, itemID)
		seen[itemID] = struct{}{}
	}
	appendIfForgeable(preferred)
	for _, itemID := range []string{"long_sword", "short_sword", "bow", "leather_armor", "leather_boots", "kite_shield", "warhammer"} {
		appendIfForgeable(itemID)
		if len(ids) >= 3 {
			break
		}
	}
	return ids
}

func forgeCosts(itemID string) []itemGrant {
	definition, ok := item.Lookup(itemID)
	if !ok || definition.Slot == "" {
		return nil
	}
	switch definition.Slot {
	case item.SlotWeapon:
		if containsAny(strings.Join(definition.Tags, " "), "ranged", "bow") {
			return []itemGrant{{ItemID: "wood", Quantity: 1}, {ItemID: "leather", Quantity: 1}, {ItemID: "iron_ore", Quantity: 1}}
		}
		return []itemGrant{{ItemID: "iron_ore", Quantity: 2}, {ItemID: "wood", Quantity: 1}}
	case item.SlotArmor:
		return []itemGrant{{ItemID: "leather", Quantity: 2}, {ItemID: "iron_ore", Quantity: 1}}
	case item.SlotShoes:
		return []itemGrant{{ItemID: "leather", Quantity: 1}, {ItemID: "cloth_roll", Quantity: 1}}
	case item.SlotAccessory:
		return []itemGrant{{ItemID: "iron_ore", Quantity: 1}, {ItemID: "gemstone", Quantity: 1}}
	default:
		return nil
	}
}

func canForgeEquipment(record unit.Record, itemID string) bool {
	return hasBackpackCosts(record, forgeCosts(itemID))
}

func upgradeCosts(stack unit.ItemStack) []itemGrant {
	nextLevel := stack.Level + 1
	if nextLevel <= 0 {
		nextLevel = 1
	}
	costs := []itemGrant{
		{ItemID: "iron_ore", Quantity: nextLevel},
		{ItemID: "stone", Quantity: nextLevel},
	}
	definition, ok := item.Lookup(stack.ItemID)
	if !ok {
		return costs
	}
	switch definition.Slot {
	case item.SlotArmor, item.SlotShoes:
		costs = append(costs, itemGrant{ItemID: "leather", Quantity: nextLevel})
	case item.SlotAccessory:
		costs = append(costs, itemGrant{ItemID: "gemstone", Quantity: max(1, nextLevel-1)})
	case item.SlotWeapon:
		if containsAny(strings.Join(definition.Tags, " "), "ranged", "bow", "crossbow") {
			costs = append(costs, itemGrant{ItemID: "wood", Quantity: nextLevel})
		}
	}
	return costs
}

func hasBackpackCosts(record unit.Record, costs []itemGrant) bool {
	for _, cost := range costs {
		if !hasBackpackQuantity(record, cost.ItemID, cost.Quantity) {
			return false
		}
	}
	return len(costs) > 0
}

func upgradeableEquipment(record unit.Record) []unit.ItemStack {
	stacks := make([]unit.ItemStack, 0, len(record.Inventory.Equipment)+len(record.Inventory.Backpack))
	for _, stack := range record.Inventory.Equipment {
		if definition, ok := item.Lookup(stack.ItemID); ok && definition.Slot != "" {
			stacks = append(stacks, stack)
		}
	}
	for _, stack := range record.Inventory.Backpack {
		if definition, ok := item.Lookup(stack.ItemID); ok && definition.Slot != "" {
			stacks = append(stacks, stack)
		}
	}
	return stacks
}

func findOwnedEquipment(record unit.Record, itemID string) (unit.ItemStack, bool) {
	for _, stack := range record.Inventory.Equipment {
		if stack.ItemID == itemID {
			return stack, true
		}
	}
	for _, stack := range record.Inventory.Backpack {
		if stack.ItemID == itemID {
			if definition, ok := item.Lookup(stack.ItemID); ok && definition.Slot != "" {
				return stack, true
			}
		}
	}
	return unit.ItemStack{}, false
}

func hasBackpackEquipment(record unit.Record, itemID string) bool {
	for _, stack := range record.Inventory.Backpack {
		if stack.ItemID != itemID {
			continue
		}
		definition, ok := item.Lookup(stack.ItemID)
		return ok && definition.Slot != ""
	}
	return false
}

func equipmentSlotDisplayName(slot item.Slot) string {
	switch slot {
	case item.SlotWeapon:
		return "武器"
	case item.SlotArmor:
		return "护甲"
	case item.SlotShoes:
		return "鞋履"
	case item.SlotAccessory:
		return "饰品"
	default:
		return string(slot)
	}
}

func displayStackName(stack unit.ItemStack) string {
	return displayNamedItem(stack.ItemID, stack.CustomName, stack.Level)
}

// formatItemEffectByID 生成给 LLM 阅读的物品功效摘要，避免只看到物品名却不知道用途。
func formatItemEffectByID(itemID string) string {
	definition, ok := item.Lookup(itemID)
	if !ok {
		return "未知用途"
	}
	return formatItemDefinitionEffect(definition, 0)
}

func formatItemStackEffect(stack unit.ItemStack) string {
	definition, ok := item.Lookup(stack.ItemID)
	if !ok {
		return "未知用途"
	}
	return formatItemDefinitionEffect(definition, stack.Level)
}

func formatItemDefinitionEffect(definition item.Definition, level int) string {
	parts := make([]string, 0, 5)
	if definition.Slot != "" {
		parts = append(parts, equipmentSlotDisplayName(definition.Slot))
		attack := definition.AttackBonus
		defense := definition.DefenseBonus
		move := definition.MoveBonus
		if level > 0 {
			switch definition.Slot {
			case item.SlotWeapon:
				attack += level * 4
				defense += level
			case item.SlotArmor:
				defense += level * 3
			case item.SlotShoes:
				defense += level
				move += level / 2
			case item.SlotAccessory:
				attack += level
				defense += level * 2
			}
		}
		if attack != 0 {
			parts = append(parts, fmt.Sprintf("攻击%+d", attack))
		}
		if defense != 0 {
			parts = append(parts, fmt.Sprintf("防御%+d", defense))
		}
		if move != 0 {
			parts = append(parts, fmt.Sprintf("移动%+d", move))
		}
	}

	switch definition.ID {
	case "ration":
		parts = append(parts, "食用恢复35点饥饿度")
	case "healing_potion":
		parts = append(parts, "食用恢复25HP")
	case "herb_bundle":
		parts = append(parts, "战地治疗/随机事件药材")
	case "antidote":
		parts = append(parts, "应对中毒或瘟疫事件")
	case "revive_stone":
		parts = append(parts, "稀有复苏物资")
	case "pickaxe":
		parts = append(parts, "工具，可交易")
	case "fishing_net":
		parts = append(parts, "渔获工具，可交易")
	case "hatchet":
		parts = append(parts, "伐木工具，可交易")
	case "torch":
		parts = append(parts, "火把，可用于控火/驱兽事件")
	case "carrier_pigeon":
		parts = append(parts, "可远程传信并附带物品")
	case "rope":
		parts = append(parts, "工具物资，可交易或事件消耗")
	case "iron_ore":
		parts = append(parts, "锻造武器/建造铁匠铺或炮台")
	case "wood":
		parts = append(parts, "建造陷阱/炮台/瞭望塔/锻造弓类")
	case "stone":
		parts = append(parts, "建造铁匠铺/强化装备")
	case "leather":
		parts = append(parts, "锻造护甲/鞋履/弓类")
	case "cloth_roll":
		parts = append(parts, "锻造鞋履或轻装材料")
	case "gemstone":
		parts = append(parts, "锻造/强化饰品")
	}

	if definition.Weight > 0 {
		parts = append(parts, fmt.Sprintf("重量%d", definition.Weight))
	}
	if len(parts) == 0 {
		parts = append(parts, "可交易物资")
	}
	return strings.Join(parts, "，")
}

func formatItemRewardEffects(rewards []itemGrant) string {
	if len(rewards) == 0 {
		return "无"
	}
	parts := make([]string, 0, len(rewards))
	seen := map[string]struct{}{}
	for _, reward := range rewards {
		if reward.ItemID == "" || reward.Quantity <= 0 {
			continue
		}
		if _, ok := seen[reward.ItemID]; ok {
			continue
		}
		seen[reward.ItemID] = struct{}{}
		parts = append(parts, fmt.Sprintf("%s=%s", displayItemName(reward.ItemID), formatItemEffectByID(reward.ItemID)))
	}
	if len(parts) == 0 {
		return "无"
	}
	return strings.Join(parts, "；")
}

func formatItemRewardsWithEffects(rewards []itemGrant) string {
	brief := formatItemRewards(rewards)
	if brief == "无" {
		return brief
	}
	return fmt.Sprintf("%s（用途：%s）", brief, formatItemRewardEffects(rewards))
}

func formatItemStacksWithEffects(stacks []unit.ItemStack) string {
	if len(stacks) == 0 {
		return "无"
	}
	rewards := make([]itemGrant, 0, len(stacks))
	for _, stack := range stacks {
		if stack.ItemID == "" || stack.Quantity <= 0 {
			continue
		}
		rewards = append(rewards, itemGrant{ItemID: stack.ItemID, Quantity: stack.Quantity})
	}
	return formatItemRewardsWithEffects(rewards)
}

func gatherCandidateSummary(state State, actor unit.Record, activity ProductionActivity, structureID string) string {
	if activity == ProductionActivityHunt {
		return "打猎有随机爆率：45% 获得口粮 x1，30% 获得皮革 x1（雪原皮革率 45%），可能空手而归，并有轻微受伤风险。"
	}
	rewards, note, err := gatherRewards(state, actor, unitDecisionPayload{
		Action:      DecisionActionGather,
		Activity:    activity,
		StructureID: structureID,
	})
	if err != nil {
		switch activity {
		case ProductionActivityFarm:
			return "收割农田，补充口粮。"
		case ProductionActivityHunt:
			return "打猎有随机爆率，收益偏低且有轻微受伤风险。"
		case ProductionActivityForage:
			return "采集附近材料，稳定换取木材、药草或少量食物。"
		case ProductionActivityMine:
			return "挖矿取得铁矿和石料，不需要铁镐，适合后续建造炮台。"
		case ProductionActivityFish:
			return "钓鱼补给口粮，收益稳定但偏低。"
		default:
			return productionActivityDisplayName(activity)
		}
	}
	text := fmt.Sprintf("%s，预计获得 %s。", productionActivityDisplayName(activity), formatItemRewardsWithEffects(rewards))
	if strings.TrimSpace(note) != "" {
		text += note
	}
	return text
}

func displayNamedItem(itemID string, customName string, level int) string {
	name := strings.TrimSpace(customName)
	if name == "" {
		name = displayItemName(itemID)
	}
	if level > 0 {
		return fmt.Sprintf("%s +%d", name, level)
	}
	return name
}

func (service *Service) generateEquipmentNameBestEffort(ctx context.Context, state State, actor unit.Record, itemID string, level int) string {
	fallback := fallbackEquipmentName(actor, itemID)
	if service == nil || service.llm == nil || llmBudgetGuardrailActive(state) {
		return fallback
	}
	definition, ok := item.Lookup(itemID)
	if !ok {
		return fallback
	}
	systemPrompt := "你是《群像》的装备命名器。为刚锻造出的装备取一个短中文名，名字要像游戏内物品名，不要解释，只返回 JSON。"
	userPrompt := fmt.Sprintf(
		"锻造者: %s\n性格: %s\n生平: %s\n装备模板: %s\n装备槽位: %s\n强化等级: +%d\n要求: 名字必须结合锻造者生平、性格和装备用途，2-8 个汉字，避免通用的“神兵/传奇”。",
		actor.DisplayName(),
		summarizeActorPersonality(actor),
		strings.TrimSpace(actor.Identity.Biography),
		definition.DisplayName,
		equipmentSlotDisplayName(definition.Slot),
		level,
	)
	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskUnitDecision,
		SchemaName:     "equipment_name",
		ResponseSchema: equipmentNameSchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.65,
		MaxTokens:      80,
		Fallback: ai.RuleFallbackFunc(func(context.Context, ai.CompletionRequest, error) (json.RawMessage, error) {
			return json.Marshal(equipmentNamePayload{Name: fallback})
		}),
	})
	if err != nil {
		return fallback
	}
	var payload equipmentNamePayload
	if err := json.Unmarshal(result.Output, &payload); err != nil {
		return fallback
	}
	name := limitTextRunes(strings.TrimSpace(payload.Name), 16)
	if name == "" {
		return fallback
	}
	return name
}

func fallbackEquipmentName(actor unit.Record, itemID string) string {
	base := displayItemName(itemID)
	name := strings.TrimSpace(actor.Identity.Name)
	if name == "" {
		return "旧誓" + base
	}
	runes := []rune(name)
	if len(runes) > 2 {
		name = string(runes[:2])
	}
	return name + "的" + base
}

// terrainSupportsActivity 定义“地形 -> 可采集活动”白名单。
func terrainSupportsActivity(terrain world.TerrainID, activity ProductionActivity) bool {
	switch activity {
	case ProductionActivityFarm:
		return terrain == world.TerrainPlains || terrain == world.TerrainRiverValley
	case ProductionActivityFish:
		return terrain == world.TerrainRiver || terrain == world.TerrainRiverValley
	case ProductionActivityForage:
		return terrain == world.TerrainForest || terrain == world.TerrainMountain || terrain == world.TerrainSwamp || terrain == world.TerrainRuins
	case ProductionActivityHunt:
		return terrain == world.TerrainForest || terrain == world.TerrainGrassland || terrain == world.TerrainSnowfield
	case ProductionActivityMine:
		return terrain == world.TerrainMountain
	default:
		return false
	}
}

// terrainSupportsStructure 定义“地形 -> 可建造设施”白名单。
func terrainSupportsStructure(terrain world.TerrainID, structureType StructureType) bool {
	switch structureType {
	case StructureTypeFarmland:
		return terrain == world.TerrainPlains || terrain == world.TerrainRiverValley
	case StructureTypeForge:
		return terrain == world.TerrainCity || terrain == world.TerrainVillage || terrain == world.TerrainRuins || terrain == world.TerrainMountain
	case StructureTypeTrap:
		return terrain != world.TerrainRiver && terrain != world.TerrainDesert && terrain != world.TerrainCity
	case StructureTypeTurret:
		return terrain == world.TerrainPlains || terrain == world.TerrainMountain || terrain == world.TerrainRoad || terrain == world.TerrainRuins || terrain == world.TerrainVillage
	case StructureTypeWatchtower:
		return terrain == world.TerrainPlains || terrain == world.TerrainGrassland || terrain == world.TerrainMountain || terrain == world.TerrainRoad || terrain == world.TerrainRuins || terrain == world.TerrainRiverValley
	default:
		return false
	}
}

// structureDisplayName 返回设施类型对应的中文展示名。
func structureDisplayName(structureType StructureType) string {
	switch structureType {
	case StructureTypeFarmland:
		return "农田"
	case StructureTypeForge:
		return "铁匠铺"
	case StructureTypeTrap:
		return "陷阱"
	case StructureTypeTurret:
		return "炮台"
	case StructureTypeWatchtower:
		return "瞭望塔"
	default:
		return string(structureType)
	}
}

// structureBuildVerb 返回设施建造时更自然的中文动词。
func structureBuildVerb(structureType StructureType) string {
	switch structureType {
	case StructureTypeFarmland:
		return "开垦"
	case StructureTypeTrap:
		return "布置"
	case StructureTypeWatchtower:
		return "搭建"
	default:
		return "修建"
	}
}

// buildCandidateSummary 生成建筑候选动作摘要，文案与真实施工回合数保持一致。
func buildCandidateSummary(structureType StructureType) string {
	required := structureBuildRequired(structureType)
	switch structureType {
	case StructureTypeFarmland:
		return fmt.Sprintf("开始开垦农田。需要连续施工 %d 回合；条件：平原/河谷；材料：无；收益：完工后可周期性收粮，缓解饥饿与补给。", required)
	case StructureTypeForge:
		return fmt.Sprintf("开始修建铁匠铺。需要连续施工 %d 回合；条件：城市/村庄/废墟/山地，木材2+石料2+铁矿1；收益：驻守 ATK+4/DEF+3，并可锻造或强化装备。", required)
	case StructureTypeTrap:
		return fmt.Sprintf("开始布置陷阱。需要连续施工 %d 回合；条件：非河流/沙漠/城市，木材1；收益：敌方踩中会受伤并被迫停顿，适合封路。", required)
	case StructureTypeTurret:
		return fmt.Sprintf("开始修建炮台。需要连续施工 %d 回合；条件：平原/山地/道路/废墟/村庄，木材2+铁矿1；收益：完工后驻守者 ATK+8、攻击距离至少3格，适合守点和远程压制。", required)
	case StructureTypeWatchtower:
		return fmt.Sprintf("开始搭建瞭望塔。需要连续施工 %d 回合；条件：平原/草原/山地/道路/废墟/河谷，木材2；收益：驻守者 ATK+2、攻击距离至少2格，并改善侦察视野。", required)
	default:
		return fmt.Sprintf("开始修建%s。需要连续施工 %d 回合。", structureDisplayName(structureType), required)
	}
}

func structureRuleSummary() string {
	return strings.Join([]string{
		fmt.Sprintf("建筑 farmland 农田：效果=成熟后可 gather:farm 收口粮，平原产口粮x2、河谷产口粮x3，长期缓解饥饿；建造条件=只能在平原/河谷且地块无其他建筑；材料=无；施工=%d回合。", structureBuildRequired(StructureTypeFarmland)),
		fmt.Sprintf("建筑 forge 铁匠铺：效果=驻守ATK+4/DEF+3，并解锁 forge 锻造装备、upgrade 强化装备；建造条件=只能在城市/村庄/废墟/山地且地块无其他建筑；材料=木材x2+石料x2+铁矿x1；施工=%d回合。", structureBuildRequired(StructureTypeForge)),
		fmt.Sprintf("建筑 trap 陷阱：效果=敌方踩中会受伤并被迫停顿，适合封路和保护后排；建造条件=不能建在河流/沙漠/城市，其余无建筑地块可建；材料=木材x1；施工=%d回合。", structureBuildRequired(StructureTypeTrap)),
		fmt.Sprintf("建筑 turret 炮台：效果=驻守者ATK+8，攻击距离至少3格，适合守点和远程压制；建造条件=只能在平原/山地/道路/废墟/村庄且地块无其他建筑；材料=木材x2+铁矿x1；施工=%d回合。", structureBuildRequired(StructureTypeTurret)),
		fmt.Sprintf("建筑 watchtower 瞭望塔：效果=驻守者ATK+2，攻击距离至少2格，并改善侦察视野；建造条件=只能在平原/草原/山地/道路/废墟/河谷且地块无其他建筑；材料=木材x2；施工=%d回合。", structureBuildRequired(StructureTypeWatchtower)),
	}, "\n")
}

func terrainProductionRuleSummary() string {
	return strings.Join([]string{
		"平原 plains：可开垦农田；可建陷阱/炮台/瞭望塔；农田成熟后 gather:farm 产出口粮x2。",
		"森林 forest：可 gather:forage 产出木材x2+药草x1+口粮x1；有武器可 gather:hunt 随机产出口粮/皮革；可建陷阱。",
		"山地 mountain：可 gather:mine 产出铁矿x1+石料x1，也可 forage 产出石料/药草；可建铁匠铺/陷阱/炮台/瞭望塔。",
		"河流 river：可 gather:fish 产出口粮x1；不可建陷阱/炮台/瞭望塔/铁匠铺/农田。",
		"河谷 river_valley：可钓鱼产出口粮x2；可开垦农田，农田成熟后产出口粮x3；可建陷阱/瞭望塔。",
		"草原 grassland：有武器可打猎，随机产出口粮/皮革；可建陷阱/瞭望塔。",
		"雪原 snowfield：有武器可打猎，皮革概率更高；可建陷阱。",
		"沼泽 swamp：可 forage 产出药草x2；可建陷阱。",
		"废墟 ruins：可 forage 产出木材x1+口粮x1；可建铁匠铺/陷阱/炮台/瞭望塔。",
		"村庄 village：可建铁匠铺/陷阱/炮台；当前没有自动集市采集，交易必须通过相邻单位 trade。",
		"城市 city：可建铁匠铺；当前没有自动集市采集，交易必须通过相邻单位 trade。",
		"沙漠 desert：当前无稳定采集/建造收益，不适合补给。",
		"道路 road：适合移动；可建陷阱/炮台/瞭望塔；当前无采集产出。",
	}, "\n")
}

func unsupportedStructureRuleSummary() string {
	return "当前可建建筑只有 farmland/forge/trap/turret/watchtower；没有小屋、房子、营地、婚房或永久定居建筑。即使历史记忆或对话提到小屋/安家，也只能当情感比喻，不能继续当成可执行建设计划；应改成开垦农田、修铁匠铺、建瞭望塔、找口粮或继续交谈等真实目标。"
}

func materialSourceSummary() string {
	return strings.Join([]string{
		"木材 wood：森林 forage 稳定获得2个；废墟 forage 可获得1个。",
		"铁矿 iron_ore：山地 mine 稳定获得1个。",
		"石料 stone：山地 mine 稳定获得1个；山地 forage 也可获得1个。",
		"口粮 ration：森林/废墟 forage、河流/河谷 fish、农田 farm、打猎 hunt 都可能获得。",
		"药草 herb_bundle：森林/山地 forage 可获得1个，沼泽 forage 可获得2个。",
		"皮革 leather：森林/草原/雪原 hunt 随机获得，雪原概率更高。",
	}, "\n")
}

// productionActivityDisplayName 返回生产活动对应的中文展示名。
func productionActivityDisplayName(activity ProductionActivity) string {
	switch activity {
	case ProductionActivityFarm:
		return "收粮"
	case ProductionActivityFish:
		return "钓鱼"
	case ProductionActivityForage:
		return "采集"
	case ProductionActivityHunt:
		return "打猎"
	case ProductionActivityMine:
		return "挖矿"
	default:
		return string(activity)
	}
}

// structureReady 判断设施是否已完工并可生效。
func structureReady(structure Structure) bool {
	return structure.Completed || structure.BuildProgress >= structure.BuildRequired
}

// structureAt 查找指定坐标上的设施；不存在时返回 nil。
func structureAt(structures []Structure, coord world.Coord) *Structure {
	for index := range structures {
		if structures[index].Q == coord.Q && structures[index].R == coord.R {
			return &structures[index]
		}
	}
	return nil
}

// structureIndexByID 按结构 ID 返回切片索引，未找到返回 -1。
func structureIndexByID(structures []Structure, structureID string) int {
	for index := range structures {
		if structures[index].ID == structureID {
			return index
		}
	}
	return -1
}

// nearestThreatDistance 计算 actor 到最近敌对可战斗单位的六边形距离。
// 若周围无敌对单位，返回较大哨兵值。
func nearestThreatDistance(state State, byID map[string]*unit.Record, actor *unit.Record) int {
	target := nearestBattleReady(visibleOpposingIDs(state, byID, actor), byID, actor)
	if target == nil {
		return 999
	}
	return unit.HexDistance(
		actor.Status.PositionQ,
		actor.Status.PositionR,
		target.Status.PositionQ,
		target.Status.PositionR,
	)
}

// directivePrefersFortify 判断当前方针文本是否偏向防守/工事建设。
func directivePrefersFortify(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	return containsAny(text, "固守", "坚守", "守住", "布防", "设伏", "陷阱", "炮台", "瞭望塔", "铁匠铺")
}

// directivePrefersBuild 判断当前方针文本是否明确要求优先建造/种田。
func directivePrefersBuild(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	return containsAny(text,
		"建造",
		"造建筑",
		"修建",
		"开垦",
		"种田",
		"农田",
		"设施",
		"建筑",
		"build",
		"farmland",
	)
}

// hasUsableWeapon 判断单位武器槽是否装备了可用武器。
func hasUsableWeapon(record unit.Record) bool {
	stack, ok := record.Inventory.Equipment[string(item.SlotWeapon)]
	return ok && stack.ItemID != ""
}

// canBuildStructure 只检查“资源是否充足”，不处理地形和占位约束。
func canBuildStructure(record unit.Record, structureType StructureType) bool {
	for _, cost := range buildCosts(structureType) {
		if !hasBackpackQuantity(record, cost.ItemID, cost.Quantity) {
			return false
		}
	}
	return true
}

// buildCosts 返回指定设施的建造材料清单。
func buildCosts(structureType StructureType) []itemGrant {
	switch structureType {
	case StructureTypeForge:
		return []itemGrant{
			{ItemID: "wood", Quantity: 2},
			{ItemID: "stone", Quantity: 2},
			{ItemID: "iron_ore", Quantity: 1},
		}
	case StructureTypeTrap:
		return []itemGrant{{ItemID: "wood", Quantity: 1}}
	case StructureTypeWatchtower:
		return []itemGrant{{ItemID: "wood", Quantity: 2}}
	case StructureTypeTurret:
		return []itemGrant{
			{ItemID: "wood", Quantity: 2},
			{ItemID: "iron_ore", Quantity: 1},
		}
	default:
		return nil
	}
}

// hasBackpackQuantity 判断背包内某物品数量是否达到需求。
func hasBackpackQuantity(record unit.Record, itemID string, quantity int) bool {
	if quantity <= 0 {
		return true
	}
	total := 0
	for _, stack := range record.Inventory.Backpack {
		if stack.ItemID == itemID {
			total += stack.Quantity
		}
	}
	return total >= quantity
}

// consumeBuildCosts 从单位背包扣除建造材料，不足时返回明确错误。
func consumeBuildCosts(record *unit.Record, costs []itemGrant) error {
	for _, cost := range costs {
		if !hasBackpackQuantity(*record, cost.ItemID, cost.Quantity) {
			return fmt.Errorf("缺少 %s x%d", displayItemName(cost.ItemID), cost.Quantity)
		}
	}
	for _, cost := range costs {
		if err := unit.ConsumeBackpackItem(record, cost.ItemID, cost.Quantity); err != nil {
			return err
		}
	}
	return nil
}

// summarizeStructureAt 生成指定格子设施状态摘要（无/施工中/已完成/成熟回合）。
func summarizeStructureAt(structures []Structure, coord world.Coord) string {
	structure := structureAt(structures, coord)
	if structure == nil {
		return "无"
	}

	if !structureReady(*structure) {
		return fmt.Sprintf("%s(施工中 %d/%d)", structureDisplayName(structure.Type), structure.BuildProgress, structure.BuildRequired)
	}
	if structure.Type == StructureTypeFarmland && structure.HarvestReadyTurn > 0 {
		return fmt.Sprintf("%s(下次成熟回合 T%d)", structureDisplayName(structure.Type), structure.HarvestReadyTurn)
	}
	return structureDisplayName(structure.Type)
}

// currentTileOpportunitySummary 汇总单位当前地块可执行的生产或建造机会，用于提示词与 UI 展示。
func currentTileOpportunitySummary(state State, byID map[string]*unit.Record, actor unit.Record) string {
	candidates := buildEconomyCandidates(state, byID, &actor)
	if len(candidates) == 0 {
		return "无"
	}

	lines := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Action != DecisionActionBuild && candidate.Action != DecisionActionGather {
			continue
		}
		lines = append(lines, candidate.Summary)
	}
	if len(lines) == 0 {
		return "无"
	}
	return strings.Join(lines, "；")
}

// moveCandidateSummary 为移动候选生成简洁移动说明；具体地块机会统一放在提示词的“可到达地块与可执行事项”。
func moveCandidateSummary(state State, coord world.Coord) string {
	terrain := terrainAt(state.Map, coord)
	return fmt.Sprintf("移动到 %d,%d（%s）。", coord.Q, coord.R, terrainDisplayName(terrain))
}

// tileOpportunitySummary 汇总指定地块可执行的采集和建造事项，供 LLM 选择移动目标时参考。
func tileOpportunitySummary(state State, actor *unit.Record, coord world.Coord) string {
	terrain := terrainAt(state.Map, coord)
	parts := []string{fmt.Sprintf("地形=%s", terrainDisplayName(terrain))}
	if structure := structureAt(state.Structures, coord); structure != nil {
		parts = append(parts, fmt.Sprintf("设施=%s", summarizeStructureAt(state.Structures, coord)))
	}
	if drops := groundLootAtCoord(state, coord.Q, coord.R); len(drops) > 0 {
		lootParts := make([]string, 0, len(drops))
		for _, drop := range drops {
			lootParts = append(lootParts, formatItemStacksWithEffects(drop.Items))
		}
		parts = append(parts, "地面物品="+strings.Join(lootParts, "/"))
	}
	activities := tileSupportedActivities(terrain, actor)
	if len(activities) > 0 {
		parts = append(parts, "可采集/生产="+strings.Join(activities, "/"))
	}
	builds := tileSupportedBuilds(state, actor, coord, terrain)
	if len(builds) > 0 {
		parts = append(parts, "可建造="+strings.Join(builds, "/"))
	}
	if len(parts) == 1 {
		parts = append(parts, "可执行事项=无")
	}
	return strings.Join(parts, "；")
}

func tileSupportedActivities(terrain world.TerrainID, actor *unit.Record) []string {
	activities := make([]string, 0, 4)
	if terrainSupportsActivity(terrain, ProductionActivityHunt) && actor != nil && hasUsableWeapon(*actor) {
		activities = append(activities, productionActivityDisplayName(ProductionActivityHunt))
	}
	if terrainSupportsActivity(terrain, ProductionActivityMine) {
		activities = append(activities, productionActivityDisplayName(ProductionActivityMine))
	}
	if terrainSupportsActivity(terrain, ProductionActivityForage) {
		activities = append(activities, productionActivityDisplayName(ProductionActivityForage))
	}
	if terrainSupportsActivity(terrain, ProductionActivityFish) {
		activities = append(activities, productionActivityDisplayName(ProductionActivityFish))
	}
	return activities
}

func tileSupportedBuilds(state State, actor *unit.Record, coord world.Coord, terrain world.TerrainID) []string {
	if actor == nil || structureAt(state.Structures, coord) != nil {
		return nil
	}
	structures := []StructureType{
		StructureTypeFarmland,
		StructureTypeForge,
		StructureTypeTrap,
		StructureTypeTurret,
		StructureTypeWatchtower,
	}
	builds := make([]string, 0, len(structures))
	for _, structureType := range structures {
		if !terrainSupportsStructure(terrain, structureType) {
			continue
		}
		if !canBuildStructure(*actor, structureType) {
			continue
		}
		builds = append(builds, structureDisplayName(structureType))
	}
	return builds
}

// validateProductionDecision 校验生产类决策是否合法。
// 覆盖采集活动合法性、设施建造前置条件、续建位置与资源约束。
func validateProductionDecision(state State, actor *unit.Record, decision unitDecisionPayload) error {
	if actor == nil {
		return fmt.Errorf("actor is required")
	}

	coord := world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}
	terrain := terrainAt(state.Map, coord)

	switch decision.Action {
	case DecisionActionGather:
		switch decision.Activity {
		case ProductionActivityFarm:
			index := structureIndexByID(state.Structures, decision.StructureID)
			if index < 0 {
				return fmt.Errorf("farm gather requires farmland structure")
			}
			structure := state.Structures[index]
			if structure.FactionID != actor.FactionID || structure.Type != StructureTypeFarmland || !structureReady(structure) {
				return fmt.Errorf("structure %s is not a ready friendly farmland", decision.StructureID)
			}
			if state.TurnState.Turn < structure.HarvestReadyTurn {
				return fmt.Errorf("farmland is not ready for harvest yet")
			}
			return nil
		case ProductionActivityFish, ProductionActivityForage, ProductionActivityHunt, ProductionActivityMine:
			if !terrainSupportsActivity(terrain, decision.Activity) {
				return fmt.Errorf("%s cannot be executed on %s", decision.Activity, terrain)
			}
			if decision.Activity == ProductionActivityHunt && !hasUsableWeapon(*actor) {
				return fmt.Errorf("hunt requires a weapon")
			}
			return nil
		default:
			return fmt.Errorf("unsupported activity %q", decision.Activity)
		}
	case DecisionActionBuild:
		if decision.StructureType == "" {
			return fmt.Errorf("build decision requires structure_type")
		}
		if decision.StructureID != "" {
			index := structureIndexByID(state.Structures, decision.StructureID)
			if index < 0 {
				return fmt.Errorf("unknown structure %s", decision.StructureID)
			}
			structure := state.Structures[index]
			if structure.FactionID != actor.FactionID || structure.Type != decision.StructureType || structureReady(structure) {
				return fmt.Errorf("structure %s is not a pending friendly %s", decision.StructureID, decision.StructureType)
			}
			if structure.Q != actor.Status.PositionQ || structure.R != actor.Status.PositionR {
				return fmt.Errorf("must stand on the unfinished structure to continue building")
			}
			return nil
		}
		if !terrainSupportsStructure(terrain, decision.StructureType) {
			return fmt.Errorf("%s cannot be built on %s", decision.StructureType, terrain)
		}
		if current := structureAt(state.Structures, coord); current != nil {
			return fmt.Errorf("tile already contains %s", current.Type)
		}
		for _, cost := range buildCosts(decision.StructureType) {
			if !hasBackpackQuantity(*actor, cost.ItemID, cost.Quantity) {
				return fmt.Errorf("missing %s x%d", cost.ItemID, cost.Quantity)
			}
		}
		return nil
	case DecisionActionForge:
		if decision.ItemID == "" {
			return fmt.Errorf("forge decision requires item_id")
		}
		if !actorAtReadyFriendlyForge(state, actor) {
			return fmt.Errorf("forge requires standing on a ready friendly forge")
		}
		definition, ok := item.Lookup(decision.ItemID)
		if !ok || definition.Slot == "" {
			return fmt.Errorf("item %s is not forgeable equipment", decision.ItemID)
		}
		if !canForgeEquipment(*actor, decision.ItemID) {
			return fmt.Errorf("missing materials for forging %s", decision.ItemID)
		}
		return nil
	case DecisionActionUpgrade:
		if decision.ItemID == "" {
			return fmt.Errorf("upgrade decision requires item_id")
		}
		if !actorAtReadyFriendlyForge(state, actor) {
			return fmt.Errorf("upgrade requires standing on a ready friendly forge")
		}
		stack, ok := findOwnedEquipment(*actor, decision.ItemID)
		if !ok {
			return fmt.Errorf("actor does not own equipment %s", decision.ItemID)
		}
		if !hasBackpackCosts(*actor, upgradeCosts(stack)) {
			return fmt.Errorf("missing materials for upgrading %s", decision.ItemID)
		}
		return nil
	case DecisionActionEquip:
		if decision.ItemID == "" {
			return fmt.Errorf("equip decision requires item_id")
		}
		if !hasBackpackEquipment(*actor, decision.ItemID) {
			return fmt.Errorf("equipment %s not found in backpack", decision.ItemID)
		}
		return nil
	default:
		return nil
	}
}

// executeGather 执行一次采集行为并结算收益、副作用与日志。
// 包括农田成熟回合推进、背包发放、行动饥饿消耗和打猎风险。
func (service *Service) executeGather(
	ctx context.Context,
	state *State,
	actor *unit.Record,
	decision unitDecisionPayload,
) error {
	// 采集日志优先使用 AI 原文，系统模板仅作为兜底。
	aiText := decisionLogText(decision)
	rewards, note, err := gatherRewards(*state, *actor, decision)
	if err != nil {
		return err
	}

	if decision.Activity == ProductionActivityFarm {
		index := structureIndexByID(state.Structures, decision.StructureID)
		if index < 0 {
			return fmt.Errorf("farmland %s not found", decision.StructureID)
		}
		cycle := farmlandHarvestCycle(terrainAt(state.Map, world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}))
		state.Structures[index].HarvestReadyTurn = state.TurnState.Turn + cycle
		state.Structures[index].UpdatedAt = time.Now().UTC()
	}

	discarded := grantItems(actor, rewards)
	if err := service.units.Save(ctx, *actor); err != nil {
		return err
	}
	if err := service.applyActionHungerCost(ctx, state, actor, productionActivityDisplayName(decision.Activity)); err != nil {
		return err
	}
	if err := service.applyGatherRisk(ctx, state, actor, decision); err != nil {
		return err
	}

	message := fmt.Sprintf("我自主%s，获得 %s。%s", productionActivityDisplayName(decision.Activity), formatItemRewardsWithEffects(rewards), note)
	if len(discarded) > 0 {
		message = fmt.Sprintf("%s 背包已满，额外掉落了 %s。", message, formatItemRewardsWithEffects(discarded))
	}
	if strings.TrimSpace(aiText) != "" {
		message = fmt.Sprintf("%s；%s", strings.TrimSpace(aiText), message)
	}
	appendLog(
		state,
		"gather",
		strings.TrimSpace(message),
		actor.ID,
		"",
	)
	return nil
}

// executeBuild 同时支持“新建”与“续建”，并统一记录 AI 文本日志。
func (service *Service) executeBuild(
	ctx context.Context,
	state *State,
	actor *unit.Record,
	decision unitDecisionPayload,
) error {
	aiText := decisionLogText(decision)
	now := time.Now().UTC()
	coord := world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}
	terrain := terrainAt(state.Map, coord)

	if decision.StructureID != "" {
		index := structureIndexByID(state.Structures, decision.StructureID)
		if index < 0 {
			return fmt.Errorf("structure %s not found", decision.StructureID)
		}
		state.Structures[index].BuildProgress++
		state.Structures[index].BuilderUnitID = actor.ID
		state.Structures[index].UpdatedAt = now
		if state.Structures[index].BuildProgress >= state.Structures[index].BuildRequired {
			completeStructure(&state.Structures[index], state.TurnState.Turn, terrain)
			appendLog(
				state,
				"build",
				strings.TrimSpace(firstNonEmptyText(
					aiText,
					buildCompletionLogText(state.Structures[index]),
				)),
				actor.ID,
				"",
			)
		} else {
			appendLog(
				state,
				"build",
				strings.TrimSpace(firstNonEmptyText(
					aiText,
					buildProgressLogText(state.Structures[index], true),
				)),
				actor.ID,
				"",
			)
		}
		if err := service.applyActionHungerCost(ctx, state, actor, "建造"); err != nil {
			return err
		}
		return nil
	}

	if err := consumeBuildCosts(actor, buildCosts(decision.StructureType)); err != nil {
		return err
	}
	if err := service.units.Save(ctx, *actor); err != nil {
		return err
	}

	structure := Structure{
		ID:            uuid.NewString(),
		Type:          decision.StructureType,
		FactionID:     actor.FactionID,
		BuilderUnitID: actor.ID,
		Q:             coord.Q,
		R:             coord.R,
		BuildProgress: 1,
		BuildRequired: structureBuildRequired(decision.StructureType),
		StartedTurn:   state.TurnState.Turn,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if structure.BuildProgress >= structure.BuildRequired {
		completeStructure(&structure, state.TurnState.Turn, terrain)
	}

	state.Structures = append(state.Structures, structure)
	if err := service.applyActionHungerCost(ctx, state, actor, "建造"); err != nil {
		return err
	}

	buildText := buildProgressLogText(structure, false)
	if structure.Completed {
		buildText = buildCompletionLogText(structure)
	}
	appendLog(
		state,
		"build",
		strings.TrimSpace(firstNonEmptyText(
			aiText,
			buildText,
		)),
		actor.ID,
		"",
	)
	return nil
}

// executeForge 在已完工铁匠铺消耗材料锻造装备，装备名优先由 LLM 生成。
func (service *Service) executeForge(
	ctx context.Context,
	state *State,
	actor *unit.Record,
	decision unitDecisionPayload,
) error {
	if err := consumeBuildCosts(actor, forgeCosts(decision.ItemID)); err != nil {
		return err
	}
	name := strings.TrimSpace(decision.ItemName)
	if name == "" {
		name = service.generateEquipmentNameBestEffort(ctx, *state, *actor, decision.ItemID, 0)
	}
	if err := unit.AddNamedItem(actor, decision.ItemID, 1, name); err != nil {
		return err
	}
	if err := service.units.Save(ctx, *actor); err != nil {
		return err
	}
	if err := service.applyActionHungerCost(ctx, state, actor, "锻造"); err != nil {
		return err
	}
	appendLog(
		state,
		"forge",
		strings.TrimSpace(firstNonEmptyText(
			decisionLogText(decision),
			fmt.Sprintf("我在铁匠铺锻造出%s，并命名为%s。", displayItemName(decision.ItemID), displayNamedItem(decision.ItemID, name, 0)),
		)),
		actor.ID,
		"",
	)
	return nil
}

// executeUpgrade 在铁匠铺消耗材料强化装备。
func (service *Service) executeUpgrade(
	ctx context.Context,
	state *State,
	actor *unit.Record,
	decision unitDecisionPayload,
) error {
	stack, ok := findOwnedEquipment(*actor, decision.ItemID)
	if !ok {
		return fmt.Errorf("equipment %s not found", decision.ItemID)
	}
	if err := consumeBuildCosts(actor, upgradeCosts(stack)); err != nil {
		return err
	}
	upgraded, err := unit.UpgradeItem(actor, decision.ItemID)
	if err != nil {
		return err
	}
	if err := service.units.Save(ctx, *actor); err != nil {
		return err
	}
	if err := service.applyActionHungerCost(ctx, state, actor, "强化装备"); err != nil {
		return err
	}
	appendLog(
		state,
		"upgrade",
		strings.TrimSpace(firstNonEmptyText(
			decisionLogText(decision),
			fmt.Sprintf("我把%s强化到 +%d。", displayStackName(upgraded), upgraded.Level),
		)),
		actor.ID,
		"",
	)
	return nil
}

// executeEquip 将背包装备穿戴到装备栏。
func (service *Service) executeEquip(
	ctx context.Context,
	state *State,
	actor *unit.Record,
	decision unitDecisionPayload,
) error {
	if err := unit.EquipBackpackItem(actor, decision.ItemID); err != nil {
		return err
	}
	if err := service.units.Save(ctx, *actor); err != nil {
		return err
	}
	appendLog(
		state,
		"equip",
		strings.TrimSpace(firstNonEmptyText(
			decisionLogText(decision),
			fmt.Sprintf("我装备了%s。", displayItemName(decision.ItemID)),
		)),
		actor.ID,
		"",
	)
	return nil
}

// structureBuildRequired 返回设施所需施工回合数。
func structureBuildRequired(structureType StructureType) int {
	switch structureType {
	case StructureTypeTurret, StructureTypeForge:
		return 4
	case StructureTypeFarmland, StructureTypeTrap, StructureTypeWatchtower:
		return 3
	default:
		return 2
	}
}

// completeStructure 标记建筑完工，并补齐完工后的额外状态。
func completeStructure(structure *Structure, currentTurn int, terrain world.TerrainID) {
	if structure == nil {
		return
	}
	structure.Completed = true
	structure.CompletedTurn = currentTurn
	switch structure.Type {
	case StructureTypeFarmland:
		structure.HarvestReadyTurn = currentTurn + farmlandHarvestCycle(terrain)
	case StructureTypeTrap:
		if structure.Charges <= 0 {
			structure.Charges = 1
		}
	}
}

// buildProgressLogText 生成施工中日志兜底文案。
func buildProgressLogText(structure Structure, continued bool) string {
	prefix := "我开始"
	if continued {
		prefix = "我继续"
	}
	return fmt.Sprintf(
		"%s%s%s，当前进度 %d/%d。",
		prefix,
		structureBuildVerb(structure.Type),
		structureDisplayName(structure.Type),
		structure.BuildProgress,
		structure.BuildRequired,
	)
}

// buildCompletionLogText 生成建筑完工日志兜底文案。
func buildCompletionLogText(structure Structure) string {
	switch structure.Type {
	case StructureTypeFarmland:
		return fmt.Sprintf("我开垦完农田，预计在 T%d 成熟。", structure.HarvestReadyTurn)
	case StructureTypeForge:
		return "我完成了铁匠铺，驻守时攻防都会提升。"
	case StructureTypeTrap:
		return "我布置好了陷阱，敌人踩上来会受伤并被迫停顿。"
	case StructureTypeTurret:
		return "我完成了炮台，后续占位攻击将获得更远射程和固定火力。"
	case StructureTypeWatchtower:
		return "我完成了瞭望塔，驻守后视野和射程都会更好。"
	default:
		return fmt.Sprintf("我完成了%s。", structureDisplayName(structure.Type))
	}
}

// farmlandHarvestCycle 返回农田在不同地形上的成熟周期。
func farmlandHarvestCycle(terrain world.TerrainID) int {
	if terrain == world.TerrainRiverValley {
		return 2
	}
	return 3
}

// gatherRewards 按活动类型与地形计算采集奖励清单与说明文本。
func gatherRewards(state State, actor unit.Record, decision unitDecisionPayload) ([]itemGrant, string, error) {
	terrain := terrainAt(state.Map, world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR})

	switch decision.Activity {
	case ProductionActivityFarm:
		quantity := 2
		if terrain == world.TerrainRiverValley {
			quantity = 3
		}
		return []itemGrant{{ItemID: "ration", Quantity: quantity}}, "农田进入下一轮生长周期。", nil
	case ProductionActivityFish:
		quantity := 1
		if terrain == world.TerrainRiverValley {
			quantity = 2
		}
		return []itemGrant{{ItemID: "ration", Quantity: quantity}}, "水边补给稳定但产量不高。", nil
	case ProductionActivityForage:
		switch terrain {
		case world.TerrainForest:
			return []itemGrant{{ItemID: "wood", Quantity: 2}, {ItemID: "herb_bundle", Quantity: 1}, {ItemID: "ration", Quantity: 1}}, "低风险搜罗了可用物资。", nil
		case world.TerrainMountain:
			return []itemGrant{{ItemID: "stone", Quantity: 1}, {ItemID: "herb_bundle", Quantity: 1}}, "山地采集以石料和药草为主。", nil
		case world.TerrainSwamp:
			return []itemGrant{{ItemID: "herb_bundle", Quantity: 2}}, "湿地更容易采到药草。", nil
		case world.TerrainRuins:
			return []itemGrant{{ItemID: "wood", Quantity: 1}, {ItemID: "ration", Quantity: 1}}, "废墟里翻出了还能用的材料。", nil
		default:
			return nil, "", fmt.Errorf("forage is not available on %s", terrain)
		}
	case ProductionActivityHunt:
		rewards := huntRewardsForRolls(
			terrain,
			productionRoll(state, actor, "hunt-ration"),
			productionRoll(state, actor, "hunt-leather"),
		)
		return rewards, "猎获改为随机爆率，收益更低，可能空手而归。", nil
	case ProductionActivityMine:
		return []itemGrant{{ItemID: "iron_ore", Quantity: 1}, {ItemID: "stone", Quantity: 1}}, "矿料可继续拿去交易或修炮台；挖矿不需要铁镐。", nil
	default:
		return nil, "", fmt.Errorf("unsupported activity %q", decision.Activity)
	}
}

func huntRewardsForRolls(terrain world.TerrainID, rationRoll float64, leatherRoll float64) []itemGrant {
	rewards := make([]itemGrant, 0, 2)
	if rationRoll < 0.45 {
		rewards = append(rewards, itemGrant{ItemID: "ration", Quantity: 1})
	}
	leatherChance := 0.30
	if terrain == world.TerrainSnowfield {
		leatherChance = 0.45
	}
	if leatherRoll < leatherChance {
		rewards = append(rewards, itemGrant{ItemID: "leather", Quantity: 1})
	}
	return rewards
}

// grantItems 尝试把奖励写入背包，并返回因容量限制未能加入的奖励。
func grantItems(record *unit.Record, rewards []itemGrant) []itemGrant {
	discarded := make([]itemGrant, 0, len(rewards))
	for _, reward := range rewards {
		if reward.Quantity <= 0 {
			continue
		}
		if err := unit.AddBackpackItem(record, reward.ItemID, reward.Quantity); err != nil {
			discarded = append(discarded, reward)
		}
	}
	return discarded
}

// formatItemRewards 将奖励列表格式化为用于日志展示的短文本。
func formatItemRewards(rewards []itemGrant) string {
	if len(rewards) == 0 {
		return "无"
	}
	parts := make([]string, 0, len(rewards))
	for _, reward := range rewards {
		parts = append(parts, fmt.Sprintf("%s x%d", displayItemName(reward.ItemID), reward.Quantity))
	}
	return strings.Join(parts, "、")
}

// applyGatherRisk 结算采集行为的风险事件（当前仅打猎）。
// 风险使用稳定随机，命中后造成伤害并处理致死分支。
func (service *Service) applyGatherRisk(
	ctx context.Context,
	state *State,
	actor *unit.Record,
	decision unitDecisionPayload,
) error {
	// 当前仅打猎活动有受伤风险；概率由稳定哈希投掷，保证同局可复现。
	if decision.Activity != ProductionActivityHunt {
		return nil
	}
	if productionRoll(*state, *actor, "hunt-risk") >= 0.18 {
		return nil
	}
	if err := service.applyStatusMutation(
		ctx,
		state,
		actor,
		status.FieldHP,
		-4,
		events.ReasonCombatHit,
		"我打猎时被野兽擦伤。",
	); err != nil {
		return err
	}
	appendLog(state, "gather_risk", "我打猎时被野兽擦伤，失去 4 HP。", actor.ID, "")
	if actor.Status.HP == 0 && actor.Status.LifeState == unit.LifeStateActive {
		if err := unit.ApplyFatalDamage(actor); err != nil {
			return err
		}
		if err := service.units.Save(ctx, *actor); err != nil {
			return err
		}
	}
	return nil
}

// productionRoll 生成生产系统使用的可复现随机值。
func productionRoll(state State, actor unit.Record, salt string) float64 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(state.ID))
	_, _ = hasher.Write([]byte(actor.ID))
	_, _ = hasher.Write([]byte(salt))
	_, _ = hasher.Write([]byte(fmt.Sprintf("%d", state.TurnState.Turn)))
	return float64(hasher.Sum32()%10000) / 10000
}

// trapAt 查找坐标上的敌方可触发陷阱，返回索引和结构体指针。
func trapAt(structures []Structure, factionID string, coord world.Coord) (int, *Structure) {
	for index := range structures {
		structure := &structures[index]
		if structure.Type != StructureTypeTrap || !structureReady(*structure) || structure.Charges <= 0 {
			continue
		}
		if structure.FactionID == factionID {
			continue
		}
		if structure.Q == coord.Q && structure.R == coord.R {
			return index, structure
		}
	}
	return -1, nil
}

// triggerTrapAt 处理单位踩到敌方陷阱的完整结算。
// 包括扣血、日志、移除陷阱及可能的死亡处理。
func (service *Service) triggerTrapAt(
	ctx context.Context,
	state *State,
	actor *unit.Record,
) (bool, error) {
	coord := world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}
	index, trap := trapAt(state.Structures, actor.FactionID, coord)
	if trap == nil {
		return false, nil
	}

	if err := service.applyStatusMutation(
		ctx,
		state,
		actor,
		status.FieldHP,
		-14,
		events.ReasonCombatHit,
		"我误触了敌方陷阱。",
	); err != nil {
		return false, err
	}

	appendLog(
		state,
		"trap",
		"我踩中了敌方陷阱，失去 14 HP。",
		actor.ID,
		"",
	)

	state.Structures = append(state.Structures[:index], state.Structures[index+1:]...)

	if actor.Status.HP == 0 && actor.Status.LifeState == unit.LifeStateActive {
		if err := unit.ApplyFatalDamage(actor); err != nil {
			return true, err
		}
		if err := service.units.Save(ctx, *actor); err != nil {
			return true, err
		}
		appendLog(state, "trap", "我因陷阱伤势过重而倒下。", actor.ID, "")
	}

	return true, nil
}
