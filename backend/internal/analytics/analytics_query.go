// 文件说明：产品分析读端（只读聚合，打通 product_events 的读闭环）。
// 写端（analytics.go::Emit）此前 write-only，漏斗/北极星不可观测；本文件补 FunnelCounts/NorthStar 两个最小聚合。
// 与游戏状态解耦：仅读 product_events，无副作用；缺数据安全返回 0（运营查询低频，O(N) 扫窗口可接受）。
package analytics

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Querier 是读聚合所需的最小依赖（*sql.DB 或 *sql.Tx 均满足）。
type Querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// queryTimeLayout 是窗口过滤的方言安全布局。
// product_events.occurred_at 由列默认值 CURRENT_TIMESTAMP 写入（SQLite 产出 "YYYY-MM-DD HH:MM:SS" UTC，
// 字典序=时间序）；与 cost_dashboard.go 同口径——cutoff 必须用**同列写入布局**才能正确 > 比较。
// 注意：这里**不能**复用 session.traceTimeLayout（'T' 分隔 + 纳秒），那是 llm_interactions 的口径，
// 与本表的空格分隔秒级 CURRENT_TIMESTAMP 错位会导致窗口比较失真。
const queryTimeLayout = "2006-01-02 15:04:05"

// cutoffFor 计算窗口下界字符串；sinceDays<=0 返回空串（全量，不设下界）。now 为注入时钟（测试用）。
func cutoffFor(sinceDays int, now time.Time) string {
	if sinceDays <= 0 {
		return ""
	}
	return now.UTC().AddDate(0, 0, -sinceDays).Format(queryTimeLayout)
}

// FunnelReport 是 AARRR 漏斗的窗口聚合（只读）。
type FunnelReport struct {
	SinceDays        int            `json:"since_days"`        // 窗口天数；<=0=全量
	ByEvent          map[string]int `json:"by_event"`          // 事件名 -> 计数
	ByStage          map[string]int `json:"by_stage"`          // 漏斗阶段 -> 计数
	DistinctSessions int            `json:"distinct_sessions"` // 窗口内去重 session 数
	GeneratedAt      string         `json:"generated_at"`      // 生成时刻（RFC3339）
}

// FunnelCounts 聚合最近 sinceDays 天 product_events 的按事件/阶段计数 + 去重会话数。
// sinceDays<=0 视为全量。缺数据安全返回零值（map 已初始化，计数为 0）。
func FunnelCounts(ctx context.Context, q Querier, sinceDays int) (FunnelReport, error) {
	report := FunnelReport{
		SinceDays:   sinceDays,
		ByEvent:     map[string]int{},
		ByStage:     map[string]int{},
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if q == nil {
		return report, fmt.Errorf("analytics funnel: nil querier")
	}
	cutoff := cutoffFor(sinceDays, time.Now())

	// 按 event_name 计数（驱动漏斗各事件量级）。
	if err := scanCounts(ctx, q,
		`SELECT event_name, COUNT(*) FROM product_events`,
		` WHERE occurred_at > ?`,
		` GROUP BY event_name`,
		cutoff, report.ByEvent); err != nil {
		return report, fmt.Errorf("analytics funnel by_event: %w", err)
	}
	// 按 stage 计数（驱动 AARRR 各阶段量级）。
	if err := scanCounts(ctx, q,
		`SELECT stage, COUNT(*) FROM product_events`,
		` WHERE occurred_at > ?`,
		` GROUP BY stage`,
		cutoff, report.ByStage); err != nil {
		return report, fmt.Errorf("analytics funnel by_stage: %w", err)
	}
	// 去重 session（窗口内 MAU 代理；NULL session_id 不计）。
	distinct, err := scanDistinctSessions(ctx, q, cutoff)
	if err != nil {
		return report, fmt.Errorf("analytics funnel distinct_sessions: %w", err)
	}
	report.DistinctSessions = distinct
	return report, nil
}

// NorthStarReport 是北极星指标的窗口聚合（只读）。
type NorthStarReport struct {
	SinceDays         int     `json:"since_days"`
	SessionsCreated   int     `json:"sessions_created"`    // session_created
	CharactersCreated int     `json:"characters_created"`  // character_created
	DecisionPending   int     `json:"decision_pending"`    // decision_pending（命运待决）
	DecisionResolved  int     `json:"decision_resolved"`   // decision_resolved（已处理）
	InboxProcessRate  float64 `json:"inbox_process_rate"`  // resolved/pending；分母 0 -> 0
	ShareInitiated    int     `json:"share_initiated"`     // share_initiated（转介）
	Purchases         int     `json:"purchases"`           // purchase（营收）
	ReturnVisits      int     `json:"return_visits"`       // return_visit（留存）
	GeneratedAt       string  `json:"generated_at"`
}

// NorthStar 按事件名聚合北极星指标：收件箱处理率（decision_resolved/decision_pending）、分享、付费、回访。
// sinceDays<=0 视为全量。缺数据安全返回 0（处理率分母 0 -> 0）。
func NorthStar(ctx context.Context, q Querier, sinceDays int) (NorthStarReport, error) {
	report := NorthStarReport{
		SinceDays:   sinceDays,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if q == nil {
		return report, fmt.Errorf("analytics north_star: nil querier")
	}
	cutoff := cutoffFor(sinceDays, time.Now())

	// 单次扫窗口按 event_name 计数，再映射到各北极星字段（复用漏斗的 by_event 口径）。
	counts := map[string]int{}
	if err := scanCounts(ctx, q,
		`SELECT event_name, COUNT(*) FROM product_events`,
		` WHERE occurred_at > ?`,
		` GROUP BY event_name`,
		cutoff, counts); err != nil {
		return report, fmt.Errorf("analytics north_star counts: %w", err)
	}
	report.SessionsCreated = counts[EventSessionCreated]
	report.CharactersCreated = counts[EventCharacterCreated]
	report.DecisionPending = counts[EventDecisionPending]
	report.DecisionResolved = counts[EventDecisionResolved]
	report.ShareInitiated = counts[EventShareInitiated]
	report.Purchases = counts[EventPurchase]
	report.ReturnVisits = counts[EventReturnVisit]
	if report.DecisionPending > 0 {
		report.InboxProcessRate = float64(report.DecisionResolved) / float64(report.DecisionPending)
	}
	return report, nil
}

// scanCounts 执行「SELECT key, COUNT(*) ... [WHERE occurred_at > cutoff] GROUP BY key」并把结果累加进 dst。
// cutoff 为空串时省略窗口子句（全量）。? 占位双驱动通用（SQLite/MySQL）。
func scanCounts(ctx context.Context, q Querier, selectClause, whereClause, groupClause, cutoff string, dst map[string]int) error {
	var rows *sql.Rows
	var err error
	if cutoff == "" {
		rows, err = q.QueryContext(ctx, selectClause+groupClause)
	} else {
		rows, err = q.QueryContext(ctx, selectClause+whereClause+groupClause, cutoff)
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key sql.NullString
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return err
		}
		k := key.String
		if !key.Valid || k == "" {
			k = "unknown"
		}
		dst[k] += n
	}
	return rows.Err()
}

// scanDistinctSessions 计窗口内去重 session 数（NULL/空 session_id 不计）。
func scanDistinctSessions(ctx context.Context, q Querier, cutoff string) (int, error) {
	var rows *sql.Rows
	var err error
	if cutoff == "" {
		rows, err = q.QueryContext(ctx, `SELECT COUNT(DISTINCT session_id) FROM product_events WHERE session_id IS NOT NULL`)
	} else {
		rows, err = q.QueryContext(ctx, `SELECT COUNT(DISTINCT session_id) FROM product_events WHERE session_id IS NOT NULL AND occurred_at > ?`, cutoff)
	}
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n int
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return 0, err
		}
	}
	return n, rows.Err()
}
