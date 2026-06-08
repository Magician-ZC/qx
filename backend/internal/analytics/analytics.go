// Package analytics 是产品分析埋点的最小写入层（AARRR 漏斗，设计 docs/验证实验设计.md §5.2）。
// 与游戏状态彻底解耦：append-only 写 product_events 表，调用方一律 best-effort（埋点失败绝不影响玩法）。
// 真实用户一出现，漏斗数据就自动流入；无用户时这些表为空、零副作用。
package analytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// occurredAtLayout 是 product_events.occurred_at 的写入布局（UTC、空格分隔秒级，字典序=时间序）。
// 必须与 analytics_query.go 的 queryTimeLayout 一致——否则窗口过滤 occurred_at > cutoff 会错位。
// 显式写入（而非依赖列默认值）：sqlite 默认 CURRENT_TIMESTAMP 恰好同格式，但 mysql schema 默认 ''（空串），
// 不显式写会导致 MySQL 上所有 occurred_at='' < cutoff、windowed 查询恒空（对抗评审 medium）。
const occurredAtLayout = "2006-01-02 15:04:05"

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
	EventCharacterDied     = "character_died" // 角色阵亡（留存/牵挂信号：一场死亡是别人传记里的「回到那一刻」）
	// 命运高光卡三键轻反馈（GDD §8 核心乐趣度量：惊喜命中率=surprise/total、OOC 率=ooc/total）。
	EventFateReactExpected = "fate_react_expected" // 意料之中
	EventFateReactSurprise = "fate_react_surprise" // 有点意外但合理 = 命中惊喜
	EventFateReactOoc      = "fate_react_ooc"      // 太离谱 = 疑似失格
)

// Event 是一条漏斗埋点。
type Event struct {
	Stage     Stage
	Name      string
	SessionID string
	UnitID    string
	Props     any // 序列化进 properties_json
	// 维度列（均可选，向后兼容：旧调用方不传=NULL）。供漏斗按用户/实验组/客户端版本切片。
	UserID     string // 关联账户（跨会话归因，缺失=匿名/未登录）
	ABBucket   string // A/B 实验分桶标识（如 "control"/"variant_a"）
	ClientTS   string // 客户端事件时间戳（前端原始口径，用于端到端延迟/时钟偏差核对）
	AppVersion string // 客户端应用版本（按版本切片转化/回归）
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
	// 显式写 occurred_at（双方言统一为 UTC 字典序时间串）——否则 MySQL 列默认 '' 会让 windowed 读端恒空。
	occurredAt := time.Now().UTC().Format(occurredAtLayout)
	// 维度列（user_id/ab_bucket/client_ts/app_version）由 schema agent 建；旧调用方不传=NULL（nullable 兜底）。
	if _, err := execer.ExecContext(ctx, `
		INSERT INTO product_events (id, stage, event_name, session_id, unit_id, properties_json, occurred_at, user_id, ab_bucket, client_ts, app_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), string(ev.Stage), ev.Name, nullable(ev.SessionID), nullable(ev.UnitID), string(encoded), occurredAt,
		nullable(ev.UserID), nullable(ev.ABBucket), nullable(ev.ClientTS), nullable(ev.AppVersion)); err != nil {
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
