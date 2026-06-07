package session

// 文件说明：LLM 交互旁路表影子双写测试（对真实 SQLite）：
//  - persist 幂等、可完整读回、空 ID / InProgress 不写；
//  - Repository.Save 在压缩抹除旧 prompt 之前把完整 prompt 写进表（blob 仍裁剪、仍权威）；
//  - 隐私擦除 LLM 细节时同步清空旁路表（红线）；
//  - 保留期清理同时删 llm_interactions 与 decision_traces（含修复上一片的清理遗漏）。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPersistLLMInteractionsIdempotentAndSkips(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	mysql := false

	it := LLMInteraction{ID: "i1", UnitID: "u1", Kind: "decision", SystemPrompt: "系统", UserPrompt: "用户"}
	if err := persistLLMInteractions(ctx, service.db, mysql, "s1", []LLMInteraction{it}); err != nil {
		t.Fatalf("写旁路表失败: %v", err)
	}
	if err := persistLLMInteractions(ctx, service.db, mysql, "s1", []LLMInteraction{it}); err != nil {
		t.Fatalf("幂等重写失败: %v", err)
	}

	list, err := service.ListLLMInteractions(ctx, "s1", 10)
	if err != nil {
		t.Fatalf("读旁路表失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("幂等写入应只有 1 条，得到 %d", len(list))
	}
	if list[0].ID != "i1" || list[0].SystemPrompt != "系统" || list[0].UserPrompt != "用户" {
		t.Fatalf("交互应可完整读回（含 prompt）：%+v", list[0])
	}

	// 空 ID 与 InProgress 都不写。
	_ = persistLLMInteractions(ctx, service.db, mysql, "s1", []LLMInteraction{
		{ID: ""},
		{ID: "i2", InProgress: true, UnitID: "u2"},
	})
	if again, _ := service.ListLLMInteractions(ctx, "s1", 10); len(again) != 1 {
		t.Fatalf("空 ID / InProgress 不应写入，仍应 1 条，得到 %d", len(again))
	}
}

func TestSaveShadowWritesFullPromptBeforeCompaction(t *testing.T) {
	service, ctx := newCutoverService(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// 构造超过 maxFullLLMHistory 的交互：压缩会把最旧的若干条 prompt 抹成空，但旁路表必须留完整 prompt。
	state := &State{ID: "s1"}
	total := maxFullLLMHistory + 3
	for i := 0; i < total; i++ {
		state.LLMInteractions = append(state.LLMInteractions, LLMInteraction{
			ID:           "i" + string(rune('a'+i)),
			UnitID:       "u",
			Kind:         "decision",
			SystemPrompt: "SYS-长长的系统提示词",
			UserPrompt:   "USR-长长的用户提示词",
			OccurredAt:   base.Add(time.Duration(i) * time.Second),
		})
	}
	oldestID := state.LLMInteractions[0].ID

	if err := service.sessions.Save(ctx, state); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	// blob 行为不变：最旧条目的 prompt 在 blob 里应已被压缩抹空（验证我们没有改变 blob 语义）。
	var blob string
	if err := service.db.QueryRowContext(ctx, `SELECT state_json FROM single_player_sessions WHERE id = 's1'`).Scan(&blob); err != nil {
		t.Fatalf("读 blob 失败: %v", err)
	}
	if strings.Count(blob, "SYS-长长的系统提示词") != maxFullLLMHistory {
		t.Fatalf("blob 应只保留最近 %d 条完整 prompt（压缩语义不变），实际含 %d 条", maxFullLLMHistory, strings.Count(blob, "SYS-长长的系统提示词"))
	}

	// 旁路表：每条都应有完整 prompt，包括被 blob 压缩抹空的最旧那条。
	list, err := service.ListLLMInteractions(ctx, "s1", 100)
	if err != nil {
		t.Fatalf("读旁路表失败: %v", err)
	}
	if len(list) != total {
		t.Fatalf("旁路表应留全部 %d 条，得到 %d", total, len(list))
	}
	var oldest *LLMInteraction
	for i := range list {
		if list[i].ID == oldestID {
			oldest = &list[i]
		}
	}
	if oldest == nil || oldest.SystemPrompt != "SYS-长长的系统提示词" || oldest.UserPrompt != "USR-长长的用户提示词" {
		t.Fatalf("被 blob 压缩抹空的最旧交互，旁路表里仍应有完整 prompt：%+v", oldest)
	}
}

func TestPrivacyEraseClearsLLMSideTable(t *testing.T) {
	service, ctx := newCutoverService(t)

	state := &State{ID: "s1", LLMInteractions: []LLMInteraction{
		{ID: "i1", UnitID: "u", Kind: "decision", SystemPrompt: "敏感系统", UserPrompt: "敏感用户", OccurredAt: time.Now().UTC()},
	}}
	if err := service.sessions.Save(ctx, state); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if list, _ := service.ListLLMInteractions(ctx, "s1", 10); len(list) != 1 {
		t.Fatalf("前置条件：旁路表应有 1 条，得到 %d", len(list))
	}

	if _, _, err := service.EraseSessionPrivateData(ctx, "s1", PrivacyEraseOptions{EraseLLMDetails: true}); err != nil {
		t.Fatalf("隐私擦除失败: %v", err)
	}

	// 红线：擦除 LLM 细节后旁路表必须清空，完整 prompt 不得残留。
	list, err := service.ListLLMInteractions(ctx, "s1", 10)
	if err != nil {
		t.Fatalf("读旁路表失败: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("擦除后旁路表应为空（不可逆擦除），仍有 %d 条：%+v", len(list), list)
	}
}

func TestLLMInteractionOrderingFixedWidth(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 跨片一致的排序回归：整秒（无小数）vs :00.5。RFC3339Nano 变宽会把整秒排到小数秒之后（'.'<'Z'）；
	// 复用 decision_traces 的 traceTimeLayout 定宽布局修正。故意乱序写，断言 List 时间序正确。
	a := LLMInteraction{ID: "a", UnitID: "u", OccurredAt: base}
	b := LLMInteraction{ID: "b", UnitID: "u", OccurredAt: base.Add(500 * time.Millisecond)}
	if err := persistLLMInteractions(ctx, service.db, false, "s1", []LLMInteraction{b, a}); err != nil {
		t.Fatalf("写旁路表失败: %v", err)
	}
	list, err := service.ListLLMInteractions(ctx, "s1", 10)
	if err != nil {
		t.Fatalf("读旁路表失败: %v", err)
	}
	// List 按 occurred_at DESC：小数秒（较晚）应在前、整秒（较早）在后。
	if len(list) != 2 || list[0].ID != "b" || list[1].ID != "a" {
		t.Fatalf("定宽时间序应让整秒(a)排在小数秒(b)之后，得到 %+v", list)
	}
}

func TestPrivacyEraseRefusedWhileAsyncExecutionRunning(t *testing.T) {
	service, ctx := newCutoverService(t)
	state := &State{ID: "s1", LLMInteractions: []LLMInteraction{
		{ID: "i1", UnitID: "u", SystemPrompt: "敏感", OccurredAt: time.Now().UTC()},
	}}
	if err := service.sessions.Save(ctx, state); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	// 模拟后台异步执行在飞：进程级注册表标记会话执行中。
	if !markAsyncExecutionRunning("s1") {
		t.Fatalf("前置：应能标记执行中")
	}
	t.Cleanup(func() { unmarkAsyncExecutionRunning("s1") })

	// 红线：执行在飞时擦除必须被拒绝（否则后台 Save 会把完整 prompt 写回旁路表/blob）。
	if _, _, err := service.EraseSessionPrivateData(ctx, "s1", PrivacyEraseOptions{EraseLLMDetails: true}); err == nil {
		t.Fatalf("执行进行中时擦除应被拒绝，却成功了")
	}
	// 旁路表里的完整 prompt 不应被擦除流程动过（因为根本没擦）。
	if list, _ := service.ListLLMInteractions(ctx, "s1", 10); len(list) != 1 {
		t.Fatalf("被拒绝的擦除不应改动数据，仍应 1 条，得到 %d", len(list))
	}

	// 执行结束后再擦应成功。
	unmarkAsyncExecutionRunning("s1")
	if _, _, err := service.EraseSessionPrivateData(ctx, "s1", PrivacyEraseOptions{EraseLLMDetails: true}); err != nil {
		t.Fatalf("执行结束后擦除应成功: %v", err)
	}
	if list, _ := service.ListLLMInteractions(ctx, "s1", 10); len(list) != 0 {
		t.Fatalf("擦除后旁路表应为空，仍有 %d 条", len(list))
	}
}

func TestPrivacyEraseClearsDecisionTraceSideTable(t *testing.T) {
	service, ctx := newCutoverService(t)
	// decision_traces 是含 LLM 自由文本的权威读源：擦除 LLM 细节须对称清空它，否则 hydrate 下次 load 读回。
	state := &State{ID: "s1", DecisionTraces: []DecisionTrace{
		{ID: "t1", UnitID: "u", Reasoning: "敏感推理", Speak: "敏感台词", OccurredAt: time.Now().UTC()},
	}}
	service.shadowDecisionTrace(ctx, "s1", state.DecisionTraces[0])
	if err := service.sessions.Save(ctx, state); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if list, _ := service.ListDecisionTraces(ctx, "s1", 10); len(list) != 1 {
		t.Fatalf("前置：decision_traces 应有 1 条，得到 %d", len(list))
	}

	_, res, err := service.EraseSessionPrivateData(ctx, "s1", PrivacyEraseOptions{EraseLLMDetails: true})
	if err != nil {
		t.Fatalf("擦除失败: %v", err)
	}
	if res.DecisionTracesErased != 1 {
		t.Fatalf("应报告擦除 1 条决策轨迹，得到 %d", res.DecisionTracesErased)
	}

	// 表清空，且重新 load（走 hydrate）不会把它读回。
	if list, _ := service.ListDecisionTraces(ctx, "s1", 10); len(list) != 0 {
		t.Fatalf("擦除后 decision_traces 表应为空，仍有 %d 条", len(list))
	}
	loaded, _, err := service.loadSession(ctx, "s1")
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if len(loaded.DecisionTraces) != 0 {
		t.Fatalf("擦除后 hydrate 不应把决策轨迹读回，得到 %+v", loaded.DecisionTraces)
	}
}

func TestPurgeDeletesLLMAndDecisionTraceSideTables(t *testing.T) {
	service, ctx := newCutoverService(t)

	// 注入一个早已过期的会话 + 两张旁路表各一条留痕。
	oldTS := time.Now().UTC().Add(-90 * 24 * time.Hour).Format(time.RFC3339Nano)
	enc, _ := json.Marshal(&State{ID: "old"})
	if _, err := service.db.ExecContext(ctx, `INSERT INTO single_player_sessions (id, state_json, created_at, updated_at) VALUES ('old', ?, ?, ?)`, string(enc), oldTS, oldTS); err != nil {
		t.Fatalf("注入过期会话失败: %v", err)
	}
	if err := persistLLMInteractions(ctx, service.db, false, "old", []LLMInteraction{{ID: "i1", UnitID: "u", SystemPrompt: "x"}}); err != nil {
		t.Fatalf("注入 llm 留痕失败: %v", err)
	}
	service.shadowDecisionTrace(ctx, "old", DecisionTrace{ID: "t1", UnitID: "u"})

	res, err := service.PurgeExpiredSessionData(ctx, 30, 100)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if res.LLMInteractionsDeleted != 1 || res.DecisionTracesDeleted != 1 {
		t.Fatalf("应各删 1 条旁路留痕，得到 llm=%d traces=%d", res.LLMInteractionsDeleted, res.DecisionTracesDeleted)
	}

	if list, _ := service.ListLLMInteractions(ctx, "old", 10); len(list) != 0 {
		t.Fatalf("清理后 llm 旁路表应为空，仍有 %d 条", len(list))
	}
	if list, _ := service.ListDecisionTraces(ctx, "old", 10); len(list) != 0 {
		t.Fatalf("清理后 decision_traces 旁路表应为空（修复上一片遗漏），仍有 %d 条", len(list))
	}
}
