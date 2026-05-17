package status

// 文件说明：状态变更执行器，统一应用字段增量并写入标准化事件 payload 与事件流水。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/unit"
)

// Field 类型定义用于统一该模块的数据表达。
type Field string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	FieldHP             Field = "hp"
	FieldMP             Field = "mp"
	FieldLivesRemaining Field = "lives_remaining"
	FieldAttack         Field = "attack"
	FieldDefense        Field = "defense"
	FieldMove           Field = "move"
	FieldHunger         Field = "hunger"
	FieldFatigue        Field = "fatigue"
	FieldMorale         Field = "morale"
	FieldLoyalty        Field = "loyalty"
	FieldWallet         Field = "wallet"
)

// Mutation 结构体用于承载该模块的核心数据。
type Mutation struct {
	UnitID       string            `json:"unit_id"`
	Turn         int               `json:"turn"`
	Field        Field             `json:"field"`
	Delta        float64           `json:"delta"`
	ReasonCode   events.ReasonCode `json:"reason_code"`
	ReasonText   string            `json:"reason_text"`
	Actors       []string          `json:"actors"`
	Location     string            `json:"location"`
	Importance   int               `json:"importance"`
	EmotionalTag string            `json:"emotional_tag"`
}

// EventPayload 结构体用于承载该模块的核心数据。
type EventPayload struct {
	UnitID       string            `json:"unit_id"`
	Turn         int               `json:"turn"`
	Field        Field             `json:"field"`
	Delta        float64           `json:"delta"`
	Before       float64           `json:"before"`
	After        float64           `json:"after"`
	ReasonCode   events.ReasonCode `json:"reason_code"`
	ReasonText   string            `json:"reason_text"`
	Actors       []string          `json:"actors"`
	Location     string            `json:"location"`
	Importance   int               `json:"importance"`
	EmotionalTag string            `json:"emotional_tag"`
	Category     events.Category   `json:"category"`
}

// Result 结构体用于承载该模块的核心数据。
type Result struct {
	Record  unit.Record  `json:"record"`
	EventID string       `json:"event_id"`
	Payload EventPayload `json:"payload"`
}

// Mutator 结构体用于承载该模块的核心数据。
type Mutator struct {
	db    *sql.DB
	units *unit.Repository
}

// NewMutator 创建状态变更器，封装单位仓储与事件落盘能力。
func NewMutator(db *sql.DB, units *unit.Repository) *Mutator {
	return &Mutator{
		db:    db,
		units: units,
	}
}

// Apply 执行一次状态变更并写入对应事件记录。
// 该方法会更新单位状态、追加 recentEventIDs，并落盘标准化 events 行。
func (mutator *Mutator) Apply(ctx context.Context, mutation Mutation) (Result, error) {
	definition, ok := events.Lookup(mutation.ReasonCode)
	if !ok {
		return Result{}, fmt.Errorf("unknown reason code %s", mutation.ReasonCode)
	}

	record, err := mutator.units.GetByID(ctx, mutation.UnitID)
	if err != nil {
		return Result{}, err
	}

	before := statusValue(record.Status, mutation.Field)
	after := applyDelta(before, mutation.Delta, mutation.Field)
	setStatusValue(&record.Status, mutation.Field, after)

	eventID := uuid.NewString()
	if mutation.ReasonText == "" {
		mutation.ReasonText = definition.DefaultReasonText
	}
	if mutation.Importance == 0 {
		mutation.Importance = (definition.ImportanceMin + definition.ImportanceMax) / 2
	}

	record.Memory.RecentEventIDs = append(record.Memory.RecentEventIDs, eventID)
	if len(record.Memory.RecentEventIDs) > 32 {
		record.Memory.RecentEventIDs = record.Memory.RecentEventIDs[len(record.Memory.RecentEventIDs)-32:]
	}

	if err := mutator.units.Save(ctx, record); err != nil {
		return Result{}, err
	}

	payload := EventPayload{
		UnitID:       mutation.UnitID,
		Turn:         mutation.Turn,
		Field:        mutation.Field,
		Delta:        mutation.Delta,
		Before:       before,
		After:        after,
		ReasonCode:   mutation.ReasonCode,
		ReasonText:   mutation.ReasonText,
		Actors:       mutation.Actors,
		Location:     mutation.Location,
		Importance:   mutation.Importance,
		EmotionalTag: mutation.EmotionalTag,
		Category:     definition.Category,
	}

	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return Result{}, fmt.Errorf("marshal event payload: %w", err)
	}

	actorID := mutation.UnitID
	if len(mutation.Actors) > 0 {
		actorID = mutation.Actors[0]
	}

	if _, err := mutator.db.ExecContext(
		ctx,
		`
		INSERT INTO events (
			id,
			session_id,
			actor_unit_id,
			target_unit_id,
			event_type,
			reason_code,
			payload_json,
			occurred_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`,
		eventID,
		record.SessionID,
		actorID,
		record.ID,
		string(definition.Category),
		string(definition.Code),
		string(encodedPayload),
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return Result{}, fmt.Errorf("insert event: %w", err)
	}

	return Result{
		Record:  record,
		EventID: eventID,
		Payload: payload,
	}, nil
}

// statusValue 读取状态字段的当前值（统一为 float64 便于计算）。
func statusValue(status unit.Status, field Field) float64 {
	switch field {
	case FieldHP:
		return float64(status.HP)
	case FieldMP:
		return float64(status.MP)
	case FieldLivesRemaining:
		return float64(status.LivesRemaining)
	case FieldAttack:
		return float64(status.Attack)
	case FieldDefense:
		return float64(status.Defense)
	case FieldMove:
		return float64(status.Move)
	case FieldHunger:
		return float64(status.Hunger)
	case FieldFatigue:
		return float64(status.Fatigue)
	case FieldMorale:
		return status.Morale
	case FieldLoyalty:
		return status.Loyalty
	case FieldWallet:
		return float64(status.Wallet)
	default:
		return 0
	}
}

// setStatusValue 按字段写回状态值，并应用字段级上下界约束。
func setStatusValue(status *unit.Status, field Field, value float64) {
	switch field {
	case FieldHP:
		status.HP = clampInt(value, 0, 100)
	case FieldMP:
		status.MP = clampInt(value, 0, 100)
	case FieldLivesRemaining:
		status.LivesRemaining = clampInt(value, 0, status.LivesMax)
	case FieldAttack:
		status.Attack = clampInt(value, 0, 999)
	case FieldDefense:
		status.Defense = clampInt(value, 0, 999)
	case FieldMove:
		status.Move = clampInt(value, 0, 99)
	case FieldHunger:
		status.Hunger = clampInt(value, 0, 100)
	case FieldFatigue:
		status.Fatigue = clampInt(value, 0, 100)
	case FieldMorale:
		status.Morale = clampFloat(value, 0, 1)
	case FieldLoyalty:
		status.Loyalty = clampFloat(value, 0, 1)
	case FieldWallet:
		status.Wallet = clampInt(value, 0, 999999)
	}
}

// applyDelta 计算变更后的字段值。
// 士气/忠诚使用浮点增量，其它数值字段按整数四舍五入处理。
func applyDelta(before float64, delta float64, field Field) float64 {
	switch field {
	case FieldMorale, FieldLoyalty:
		return before + delta
	default:
		return before + math.Round(delta)
	}
}

// clampInt 对数值做四舍五入并夹紧到整数范围。
func clampInt(value float64, min int, max int) int {
	rounded := int(math.Round(value))
	if rounded < min {
		return min
	}
	if rounded > max {
		return max
	}
	return rounded
}

// clampFloat 把浮点数夹紧到范围并保留两位小数。
func clampFloat(value float64, min float64, max float64) float64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return math.Round(value*100) / 100
}
