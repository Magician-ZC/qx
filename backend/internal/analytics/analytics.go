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
// 显式写入（而非依赖列默认值）：sqlite 默认 CURRENT_TIMESTAMP 恰好同格式，但 mysql schema 默认 ”（空串），
// 不显式写会导致 MySQL 上所有 occurred_at=” < cutoff、windowed 查询恒空（对抗评审 medium）。
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
	// SeasonID 是赛季维度（live-ops）。product_events 表无独立 season_id 列（schema 稳定），
	// 故 SeasonID 非空时由 Emit 注入 properties_json 顶层 "season_id" 键——
	// 读端（analytics_query.NorthStarBySeason）用方言安全的 properties_json LIKE 过滤即可按赛季切片。
	SeasonID string
	// ActorID 是「牵挂回访」的角色维度（attachment 回访计数的数据源）。同样无独立列，非空时注入 properties_json 顶层 "actor_id"。
	ActorID string
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
	// season_id / actor_id 没有独立列（保持 product_events schema 稳定），故注入 properties_json 顶层。
	// 仅在非空时注入，避免污染无关埋点；读端按 LIKE '%"season_id":"X"%' 切片（json.Marshal map 键有序，文本稳定）。
	props := mergeDimensionProps(ev.Props, ev.SeasonID, ev.ActorID)
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

// seasonPropKey / actorPropKey 是注入 properties_json 的赛季/角色维度键名。
// 读端（analytics_query）按这两个键的 LIKE 文本片段切片，必须与此处保持一致。
const (
	seasonPropKey = "season_id"
	actorPropKey  = "actor_id"
)

// mergeDimensionProps 把 seasonID/actorID（非空时）注入 props 顶层，返回可序列化的 map。
// 不修改调用方传入的 Props（拷贝出新 map），避免就地改写其结构体引用。
func mergeDimensionProps(props any, seasonID, actorID string) any {
	if seasonID == "" && actorID == "" {
		if props == nil {
			return map[string]any{}
		}
		return props
	}
	merged := map[string]any{}
	// 尽量保留原 props 的键：仅当原 props 本身是 map[string]any 时合并；否则原样塞进 "props" 子键，保证不丢数据。
	switch p := props.(type) {
	case nil:
		// 无原 props，merged 起步即空。
	case map[string]any:
		for k, v := range p {
			merged[k] = v
		}
	default:
		merged["props"] = p
	}
	if seasonID != "" {
		merged[seasonPropKey] = seasonID
	}
	if actorID != "" {
		merged[actorPropKey] = actorID
	}
	return merged
}

// EmitReturnVisit 发射一条「牵挂回访」埋点：玩家专程回来看某个角色（actorID）。
// 这是 attachment 牵挂四维之一「回访」的唯一权威数据源——session.returnVisitsForActor 据此 COUNT。
// actorID 必填（回访必须绑定到具体角色才有意义）；sessionID/userID 可选（归因用）。best-effort，调用方忽略错误。
func EmitReturnVisit(ctx context.Context, execer Execer, actorID, sessionID, userID string) error {
	if actorID == "" {
		return fmt.Errorf("analytics emit return visit: empty actor_id")
	}
	return Emit(ctx, execer, Event{
		Stage:     StageRetention,
		Name:      EventReturnVisit,
		SessionID: sessionID,
		UserID:    userID,
		UnitID:    actorID,
		ActorID:   actorID,
	})
}
