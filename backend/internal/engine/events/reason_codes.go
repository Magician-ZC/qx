package events

// 文件说明：维护状态变更原因码目录，提供内置定义查询与数据库种子写入/读取。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Category 类型定义用于统一该模块的数据表达。
type Category string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	CategoryCombat   Category = "combat_damage"
	CategorySurvival Category = "survival_consumption"
	CategoryEmotion  Category = "emotion_event"
	CategoryEconomy  Category = "economy_material"
	CategoryRelation Category = "relation_change"
	CategoryCommand  Category = "command_response"
)

// ReasonCode 类型定义用于统一该模块的数据表达。
type ReasonCode string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	ReasonCombatHit       ReasonCode = "COMBAT_HIT"
	ReasonCombatDown      ReasonCode = "COMBAT_DOWN"
	ReasonSurvivalMarch   ReasonCode = "SURVIVAL_MARCH_EXHAUST"
	ReasonSurvivalHunger  ReasonCode = "SURVIVAL_HUNGER"
	ReasonEmotionTrauma   ReasonCode = "EMOTION_TRAUMA"
	ReasonEmotionReward   ReasonCode = "EMOTION_REWARD"
	ReasonEconomyPurchase ReasonCode = "ECONOMY_PURCHASE"
	ReasonEconomyReward   ReasonCode = "ECONOMY_REWARD"
	ReasonEconomyLoot     ReasonCode = "ECONOMY_LOOT"
	ReasonRelationRescue  ReasonCode = "RELATION_RESCUED"
	ReasonRelationBetray  ReasonCode = "RELATION_BETRAYAL"
	ReasonCommandForced   ReasonCode = "COMMAND_FORCED_ORDER"
	ReasonCommandAccepted ReasonCode = "COMMAND_ACCEPTED_ADVICE"
)

// ReasonCodeDefinition 结构体用于承载该模块的核心数据。
type ReasonCodeDefinition struct {
	Code              ReasonCode `json:"code"`
	Category          Category   `json:"category"`
	DisplayName       string     `json:"display_name"`
	DefaultReasonText string     `json:"default_reason_text"`
	StatDomains       []string   `json:"stat_domains"`
	ImportanceMin     int        `json:"importance_min"`
	ImportanceMax     int        `json:"importance_max"`
}

// Catalog 返回内置事件原因码目录，用于状态变更与事件落盘标准化。
func Catalog() []ReasonCodeDefinition {
	return []ReasonCodeDefinition{
		{Code: ReasonCombatHit, Category: CategoryCombat, DisplayName: "战斗受伤", DefaultReasonText: "在战斗中受到伤害", StatDomains: []string{"hp"}, ImportanceMin: 5, ImportanceMax: 8},
		{Code: ReasonCombatDown, Category: CategoryCombat, DisplayName: "倒地濒死", DefaultReasonText: "在战斗中被打至倒地", StatDomains: []string{"hp", "lives_remaining"}, ImportanceMin: 8, ImportanceMax: 10},
		{Code: ReasonSurvivalMarch, Category: CategorySurvival, DisplayName: "行军透支", DefaultReasonText: "连续行军导致疲劳上升", StatDomains: []string{"fatigue"}, ImportanceMin: 2, ImportanceMax: 4},
		{Code: ReasonSurvivalHunger, Category: CategorySurvival, DisplayName: "饥饿消耗", DefaultReasonText: "补给不足导致饥饿加深", StatDomains: []string{"hunger"}, ImportanceMin: 2, ImportanceMax: 4},
		{Code: ReasonEmotionTrauma, Category: CategoryEmotion, DisplayName: "创伤事件", DefaultReasonText: "目睹惨烈事件后情绪受挫", StatDomains: []string{"morale"}, ImportanceMin: 6, ImportanceMax: 9},
		{Code: ReasonEmotionReward, Category: CategoryEmotion, DisplayName: "荣誉奖励", DefaultReasonText: "获得奖励后士气提升", StatDomains: []string{"morale"}, ImportanceMin: 6, ImportanceMax: 8},
		{Code: ReasonEconomyPurchase, Category: CategoryEconomy, DisplayName: "物资购买", DefaultReasonText: "花费金币购入物资", StatDomains: []string{"wallet"}, ImportanceMin: 2, ImportanceMax: 5},
		{Code: ReasonEconomyReward, Category: CategoryEconomy, DisplayName: "任务奖励", DefaultReasonText: "完成任务后获得奖励", StatDomains: []string{"wallet"}, ImportanceMin: 3, ImportanceMax: 6},
		{Code: ReasonEconomyLoot, Category: CategoryEconomy, DisplayName: "战利品继承", DefaultReasonText: "继承敌方资产", StatDomains: []string{"wallet"}, ImportanceMin: 4, ImportanceMax: 7},
		{Code: ReasonRelationRescue, Category: CategoryRelation, DisplayName: "被队友救援", DefaultReasonText: "被同伴救下一命", StatDomains: []string{"loyalty"}, ImportanceMin: 5, ImportanceMax: 8},
		{Code: ReasonRelationBetray, Category: CategoryRelation, DisplayName: "遭遇背叛", DefaultReasonText: "对同伴的信任受到打击", StatDomains: []string{"loyalty"}, ImportanceMin: 5, ImportanceMax: 8},
		{Code: ReasonCommandForced, Category: CategoryCommand, DisplayName: "强制命令", DefaultReasonText: "被强令执行高风险命令", StatDomains: []string{"loyalty", "morale"}, ImportanceMin: 3, ImportanceMax: 7},
		{Code: ReasonCommandAccepted, Category: CategoryCommand, DisplayName: "建议被采纳", DefaultReasonText: "自己的建议被采纳", StatDomains: []string{"loyalty", "morale"}, ImportanceMin: 3, ImportanceMax: 6},
	}
}

// Lookup 按原因码查找定义。
func Lookup(code ReasonCode) (ReasonCodeDefinition, bool) {
	for _, definition := range Catalog() {
		if definition.Code == code {
			return definition, true
		}
	}
	return ReasonCodeDefinition{}, false
}

// SeedReasonCodeCatalog 把内置原因码目录写入数据库（存在则更新）。
func SeedReasonCodeCatalog(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reason code transaction: %w", err)
	}
	defer tx.Rollback()

	for _, definition := range Catalog() {
		domains, err := json.Marshal(definition.StatDomains)
		if err != nil {
			return fmt.Errorf("marshal stat domains for %s: %w", definition.Code, err)
		}

		if _, err := tx.ExecContext(
			ctx,
			`
			INSERT INTO event_reason_codes (
				code,
				category,
				display_name,
				default_reason_text,
				stat_domains_json,
				importance_min,
				importance_max
			) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(code) DO UPDATE SET
				category = excluded.category,
				display_name = excluded.display_name,
				default_reason_text = excluded.default_reason_text,
				stat_domains_json = excluded.stat_domains_json,
				importance_min = excluded.importance_min,
				importance_max = excluded.importance_max
			`,
			string(definition.Code),
			string(definition.Category),
			definition.DisplayName,
			definition.DefaultReasonText,
			string(domains),
			definition.ImportanceMin,
			definition.ImportanceMax,
		); err != nil {
			return fmt.Errorf("upsert reason code %s: %w", definition.Code, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reason code transaction: %w", err)
	}

	return nil
}

// LoadReasonCodeCatalog 从数据库读取原因码目录并反序列化 stat_domains 字段。
func LoadReasonCodeCatalog(ctx context.Context, db *sql.DB) ([]ReasonCodeDefinition, error) {
	rows, err := db.QueryContext(
		ctx,
		`
		SELECT code, category, display_name, default_reason_text, stat_domains_json, importance_min, importance_max
		FROM event_reason_codes
		ORDER BY rowid
		`,
	)
	if err != nil {
		return nil, fmt.Errorf("query reason codes: %w", err)
	}
	defer rows.Close()

	definitions := make([]ReasonCodeDefinition, 0, len(Catalog()))
	for rows.Next() {
		var definition ReasonCodeDefinition
		var domains string
		if err := rows.Scan(
			&definition.Code,
			&definition.Category,
			&definition.DisplayName,
			&definition.DefaultReasonText,
			&domains,
			&definition.ImportanceMin,
			&definition.ImportanceMax,
		); err != nil {
			return nil, fmt.Errorf("scan reason code: %w", err)
		}

		if err := json.Unmarshal([]byte(domains), &definition.StatDomains); err != nil {
			return nil, fmt.Errorf("decode stat domains for %s: %w", definition.Code, err)
		}

		definitions = append(definitions, definition)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reason codes: %w", err)
	}

	return definitions, nil
}
