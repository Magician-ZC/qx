package session

// 文件说明：守护 EnrichUnitIdentityNarrativesBestEffort 开局/捏人身份补全主链路入口的行为契约——
// 批量去重、缓存命中跳过 LLM、失败回退本地模板、已有叙事跳过、以及 best-effort panic 不冒泡。

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"qunxiang/backend/internal/ai"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
)

// fakeBatchLLM 同时实现 completionClient 与 batchCompletionClient，
// 以便测试覆盖逐个请求与批量请求两条路径，并统计真实调用次数。
type fakeBatchLLM struct {
	singleCalls int32
	batchCalls  int32
	batchKeys   int32
	// failKeys 中的 key 会被批量路径标记为错误，验证回退分支。
	failKeys map[string]struct{}
	// failSingle 为真时 GenerateJSON 直接返回错误，验证单条回退分支。
	failSingle bool
	// panicOnSingle 为真时 GenerateJSON 触发 panic，验证 best-effort 护栏。
	panicOnSingle bool
}

func (f *fakeBatchLLM) GenerateJSON(_ context.Context, _ ai.CompletionRequest) (ai.CompletionResult, error) {
	atomic.AddInt32(&f.singleCalls, 1)
	if f.panicOnSingle {
		panic("boom: simulated llm panic")
	}
	if f.failSingle {
		return ai.CompletionResult{}, errors.New("simulated single failure")
	}
	return ai.CompletionResult{
		Provider: "fake",
		Model:    "fake-model",
		Output:   []byte(`{"biography":"这是一段足够长的人物传记，描述他在边地行伍多年的经历与谨慎稳健的作风，凡事先评估再行动。","recruitment_pitch":"看清局势再出手，跟我走。"}`),
	}, nil
}

func (f *fakeBatchLLM) GenerateJSONBatch(_ context.Context, requests []ai.BatchRequest, _ ai.BatchOptions) []ai.BatchResult {
	atomic.AddInt32(&f.batchCalls, 1)
	atomic.AddInt32(&f.batchKeys, int32(len(requests)))
	results := make([]ai.BatchResult, 0, len(requests))
	for _, request := range requests {
		if _, bad := f.failKeys[request.Key]; bad {
			results = append(results, ai.BatchResult{Key: request.Key, Err: errors.New("simulated batch failure")})
			continue
		}
		results = append(results, ai.BatchResult{
			Key: request.Key,
			Result: ai.CompletionResult{
				Provider: "fake",
				Model:    "fake-model",
				Output:   []byte(`{"biography":"批量生成的人物传记，长度足够，体现该单位会基于环境、性格与记忆自主判断，不做玩家遥控器。","recruitment_pitch":"我会先看清局势再决定。"}`),
			},
		})
	}
	return results
}

func newNarrativeTestService(t *testing.T, llm completionClient) (*Service, context.Context) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "narrative.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewServiceWithColdStore(db, llm, nil), context.Background()
}

func newNarrativeRecord(id string) *unit.Record {
	return &unit.Record{
		ID:        id,
		SessionID: "s-narrative",
		FactionID: "faction-a",
		Identity: unit.Identity{
			Name:   id,
			Gender: "female",
			Age:    24,
		},
	}
}

// TestEnrichBatch_FillsAllRecordsViaBatch 多单位走批量路径，全部被补全且只发起一次批量调用。
func TestEnrichBatch_FillsAllRecordsViaBatch(t *testing.T) {
	llm := &fakeBatchLLM{}
	service, ctx := newNarrativeTestService(t, llm)
	state := &State{}

	records := []*unit.Record{
		newNarrativeRecord("u1"),
		newNarrativeRecord("u2"),
		newNarrativeRecord("u3"),
	}
	service.EnrichUnitIdentityNarrativesBestEffort(ctx, state, records)

	for _, record := range records {
		if strings.TrimSpace(record.Identity.Biography) == "" || strings.TrimSpace(record.Identity.RecruitmentPitch) == "" {
			t.Fatalf("单位 %s 身份叙事未补全: bio=%q pitch=%q", record.ID, record.Identity.Biography, record.Identity.RecruitmentPitch)
		}
	}
	if atomic.LoadInt32(&llm.batchCalls) != 1 {
		t.Fatalf("应只发起 1 次批量调用，实际 %d", llm.batchCalls)
	}
	if atomic.LoadInt32(&llm.batchKeys) != 3 {
		t.Fatalf("批量应包含 3 条请求，实际 %d", llm.batchKeys)
	}
	if atomic.LoadInt32(&llm.singleCalls) != 0 {
		t.Fatalf("多单位不应走逐个路径，实际 %d", llm.singleCalls)
	}
}

// TestEnrichBatch_CacheHitSkipsLLM 第二次补全相同指纹的单位应命中缓存、不再发起任何 LLM 调用。
func TestEnrichBatch_CacheHitSkipsLLM(t *testing.T) {
	llm := &fakeBatchLLM{}
	service, ctx := newNarrativeTestService(t, llm)
	state := &State{}

	first := []*unit.Record{newNarrativeRecord("u1"), newNarrativeRecord("u2")}
	service.EnrichUnitIdentityNarrativesBestEffort(ctx, state, first)
	if atomic.LoadInt32(&llm.batchCalls) != 1 {
		t.Fatalf("首轮应有 1 次批量调用，实际 %d", llm.batchCalls)
	}

	// 用全新的、空叙事但同指纹（同名/同性别/同年龄/同性格）的记录复跑，应全部走缓存。
	second := []*unit.Record{newNarrativeRecord("u1"), newNarrativeRecord("u2")}
	service.EnrichUnitIdentityNarrativesBestEffort(ctx, state, second)

	for _, record := range second {
		if strings.TrimSpace(record.Identity.Biography) == "" || strings.TrimSpace(record.Identity.RecruitmentPitch) == "" {
			t.Fatalf("单位 %s 应从缓存补全", record.ID)
		}
	}
	if atomic.LoadInt32(&llm.batchCalls) != 1 {
		t.Fatalf("第二轮应全命中缓存、不新增批量调用，实际累计 %d", llm.batchCalls)
	}
	if atomic.LoadInt32(&llm.singleCalls) != 0 {
		t.Fatalf("缓存命中不应触发逐个调用，实际 %d", llm.singleCalls)
	}
}

// TestEnrichBatch_BatchFailureFallsBackToTemplate 批量结果失败的单位应落本地模板回退，不留空。
func TestEnrichBatch_BatchFailureFallsBackToTemplate(t *testing.T) {
	llm := &fakeBatchLLM{failKeys: map[string]struct{}{"u2": {}}}
	service, ctx := newNarrativeTestService(t, llm)
	state := &State{}

	records := []*unit.Record{
		newNarrativeRecord("u1"),
		newNarrativeRecord("u2"),
	}
	service.EnrichUnitIdentityNarrativesBestEffort(ctx, state, records)

	if strings.TrimSpace(records[1].Identity.Biography) == "" || strings.TrimSpace(records[1].Identity.RecruitmentPitch) == "" {
		t.Fatalf("批量失败单位应回退本地模板，不应留空")
	}
	if records[1].Identity.Biography != fallbackUnitBiography(*records[1]) {
		t.Fatalf("批量失败单位应使用 fallbackUnitBiography，实际 %q", records[1].Identity.Biography)
	}
}

// TestEnrichBatch_SkipsAlreadyFilled 已有完整叙事的单位应被跳过、不进入 LLM 计划。
func TestEnrichBatch_SkipsAlreadyFilled(t *testing.T) {
	llm := &fakeBatchLLM{}
	service, ctx := newNarrativeTestService(t, llm)
	state := &State{}

	filled := newNarrativeRecord("u-filled")
	filled.Identity.Biography = "既有传记，已经写满了，不该被覆盖。"
	filled.Identity.RecruitmentPitch = "既有招募词。"
	pending := newNarrativeRecord("u-pending")

	service.EnrichUnitIdentityNarrativesBestEffort(ctx, state, []*unit.Record{filled, pending})

	if filled.Identity.Biography != "既有传记，已经写满了，不该被覆盖。" {
		t.Fatalf("已有叙事的单位不应被覆盖，实际 %q", filled.Identity.Biography)
	}
	// 只有一个待补全单位 → 走逐个路径（singleCalls=1），批量不触发。
	if atomic.LoadInt32(&llm.singleCalls) != 1 {
		t.Fatalf("仅 1 个待补全单位应走逐个路径 1 次，实际 %d", llm.singleCalls)
	}
	if atomic.LoadInt32(&llm.batchCalls) != 0 {
		t.Fatalf("仅 1 个待补全单位不应走批量，实际 %d", llm.batchCalls)
	}
}

// TestEnrichBatch_SingleFailureFallsBack 单条路径失败也应回退本地模板。
func TestEnrichBatch_SingleFailureFallsBack(t *testing.T) {
	llm := &fakeBatchLLM{failSingle: true}
	service, ctx := newNarrativeTestService(t, llm)
	state := &State{}

	record := newNarrativeRecord("u-solo")
	service.EnrichUnitIdentityNarrativesBestEffort(ctx, state, []*unit.Record{record})

	if record.Identity.Biography != fallbackUnitBiography(*record) {
		t.Fatalf("单条失败应回退本地模板，实际 %q", record.Identity.Biography)
	}
	if strings.TrimSpace(record.Identity.RecruitmentPitch) == "" {
		t.Fatalf("单条失败回退后招募词不应为空")
	}
}

// TestEnrichBatch_PanicIsRecovered LLM 内部 panic 必须被 best-effort 护栏 recover，不冒泡。
func TestEnrichBatch_PanicIsRecovered(t *testing.T) {
	llm := &fakeBatchLLM{panicOnSingle: true}
	service, ctx := newNarrativeTestService(t, llm)
	state := &State{}

	record := newNarrativeRecord("u-panic")
	// 不应 panic；若冒泡测试进程会崩。
	service.EnrichUnitIdentityNarrativesBestEffort(ctx, state, []*unit.Record{record})

	// 护栏应在 state 上留一条日志。
	found := false
	for _, entry := range state.Logs {
		if entry.Kind == "unit_profile_panic" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("panic 被 recover 后应在 state.Logs 留 unit_profile_panic 日志")
	}
}

// TestEnrichBatch_NilAndEmptyInputsAreSafe nil service/空切片/含 nil 记录的输入都不应 panic。
func TestEnrichBatch_NilAndEmptyInputsAreSafe(t *testing.T) {
	llm := &fakeBatchLLM{}
	service, ctx := newNarrativeTestService(t, llm)

	service.EnrichUnitIdentityNarrativesBestEffort(ctx, nil, nil)
	service.EnrichUnitIdentityNarrativesBestEffort(ctx, &State{}, []*unit.Record{})
	service.EnrichUnitIdentityNarrativesBestEffort(ctx, &State{}, []*unit.Record{nil, nil})

	var nilService *Service
	nilService.EnrichUnitIdentityNarrativesBestEffort(ctx, &State{}, []*unit.Record{newNarrativeRecord("u1")})
}
