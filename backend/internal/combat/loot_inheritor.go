package combat

// 文件说明：击杀后战利品继承流程，处理候选筛选、保留决策、掉落落盘与双方资产更新。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/item"
	"qunxiang/backend/internal/unit"
)

// lootDecider 接口定义该模块需要实现的能力约束。
type lootDecider interface {
	GenerateJSON(context.Context, ai.CompletionRequest) (ai.CompletionResult, error)
}

// LootInheritor 结构体用于承载该模块的核心数据。
type LootInheritor struct {
	db      *sql.DB
	units   *unit.Repository
	decider lootDecider
}

// Candidate 结构体用于承载该模块的核心数据。
type Candidate struct {
	KeepID      string `json:"keep_id"`
	ItemID      string `json:"item_id"`
	DisplayName string `json:"display_name"`
	Source      string `json:"source"`
	Quantity    int    `json:"quantity"`
	Price       int    `json:"price"`
}

// ResolveRequest 结构体用于承载该模块的核心数据。
type ResolveRequest struct {
	KillerUnitID string `json:"killer_unit_id"`
	VictimUnitID string `json:"victim_unit_id"`
	Location     string `json:"location"`
}

// ResolveResult 结构体用于承载该模块的核心数据。
type ResolveResult struct {
	Killer         unit.Record      `json:"killer"`
	Victim         unit.Record      `json:"victim"`
	KeptCandidates []Candidate      `json:"kept_candidates"`
	DroppedItems   []unit.ItemStack `json:"dropped_items"`
	DropID         string           `json:"drop_id,omitempty"`
}

// NewLootInheritor 创建战利品继承器，封装单位仓储与战利品决策器。
func NewLootInheritor(db *sql.DB, units *unit.Repository, decider lootDecider) *LootInheritor {
	return &LootInheritor{
		db:      db,
		units:   units,
		decider: decider,
	}
}

// Resolve 结算一次击杀后的战利品继承流程。
// 包括钱包继承、装备/背包候选收集、保留决策、掉落落盘与双方单位保存。
func (service *LootInheritor) Resolve(ctx context.Context, request ResolveRequest) (ResolveResult, error) {
	killer, err := service.units.GetByID(ctx, request.KillerUnitID)
	if err != nil {
		return ResolveResult{}, err
	}
	victim, err := service.units.GetByID(ctx, request.VictimUnitID)
	if err != nil {
		return ResolveResult{}, err
	}

	killer.Status.Wallet += victim.Status.Wallet
	victim.Status.Wallet = 0

	candidates := make([]Candidate, 0, len(victim.Inventory.Backpack)+len(victim.Inventory.Equipment))
	for slot, stack := range victim.Inventory.Equipment {
		if stack.ItemID == "" {
			continue
		}

		definition, ok := item.Lookup(stack.ItemID)
		if !ok {
			continue
		}

		if current, occupied := killer.Inventory.Equipment[slot]; !occupied || current.ItemID == "" {
			killer.Inventory.Equipment[slot] = stack
			continue
		}

		candidates = append(candidates, Candidate{
			KeepID:      "equip:" + slot + ":" + stack.ItemID,
			ItemID:      stack.ItemID,
			DisplayName: definition.DisplayName,
			Source:      "equipment",
			Quantity:    stack.Quantity,
			Price:       definition.Price,
		})
	}

	for index, stack := range victim.Inventory.Backpack {
		definition, ok := item.Lookup(stack.ItemID)
		if !ok {
			continue
		}

		candidates = append(candidates, Candidate{
			KeepID:      fmt.Sprintf("bag:%d:%s", index, stack.ItemID),
			ItemID:      stack.ItemID,
			DisplayName: definition.DisplayName,
			Source:      "backpack",
			Quantity:    stack.Quantity,
			Price:       definition.Price,
		})
	}

	availableSlots := unit.BackpackCapacity - len(killer.Inventory.Backpack)
	if availableSlots < 0 {
		availableSlots = 0
	}

	keptIDs, err := service.decideKeepers(ctx, killer, candidates, availableSlots)
	if err != nil {
		return ResolveResult{}, err
	}

	keptSet := make(map[string]struct{}, len(keptIDs))
	for _, keepID := range keptIDs {
		keptSet[keepID] = struct{}{}
	}

	keptCandidates := make([]Candidate, 0, len(keptSet))
	droppedItems := make([]unit.ItemStack, 0)
	for _, candidate := range candidates {
		if _, ok := keptSet[candidate.KeepID]; ok {
			if err := unit.AddBackpackItem(&killer, candidate.ItemID, candidate.Quantity); err != nil {
				droppedItems = append(droppedItems, unit.ItemStack{ItemID: candidate.ItemID, Quantity: candidate.Quantity})
				continue
			}
			keptCandidates = append(keptCandidates, candidate)
			continue
		}

		droppedItems = append(droppedItems, unit.ItemStack{ItemID: candidate.ItemID, Quantity: candidate.Quantity})
	}

	victim.Inventory = unit.Inventory{
		Equipment: map[string]unit.ItemStack{},
		Backpack:  []unit.ItemStack{},
	}
	unit.RecalculateDerivedStats(&killer)
	unit.RecalculateDerivedStats(&victim)

	if err := service.units.Save(ctx, killer); err != nil {
		return ResolveResult{}, err
	}
	if err := service.units.Save(ctx, victim); err != nil {
		return ResolveResult{}, err
	}

	dropID := ""
	if len(droppedItems) > 0 {
		dropID, err = service.persistDrop(ctx, request.Location, victim.ID, killer.ID, droppedItems)
		if err != nil {
			return ResolveResult{}, err
		}
	}

	return ResolveResult{
		Killer:         killer,
		Victim:         victim,
		KeptCandidates: keptCandidates,
		DroppedItems:   droppedItems,
		DropID:         dropID,
	}, nil
}

// decideKeepers 决定击杀者从候选物资中保留哪些条目。
// 优先调用 LLM 决策，失败时回退价值优先规则。
func (service *LootInheritor) decideKeepers(
	ctx context.Context,
	killer unit.Record,
	candidates []Candidate,
	limit int,
) ([]string, error) {
	if limit <= 0 || len(candidates) == 0 {
		return []string{}, nil
	}
	if len(candidates) <= limit {
		ids := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			ids = append(ids, candidate.KeepID)
		}
		return ids, nil
	}

	fallback := ai.RuleFallbackFunc(func(context.Context, ai.CompletionRequest, error) (json.RawMessage, error) {
		return json.Marshal(map[string]any{
			"keep_ids": fallbackKeepers(candidates, limit),
		})
	})

	if service.decider == nil {
		raw, err := fallback.Fallback(ctx, ai.CompletionRequest{}, nil)
		if err != nil {
			return nil, err
		}

		var payload struct {
			KeepIDs []string `json:"keep_ids"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, err
		}
		return payload.KeepIDs, nil
	}

	prompt, err := json.Marshal(map[string]any{
		"killer_personality": killer.Personality,
		"killer_status":      killer.Status,
		"current_equipment":  killer.Inventory.Equipment,
		"candidates":         candidates,
		"limit":              limit,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal loot prompt: %w", err)
	}

	result, err := service.decider.GenerateJSON(ctx, ai.CompletionRequest{
		Task:         ai.TaskUnitDecision,
		SystemPrompt: "Select the loot keep_ids the unit should retain. Return JSON only.",
		UserPrompt:   string(prompt),
		SchemaName:   "loot_inheritor_choice",
		ResponseSchema: []byte(`{
			"type":"object",
			"properties":{"keep_ids":{"type":"array","items":{"type":"string"}}},
			"required":["keep_ids"],
			"additionalProperties":false
		}`),
		Timeout:  60 * time.Second,
		Fallback: fallback,
	})
	if err != nil {
		return nil, err
	}

	var payload struct {
		KeepIDs []string `json:"keep_ids"`
	}
	if err := json.Unmarshal(result.Output, &payload); err != nil {
		return nil, fmt.Errorf("decode loot decision: %w", err)
	}

	return payload.KeepIDs, nil
}

// fallbackKeepers 按“总价值 = 单价 * 数量”排序，选取前 limit 个候选。
func fallbackKeepers(candidates []Candidate, limit int) []string {
	sorted := append([]Candidate(nil), candidates...)
	sort.Slice(sorted, func(i int, j int) bool {
		leftValue := sorted[i].Price * sorted[i].Quantity
		rightValue := sorted[j].Price * sorted[j].Quantity
		if leftValue == rightValue {
			return sorted[i].KeepID < sorted[j].KeepID
		}
		return leftValue > rightValue
	})

	ids := make([]string, 0, limit)
	for index := 0; index < len(sorted) && index < limit; index++ {
		ids = append(ids, sorted[index].KeepID)
	}
	return ids
}

// persistDrop 把未继承的战利品写入地面掉落表，供后续拾取。
func (service *LootInheritor) persistDrop(
	ctx context.Context,
	location string,
	sourceUnitID string,
	inheritorUnitID string,
	items []unit.ItemStack,
) (string, error) {
	encodedItems, err := json.Marshal(items)
	if err != nil {
		return "", fmt.Errorf("marshal dropped items: %w", err)
	}

	dropID := uuid.NewString()
	if _, err := service.db.ExecContext(
		ctx,
		`
		INSERT INTO ground_loot_drops (id, location, source_unit_id, inheritor_unit_id, items_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		`,
		dropID,
		location,
		sourceUnitID,
		inheritorUnitID,
		string(encodedItems),
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return "", fmt.Errorf("insert ground loot drop: %w", err)
	}

	return dropID, nil
}
