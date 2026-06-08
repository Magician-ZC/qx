// 文件说明：A/B 实验框架测试（确定性分桶 + 按桶漏斗聚合）。
// 覆盖：AssignBucket 确定性/边界/均匀性（大样本各桶占比接近期望）；
// FunnelByBucket 用临时 sqlite 插带 ab_bucket 的 product_events 后断言分组计数（含非空过滤、窗口口径）；
// BucketConversion 桶内 from->to 转化率（分母 0 不出现）。
package analytics

import (
	"context"
	"fmt"
	"math"
	"testing"
)

// TestAssignBucketDeterministic 同 (experiment, subjectID) 多次调用恒落同桶（可复现，无需持久化）。
func TestAssignBucketDeterministic(t *testing.T) {
	variants := []string{"control", "variant_a", "variant_b"}
	first := AssignBucket("redline_ab", "vid-123", variants)
	for i := 0; i < 100; i++ {
		if got := AssignBucket("redline_ab", "vid-123", variants); got != first {
			t.Fatalf("分桶不确定：第 %d 次得到 %q，首次 %q", i, got, first)
		}
	}
	// 落桶必须是给定变体之一。
	if first != "control" && first != "variant_a" && first != "variant_b" {
		t.Fatalf("落桶 %q 不在变体集合内", first)
	}
}

// TestAssignBucketExperimentIsolation 同 subjectID 在不同实验下独立分桶（拼 experiment 进哈希）。
// 不要求一定不同（可能巧合相同），但要求至少存在一对实验对同一 subject 给出不同桶，
// 证明 experiment 确实进了哈希（否则所有实验对同 subject 永远同桶）。
func TestAssignBucketExperimentIsolation(t *testing.T) {
	variants := []string{"a", "b", "c", "d"}
	const subject = "vid-isolation"
	base := AssignBucket("exp_0", subject, variants)
	diverged := false
	for i := 1; i < 50; i++ {
		if AssignBucket(fmt.Sprintf("exp_%d", i), subject, variants) != base {
			diverged = true
			break
		}
	}
	if !diverged {
		t.Fatalf("50 个实验对同一 subject 全部同桶 —— experiment 疑似未进哈希")
	}
}

// TestAssignBucketBoundaries 边界：空 variants 返回 ""；单 variant 直接返回之。
func TestAssignBucketBoundaries(t *testing.T) {
	if got := AssignBucket("exp", "vid", nil); got != "" {
		t.Fatalf("空 variants 应返回空串，得到 %q", got)
	}
	if got := AssignBucket("exp", "vid", []string{}); got != "" {
		t.Fatalf("空切片 variants 应返回空串，得到 %q", got)
	}
	if got := AssignBucket("exp", "vid", []string{"only"}); got != "only" {
		t.Fatalf("单 variant 应直接返回，得到 %q", got)
	}
}

// TestAssignBucketUniformity 大样本下各桶占比接近期望 1/len（均匀性）。
// 1 万个不同 subject 分 4 桶，每桶占比应在 25% ±5pp 内（FNV-64a 雪崩足够）。
func TestAssignBucketUniformity(t *testing.T) {
	variants := []string{"v0", "v1", "v2", "v3"}
	const n = 10000
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		b := AssignBucket("uniformity", fmt.Sprintf("subject-%d", i), variants)
		counts[b]++
	}
	if len(counts) != len(variants) {
		t.Fatalf("应覆盖全部 %d 桶，实际只命中 %d 桶: %v", len(variants), len(counts), counts)
	}
	for _, v := range variants {
		ratio := float64(counts[v]) / float64(n)
		if math.Abs(ratio-0.25) > 0.05 {
			t.Fatalf("桶 %q 占比 %.3f 偏离期望 0.25 超 5pp（counts=%v）", v, ratio, counts)
		}
	}
}

// TestFunnelByBucket 临时 sqlite 插带 ab_bucket 的埋点后断言按桶分组计数。
func TestFunnelByBucket(t *testing.T) {
	ctx, db := newDB(t)

	// redline_on 桶：2 inbox_opened + 1 decision_resolved。
	mustEmit(t, ctx, db, Event{Stage: StageRetention, Name: EventInboxOpened, SessionID: "s1", ABBucket: "redline_on"})
	mustEmit(t, ctx, db, Event{Stage: StageRetention, Name: EventInboxOpened, SessionID: "s2", ABBucket: "redline_on"})
	mustEmit(t, ctx, db, Event{Stage: StageRetention, Name: EventDecisionResolved, SessionID: "s1", ABBucket: "redline_on"})
	// redline_off 桶：1 inbox_opened。
	mustEmit(t, ctx, db, Event{Stage: StageRetention, Name: EventInboxOpened, SessionID: "s3", ABBucket: "redline_off"})
	// 未分桶（ab_bucket 空）：不应进任何桶。
	mustEmit(t, ctx, db, Event{Stage: StageRetention, Name: EventInboxOpened, SessionID: "s4"})

	byBucket, err := FunnelByBucket(ctx, db, "redline_ab", 0)
	if err != nil {
		t.Fatalf("FunnelByBucket 失败: %v", err)
	}
	if len(byBucket) != 2 {
		t.Fatalf("应有 2 个非空桶，得到 %d: %v", len(byBucket), byBucket)
	}
	if got := byBucket["redline_on"][EventInboxOpened]; got != 2 {
		t.Fatalf("redline_on 的 inbox_opened 应为 2，得到 %d", got)
	}
	if got := byBucket["redline_on"][EventDecisionResolved]; got != 1 {
		t.Fatalf("redline_on 的 decision_resolved 应为 1，得到 %d", got)
	}
	if got := byBucket["redline_off"][EventInboxOpened]; got != 1 {
		t.Fatalf("redline_off 的 inbox_opened 应为 1，得到 %d", got)
	}
	// 未分桶埋点不得出现在任何桶里。
	if _, ok := byBucket[""]; ok {
		t.Fatalf("空桶不应作为分组键出现: %v", byBucket)
	}
}

// TestFunnelByBucketEmpty 无数据安全返回空 map（不报错、不 panic）。
func TestFunnelByBucketEmpty(t *testing.T) {
	ctx, db := newDB(t)
	byBucket, err := FunnelByBucket(ctx, db, "exp", 7)
	if err != nil {
		t.Fatalf("空库 FunnelByBucket 不应报错: %v", err)
	}
	if len(byBucket) != 0 {
		t.Fatalf("空库应返回空 map，得到 %v", byBucket)
	}
}

// TestFunnelByBucketNilQuerier nil querier 报错且返回已初始化的空 map。
func TestFunnelByBucketNilQuerier(t *testing.T) {
	byBucket, err := FunnelByBucket(context.Background(), nil, "exp", 0)
	if err == nil {
		t.Fatalf("nil querier 应报错")
	}
	if byBucket == nil {
		t.Fatalf("即便报错也应返回已初始化 map（非 nil）")
	}
}

// TestExperimentFunnel 带元数据封装回显 experiment/窗口并携带按桶漏斗。
func TestExperimentFunnel(t *testing.T) {
	ctx, db := newDB(t)
	mustEmit(t, ctx, db, Event{Stage: StageRevenue, Name: EventPurchase, SessionID: "s1", ABBucket: "sku_a"})
	report, err := ExperimentFunnel(ctx, db, "pricing_ab", 0)
	if err != nil {
		t.Fatalf("ExperimentFunnel 失败: %v", err)
	}
	if report.Experiment != "pricing_ab" {
		t.Fatalf("experiment 未回显，得到 %q", report.Experiment)
	}
	if report.GeneratedAt == "" {
		t.Fatalf("GeneratedAt 应非空")
	}
	if got := report.ByBucket["sku_a"][EventPurchase]; got != 1 {
		t.Fatalf("sku_a 的 purchase 应为 1，得到 %d", got)
	}
}

// TestBucketConversion 桶内 from->to 转化率；from 计数为 0 的桶不出现。
func TestBucketConversion(t *testing.T) {
	ctx, db := newDB(t)
	// obey 桶：4 inbox_opened，2 decision_resolved -> 转化 0.5。
	for i := 0; i < 4; i++ {
		mustEmit(t, ctx, db, Event{Stage: StageRetention, Name: EventInboxOpened, SessionID: fmt.Sprintf("o%d", i), ABBucket: "obey"})
	}
	for i := 0; i < 2; i++ {
		mustEmit(t, ctx, db, Event{Stage: StageRetention, Name: EventDecisionResolved, SessionID: fmt.Sprintf("o%d", i), ABBucket: "obey"})
	}
	// defy 桶：只有 decision_resolved，无 inbox_opened -> from=0，不应出现在结果。
	mustEmit(t, ctx, db, Event{Stage: StageRetention, Name: EventDecisionResolved, SessionID: "d0", ABBucket: "defy"})

	conv, err := BucketConversion(ctx, db, "obedience_ab", EventInboxOpened, EventDecisionResolved, 0)
	if err != nil {
		t.Fatalf("BucketConversion 失败: %v", err)
	}
	if got, ok := conv["obey"]; !ok || math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("obey 桶转化率应为 0.5，得到 %v (ok=%v)", got, ok)
	}
	if _, ok := conv["defy"]; ok {
		t.Fatalf("defy 桶 from=0 不应出现在转化结果: %v", conv)
	}
}

// mustEmit 是测试用 Emit 包装：失败即 Fatalf。
func mustEmit(t *testing.T, ctx context.Context, db Execer, ev Event) {
	t.Helper()
	if err := Emit(ctx, db, ev); err != nil {
		t.Fatalf("emit 失败: %v", err)
	}
}
