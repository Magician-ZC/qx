package session

// 文件说明：决策轨迹旁路表影子双写测试（对真实 SQLite）：写入幂等、可读回、append 返回轨迹、空 ID 不写。

import (
	"context"
	"testing"
)

func TestShadowDecisionTrace(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()

	tr := DecisionTrace{ID: "t1", UnitID: "u1", NextAction: "她去打水", Reasoning: "口渴"}
	service.shadowDecisionTrace(ctx, "s1", tr)
	service.shadowDecisionTrace(ctx, "s1", tr) // 幂等：再写一次不重复

	list, err := service.ListDecisionTraces(ctx, "s1", 10)
	if err != nil {
		t.Fatalf("读旁路表失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("幂等写入应只有 1 条，得到 %d", len(list))
	}
	if list[0].ID != "t1" || list[0].NextAction != "她去打水" {
		t.Fatalf("轨迹应可完整读回：%+v", list[0])
	}

	// 空 ID 不写。
	service.shadowDecisionTrace(ctx, "s1", DecisionTrace{})
	if again, _ := service.ListDecisionTraces(ctx, "s1", 10); len(again) != 1 {
		t.Fatalf("空 ID 不应写入，仍应 1 条，得到 %d", len(again))
	}
}

func TestAppendDecisionTraceReturnsTrace(t *testing.T) {
	// blob 行为不变：append 仍把轨迹放进 state，且返回所追加的轨迹（供影子双写）。
	state := &State{}
	got := appendDecisionTrace(state, DecisionTrace{ID: "x", UnitID: "u"})
	if got.ID != "x" {
		t.Fatalf("应返回所追加的轨迹，得到 %q", got.ID)
	}
	if len(state.DecisionTraces) != 1 || state.DecisionTraces[0].ID != "x" {
		t.Fatalf("blob 仍应正常 append")
	}
}
