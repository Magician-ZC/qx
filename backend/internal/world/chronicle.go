package world

// 文件说明：世界编年史存储原语（分区大世界阶段4 §7）——记录整个世界的纪元大事，
// 独立于单角色编年史（chronicle_entries 是「她的人生」，本表 world_chronicle 是「她所处时代的洪流」）。
//
// 本文件只提供**纯存储 + 纪元/容量逻辑**（写一条、按世界倒序读、纪元推进判定、容量裁剪），
// 不含任何业务入史规则——入史触发点（boss 讨平 / 区域解锁 / 传奇诞生陨落 / 阵营之战）在 session 层
// （session/world_chronicle.go）按 §7.2 调本文件的 RecordWorldChronicle。决策用 LLM、结算用代码：
// 本层全代码、确定性、可测；与 world 包其余原语（世界注册/时钟/归属）同处「世界根」。
//
// 双驱动方言安全：写入用 ON CONFLICT(SQLite)/INSERT IGNORE(MySQL) 幂等。created_at 用 RFC3339Nano-ish 定宽布局
// （字典序即时间序），与 chronicle_entries 同口径，倒序读 world_tick DESC, created_at DESC。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// WorldChronicleEntry 是一条世界级编年史条目（与 world_chronicle 表逐列对应）。
type WorldChronicleEntry struct {
	ID          string   `json:"id"`
	WorldID     string   `json:"world_id"`
	WorldTick   int      `json:"world_tick"`
	Era         string   `json:"era"`          // 纪元（按重大事件分段，如「开拓纪元」「三阵营之战」），叙事用
	Category    string   `json:"category"`     // boss_slain/zone_unlocked/hero_born/hero_died/faction_war/cataclysm
	TitleZH     string   `json:"title_zh"`     // 「秩序之军攻陷晨曦城」
	NarrativeZH string   `json:"narrative_zh"` // 一段史官口吻叙事（规则文案 + 可选 LLM 润色）
	ActorRefs   []string `json:"actor_refs,omitempty"`
	Importance  int      `json:"importance"` // 重要度 [1,10]，容量裁剪时保留高重要度（仿角色记忆衰减）
	CreatedAt   string   `json:"created_at,omitempty"`
}

// 世界编年史的容量与纪元参数（确定性、纯代码）。
const (
	// MaxWorldChronicleList 单次读取上限，避免世界史面板一次拉爆。
	MaxWorldChronicleList = 500
	// worldChronicleCapacity 单个世界保留的编年史条目上限——超过则按「先低重要度、再旧」裁剪，防无限膨胀（§11 风险）。
	worldChronicleCapacity = 2000
	// worldEraEventThreshold 是「累计重大事件达此阈值即开新纪元」（§7.2 纪元更替）。重大=Importance≥worldEraMajorImportance。
	worldEraEventThreshold = 12
	// worldEraMajorImportance 是计入纪元推进的「重大事件」重要度门槛。
	worldEraMajorImportance = 7
)

// worldChronicleTimeLayout 是 created_at 的定宽布局（字典序=时间序，双驱动一致，与 chronicle_entries 同口径）。
const worldChronicleTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// formatWorldChronicleTime 把时刻格式化为定宽布局；零值回退到 now（与 chronicle_entries 的 formatTraceTime 同语义）。
func formatWorldChronicleTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now().UTC()
	}
	return t.UTC().Format(worldChronicleTimeLayout)
}

// RecordWorldChronicle 把一条世界编年史条目落 world_chronicle 表（append-only、双驱动方言安全），返回写入条目 id。
// best-effort 语义由调用方决定：本函数只在 DB/参数无效时返错；id 为空自动生成、ActorRefs 序列化为 JSON、Importance 夹 [1,10]。
// worldID 为空直接返错（世界史必须挂在某个世界上）——session 层在 WorldID 非空时才调（旧单图档无世界、不写世界史）。
func RecordWorldChronicle(ctx context.Context, db DB, mysql bool, entry WorldChronicleEntry) (string, error) {
	if db == nil {
		return "", fmt.Errorf("record world chronicle: nil db")
	}
	if strings.TrimSpace(entry.WorldID) == "" {
		return "", fmt.Errorf("record world chronicle: empty world id")
	}
	if entry.ID == "" {
		entry.ID = uuid.NewString()
	}
	if entry.Importance < 1 {
		entry.Importance = 1
	}
	if entry.Importance > 10 {
		entry.Importance = 10
	}
	if entry.CreatedAt == "" {
		entry.CreatedAt = formatWorldChronicleTime(time.Time{})
	}
	refs := entry.ActorRefs
	if refs == nil {
		refs = []string{}
	}
	refsJSON, err := json.Marshal(refs)
	if err != nil {
		return "", fmt.Errorf("marshal actor refs: %w", err)
	}
	query := `INSERT INTO world_chronicle
		(id, world_id, world_tick, era, category, title_zh, narrative_zh, actor_refs, importance, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`
	if mysql {
		query = `INSERT IGNORE INTO world_chronicle
			(id, world_id, world_tick, era, category, title_zh, narrative_zh, actor_refs, importance, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	}
	if _, err := db.ExecContext(ctx, query,
		entry.ID, entry.WorldID, entry.WorldTick, entry.Era, entry.Category,
		entry.TitleZH, entry.NarrativeZH, string(refsJSON), entry.Importance, entry.CreatedAt,
	); err != nil {
		return "", fmt.Errorf("insert world chronicle %s: %w", entry.ID, err)
	}
	return entry.ID, nil
}

// ListWorldChronicle 按世界倒序读回编年史条目（world_tick DESC, created_at DESC）——纪元/tick 由近及远。
// limit<=0 或越界归默认上限。actor_refs 反序列化失败的行降级为空切片（不阻断整页）。
func ListWorldChronicle(ctx context.Context, db DB, worldID string, limit int) ([]WorldChronicleEntry, error) {
	if db == nil {
		return nil, fmt.Errorf("list world chronicle: nil db")
	}
	if strings.TrimSpace(worldID) == "" {
		return nil, fmt.Errorf("list world chronicle: empty world id")
	}
	if limit <= 0 || limit > MaxWorldChronicleList {
		limit = MaxWorldChronicleList
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, world_id, world_tick, era, category, title_zh, narrative_zh, actor_refs, importance, created_at
		FROM world_chronicle WHERE world_id = ?
		ORDER BY world_tick DESC, created_at DESC, id DESC LIMIT ?`, worldID, limit)
	if err != nil {
		return nil, fmt.Errorf("query world chronicle: %w", err)
	}
	defer rows.Close()
	return scanWorldChronicleRows(rows)
}

// scanWorldChronicleRows 把查询结果扫成 WorldChronicleEntry 切片（actor_refs 从 JSON 解回，失败降级空切片）。
func scanWorldChronicleRows(rows *sql.Rows) ([]WorldChronicleEntry, error) {
	out := make([]WorldChronicleEntry, 0)
	for rows.Next() {
		var (
			entry    WorldChronicleEntry
			refsJSON sql.NullString
		)
		if err := rows.Scan(
			&entry.ID, &entry.WorldID, &entry.WorldTick, &entry.Era, &entry.Category,
			&entry.TitleZH, &entry.NarrativeZH, &refsJSON, &entry.Importance, &entry.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan world chronicle entry: %w", err)
		}
		if refsJSON.Valid && refsJSON.String != "" {
			var refs []string
			if err := json.Unmarshal([]byte(refsJSON.String), &refs); err == nil {
				entry.ActorRefs = refs
			}
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// CountMajorWorldChronicle 统计某世界**当前纪元**内的重大事件数（Importance≥worldEraMajorImportance），
// 供纪元推进判定（§7.2：累计重大事件达阈值即开新纪元）。era 为空（旧库/首纪元）时统计 era=” 的条目。
func CountMajorWorldChronicle(ctx context.Context, db DB, worldID, era string) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("count major world chronicle: nil db")
	}
	if strings.TrimSpace(worldID) == "" {
		return 0, fmt.Errorf("count major world chronicle: empty world id")
	}
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM world_chronicle
		WHERE world_id = ? AND era = ? AND importance >= ?`,
		worldID, era, worldEraMajorImportance,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count major world chronicle: %w", err)
	}
	return n, nil
}

// EraShouldAdvance 判定累计重大事件计数是否够开新纪元（纯函数、确定性、可测）。
func EraShouldAdvance(majorCount int) bool {
	return majorCount >= worldEraEventThreshold
}

// TrimWorldChronicle 把某世界的编年史裁剪到 worldChronicleCapacity 以内（防无限膨胀，§11 风险）：
// 保留口径=「先低重要度、再旧」（与读序 world_tick DESC 相反——删的是低重要度且早的）。返回删除条数。
// best-effort：调用方在写入后低频触发；删不动（DB 错）只返错、不影响已写入条目。
func TrimWorldChronicle(ctx context.Context, db DB, worldID string) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("trim world chronicle: nil db")
	}
	if strings.TrimSpace(worldID) == "" {
		return 0, fmt.Errorf("trim world chronicle: empty world id")
	}
	var total int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM world_chronicle WHERE world_id = ?`, worldID,
	).Scan(&total); err != nil {
		return 0, fmt.Errorf("trim world chronicle count: %w", err)
	}
	if total <= worldChronicleCapacity {
		return 0, nil
	}
	// 删除「保留高重要度新条目」之外的溢出条：按 importance DESC, world_tick DESC, created_at DESC 排前 capacity 条保留，
	// 其余删除。用子查询取「不在保留集合里」的 id（双驱动通用语法，无 ON CONFLICT/方言差异）。
	res, err := db.ExecContext(ctx, `
		DELETE FROM world_chronicle
		WHERE world_id = ? AND id NOT IN (
			SELECT id FROM world_chronicle WHERE world_id = ?
			ORDER BY importance DESC, world_tick DESC, created_at DESC, id DESC
			LIMIT ?
		)`, worldID, worldID, worldChronicleCapacity)
	if err != nil {
		return 0, fmt.Errorf("trim world chronicle delete: %w", err)
	}
	deleted, _ := res.RowsAffected()
	return int(deleted), nil
}
