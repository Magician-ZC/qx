// Package analytics 是产品分析埋点的最小写入层（AARRR 漏斗，设计 docs/验证实验设计.md §5.2）。
// 与游戏状态彻底解耦：append-only 写 product_events 表，调用方一律 best-effort（埋点失败绝不影响玩法）。
// 真实用户一出现，漏斗数据就自动流入；无用户时这些表为空、零副作用。
package analytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// Stage 是 AARRR 漏斗阶段。
type Stage string

const (
	StageAcquisition Stage = "acquisition" // 获客（落地/注册）
	StageActivation  Stage = "activation"  // 激活（建角色/首个意外）
	StageRetention   Stage = "retention"   // 留存（回访/处理待决策）
	StageRevenue     Stage = "revenue"     // 营收（付费）
	StageReferral    Stage = "referral"    // 转介（分享传记卡）
)

// 常用事件名（北极星相关：D2 inbox 处理率靠 decision_pending/decision_resolved 算）。
const (
	EventSessionCreated   = "session_created"
	EventCharacterCreated = "character_created"
	EventDecisionPending  = "decision_pending"
	EventDecisionResolved = "decision_resolved"
	EventIntervention     = "player_intervention"
	EventReturnVisit      = "return_visit"
	// P1 漏斗补全：注册/开盒契约完成/收件箱打开/状态卡查看/分享/付费/合规拦截。
	EventAccountRegistered = "account_registered"
	EventCharterCompleted  = "charter_completed"
	EventInboxOpened       = "inbox_opened"
	EventStatusCardViewed  = "status_card_viewed"
	EventShareInitiated    = "share_initiated"
	EventPurchase          = "purchase"
	EventComplianceBlocked = "compliance_blocked"
)

// Event 是一条漏斗埋点。
type Event struct {
	Stage     Stage
	Name      string
	SessionID string
	UnitID    string
	Props     any // 序列化进 properties_json
}

// Execer 是写入所需的最小依赖（*sql.DB 或 *sql.Tx 均满足）。
type Execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// Emit 追加一条漏斗埋点。调用方应 best-effort 忽略错误（埋点不能拖垮玩法）。
func Emit(ctx context.Context, execer Execer, ev Event) error {
	if execer == nil {
		return fmt.Errorf("analytics emit: nil execer")
	}
	if ev.Stage == "" || ev.Name == "" {
		return fmt.Errorf("analytics emit: empty stage or name")
	}
	props := ev.Props
	if props == nil {
		props = map[string]any{}
	}
	encoded, err := json.Marshal(props)
	if err != nil {
		return fmt.Errorf("analytics marshal props: %w", err)
	}
	if _, err := execer.ExecContext(ctx, `
		INSERT INTO product_events (id, stage, event_name, session_id, unit_id, properties_json)
		VALUES (?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), string(ev.Stage), ev.Name, nullable(ev.SessionID), nullable(ev.UnitID), string(encoded)); err != nil {
		return fmt.Errorf("analytics insert: %w", err)
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
