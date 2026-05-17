package world

// 文件说明：地形规则目录定义与持久化模块，维护地形属性并提供数据库种子与读取接口。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// TerrainID 类型定义用于统一该模块的数据表达。
type TerrainID string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	TerrainPlains      TerrainID = "plains"
	TerrainForest      TerrainID = "forest"
	TerrainMountain    TerrainID = "mountain"
	TerrainRiver       TerrainID = "river"
	TerrainRiverValley TerrainID = "river_valley"
	TerrainGrassland   TerrainID = "grassland"
	TerrainDesert      TerrainID = "desert"
	TerrainSwamp       TerrainID = "swamp"
	TerrainRuins       TerrainID = "ruins"
	TerrainVillage     TerrainID = "village"
	TerrainCity        TerrainID = "city"
	TerrainSnowfield   TerrainID = "snowfield"
	TerrainRoad        TerrainID = "road"
)

// TerrainDefinition 描述一种地形的移动、视野、战斗与资源规则。
type TerrainDefinition struct {
	ID           TerrainID `json:"id"`
	DisplayName  string    `json:"display_name"`
	MoveCost     float64   `json:"move_cost"`
	VisionRange  int       `json:"vision_range"`
	CombatRules  []string  `json:"combat_rules"`
	Activities   []string  `json:"activities"`
	Resources    []string  `json:"resources"`
	SpecialRules []string  `json:"special_rules"`
}

// TerrainCatalog 返回内置地形规则配置清单。
func TerrainCatalog() []TerrainDefinition {
	return []TerrainDefinition{
		{
			ID:           TerrainPlains,
			DisplayName:  "平原",
			MoveCost:     1,
			VisionRange:  5,
			CombatRules:  []string{"无掩体", "骑兵 +20% ATK"},
			Activities:   []string{"种地", "行军", "建造"},
			Resources:    []string{"食物", "农田"},
			SpecialRules: []string{"开阔易被发现", "适合屯田"},
		},
		{
			ID:           TerrainForest,
			DisplayName:  "森林",
			MoveCost:     2,
			VisionRange:  2,
			CombatRules:  []string{"掩体 DEF +10", "弓箭命中 -20%"},
			Activities:   []string{"打猎", "采集", "砍伐"},
			Resources:    []string{"木材", "皮革", "药草"},
			SpecialRules: []string{"潜行 +30%", "埋伏首选"},
		},
		{
			ID:           TerrainMountain,
			DisplayName:  "山地",
			MoveCost:     3,
			VisionRange:  8,
			CombatRules:  []string{"DEF +15", "高地射程 +1"},
			Activities:   []string{"挖矿", "采集"},
			Resources:    []string{"铁矿", "石料"},
			SpecialRules: []string{"骑兵不可进入", "行军疲劳 x2"},
		},
		{
			ID:           TerrainRiver,
			DisplayName:  "河流",
			MoveCost:     4,
			VisionRange:  3,
			CombatRules:  []string{"泅渡中 DEF -20"},
			Activities:   []string{"钓鱼", "取水"},
			Resources:    []string{"鱼", "淡水"},
			SpecialRules: []string{"有船则移动消耗 1", "饮水补给点"},
		},
		{
			ID:           TerrainRiverValley,
			DisplayName:  "河谷",
			MoveCost:     1,
			VisionRange:  4,
			CombatRules:  []string{"无特殊"},
			Activities:   []string{"种地", "钓鱼"},
			Resources:    []string{"高产食物"},
			SpecialRules: []string{"食物产出比平原 +30%", "兵家必争之地"},
		},
		{
			ID:           TerrainGrassland,
			DisplayName:  "草原",
			MoveCost:     1,
			VisionRange:  6,
			CombatRules:  []string{"骑兵 +15% ATK/MOV"},
			Activities:   []string{"打猎", "放牧"},
			Resources:    []string{"肉食", "皮革"},
			SpecialRules: []string{"风大", "火攻伤害 +50%"},
		},
		{
			ID:           TerrainDesert,
			DisplayName:  "沙漠",
			MoveCost:     3,
			VisionRange:  4,
			CombatRules:  []string{"无掩体", "中暑检定"},
			Activities:   []string{"极少", "寻宝"},
			Resources:    []string{"金币遗迹"},
			SpecialRules: []string{"饥饿/疲劳 x1.5", "带水囊才能稳定穿越"},
		},
		{
			ID:           TerrainSwamp,
			DisplayName:  "沼泽",
			MoveCost:     4,
			VisionRange:  2,
			CombatRules:  []string{"所有单位 MOV -1", "疾病风险"},
			Activities:   []string{"采集"},
			Resources:    []string{"稀有药草", "毒物"},
			SpecialRules: []string{"每回合 10% 得病风险", "药剂产出珍贵"},
		},
		{
			ID:           TerrainRuins,
			DisplayName:  "废墟",
			MoveCost:     2,
			VisionRange:  3,
			CombatRules:  []string{"掩体 DEF +8", "陷阱风险"},
			Activities:   []string{"探索", "采集"},
			Resources:    []string{"古物", "装备图纸"},
			SpecialRules: []string{"进入触发事件检定"},
		},
		{
			ID:           TerrainVillage,
			DisplayName:  "村庄",
			MoveCost:     1,
			VisionRange:  3,
			CombatRules:  []string{"街巷战", "DEF +5"},
			Activities:   []string{"集市交易", "招募"},
			Resources:    []string{"可买卖通用物资"},
			SpecialRules: []string{"NPC 委托任务", "开战会损声望"},
		},
		{
			ID:           TerrainCity,
			DisplayName:  "城市",
			MoveCost:     1,
			VisionRange:  3,
			CombatRules:  []string{"城墙 DEF +25", "远程 -30%"},
			Activities:   []string{"高级集市", "物资交换", "情报"},
			Resources:    []string{"稀有装备", "信鸽"},
			SpecialRules: []string{"攻城需要攻城器械"},
		},
		{
			ID:           TerrainSnowfield,
			DisplayName:  "雪原",
			MoveCost:     3,
			VisionRange:  5,
			CombatRules:  []string{"所有 MOV -1"},
			Activities:   []string{"打猎"},
			Resources:    []string{"皮毛", "稀有肉"},
			SpecialRules: []string{"饥饿 x1.3", "需厚衣保暖"},
		},
		{
			ID:           TerrainRoad,
			DisplayName:  "道路",
			MoveCost:     0.5,
			VisionRange:  4,
			CombatRules:  []string{"无特殊"},
			Activities:   []string{"行军", "运输"},
			Resources:    []string{},
			SpecialRules: []string{"建造道路 2 回合/格", "加速行军"},
		},
	}
}

// SeedTerrainCatalog 把内置地形配置写入数据库（幂等 upsert）。
func SeedTerrainCatalog(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin terrain catalog transaction: %w", err)
	}
	defer tx.Rollback()

	for _, terrain := range TerrainCatalog() {
		combatRules, err := json.Marshal(terrain.CombatRules)
		if err != nil {
			return fmt.Errorf("marshal combat rules for %s: %w", terrain.ID, err)
		}
		activities, err := json.Marshal(terrain.Activities)
		if err != nil {
			return fmt.Errorf("marshal activities for %s: %w", terrain.ID, err)
		}
		resources, err := json.Marshal(terrain.Resources)
		if err != nil {
			return fmt.Errorf("marshal resources for %s: %w", terrain.ID, err)
		}
		specialRules, err := json.Marshal(terrain.SpecialRules)
		if err != nil {
			return fmt.Errorf("marshal special rules for %s: %w", terrain.ID, err)
		}

		if _, err := tx.ExecContext(
			ctx,
			`
			INSERT INTO terrain_types (
				id,
				display_name,
				move_cost,
				vision_range,
				combat_rules_json,
				activities_json,
				resources_json,
				special_rules_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				display_name = excluded.display_name,
				move_cost = excluded.move_cost,
				vision_range = excluded.vision_range,
				combat_rules_json = excluded.combat_rules_json,
				activities_json = excluded.activities_json,
				resources_json = excluded.resources_json,
				special_rules_json = excluded.special_rules_json
			`,
			string(terrain.ID),
			terrain.DisplayName,
			terrain.MoveCost,
			terrain.VisionRange,
			string(combatRules),
			string(activities),
			string(resources),
			string(specialRules),
		); err != nil {
			return fmt.Errorf("upsert terrain %s: %w", terrain.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit terrain catalog transaction: %w", err)
	}

	return nil
}

// LoadTerrainCatalog 从数据库读取地形配置列表。
func LoadTerrainCatalog(ctx context.Context, db *sql.DB) ([]TerrainDefinition, error) {
	rows, err := db.QueryContext(
		ctx,
		`
		SELECT
			id,
			display_name,
			move_cost,
			vision_range,
			combat_rules_json,
			activities_json,
			resources_json,
			special_rules_json
		FROM terrain_types
		ORDER BY rowid
		`,
	)
	if err != nil {
		return nil, fmt.Errorf("query terrain catalog: %w", err)
	}
	defer rows.Close()

	terrains := make([]TerrainDefinition, 0, 13)
	for rows.Next() {
		var terrain TerrainDefinition
		var combatRules string
		var activities string
		var resources string
		var specialRules string
		if err := rows.Scan(
			&terrain.ID,
			&terrain.DisplayName,
			&terrain.MoveCost,
			&terrain.VisionRange,
			&combatRules,
			&activities,
			&resources,
			&specialRules,
		); err != nil {
			return nil, fmt.Errorf("scan terrain catalog row: %w", err)
		}

		if err := json.Unmarshal([]byte(combatRules), &terrain.CombatRules); err != nil {
			return nil, fmt.Errorf("decode combat rules for %s: %w", terrain.ID, err)
		}
		if err := json.Unmarshal([]byte(activities), &terrain.Activities); err != nil {
			return nil, fmt.Errorf("decode activities for %s: %w", terrain.ID, err)
		}
		if err := json.Unmarshal([]byte(resources), &terrain.Resources); err != nil {
			return nil, fmt.Errorf("decode resources for %s: %w", terrain.ID, err)
		}
		if err := json.Unmarshal([]byte(specialRules), &terrain.SpecialRules); err != nil {
			return nil, fmt.Errorf("decode special rules for %s: %w", terrain.ID, err)
		}

		terrains = append(terrains, terrain)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate terrain catalog: %w", err)
	}

	return terrains, nil
}
