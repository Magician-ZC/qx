package session

// 文件说明：地块事件系统——玩家直驱的地块动作结算入口（ExecuteTileAction）。
// 决策层=玩家点击即玩家意志（零 LLM）；结算层全走既有 executeGather/executeBuild/executeForge/
// executeUpgrade/executeDemolish/applyAttackToStructure（饥饿经 Mutator、打猎风险、背包发放、appendLog 全在里面），
// 受保护字段恒经 status.Mutator。校验链：unit 归属玩家方 → 执行互斥 → 站在目标格 → affordance 同口径复检（防直接 POST 绕过）。
// 落痕：appendLog + events.EmitProcessEvent(ReasonPlayerTileAction) + WS fate_life_beat 推送（best-effort 不阻断）。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// 玩家地块动作的哨兵错误（router 据此映射 HTTP 状态：互斥→409、不在场→400、找不到→404）。
var (
	// ErrTileExecutionBusy 执行阶段互斥——与 player_actions.go 的 ErrExecutionBusy 同一实例
	// （评审 minor#2 合一：两个文案相同的哨兵让 router 的 errors.Is 必须双判，统一后保留别名兼容既有调用方）。
	ErrTileExecutionBusy = ErrExecutionBusy
	// ErrTileNotThere 单位未站在目标格。
	ErrTileNotThere = errors.New("她还没走到那里")
	// ErrTileUnitNotFound 单位不属于本会话。
	ErrTileUnitNotFound = errors.New("这不是本局的人")
	// ErrTileUnitNotYours 单位不属玩家方（不能差遣 NPC/敌方）。
	ErrTileUnitNotYours = errors.New("她不归你差遣")
)

// TileActionRequest 是玩家直驱地块动作的请求体。
// ItemID 可选：forge/upgrade 指定目标装备，留空则取目录默认（首个可锻/可强化件）。
type TileActionRequest struct {
	Action        string `json:"action"` // gather/harvest/build/forge/upgrade/demolish
	Q             int    `json:"q"`
	R             int    `json:"r"`
	Activity      string `json:"activity,omitempty"`       // gather 专用：fish/forage/hunt/mine
	StructureType string `json:"structure_type,omitempty"` // build 专用
	ItemID        string `json:"item_id,omitempty"`        // forge/upgrade 可选
}

// TileActionEffect 是一条结算后的增减明细（物品或状态）。
type TileActionEffect struct {
	Kind    string `json:"kind"` // item / status
	ItemID  string `json:"item_id,omitempty"`
	LabelZH string `json:"label_zh"`
	Delta   int    `json:"delta"`
}

// TileActionResult 是玩家地块动作的结算结果。
type TileActionResult struct {
	OK        bool               `json:"ok"`
	Action    string             `json:"action"`
	SummaryZH string             `json:"summary_zh"`
	Effects   []TileActionEffect `json:"effects,omitempty"`
}

// ExecuteTileAction 结算一次玩家直驱的地块动作（零 LLM，结算全复用既有 execute* 路径）。
func (service *Service) ExecuteTileAction(ctx context.Context, sessionID string, unitID string, req TileActionRequest) (TileActionResult, error) {
	if service == nil || service.units == nil || service.sessions == nil {
		return TileActionResult{}, fmt.Errorf("tile action: service unavailable")
	}
	// 统一前置（评审 C1 后收口）：直驱锁（防玩家请求并发双写）+ 执行互斥 + 终局门 + 归属本局玩家方。
	state, units, actor, release, err := service.guardPlayerAction(ctx, sessionID, unitID)
	if err != nil {
		return TileActionResult{}, err
	}
	defer release()
	if actor.Status.LifeState != unit.LifeStateActive {
		return TileActionResult{}, fmt.Errorf("她已无法行动")
	}
	// talk/trade/poi_encounter 只进目录、不在本入口结算（先于站位校验拒绝：talk/trade 本就允许相邻）。
	switch req.Action {
	case "poi_encounter", "talk", "trade":
		return TileActionResult{}, fmt.Errorf("这件事走专门的路子（探索/交谈/交易入口）")
	}
	if actor.Status.PositionQ != req.Q || actor.Status.PositionR != req.R {
		return TileActionResult{}, ErrTileNotThere
	}

	// affordance 同口径复检（防直接 POST 绕过目录白名单）。
	affordances := computeTileAffordances(&state, units, actor, req.Q, req.R)
	entry, found := matchTileAffordance(affordances, req)
	if !found {
		return TileActionResult{}, fmt.Errorf("这一格做不了这件事")
	}
	if !entry.Available {
		reason := strings.TrimSpace(entry.ReasonZH)
		if reason == "" {
			reason = "现在做不了这件事"
		}
		return TileActionResult{}, errors.New(reason)
	}

	coord := world.Coord{Q: req.Q, R: req.R}
	logStart := len(state.Logs)
	baseline := captureTileEffectBaseline(*actor)
	hostileDemolish := false

	switch req.Action {
	case "gather":
		decision := unitDecisionPayload{Action: DecisionActionGather, Activity: ProductionActivity(strings.TrimSpace(req.Activity))}
		if err := validateProductionDecision(state, actor, decision); err != nil {
			return TileActionResult{}, fmt.Errorf("此地做不得这事：%w", err)
		}
		if err := service.executeGather(ctx, &state, actor, decision); err != nil {
			return TileActionResult{}, fmt.Errorf("tile action (gather): %w", err)
		}
	case "harvest":
		structure := structureAt(state.Structures, coord)
		if structure == nil {
			return TileActionResult{}, fmt.Errorf("这里没有农田")
		}
		decision := unitDecisionPayload{Action: DecisionActionGather, Activity: ProductionActivityFarm, StructureID: structure.ID}
		if err := validateProductionDecision(state, actor, decision); err != nil {
			return TileActionResult{}, fmt.Errorf("此地做不得这事：%w", err)
		}
		if err := service.executeGather(ctx, &state, actor, decision); err != nil {
			return TileActionResult{}, fmt.Errorf("tile action (harvest): %w", err)
		}
	case "build":
		decision := unitDecisionPayload{Action: DecisionActionBuild, StructureType: StructureType(strings.TrimSpace(req.StructureType))}
		// 本格已有己方未完工设施 → 续建（与 buildEconomyCandidates 的续建候选同口径）。
		if current := structureAt(state.Structures, coord); current != nil &&
			current.FactionID == actor.FactionID && !structureReady(*current) {
			decision.StructureID = current.ID
			decision.StructureType = current.Type
		}
		if err := validateProductionDecision(state, actor, decision); err != nil {
			return TileActionResult{}, fmt.Errorf("此地做不得这事：%w", err)
		}
		if err := service.executeBuild(ctx, &state, actor, decision); err != nil {
			return TileActionResult{}, fmt.Errorf("tile action (build): %w", err)
		}
	case "forge":
		itemID := strings.TrimSpace(req.ItemID)
		if itemID == "" {
			if forgeable := forgeableEquipmentIDs(*actor); len(forgeable) > 0 {
				itemID = forgeable[0]
			}
		}
		decision := unitDecisionPayload{Action: DecisionActionForge, ItemID: itemID}
		if err := validateProductionDecision(state, actor, decision); err != nil {
			return TileActionResult{}, fmt.Errorf("打不了这件铁：%w", err)
		}
		if err := service.executeForge(ctx, &state, actor, decision); err != nil {
			return TileActionResult{}, fmt.Errorf("tile action (forge): %w", err)
		}
	case "upgrade":
		itemID := strings.TrimSpace(req.ItemID)
		if itemID == "" {
			if stack, ok := defaultUpgradeTarget(*actor); ok {
				itemID = stack.ItemID
			}
		}
		decision := unitDecisionPayload{Action: DecisionActionUpgrade, ItemID: itemID}
		if err := validateProductionDecision(state, actor, decision); err != nil {
			return TileActionResult{}, fmt.Errorf("强化不了这件装备：%w", err)
		}
		if err := service.executeUpgrade(ctx, &state, actor, decision); err != nil {
			return TileActionResult{}, fmt.Errorf("tile action (upgrade): %w", err)
		}
	case "demolish":
		structure := structureAt(state.Structures, coord)
		if structure == nil {
			return TileActionResult{}, fmt.Errorf("脚下没有设施")
		}
		if structure.FactionID == actor.FactionID {
			decision := unitDecisionPayload{Action: DecisionActionDemolish, StructureID: structure.ID}
			if err := service.executeDemolish(ctx, &state, actor, decision); err != nil {
				return TileActionResult{}, fmt.Errorf("tile action (demolish): %w", err)
			}
		} else {
			// 敌对设施经战斗摧毁（structure_actions.go 口径：确定性掷骰，可能落空）。affordance 已校验战争状态。
			hostileDemolish = true
			index := structureIndexByID(state.Structures, structure.ID)
			if err := service.applyAttackToStructure(ctx, &state, actor, index, normalAttackStyle, ""); err != nil {
				return TileActionResult{}, fmt.Errorf("tile action (demolish hostile): %w", err)
			}
		}
	default:
		return TileActionResult{}, fmt.Errorf("不认识的动作 %q", req.Action)
	}

	// 结算摘要：优先取本次结算新追加的叙事日志（execute* 内部已落），兜底用通用文案。
	summary := firstNewLogMessage(state.Logs, logStart, tileActionLogKinds(req.Action))
	if summary == "" {
		summary = fmt.Sprintf("%s照你的意思，在这片土地上动了手。", actor.DisplayName())
	}
	effects := diffTileEffects(baseline, *actor)

	// 玩家直驱落痕：玩家视角日志 + 流程事件 + WS 推送（流程事件与推送 best-effort，不阻断结算）。
	appendLog(&state, "player_tile_action",
		fmt.Sprintf("依你的指点，%s在 (%d,%d) %s。", actor.DisplayName(), req.Q, req.R, entry.LabelZH),
		actor.ID, "")
	service.emitTileActionTraceBestEffort(ctx, &state, actor, req, entry, summary, hostileDemolish)
	service.pushRealtime(state.ID, "fate_life_beat", map[string]any{
		"unit_id":   actor.ID,
		"narrative": summary,
		"turn":      state.TurnState.Turn,
	})

	// 合并保存（评审 minor#4 统一口径）：日志/ConsumedPOIs 随会话整块持久化，不覆盖并发写入的外部事件。
	if err := service.saveSessionMergingExternalEvents(ctx, &state); err != nil {
		return TileActionResult{}, fmt.Errorf("tile action (save): %w", err)
	}
	return TileActionResult{
		OK:        true,
		Action:    req.Action,
		SummaryZH: summary,
		Effects:   effects,
	}, nil
}

// matchTileAffordance 在动作目录里找与请求匹配的条目（gather 还须 activity 一致、build 须 structure_type 一致）。
func matchTileAffordance(affordances TileAffordances, req TileActionRequest) (TileAction, bool) {
	for _, action := range affordances.Actions {
		if action.Action != req.Action {
			continue
		}
		if req.Action == "gather" && action.Activity != strings.TrimSpace(req.Activity) {
			continue
		}
		if req.Action == "build" && action.StructureType != strings.TrimSpace(req.StructureType) {
			continue
		}
		return action, true
	}
	return TileAction{}, false
}

// tileActionLogKinds 返回各动作结算时 execute* 会落的叙事日志 kind（按优先序）。
func tileActionLogKinds(action string) []string {
	switch action {
	case "gather", "harvest":
		return []string{"gather"}
	case "build":
		return []string{"build"}
	case "forge":
		return []string{"forge"}
	case "upgrade":
		return []string{"upgrade"}
	case "demolish":
		return []string{"demolish", "attack", "attack_miss", "hold"}
	default:
		return nil
	}
}

// firstNewLogMessage 在本次结算新增的日志里，按 kinds 优先序取第一条叙事文本。
func firstNewLogMessage(logs []LogEntry, start int, kinds []string) string {
	if start < 0 || start > len(logs) {
		return ""
	}
	for _, kind := range kinds {
		for _, entry := range logs[start:] {
			if entry.Kind == kind && strings.TrimSpace(entry.Message) != "" {
				return strings.TrimSpace(entry.Message)
			}
		}
	}
	return ""
}

// tileEffectBaseline 是结算前的单位快照基线，用于结算后 diff 出增减明细。
type tileEffectBaseline struct {
	items  map[string]int // itemID → 背包+装备合计件数
	hp     int
	hunger int
	wallet int
}

// captureTileEffectBaseline 记录结算前的物品/状态基线。
func captureTileEffectBaseline(record unit.Record) tileEffectBaseline {
	return tileEffectBaseline{
		items:  countUnitItems(record),
		hp:     record.Status.HP,
		hunger: record.Status.Hunger,
		wallet: record.Status.Wallet,
	}
}

// countUnitItems 统计单位背包 + 装备栏的每种物品合计件数。
func countUnitItems(record unit.Record) map[string]int {
	counts := make(map[string]int, len(record.Inventory.Backpack)+len(record.Inventory.Equipment))
	for _, stack := range record.Inventory.Backpack {
		counts[stack.ItemID] += stack.Quantity
	}
	for _, stack := range record.Inventory.Equipment {
		counts[stack.ItemID] += stack.Quantity
	}
	return counts
}

// diffTileEffects 对比基线与结算后的单位，产出物品/状态增减明细（物品按 ID 排序保证确定性输出）。
func diffTileEffects(baseline tileEffectBaseline, record unit.Record) []TileActionEffect {
	effects := make([]TileActionEffect, 0, 8)

	after := countUnitItems(record)
	itemIDs := make([]string, 0, len(after)+len(baseline.items))
	seen := map[string]struct{}{}
	for itemID := range after {
		itemIDs = append(itemIDs, itemID)
		seen[itemID] = struct{}{}
	}
	for itemID := range baseline.items {
		if _, ok := seen[itemID]; !ok {
			itemIDs = append(itemIDs, itemID)
		}
	}
	sort.Strings(itemIDs)
	for _, itemID := range itemIDs {
		delta := after[itemID] - baseline.items[itemID]
		if delta == 0 {
			continue
		}
		effects = append(effects, TileActionEffect{
			Kind:    "item",
			ItemID:  itemID,
			LabelZH: displayItemName(itemID),
			Delta:   delta,
		})
	}

	if delta := record.Status.HP - baseline.hp; delta != 0 {
		effects = append(effects, TileActionEffect{Kind: "status", LabelZH: "体力", Delta: delta})
	}
	if delta := record.Status.Hunger - baseline.hunger; delta != 0 {
		effects = append(effects, TileActionEffect{Kind: "status", LabelZH: "饱腹", Delta: delta})
	}
	if delta := record.Status.Wallet - baseline.wallet; delta != 0 {
		effects = append(effects, TileActionEffect{Kind: "status", LabelZH: "银钱", Delta: delta})
	}
	return effects
}

// emitTileActionTraceBestEffort 把玩家直驱动作写成命运流程事件（ReasonPlayerTileAction，append-only 留痕）。
// importance 按动作分量：采集/收割 2，建造/锻造/强化/拆自家 3，捣毁敌方设施 4。best-effort：失败吞错不阻断结算。
func (service *Service) emitTileActionTraceBestEffort(
	ctx context.Context,
	state *State,
	actor *unit.Record,
	req TileActionRequest,
	entry TileAction,
	summary string,
	hostileDemolish bool,
) {
	if service == nil || service.db == nil || state == nil || actor == nil {
		return
	}
	importance := 3
	switch req.Action {
	case "gather", "harvest":
		importance = 2
	case "demolish":
		if hostileDemolish {
			importance = 4
		}
	}
	_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   state.ID,
		OwnerUnitID: actor.ID,
		Code:        events.ReasonPlayerTileAction,
		Category:    events.CategoryLifecycle,
		Payload: map[string]any{
			"action":         req.Action,
			"activity":       strings.TrimSpace(req.Activity),
			"structure_type": strings.TrimSpace(req.StructureType),
			"q":              req.Q,
			"r":              req.R,
			"label_zh":       entry.LabelZH,
			"summary":        summary,
			"importance":     importance,
			"turn":           state.TurnState.Turn,
		},
		WorldID:  state.WorldID,
		RegionID: state.ID,
		Tick:     state.TurnState.Turn,
	})
}
