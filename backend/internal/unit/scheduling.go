package unit

// 文件说明：单位的世界作用域 + 生命态调度访问（沙盘 §8.7 / §8.2）。
// 这些方法操作 units 表的去规范化列（world_id/region_id/life_state/last_active_tick），为大世界的
// region-runner、HOT/WARM/COLD 分层与 wake 队列提供「按 region 查可行动单位 / 记活跃 tick」的能力。
//
// 两类列的同步语义**刻意不同**，调度层必须分清：
//   - world_id / region_id / last_active_tick：**仅由本文件方法赋值，Repository.Save 不写**（不在其 INSERT 列/UPDATE SET 里），
//     故整记录 Save 不会覆盖它们——这三列由调度层单向拥有。
//   - life_state：是 status_json.LifeState 的**去规范化只读索引**，权威永远是 status_json。Repository.Save 每次从
//     status_json.LifeState **单向覆盖写**本列（blob 赢）。因此 SetLifeState 的列内翻转**会被下一次该单位的任意 Save 静默还原**
//     （hunger/combat/每次 status.Mutator.Apply 都会 Save）——它只对「已与 blob 一致」的值持久，对 blob 里没有的值（如未来的
//     分层态）**不持久**。**任何持久的生命态变更必须改 record.Status.LifeState 再 Save，绝不能只调 SetLifeState。**
// LifeState 取值见 lives.go：active/down/recovering/dead（无 dormant；分层态若需要应另立列，不要复用 life_state）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// SetUnitScope 给单位赋值所属世界 + region（调度层 / 接入世界时调用）。只动作用域列，不触碰记录主体。
func (repository *Repository) SetUnitScope(ctx context.Context, unitID string, worldID string, regionID string) error {
	if _, err := repository.db.ExecContext(
		ctx,
		`UPDATE units SET world_id = ?, region_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		nullableUnitStr(worldID), nullableUnitStr(regionID), unitID,
	); err != nil {
		return fmt.Errorf("set unit scope %s: %w", unitID, err)
	}
	return nil
}

// SetLifeState 只翻转 life_state 索引列、不碰 status_json。⚠️ 非权威：权威是 status_json.LifeState，本翻转**会被该单位下一次
// 任意整记录 Save 从 blob 单向还原**（见文件头）。仅用于「blob 已是该值、想免去整记录 marshal 单独刷新索引列」的场景；
// 要持久改生命态，请改 record.Status.LifeState 后 Save。
func (repository *Repository) SetLifeState(ctx context.Context, unitID string, lifeState string) error {
	if _, err := repository.db.ExecContext(
		ctx,
		`UPDATE units SET life_state = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		normalizedLifeState(lifeState), unitID,
	); err != nil {
		return fmt.Errorf("set unit life_state %s: %w", unitID, err)
	}
	return nil
}

// TouchLastActiveTick 记录单位最近活跃的世界 tick（wake 调度 / 冷热分层判定用）。
func (repository *Repository) TouchLastActiveTick(ctx context.Context, unitID string, tick int64) error {
	if _, err := repository.db.ExecContext(
		ctx,
		`UPDATE units SET last_active_tick = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		tick, unitID,
	); err != nil {
		return fmt.Errorf("touch unit last_active_tick %s: %w", unitID, err)
	}
	return nil
}

// SchedulingState 读单位的调度态：last_active_tick（冷热分层用）+ life_state（跳过死/倒地用）。
// 这两列是去规范化调度列，不在 Record blob 里，故单独读。
func (repository *Repository) SchedulingState(ctx context.Context, unitID string) (lastActiveTick int64, lifeState string, err error) {
	var life sql.NullString
	if err := repository.db.QueryRowContext(ctx, `SELECT last_active_tick, life_state FROM units WHERE id = ?`, unitID).Scan(&lastActiveTick, &life); err != nil {
		return 0, "", fmt.Errorf("get unit scheduling state %s: %w", unitID, err)
	}
	if life.String == "" {
		return lastActiveTick, LifeStateActive, nil
	}
	return lastActiveTick, life.String, nil
}

// CountActiveByRegion 统计某 region 内生命态为 active 的单位数（调度容量/背压判定用，避免拉全量记录）。
func (repository *Repository) CountActiveByRegion(ctx context.Context, regionID string) (int, error) {
	var count int
	if err := repository.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM units WHERE region_id = ? AND life_state = ?`,
		regionID, LifeStateActive,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count active units in region %s: %w", regionID, err)
	}
	return count, nil
}

// ListActiveByRegion 列出某 region 内生命态为 active 的完整单位记录（region-runner 唤醒决策候选）。
func (repository *Repository) ListActiveByRegion(ctx context.Context, regionID string) ([]Record, error) {
	rows, err := repository.db.QueryContext(
		ctx,
		`
		SELECT id, session_id, faction_id, display_name, profile_json, personality_json, status_json, inventory_json
		FROM units
		WHERE region_id = ? AND life_state = ?
		ORDER BY last_active_tick ASC, id`,
		regionID, LifeStateActive,
	)
	if err != nil {
		return nil, fmt.Errorf("list active units in region %s: %w", regionID, err)
	}
	defer rows.Close()

	records := make([]Record, 0)
	for rows.Next() {
		record, err := decodeUnitRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan active unit in region %s: %w", regionID, err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active units in region %s: %w", regionID, err)
	}
	return records, nil
}

// decodeUnitRow 把单位八列（id/session/faction/display_name/profile/personality/status/inventory）解码成 Record。
// scan 形如 sql.Row.Scan / sql.Rows.Scan（dest ...any) error），故同一逻辑可服务行查询与单行查询。
func decodeUnitRow(scan func(dest ...any) error) (Record, error) {
	var record Record
	var displayName, encodedProfile, encodedPersonality, encodedStatus, encodedInventory string
	if err := scan(
		&record.ID, &record.SessionID, &record.FactionID, &displayName,
		&encodedProfile, &encodedPersonality, &encodedStatus, &encodedInventory,
	); err != nil {
		return Record{}, err
	}
	var profile profileDocument
	if err := json.Unmarshal([]byte(encodedProfile), &profile); err != nil {
		return Record{}, fmt.Errorf("decode unit profile: %w", err)
	}
	if err := json.Unmarshal([]byte(encodedPersonality), &record.Personality); err != nil {
		return Record{}, fmt.Errorf("decode unit personality: %w", err)
	}
	if err := json.Unmarshal([]byte(encodedStatus), &record.Status); err != nil {
		return Record{}, fmt.Errorf("decode unit status: %w", err)
	}
	if err := json.Unmarshal([]byte(encodedInventory), &record.Inventory); err != nil {
		return Record{}, fmt.Errorf("decode unit inventory: %w", err)
	}
	record.Identity = profile.Identity
	record.Stats = profile.Stats
	record.Skills = profile.Skills
	record.Social = profile.Social
	if record.Social.ParentUnitIDs == nil {
		record.Social.ParentUnitIDs = []string{}
	}
	if record.Social.ChildUnitIDs == nil {
		record.Social.ChildUnitIDs = []string{}
	}
	record.Memory = profile.Memory
	if record.Identity.Name == "" {
		record.Identity.Name = displayName
	}
	return record, nil
}

// nullableUnitStr 把空串转 nil，让 world_id/region_id 列存 NULL 而非空串（与 nullable 列语义一致）。
func nullableUnitStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
