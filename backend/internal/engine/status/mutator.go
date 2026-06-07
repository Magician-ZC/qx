package status

// 文件说明：状态变更执行器，统一应用字段增量并写入标准化事件 payload 与事件流水。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

// ErrConcurrentModification 表示乐观并发写未命中（自读取以来单位被其它写者改过），ApplyOptimistic 据此让调用方退避。
var ErrConcurrentModification = errors.New("status: unit modified concurrently (optimistic conflict)")

// Apply 执行一次状态变更并写入对应事件记录（无条件覆盖写，原有语义）。
func (mutator *Mutator) Apply(ctx context.Context, mutation Mutation) (Result, error) {
	return mutator.apply(ctx, mutation, func(ctx context.Context, record unit.Record) (bool, error) {
		return true, mutator.units.Save(ctx, record)
	})
}

// ApplyOptimistic 与 Apply 同，但用乐观并发条件写（SaveOptimistic）：若自 GetByID 以来该单位被其它写者改过，
// 不覆盖、不落事件，返回 ErrConcurrentModification。供 region-runner 离线写让位战斗/HTTP，避免丢更新（real-3-0）。
func (mutator *Mutator) ApplyOptimistic(ctx context.Context, mutation Mutation) (Result, error) {
	return mutator.apply(ctx, mutation, func(ctx context.Context, record unit.Record) (bool, error) {
		return mutator.units.SaveOptimistic(ctx, record)
	})
}

// apply 是 Apply / ApplyOptimistic 的共用主体：查表→读单位→改字段→追加 recentEventIDs→按 save 策略落盘→落事件。
// save 返回 (applied, err)：applied=false（仅乐观写可能）表示并发冲突，此时不落事件、返回 ErrConcurrentModification。
func (mutator *Mutator) apply(ctx context.Context, mutation Mutation, save func(context.Context, unit.Record) (bool, error)) (Result, error) {
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

	applied, err := save(ctx, record)
	if err != nil {
		return Result{}, err
	}
	if !applied {
		// 乐观写冲突：自读取以来单位被改过，不覆盖、不落事件，让调用方退避。
		return Result{}, ErrConcurrentModification
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

// ApplyBatch 批量应用一组状态变更，是 Apply 的高吞吐版本（引擎升级，详见设计方案 §11.2）。
// 现状：Apply 每次 mutation 做 3 次 DB 往返（GetByID + Save + INSERT events），一个决策常
// 触发 5–8 个 mutation → 约 15 次往返。ApplyBatch 把往返收敛为：按单位分组「读一次/写一次」
// + 所有事件在「单个事务」内批量插入，大幅降低 DB 压力。语义与逐次 Apply 等价（顺序一致）。
//
// 注意：单位状态的写入(Save)在事务之外按单位串行完成（与 Apply 一致，避免与 SQLite 单连接
// 的事务持有冲突）；仅事件插入走单事务。返回结果按入参 mutations 的原始顺序对齐。
func (mutator *Mutator) ApplyBatch(ctx context.Context, mutations []Mutation) ([]Result, error) {
	if len(mutations) == 0 {
		return nil, nil
	}

	// 预校验所有原因码，任一未知则整批拒绝（与 Apply 的严格语义一致）。
	definitions := make([]events.ReasonCodeDefinition, len(mutations))
	for i := range mutations {
		definition, ok := events.Lookup(mutations[i].ReasonCode)
		if !ok {
			return nil, fmt.Errorf("unknown reason code %s", mutations[i].ReasonCode)
		}
		definitions[i] = definition
	}

	// 按单位分组，保留首次出现顺序。
	order := make([]string, 0)
	grouped := make(map[string][]int)
	for i := range mutations {
		id := mutations[i].UnitID
		if _, ok := grouped[id]; !ok {
			order = append(order, id)
		}
		grouped[id] = append(grouped[id], i)
	}

	results := make([]Result, len(mutations))
	type pendingEvent struct {
		eventID   string
		sessionID string
		actorID   string
		targetID  string
		category  events.Category
		code      events.ReasonCode
		payload   string
	}
	pending := make([]pendingEvent, 0, len(mutations))

	// 逐单位：读一次 → 顺序应用该单位的全部 mutation → 写一次。
	for _, unitID := range order {
		record, err := mutator.units.GetByID(ctx, unitID)
		if err != nil {
			return nil, err
		}

		for _, idx := range grouped[unitID] {
			mutation := mutations[idx]
			definition := definitions[idx]

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
				return nil, fmt.Errorf("marshal event payload: %w", err)
			}

			actorID := mutation.UnitID
			if len(mutation.Actors) > 0 {
				actorID = mutation.Actors[0]
			}

			results[idx] = Result{EventID: eventID, Payload: payload}
			pending = append(pending, pendingEvent{
				eventID:   eventID,
				sessionID: record.SessionID,
				actorID:   actorID,
				targetID:  record.ID,
				category:  definition.Category,
				code:      definition.Code,
				payload:   string(encodedPayload),
			})
		}

		if err := mutator.units.Save(ctx, record); err != nil {
			return nil, err
		}
		// 该单位的最终记录回填到它的每一条结果上。
		for _, idx := range grouped[unitID] {
			results[idx].Record = record
		}
	}

	// 所有事件在单个事务内批量插入（一次提交/一次 fsync）。
	tx, err := mutator.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin batch event transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const insertEvent = `
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
		`
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, ev := range pending {
		if _, err := tx.ExecContext(
			ctx, insertEvent,
			ev.eventID, ev.sessionID, ev.actorID, ev.targetID,
			string(ev.category), string(ev.code), ev.payload, now,
		); err != nil {
			return nil, fmt.Errorf("insert batch event: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit batch event transaction: %w", err)
	}

	return results, nil
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
