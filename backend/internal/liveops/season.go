// 文件说明：赛季骨架（设计 docs/产品方案PRD.md §8「无 live-ops 即慢性死亡」、docs/验证实验设计.md）。
// 一个赛季绑定一个世界：CreateSeason 建世界 + 落 seasons 行（可选挂内容母题）；FinalizeSeason 收尾——
// 把该世界**存活成员**回流名人堂（复用既有传承归档，本包只调用）、世界封存（只读）、广播 season finalized。
// 收尾是「让这一季的人留下传记、把舞台清空给下一季」，回流绝不阻断封存（best-effort 吞错每个角色独立）。

package liveops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"qunxiang/backend/internal/world"
)

// SeasonStatus 是赛季生命周期状态。
type SeasonStatus string

const (
	SeasonActive    SeasonStatus = "active"    // 进行中
	SeasonFinalized SeasonStatus = "finalized" // 已收尾（传承已回流、世界已封存）
)

// ErrSeasonNotFound 表示按 ID 找不到赛季。
var ErrSeasonNotFound = errors.New("season not found")

// Season 是一条赛季记录。
type Season struct {
	ID             string       `json:"id"`
	WorldID        string       `json:"world_id"`
	Name           string       `json:"name"`
	Status         SeasonStatus `json:"status"`
	StartedAt      string       `json:"started_at"`
	EndsAt         string       `json:"ends_at"`
	ContentThemeID string       `json:"content_theme_id"`
	CreatedAt      string       `json:"created_at"`
}

// CreateSeasonInput 是创建赛季的入参。
type CreateSeasonInput struct {
	Name           string // 赛季名（必填）
	WorldName      string // 新建世界名（空则用赛季名）
	ContentThemeID string // 可选：内容母题库指针（season_content_themes.id）
	MaxPopulation  int    // 新世界满员上限（透传 world.Create）
	RegionSeed     string // 新世界区域种子（确定性地形）
}

// CreateSeason 创建一个赛季：先建一个新世界，再落 seasons 行。返回赛季记录。
// 两步包事务：世界建好但 seasons 没落库会留下孤儿世界，故原子化。
func (s *LiveopsService) CreateSeason(ctx context.Context, in CreateSeasonInput) (Season, error) {
	if !s.ready() {
		return Season{}, fmt.Errorf("liveops create season: service not ready")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return Season{}, fmt.Errorf("liveops create season: empty name")
	}
	worldName := strings.TrimSpace(in.WorldName)
	if worldName == "" {
		worldName = name
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Season{}, fmt.Errorf("liveops create season: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	worldID, err := world.Create(ctx, tx, world.World{
		Name:          worldName,
		Status:        world.StatusActive,
		MaxPopulation: in.MaxPopulation,
		RegionSeed:    in.RegionSeed,
	})
	if err != nil {
		return Season{}, fmt.Errorf("liveops create season: create world: %w", err)
	}

	seasonID := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO seasons (id, world_id, name, status, content_theme_id)
		VALUES (?, ?, ?, ?, ?)`,
		seasonID, worldID, name, string(SeasonActive), strings.TrimSpace(in.ContentThemeID)); err != nil {
		return Season{}, fmt.Errorf("liveops create season: insert season: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Season{}, fmt.Errorf("liveops create season: commit: %w", err)
	}
	committed = true

	return s.GetSeason(ctx, seasonID)
}

// GetSeason 按 ID 取赛季；不存在时返回 ErrSeasonNotFound。
func (s *LiveopsService) GetSeason(ctx context.Context, seasonID string) (Season, error) {
	if !s.ready() {
		return Season{}, fmt.Errorf("liveops get season: service not ready")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, world_id, name, status, started_at, ends_at, content_theme_id, created_at
		FROM seasons WHERE id = ?`, seasonID)
	var se Season
	var status string
	if err := row.Scan(&se.ID, &se.WorldID, &se.Name, &status, &se.StartedAt, &se.EndsAt, &se.ContentThemeID, &se.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Season{}, ErrSeasonNotFound
		}
		return Season{}, fmt.Errorf("liveops get season: %w", err)
	}
	se.Status = SeasonStatus(status)
	return se, nil
}

// FinalizeResult 是赛季收尾的结果回执。
type FinalizeResult struct {
	SeasonID      string   `json:"season_id"`
	WorldID       string   `json:"world_id"`
	MembersTotal  int      `json:"members_total"`  // 收尾时世界成员总数
	Archived      int      `json:"archived"`       // 成功回流名人堂的角色数
	ArchiveErrors []string `json:"archive_errors"` // 回流失败的角色（best-effort，不阻断封存）
	Sealed        bool     `json:"sealed"`         // 世界是否已封存
}

// FinalizeSeason 收尾一个赛季：① 把该世界存活成员回流名人堂（复用既有传承归档）；② 世界封存（只读）；
// ③ seasons.status=finalized；④ 广播 season finalized。回流对每个角色独立 best-effort（一个失败不影响其余与封存）。
// 幂等保护：已 finalized 的赛季直接返回（不重复回流/封存）。
func (s *LiveopsService) FinalizeSeason(ctx context.Context, seasonID string) (FinalizeResult, error) {
	if !s.ready() {
		return FinalizeResult{}, fmt.Errorf("liveops finalize season: service not ready")
	}
	se, err := s.GetSeason(ctx, seasonID)
	if err != nil {
		return FinalizeResult{}, err
	}
	res := FinalizeResult{SeasonID: se.ID, WorldID: se.WorldID}
	if se.Status == SeasonFinalized {
		// 幂等：已收尾，不重复动作。
		res.Sealed = true
		return res, nil
	}

	// ① 回流存活成员（复用既有名人堂归档；archiver 未注入时跳过回流，仍执行封存）。
	members, err := world.Members(ctx, s.db, se.WorldID, 0)
	if err != nil {
		return res, fmt.Errorf("liveops finalize season: list members: %w", err)
	}
	res.MembersTotal = len(members)
	if s.archiver != nil {
		for _, m := range members {
			if strings.TrimSpace(m.CharacterID) == "" {
				continue
			}
			if archiveErr := s.archiver.ArchiveCharacterToHall(ctx, se.WorldID, m.CharacterID); archiveErr != nil {
				res.ArchiveErrors = append(res.ArchiveErrors, fmt.Sprintf("%s: %v", m.CharacterID, archiveErr))
				continue
			}
			res.Archived++
		}
	}

	// ② 世界封存（只读）。已被别处封存也无妨（Seal 幂等地置 status）。
	if sealErr := world.Seal(ctx, s.db, se.WorldID); sealErr != nil && !errors.Is(sealErr, world.ErrNotFound) {
		return res, fmt.Errorf("liveops finalize season: seal world: %w", sealErr)
	}
	res.Sealed = true

	// ③ seasons.status=finalized（updated_at 由调用侧默认值/触发器维护；这里只置状态）。
	if _, err := s.db.ExecContext(ctx, `UPDATE seasons SET status = ? WHERE id = ?`, string(SeasonFinalized), se.ID); err != nil {
		return res, fmt.Errorf("liveops finalize season: update status: %w", err)
	}

	// ④ best-effort 广播 season finalized（吞错）。
	if s.broadcaster != nil {
		s.broadcaster.BroadcastWorldNotice(se.WorldID, "season_finalized", fmt.Sprintf("「%s」一季落幕，%d 人入了名人堂。", se.Name, res.Archived))
	}

	return res, nil
}
