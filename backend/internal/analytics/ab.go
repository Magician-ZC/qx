// 文件说明：A/B 实验框架（确定性分桶 + 按桶漏斗聚合，设计 docs/验证实验设计.md / SH2.3 红线 A/B）。
// 此前 product_events 虽有 ab_bucket 列却无分桶产生器、也无按桶拆分的读聚合 —— 实验「无法跑」。
// 本文件补两块纯逻辑：①AssignBucket 把 (experiment, subjectID) 确定性映射到一个变体（无需持久化、可复现）；
// ②FunnelByBucket 从 product_events 按 ab_bucket 分组出漏斗，使「设红线 vs 未设红线」「卖点 A/B/C」
// 「服从 vs 违背」等子实验能按桶拆分对比。全部确定性、零副作用、缺数据安全返回空。
package analytics

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"
)

// AssignBucket 把一个实验主体（subjectID，用 vid/user_id）确定性映射到一个变体桶。
//
// 算法：FNV-64a(experiment + "|" + subjectID) % len(variants)。
//   - 同 (experiment, subjectID) 恒落同桶 —— 无需任何持久化、跨进程/跨重启可复现；
//   - 不同 experiment 用同 subjectID 也会独立分桶（拼接 experiment 进哈希，避免跨实验同相关性）；
//   - 选 FNV-64a 与全仓「确定性随机用 FNV」约定一致（CLAUDE.md），不依赖全局 rand。
//
// 边界：variants 为空 -> 返回 ""（不参与实验，埋点 ab_bucket 落 NULL）；
// 单 variant -> 直接返回之（全员对照，便于灰度开关）。
//
// 均匀性：FNV-64a 雪崩良好，对人类可读的短 key 也能把低位散开；大样本下各桶占比趋近 1/len(variants)
// （ab_test.go 有大样本占比断言）。注意分桶**均匀**不代表样本量**相等**，仅期望相等。
func AssignBucket(experiment, subjectID string, variants []string) string {
	if len(variants) == 0 {
		return ""
	}
	if len(variants) == 1 {
		return variants[0]
	}
	h := fnv.New64a()
	// 写入恒不返回错误（hash.Hash 契约），忽略返回值。分隔符 "|" 防止
	// (exp="ab", sub="cd") 与 (exp="abc", sub="d") 这类边界拼接撞键。
	_, _ = h.Write([]byte(experiment))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(subjectID))
	idx := h.Sum64() % uint64(len(variants))
	return variants[idx]
}

// ExperimentReport 是一个实验按桶拆分的漏斗聚合（只读）。
type ExperimentReport struct {
	Experiment string `json:"experiment"` // 实验标识（=查询入参，回显便于核对）
	SinceDays  int    `json:"since_days"` // 窗口天数；<=0=全量
	// ByBucket: ab_bucket -> (event_name -> 计数)。仅含 ab_bucket 非空的埋点。
	// 桶间同事件相减/相除即得「设红线 vs 未设红线」等子实验的转化对比。
	ByBucket    map[string]map[string]int `json:"by_bucket"`
	GeneratedAt string                    `json:"generated_at"` // 生成时刻（RFC3339）
}

// FunnelByBucket 聚合最近 sinceDays 天某实验的「按 ab_bucket 分组的漏斗」。
//
// 当前 product_events 不带 experiment 列：ab_bucket 是「某实验的桶名」，实验维度由调用方约定
// （桶名本身编码实验，如 "redline_on"/"redline_off"、"sku_a"/"sku_b"/"sku_c"）。experiment 入参
// 用于回显与未来扩展（若后续加 experiment 列即可在此加 WHERE）；窗口口径与 cutoffFor 一致。
//
// 仅统计 ab_bucket 非空（IS NOT NULL AND != ''）的埋点 —— 未分桶流量不污染实验对比。
// sinceDays<=0 视为全量。缺数据安全返回空 map（已初始化）。
func FunnelByBucket(ctx context.Context, q Querier, experiment string, sinceDays int) (map[string]map[string]int, error) {
	byBucket := map[string]map[string]int{}
	if q == nil {
		return byBucket, fmt.Errorf("analytics ab funnel: nil querier")
	}
	cutoff := cutoffFor(sinceDays, time.Now())

	// SELECT ab_bucket, event_name, COUNT(*) ... WHERE ab_bucket 非空 [AND occurred_at > ?] GROUP BY 二维。
	// ab_bucket 非空过滤是写死的实验前提；窗口子句按 cutoff 是否为空动态拼。
	const selectClause = `SELECT ab_bucket, event_name, COUNT(*) FROM product_events`
	const bucketFilter = ` WHERE ab_bucket IS NOT NULL AND ab_bucket != ''`
	const groupClause = ` GROUP BY ab_bucket, event_name`

	var query string
	var args []any
	if cutoff == "" {
		query = selectClause + bucketFilter + groupClause
	} else {
		query = selectClause + bucketFilter + ` AND occurred_at > ?` + groupClause
		args = []any{cutoff}
	}

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return byBucket, fmt.Errorf("analytics ab funnel query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var bucket, event string
		var n int
		if err := rows.Scan(&bucket, &event, &n); err != nil {
			return byBucket, fmt.Errorf("analytics ab funnel scan: %w", err)
		}
		// bucketFilter 已排除 NULL/空桶，但事件名兜底成 "unknown" 防意外空串污染列。
		if event == "" {
			event = "unknown"
		}
		if byBucket[bucket] == nil {
			byBucket[bucket] = map[string]int{}
		}
		byBucket[bucket][event] += n
	}
	if err := rows.Err(); err != nil {
		return byBucket, fmt.Errorf("analytics ab funnel rows: %w", err)
	}
	return byBucket, nil
}

// ExperimentFunnel 是 FunnelByBucket 的带元数据封装：除按桶漏斗外回显 experiment/窗口/生成时刻，
// 直接可作为 /api/ops/experiment 的 JSON 响应体。
func ExperimentFunnel(ctx context.Context, q Querier, experiment string, sinceDays int) (ExperimentReport, error) {
	report := ExperimentReport{
		Experiment:  experiment,
		SinceDays:   sinceDays,
		ByBucket:    map[string]map[string]int{},
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}
	byBucket, err := FunnelByBucket(ctx, q, experiment, sinceDays)
	if err != nil {
		return report, err
	}
	report.ByBucket = byBucket
	return report, nil
}

// BucketConversion 算各桶内「from_event -> to_event」的转化率（to/from，桶内比率）。
// 复用 FunnelByBucket 的按桶计数，再对每桶做 to/from；from 计数为 0 的桶不出现在结果里
// （分母 0 无意义，避免假 0 转化误导）。便于「服从 vs 违背」桶比「inbox_opened -> decision_resolved」等。
func BucketConversion(ctx context.Context, q Querier, experiment, fromEvent, toEvent string, sinceDays int) (map[string]float64, error) {
	out := map[string]float64{}
	byBucket, err := FunnelByBucket(ctx, q, experiment, sinceDays)
	if err != nil {
		return out, err
	}
	for bucket, counts := range byBucket {
		from := counts[fromEvent]
		if from <= 0 {
			continue
		}
		out[bucket] = float64(counts[toEvent]) / float64(from)
	}
	return out, nil
}
