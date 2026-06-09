package session

// 文件说明：translation_templates（命运 beat 文案翻译矩阵）的运营 CRUD。
// 表已是 DB 权威 + 启动 SeedTranslationTemplates 幂等播种 + 运行时 loadTranslationTemplate 读表（带缓存），
// 但此前补/改模板只能改内置矩阵代码 + 重启 seed。本文件给 GM 后台提供运营态增删改：写后 invalidateTranslationCache()
// 让新文案即时生效（无需重启）。updated_at 是审计时戳（非模拟逻辑），用 time.Now，与 SeedTranslationTemplates 同口径。
//
// 注意：启动 SeedTranslationTemplates 会用内置矩阵覆盖同 (reason_code, anchor_kind) 行——运营增补建议用内置矩阵未覆盖的
// reason_code/anchor 组合（force_pending 专属/新码），避免重启被基线 upsert 覆盖（见 translation_seed.go:239 注释）。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"qunxiang/backend/internal/storage/dbdialect"
)

// TranslationTemplateRow 是一条翻译模板的运营视图（GM 后台增删改用）。
type TranslationTemplateRow struct {
	ID                string `json:"id"`
	ReasonCode        string `json:"reason_code"`
	AnchorKind        string `json:"anchor_kind"`
	NarrativeTemplate string `json:"narrative_template"`
	ForcePending      bool   `json:"force_pending"`
	Priority          int    `json:"priority"`
	UpdatedAt         string `json:"updated_at"`
}

// ListTranslationTemplates 列出全部翻译模板（按 reason_code, anchor_kind 稳定排序）。
func ListTranslationTemplates(ctx context.Context, db *sql.DB) ([]TranslationTemplateRow, error) {
	if db == nil {
		return nil, fmt.Errorf("list translation templates: nil db")
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, reason_code, anchor_kind, narrative_template, force_pending, priority, updated_at
		FROM translation_templates ORDER BY reason_code, anchor_kind`)
	if err != nil {
		return nil, fmt.Errorf("list translation templates: %w", err)
	}
	defer rows.Close()
	out := []TranslationTemplateRow{}
	for rows.Next() {
		var r TranslationTemplateRow
		var forcePending int
		if err := rows.Scan(&r.ID, &r.ReasonCode, &r.AnchorKind, &r.NarrativeTemplate, &forcePending, &r.Priority, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("list translation templates (scan): %w", err)
		}
		r.ForcePending = forcePending != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertTranslationTemplate 运营态写一条模板（按 (reason_code, anchor_kind) upsert），写后清缓存即时生效。
func UpsertTranslationTemplate(ctx context.Context, db *sql.DB, row TranslationTemplateRow) error {
	if db == nil {
		return fmt.Errorf("upsert translation template: nil db")
	}
	reasonCode := strings.TrimSpace(row.ReasonCode)
	anchorKind := strings.TrimSpace(row.AnchorKind)
	if reasonCode == "" || anchorKind == "" {
		return fmt.Errorf("upsert translation template: empty reason_code or anchor_kind")
	}
	forcePending := 0
	if row.ForcePending {
		forcePending = 1
	}
	// id 用稳定派生键（与 SeedTranslationTemplates 同口径），确定性、与 UNIQUE(reason_code, anchor_kind) 一致。
	id := "tt_" + reasonCode + "|" + anchorKind
	now := time.Now().UTC().Format(time.RFC3339)
	upsert := `
		INSERT INTO translation_templates (id, reason_code, anchor_kind, narrative_template, force_pending, priority, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(reason_code, anchor_kind) DO UPDATE SET
			narrative_template = excluded.narrative_template,
			force_pending = excluded.force_pending,
			priority = excluded.priority,
			updated_at = excluded.updated_at`
	if dbdialect.IsMySQL(db) {
		upsert = `
		INSERT INTO translation_templates (id, reason_code, anchor_kind, narrative_template, force_pending, priority, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			narrative_template = VALUES(narrative_template),
			force_pending = VALUES(force_pending),
			priority = VALUES(priority),
			updated_at = VALUES(updated_at)`
	}
	if _, err := db.ExecContext(ctx, upsert, id, reasonCode, anchorKind, row.NarrativeTemplate, forcePending, row.Priority, now); err != nil {
		return fmt.Errorf("upsert translation template: %w", err)
	}
	invalidateTranslationCache()
	return nil
}

// DeleteTranslationTemplate 删一条模板（按 reason_code+anchor_kind），删后清缓存。
// 注意：若该组合在内置矩阵里，重启 SeedTranslationTemplates 会复建——运营删除仅对运营增补的非基线行持久。
func DeleteTranslationTemplate(ctx context.Context, db *sql.DB, reasonCode, anchorKind string) error {
	if db == nil {
		return fmt.Errorf("delete translation template: nil db")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM translation_templates WHERE reason_code = ? AND anchor_kind = ?`,
		strings.TrimSpace(reasonCode), strings.TrimSpace(anchorKind)); err != nil {
		return fmt.Errorf("delete translation template: %w", err)
	}
	invalidateTranslationCache()
	return nil
}
