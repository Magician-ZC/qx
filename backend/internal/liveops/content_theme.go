package liveops

// 文件说明：赛季内容母题库（season_content_themes）的运营 CRUD + 默认播种。
// 母题 = 一个赛季的内容母题包：决定性事件 id 集 + 称号 id 集 + 地标名集。原表是「死表」（只建表、零读写）；
// 本文件给 GM 后台提供增删改 + 启动幂等播种，使 CreateSeason 的 content_theme_id 指向真实可运营的母题。
// 双驱动 upsert（ON CONFLICT / ON DUPLICATE KEY）；JSON 数组列以 JSON 文本存储。best-effort 风格对齐 liveops 其余能力。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"qunxiang/backend/internal/storage/dbdialect"
)

// ContentTheme 是一个赛季内容母题（GM 可运营）。三个 *IDs/Names 是确定性内容指针集，供叙事/称号/地标消费。
type ContentTheme struct {
	ID               string   `json:"id"`
	SeasonID         string   `json:"season_id"`
	DecisiveEventIDs []string `json:"decisive_event_ids"`
	TitleIDs         []string `json:"title_ids"`
	LandmarkNames    []string `json:"landmark_names"`
	CreatedAt        string   `json:"created_at"`
}

// ListContentThemes 列出全部母题（按创建时间倒序）。
func (s *LiveopsService) ListContentThemes(ctx context.Context) ([]ContentTheme, error) {
	if !s.ready() {
		return nil, fmt.Errorf("liveops list content themes: service not ready")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, season_id, decisive_event_ids, title_ids, landmark_names, created_at
		FROM season_content_themes ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("liveops list content themes: %w", err)
	}
	defer rows.Close()
	out := []ContentTheme{}
	for rows.Next() {
		var t ContentTheme
		var de, ti, la string
		if err := rows.Scan(&t.ID, &t.SeasonID, &de, &ti, &la, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("liveops list content themes (scan): %w", err)
		}
		t.DecisiveEventIDs = decodeJSONStrings(de)
		t.TitleIDs = decodeJSONStrings(ti)
		t.LandmarkNames = decodeJSONStrings(la)
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpsertContentTheme 幂等写一个母题（id 空时新建 uuid）。返回母题 id。
func (s *LiveopsService) UpsertContentTheme(ctx context.Context, theme ContentTheme) (string, error) {
	if !s.ready() {
		return "", fmt.Errorf("liveops upsert content theme: service not ready")
	}
	id := strings.TrimSpace(theme.ID)
	if id == "" {
		id = uuid.NewString()
	}
	de := encodeJSONStrings(theme.DecisiveEventIDs)
	ti := encodeJSONStrings(theme.TitleIDs)
	la := encodeJSONStrings(theme.LandmarkNames)
	query := `
		INSERT INTO season_content_themes (id, season_id, decisive_event_ids, title_ids, landmark_names, created_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET season_id = excluded.season_id, decisive_event_ids = excluded.decisive_event_ids,
			title_ids = excluded.title_ids, landmark_names = excluded.landmark_names`
	if s.dialect == dbdialect.DialectMySQL {
		query = `
			INSERT INTO season_content_themes (id, season_id, decisive_event_ids, title_ids, landmark_names, created_at)
			VALUES (?, ?, ?, ?, ?, UTC_TIMESTAMP())
			ON DUPLICATE KEY UPDATE season_id = VALUES(season_id), decisive_event_ids = VALUES(decisive_event_ids),
				title_ids = VALUES(title_ids), landmark_names = VALUES(landmark_names)`
	}
	if _, err := s.db.ExecContext(ctx, query, id, strings.TrimSpace(theme.SeasonID), de, ti, la); err != nil {
		return "", fmt.Errorf("liveops upsert content theme: %w", err)
	}
	return id, nil
}

// DeleteContentTheme 删一个母题。
func (s *LiveopsService) DeleteContentTheme(ctx context.Context, id string) error {
	if !s.ready() {
		return fmt.Errorf("liveops delete content theme: service not ready")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM season_content_themes WHERE id = ?`, strings.TrimSpace(id)); err != nil {
		return fmt.Errorf("liveops delete content theme: %w", err)
	}
	return nil
}

// SeedDefaultContentThemes 幂等播种一组默认母题骨架（修「母题库死表零播种」缺口）。
// 默认母题 season_id 留空（全局可挂任意赛季），id 用稳定前缀便于幂等与运营识别——重复调用不重复造。
// 内容仅放代表性骨架（少量决定性事件/称号/地标占位），完整 40+ 母题由 GM 后台 Upsert 续填（本函数只通电管线）。
func (s *LiveopsService) SeedDefaultContentThemes(ctx context.Context) error {
	if !s.ready() {
		return fmt.Errorf("liveops seed content themes: service not ready")
	}
	defaults := []ContentTheme{
		{ID: "theme_strife", DecisiveEventIDs: []string{"war_outbreak", "famine", "betrayal"}, TitleIDs: []string{"乱世枭雄", "流亡者"}, LandmarkNames: []string{"焦土关", "断魂崖"}},
		{ID: "theme_prosperity", DecisiveEventIDs: []string{"harvest", "wedding", "founding"}, TitleIDs: []string{"治世能臣", "桃源主人"}, LandmarkNames: []string{"丰禾镇", "听雨阁"}},
		{ID: "theme_mystery", DecisiveEventIDs: []string{"omen", "vanishing", "ancient_relic"}, TitleIDs: []string{"解谜者", "守秘人"}, LandmarkNames: []string{"雾隐谷", "无名碑林"}},
	}
	for _, t := range defaults {
		if _, err := s.UpsertContentTheme(ctx, t); err != nil {
			return fmt.Errorf("liveops seed content themes (%s): %w", t.ID, err)
		}
	}
	return nil
}

// decodeJSONStrings 把 JSON 文本数组解析为 []string（解析失败/空返回空切片，绝不 panic）。
func decodeJSONStrings(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []string{}
	}
	return out
}

// encodeJSONStrings 把 []string 序列化为 JSON 文本（nil 编为 []）。
func encodeJSONStrings(in []string) string {
	if in == nil {
		in = []string{}
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "[]"
	}
	return string(b)
}
