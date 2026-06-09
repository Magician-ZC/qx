package session

// 文件说明：客户管理「某玩家的角色/命运进度」聚合读 + 按账户的数据擦除编排。
// 供司命台「客户管理」面板：列一个账户在各世界的角色摘要（轻量：sessionID/worldID/回合/主角名/生命态），
// 以及把「按账户擦除」编排为「找该账户所有 session → 逐个 EraseSessionPrivateData」。全程只读/best-effort，绝不碰受保护字段。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// CharacterSummary 是某玩家一个角色（一局 session）的轻量摘要（命运进度概览）。
type CharacterSummary struct {
	SessionID string `json:"session_id"`
	WorldID   string `json:"world_id"`
	Turn      int    `json:"turn"`
	HeroName  string `json:"hero_name"`
	LifeState string `json:"life_state"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ListCharactersByAccount 列出某账户在各世界的角色摘要（按更新时间倒序）。
// 轻量：只解析 state_json 的 player_unit_ids/turn，再对主角单位查一次 units 表取名与生命态。
func (service *Service) ListCharactersByAccount(ctx context.Context, accountID string) ([]CharacterSummary, error) {
	if service == nil || service.db == nil {
		return nil, fmt.Errorf("list characters by account: nil service or db")
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, fmt.Errorf("list characters by account: empty account id")
	}
	rows, err := service.db.QueryContext(ctx,
		`SELECT id, world_id, state_json, created_at, updated_at FROM single_player_sessions WHERE account_id = ? ORDER BY updated_at DESC`,
		accountID)
	if err != nil {
		return nil, fmt.Errorf("list characters by account: %w", err)
	}
	defer rows.Close()
	out := []CharacterSummary{}
	for rows.Next() {
		var (
			id, worldID, stateJSON string
			createdAt, updatedAt    sql.NullString
		)
		if err := rows.Scan(&id, &worldID, &stateJSON, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("list characters by account (scan): %w", err)
		}
		cs := CharacterSummary{SessionID: id, WorldID: worldID, CreatedAt: createdAt.String, UpdatedAt: updatedAt.String}
		var slim struct {
			PlayerUnitIDs []string `json:"player_unit_ids"`
			TurnState     struct {
				Turn int `json:"turn"`
			} `json:"turn_state"`
		}
		if err := json.Unmarshal([]byte(stateJSON), &slim); err == nil {
			cs.Turn = slim.TurnState.Turn
			if len(slim.PlayerUnitIDs) > 0 {
				cs.HeroName, cs.LifeState = service.heroNameLifeState(ctx, slim.PlayerUnitIDs[0])
			}
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

// heroNameLifeState best-effort 查主角单位的展示名与生命态（units 表）。查不到返回空串，绝不阻断摘要。
func (service *Service) heroNameLifeState(ctx context.Context, unitID string) (string, string) {
	if service == nil || service.db == nil || strings.TrimSpace(unitID) == "" {
		return "", ""
	}
	var name, lifeState sql.NullString
	if err := service.db.QueryRowContext(ctx,
		`SELECT display_name, life_state FROM units WHERE id = ?`, unitID).Scan(&name, &lifeState); err != nil {
		return "", ""
	}
	return name.String, lifeState.String
}

// EraseAccountPrivateData 按账户编排数据擦除：找该账户全部 session，逐个全量 EraseSessionPrivateData。
// best-effort：单个 session 擦除失败不阻断其余，返回成功擦除的 session 数与最后一个错误（供端点透传）。
// 这是高危不可逆操作——调用方（httpapi）须经 RBAC admin + 审计把关。
func (service *Service) EraseAccountPrivateData(ctx context.Context, accountID string) (int, error) {
	if service == nil || service.db == nil {
		return 0, fmt.Errorf("erase account data: nil service or db")
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return 0, fmt.Errorf("erase account data: empty account id")
	}
	rows, err := service.db.QueryContext(ctx,
		`SELECT id FROM single_player_sessions WHERE account_id = ?`, accountID)
	if err != nil {
		return 0, fmt.Errorf("erase account data (list sessions): %w", err)
	}
	var sessionIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("erase account data (scan): %w", err)
		}
		sessionIDs = append(sessionIDs, id)
	}
	rows.Close()

	erased := 0
	var lastErr error
	full := PrivacyEraseOptions{EraseDialogue: true, EraseLLMDetails: true, EraseAuditTrail: true, EraseMemories: true, EraseReports: true}
	for _, sid := range sessionIDs {
		if _, _, err := service.EraseSessionPrivateData(ctx, sid, full); err != nil {
			lastErr = err
			continue
		}
		erased++
	}
	return erased, lastErr
}
